package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
)

func spawnTestToolCall(name, args string) messages.ChatMessage {
	return messages.ChatMessage{
		Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
		ToolCalls: []messages.ChatMessageToolCall{{ID: "call-" + name, Name: name, Arguments: args}},
	}
}

func spawnTestReply(text string) messages.ChatMessage {
	return messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: text, StopReason: messages.StopReasonEndTurn}
}

// newSpawnTestParent builds a parent conversation on a memory store with a
// probe tool that counts its calls, an unrelated tool, and the spawn tool.
func newSpawnTestParent(t *testing.T, model llm.LLM, hits *int) (*conversationState, sessions.SessionStore) {
	t.Helper()
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "parent-work")
	registry := tools.NewToolRegistry([]tools.Tool{
		&tools.Func{Name: "probe", Desc: "probe", Run: func(context.Context, tools.Args) (string, error) { *hits++; return "pong", nil }},
		&tools.Func{Name: "other", Desc: "other"},
	})
	artifactStore := session.ArtifactStore()
	parent := &conversationState{
		sessionStore: store, session: session, artifactStore: artifactStore, toolRegistry: registry,
		agent:    llm.NewAgent(model, registry, llm.AgentConfig{ArtifactStore: artifactStore}),
		settings: Settings{Model: "test/model", MaxTokens: 128, MaxIterations: 10},
	}
	registerSpawnTool(parent, &Config{}, model)
	return parent, store
}

func readChildSession(t *testing.T, store sessions.SessionStore, name string) (*sessions.Metadata, []messages.ChatMessage) {
	t.Helper()
	child, err := store.Acquire(context.Background(), name, sessions.AcquireOptions{})
	if err != nil {
		t.Fatalf("the child session %q is not on the store: %v", name, err)
	}
	defer child.Close()
	md, err := child.GetMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	history, err := child.GetHistory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return md, history
}

func TestSpawnRunnerRunsAChildOnItsOwnSession(t *testing.T) {
	hits := 0
	model := &scriptedStreamLLM{
		responses: []messages.ChatMessage{spawnTestToolCall("probe", `{}`), spawnTestReply("found it")},
		failErr:   errors.New("too many calls"),
	}
	parent, store := newSpawnTestParent(t, model, &hits)
	if _, ok := parent.toolRegistry.Get(subagent.ToolName); !ok {
		t.Fatal("the parent has no spawn tool")
	}
	run := spawnRunner(&Config{}, model, parent)

	res, err := run(context.Background(), subagent.Request{Task: "look around", Label: "explore", Tools: []string{"probe"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "found it" || res.Session == "" || hits != 1 {
		t.Fatalf("result = %+v, probe hits %d", res, hits)
	}

	md, history := readChildSession(t, store, res.Session)
	if md.Parent != "parent-work" || md.Description != "explore" || md.Model != "test/model" {
		t.Fatalf("child metadata = parent %q description %q model %q", md.Parent, md.Description, md.Model)
	}
	if len(history) != 4 || history[0].Content != "look around" || history[3].Content != "found it" {
		t.Fatalf("child history = %d messages: %+v", len(history), history)
	}
	parentHistory, err := parent.session.GetHistory(context.Background())
	if err != nil || len(parentHistory) != 0 {
		t.Fatalf("the child's turn reached the parent's transcript: %d messages, %v", len(parentHistory), err)
	}
	if _, ok := parent.toolRegistry.Get("other"); !ok {
		t.Fatal("the child's tool view changed the parent's tools")
	}
}

func TestSpawnRunnerAcceptsAToolListOfAgentBuiltins(t *testing.T) {
	hits := 0
	model := &scriptedStreamLLM{responses: []messages.ChatMessage{spawnTestReply("described")}, failErr: errors.New("too many calls")}
	parent, _ := newSpawnTestParent(t, model, &hits)
	run := spawnRunner(&Config{}, model, parent)

	res, err := run(context.Background(), subagent.Request{Task: "describe the shot", Tools: []string{"view_image"}})
	if err != nil {
		t.Fatalf("a brief naming only a built-in was refused: %v", err)
	}
	if res.Text != "described" || hits != 0 {
		t.Fatalf("result = %+v, probe hits %d", res, hits)
	}
}

func TestSpawnRunnerRefusesAnUnknownToolList(t *testing.T) {
	hits := 0
	model := &scriptedStreamLLM{failErr: errors.New("no model call expected")}
	parent, store := newSpawnTestParent(t, model, &hits)
	run := spawnRunner(&Config{}, model, parent)

	_, err := run(context.Background(), subagent.Request{Task: "look", Tools: []string{"nope"}})
	if !errors.Is(err, subagent.ErrNoMatchingTools) {
		t.Fatalf("err = %v, want ErrNoMatchingTools", err)
	}
	all, err := store.GetAllMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("%d sessions on the store after a refused spawn, want the parent alone", len(all))
	}
}

type denyingTurnUI struct {
	childTurnUI
	approvals int
}

func (d *denyingTurnUI) ApproveToolCalls(calls []messages.ChatMessageToolCall) []bool {
	d.approvals++
	return make([]bool, len(calls))
}

func TestSpawnRunnerForwardsApprovalsToTheParentTurnUI(t *testing.T) {
	hits := 0
	model := &scriptedStreamLLM{
		responses: []messages.ChatMessage{spawnTestToolCall("probe", `{}`), spawnTestReply("gave up")},
		failErr:   errors.New("too many calls"),
	}
	parent, _ := newSpawnTestParent(t, model, &hits)
	parentUI := &denyingTurnUI{}
	run := spawnRunner(&Config{Confirm: true}, model, parent)

	// A batch the parent's UI denies in full ends the child's run, as it
	// would any turn; the probe never ran.
	res, err := run(withParentTurnUI(context.Background(), parentUI), subagent.Request{Task: "look"})
	if err != nil {
		t.Fatal(err)
	}
	if parentUI.approvals != 1 || hits != 0 || res.Session == "" {
		t.Fatalf("approvals %d, probe hits %d, session %q; want the parent's UI to deny the call", parentUI.approvals, hits, res.Session)
	}
}

func TestChildTurnUIKeepsTheFinalReply(t *testing.T) {
	ui := &childTurnUI{}
	ui.AppendAssistantText("let me look")
	ui.AppendToolStart(nil)
	ui.AppendAssistantText("the ")
	ui.AppendAssistantText("answer")
	ui.RecordTurnTokens(5, 2)
	if reply, in, out := ui.result(); reply != "the answer" || in != 5 || out != 2 {
		t.Fatalf("unfinished result = %q %d %d", reply, in, out)
	}
	ui.FinishTextTurn()
	if reply, _, _ := ui.result(); reply != "the answer" {
		t.Fatalf("finished result = %q", reply)
	}
	if got := ui.ApproveToolCalls([]messages.ChatMessageToolCall{{Name: "bash"}}); len(got) != 1 || !got[0] {
		t.Fatal("a child without a parent UI should approve")
	}
}

func TestSpawnAgentToolRunsInsideAParentTurn(t *testing.T) {
	hits := 0
	model := &scriptedStreamLLM{
		responses: []messages.ChatMessage{
			spawnTestToolCall(subagent.ToolName, `{"task":"look around","label":"explore","tools":["probe"]}`),
			spawnTestToolCall("probe", `{}`),
			spawnTestReply("found it"),
			spawnTestReply("the agent says: found it"),
		},
		failErr: errors.New("too many calls"),
	}
	parent, store := newSpawnTestParent(t, model, &hits)
	parentUI := &childTurnUI{}
	userMsg := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "delegate this"}
	if _, err := executeTurnWithUserMessage(context.Background(), &Config{}, parent, userMsg, nil, nil, parentUI, false); err != nil {
		t.Fatal(err)
	}
	if reply, _, _ := parentUI.result(); reply != "the agent says: found it" || hits != 1 {
		t.Fatalf("parent reply %q, probe hits %d", reply, hits)
	}
	history, err := parent.session.GetHistory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var toolResult string
	for _, m := range history {
		if m.Role == messages.MessageRoleTool && m.ToolName == subagent.ToolName {
			toolResult = m.GetContent()
		}
	}
	if !strings.HasPrefix(toolResult, "found it\n\n(agent session ") {
		t.Fatalf("spawn tool result = %q", toolResult)
	}
	name := strings.TrimSuffix(strings.TrimPrefix(toolResult, "found it\n\n(agent session "), ")")
	md, _ := readChildSession(t, store, name)
	if md.Parent != "parent-work" || md.Description != "explore" {
		t.Fatalf("child metadata = %+v", md)
	}
}

func TestSpawnedChildOpensOnTheParentsPersonaAndDisplayContract(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, persona := range []string{"be a pirate", ""} {
		name := "persona"
		if persona == "" {
			name = "coding"
		}
		t.Run(name, func(t *testing.T) {
			// The store seeds every new session with the launch persona; the
			// child must open on the parent's, which a resumed context may
			// have changed or cleared.
			store := testOpenMemoryStore(t, &sessions.Metadata{SystemPrompt: "launch persona"})
			session := testAcquireSession(t, store, "parent-work")
			registry := tools.NewToolRegistry(nil)
			artifactStore := session.ArtifactStore()
			model := &captureCompletionLLM{response: spawnTestReply("done")}
			parent := &conversationState{
				sessionStore: store, session: session, artifactStore: artifactStore, toolRegistry: registry,
				agent:           llm.NewAgent(model, registry, llm.AgentConfig{ArtifactStore: artifactStore}),
				displayContract: richTerminalDisplayContract,
				settings:        Settings{Model: "test/model", MaxTokens: 128, MaxIterations: 10, SystemPrompt: persona},
			}
			registerSpawnTool(parent, &Config{}, model)
			run := spawnRunner(&Config{}, model, parent)

			res, err := run(context.Background(), subagent.Request{Task: "look around"})
			if err != nil || res.Text != "done" {
				t.Fatalf("result = %+v, err %v", res, err)
			}
			request := projectedRequestText(model.request)
			if len(model.request) == 0 || model.request[0].Role != messages.MessageRoleSystem {
				t.Fatalf("the child's request has no system message: %+v", model.request)
			}
			if strings.Contains(request, "launch persona") {
				t.Fatalf("the child opened on the store's launch persona instead of the parent's: %q", request)
			}
			for _, want := range []string{richTerminalDisplayContract, contextMechanicsContract} {
				if !strings.Contains(request, want) {
					t.Fatalf("the child's request lacks %q:\n%s", want, request)
				}
			}
			md, history := readChildSession(t, store, res.Session)
			if md.SystemPrompt != persona {
				t.Fatalf("child metadata persona = %q, want %q", md.SystemPrompt, persona)
			}
			if persona != "" {
				if !strings.Contains(request, persona) || strings.Contains(request, codingContract) {
					t.Fatalf("a persona child's request should carry the persona and no coding contract:\n%s", request)
				}
				if len(history) != 3 || history[0].Role != messages.MessageRoleSystem || history[0].Content != persona || history[1].Content != "look around" {
					t.Fatalf("persona child history = %+v", history)
				}
				return
			}
			if !strings.Contains(request, codingContract) {
				t.Fatalf("a child without a persona should get the coding contract:\n%s", request)
			}
			if len(history) != 2 || history[0].Content != "look around" {
				t.Fatalf("coding child history = %+v", history)
			}
		})
	}
}
