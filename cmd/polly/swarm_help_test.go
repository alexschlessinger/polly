package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/swarm"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestSwarmHelpPromptBoundaries(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, tc := range []struct {
		name       string
		persona    string
		filterHelp bool
		noSwarm    bool
		schema     *llm.Schema
		wantHelp   bool
	}{
		{name: "parent", wantHelp: true},
		{name: "unavailable", noSwarm: true},
		{name: "filtered", filterHelp: true},
		{name: "custom persona", persona: "Translate the user's text into French."},
		{name: "schema", schema: llm.SchemaFromJSON(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSwarmTestREPL(t, integrationModel(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
				return spawnTestReply("done")
			}), nil)
			state := r.state
			state.settings.SystemPrompt = tc.persona
			if tc.noSwarm {
				state.toolRegistry = tools.NewToolRegistry(nil)
				t.Cleanup(func() { state.toolRegistry.Close() })
			} else if tc.filterHelp {
				state.toolRegistry = state.toolRegistry.Derive(tools.AllowTools("spawn_agent"))
				t.Cleanup(func() { state.toolRegistry.Close() })
			}
			turn := &turnExecution{ctx: context.Background(), state: state, settings: &state.settings, schema: tc.schema, userMsg: messages.ChatMessage{Role: messages.MessageRoleUser, Content: "hello"}}
			request, _, err := turn.prepareRequest()
			if err != nil {
				t.Fatal(err)
			}
			var system string
			for _, message := range request {
				if message.Role == messages.MessageRoleSystem {
					system += message.Content
				}
			}
			if strings.Contains(system, swarmHelpContract) != tc.wantHelp {
				t.Fatalf("help reminder present = %v, want %v: %q", strings.Contains(system, swarmHelpContract), tc.wantHelp, system)
			}
			if tc.schema == nil && tc.persona == "" && !strings.Contains(system, codingContract) {
				t.Fatal("default coding contract lost")
			}
			if (tc.schema != nil || tc.persona != "") && strings.Contains(system, codingContract) {
				t.Fatal("coding defaults leaked into schema or custom persona")
			}
		})
	}
}

func TestHelpResultsRetainedAsToolResultWithStablePrefix(t *testing.T) {
	for _, helpName := range []string{"swarm_help", "workflow_help"} {
		t.Run(helpName, func(t *testing.T) {
			t.Chdir(t.TempDir())
			var systems, definitions, keys []string
			var guide string
			model := integrationModel(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				var system []string
				helpResults := 0
				for _, message := range req.Messages {
					if message.Role == messages.MessageRoleSystem {
						system = append(system, message.Content)
					}
					if message.Role == messages.MessageRoleTool && message.ToolName == helpName {
						helpResults++
						guide = message.Content
					}
				}
				systems = append(systems, strings.Join(system, "\n"))
				toolSchemas := map[string]*schema.ToolSchema{}
				for _, tool := range req.Tools {
					toolSchemas[tool.GetName()] = tool.GetSchema()
				}
				for _, name := range []string{"swarm_help", "workflow_help", "spawn_agent", "wait_agent", "swarm_integrate", "workflow_run"} {
					if toolSchemas[name] == nil {
						t.Errorf("coordination tool %q unavailable", name)
					}
				}
				data, err := json.Marshal(toolSchemas)
				if err != nil {
					t.Error(err)
				}
				definitions = append(definitions, string(data))
				keys = append(keys, req.PromptCacheKey)
				if len(systems) == 1 || len(systems) == 4 {
					if helpResults != 0 {
						t.Error("guide loaded without a tool call")
					}
					return spawnTestToolCall(helpName, `{}`)
				}
				if helpResults != 1 {
					t.Errorf("retained guide results = %d, want 1", helpResults)
				}
				return spawnTestReply("done")
			})
			r := newSwarmTestREPL(t, model, nil)
			for _, prompt := range []string{"Plan useful independent investigations.", "Continue using the guide already in history."} {
				code, err := executeTurnWithUserMessage(context.Background(), r.config, r.state, messages.ChatMessage{Role: messages.MessageRoleUser, Content: prompt}, nil, nil, &collectingTurnUI{}, false)
				if err != nil || code != 0 {
					t.Fatalf("turn = %d, %v", code, err)
				}
			}
			if len(systems) != 3 || keys[0] == "" || guide == "" {
				t.Fatalf("missing request or guide: requests=%d keys=%v guide=%q", len(systems), keys, guide)
			}
			for i := range systems {
				if systems[i] != systems[0] || definitions[i] != definitions[0] || keys[i] != keys[0] {
					t.Fatalf("request %d changed system, tools, or cache key after help", i)
				}
				if !strings.Contains(systems[i], swarmHelpContract) || strings.Contains(systems[i], guide) {
					t.Fatal("guide entered the system prompt or displaced the reminder")
				}
			}
			history := testSessionHistory(t, r.state.session)
			count := 0
			for _, message := range history {
				if message.Role == messages.MessageRoleSystem {
					t.Fatal("send-time guidance was stored as system history")
				}
				if message.Role == messages.MessageRoleTool && message.ToolName == helpName {
					count++
					if message.Content != guide {
						t.Fatal("persisted guide differs from the ordinary tool result")
					}
				}
			}
			if count != 1 {
				t.Fatalf("persisted guide count = %d", count)
			}

			// Push the original guide out through normal history projection. It stays
			// in durable history, but the next request must reload it as ordinary output.
			if err := r.state.session.AddMessages(context.Background(), []messages.ChatMessage{
				{Role: messages.MessageRoleUser, Content: strings.Repeat("old task context ", 20_000)},
				spawnTestReply("old task finished"),
			}); err != nil {
				t.Fatal(err)
			}
			r.state.settings.MaxHistoryTokens = 20_000
			code, err := executeTurnWithUserMessage(context.Background(), r.config, r.state, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "Coordinate another investigation."}, nil, nil, &collectingTurnUI{}, false)
			if err != nil || code != 0 {
				t.Fatalf("turn after trimming = %d, %v", code, err)
			}
			if len(systems) != 5 || !strings.Contains(systems[3], swarmHelpContract) || systems[3] != systems[4] || definitions[3] != definitions[4] || keys[3] != keys[4] {
				t.Fatal("guide reload changed the prefix or lost the persistent reminder")
			}
			count = 0
			for _, message := range testSessionHistory(t, r.state.session) {
				if message.Role == messages.MessageRoleTool && message.ToolName == helpName {
					count++
				}
			}
			if count != 2 {
				t.Fatalf("durable history lost the old guide or duplicated its reload: %d", count)
			}
		})
	}
}

func TestOrdinaryTurnDoesNotRequireSwarmHelp(t *testing.T) {
	t.Chdir(t.TempDir())
	calls := 0
	r := newSwarmTestREPL(t, integrationModel(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		calls++
		return spawnTestReply("Use len(slice) to get its length.")
	}), nil)
	code, err := executeTurnWithUserMessage(context.Background(), r.config, r.state, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "How do I get a Go slice's length?"}, nil, nil, &collectingTurnUI{}, false)
	if err != nil || code != 0 || calls != 1 {
		t.Fatalf("ordinary answer required coordination: code=%d calls=%d err=%v", code, calls, err)
	}
}

func TestSwarmChildKeepsSharedContractAndSavedPrompt(t *testing.T) {
	t.Chdir(t.TempDir())
	var prompts []string
	model := integrationModel(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		var system string
		for _, message := range req.Messages {
			if message.Role == messages.MessageRoleSystem {
				system += message.Content
			}
		}
		prompts = append(prompts, system)
		for _, want := range []string{codingContract, "Your identity is", "Work in", "completion requirement", "Private conversations remain private", "host's model-call limit", "user-directed client action"} {
			if !strings.Contains(system, want) {
				t.Errorf("child prompt lacks %q", want)
			}
		}
		if strings.Contains(system, swarmHelpContract) || strings.Contains(system, "Basic sequences:") {
			t.Error("child inherited the parent guide or reminder")
		}
		for _, tool := range req.Tools {
			if tool.GetName() == "swarm_help" || tool.GetName() == "workflow_help" {
				t.Error("child can load parent guidance")
			}
		}
		return spawnTestReply("saved finding")
	})
	r := newSwarmTestREPL(t, model, nil)
	first, err := r.state.swarm.Agent(context.Background(), "", swarm.AgentRequest{Label: "Research", Task: "Investigate the assigned scope.", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	r.state.settings.SystemPrompt = "Later parent persona must not replace saved child instructions."
	updateSwarmDefaults(r.state, createCompletionRequest(r.config, &r.state.settings, nil, r.state.toolRegistry, nil, nil), r.state.settings)
	if _, err := r.state.swarm.Agent(context.Background(), "", swarm.AgentRequest{Session: first.Session, Task: "Continue the investigation."}); err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 || prompts[0] != prompts[1] {
		t.Fatalf("continuation replaced child prompt: %q", prompts)
	}
}
