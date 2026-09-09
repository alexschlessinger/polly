package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/swarm"
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

type denyingTurnUI struct {
	childTurnUI
	approvals int
}

func (d *denyingTurnUI) ApproveToolCalls(calls []messages.ChatMessageToolCall) []bool {
	d.approvals++
	return make([]bool, len(calls))
}

// collectingTurnUI retains the parent's final text for integration assertions.
type collectingTurnUI struct {
	childTurnUI
	mu   sync.Mutex
	text strings.Builder
}

func (u *collectingTurnUI) AppendAssistantText(content string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.text.WriteString(content)
}

func (u *collectingTurnUI) AppendToolStart([]messages.ChatMessageToolCall) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.text.Reset()
}

func (u *collectingTurnUI) result() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.text.String()
}

// Both command and model launch tests use the production swarm runtime. Tabs
// are only the parent screen; no child execution or report delivery is mocked.
func newSwarmTestREPL(t *testing.T, model llm.LLM, configure func(*swarm.Config)) *managedREPL {
	t.Helper()
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "parent-work")
	state := r.state
	state.settings = Settings{Model: "test/model", MaxTokens: 128, MaxIterations: 10}
	registry := tools.NewToolRegistry(nil, tools.WithUnsafeNoSandbox())
	state.toolRegistry = registry
	state.agent = llm.NewAgent(model, registry, llm.AgentConfig{ArtifactStore: state.artifactStore})
	c := swarm.Config{Store: state.sessionStore, Parent: state.session, Registry: registry, Client: model,
		Root: t.TempDir(), Directory: filepath.Join(t.TempDir(), "runtime"),
		Request: *createCompletionRequest(r.config, &state.settings, nil, registry, nil, nil),
		Agent:   llm.AgentConfig{MaxIterations: 10}, Callbacks: memberCallbacks(r.config, state)}
	if configure != nil {
		configure(&c)
	}
	runtime, err := swarm.New(c)
	if err != nil {
		t.Fatal(err)
	}
	state.swarm = runtime
	runtime.RegisterParentTools(registry)
	updateSwarmDefaults(state, &c.Request, state.settings)
	return r
}

func waitSwarmIdle(t *testing.T, runtime *swarm.Runtime) *swarm.State {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for runtime.HasActive() {
		select {
		case <-ctx.Done():
			t.Fatal("swarm did not settle")
		case <-time.After(time.Millisecond):
		}
	}
	s, err := runtime.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestModelSpawnUsesParentSwarmAndPrivateSession(t *testing.T) {
	var r *managedREPL
	var calls atomic.Int32
	model := integrationModel(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		switch calls.Add(1) {
		case 1:
			return spawnTestToolCall(subagent.ToolName, `{"task":"look around","label":"explore","read_only":true}`)
		case 2:
			return spawnTestToolCall("list_agents", `{}`)
		case 3:
			return spawnTestReply("found it")
		case 4:
			s, err := r.state.swarm.State(ctx)
			if err != nil {
				t.Error(err)
				return spawnTestReply("blocked")
			}
			for _, task := range s.Tasks {
				return spawnTestToolCall("swarm_review", tools.Result(map[string]any{"task": task.ID, "revision": task.Revision, "accept": true}))
			}
		case 5:
			return spawnTestReply("the agent says: found it")
		}
		t.Errorf("unexpected model call %d", calls.Load())
		return spawnTestReply("blocked")
	})
	r = newSwarmTestREPL(t, model, nil)
	parentUI := &collectingTurnUI{}
	_, err := executeTurnWithUserMessage(context.Background(), r.config, r.state, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "delegate this"}, nil, nil, parentUI, false)
	if err != nil {
		t.Fatal(err)
	}
	if reply := parentUI.result(); reply != "the agent says: found it" {
		t.Fatal(reply)
	}
	s := waitSwarmIdle(t, r.state.swarm)
	if len(s.Members) != 1 || len(s.Executions) != 1 || len(r.tabs) != 1 {
		t.Fatalf("unexpected launch state: %+v", s)
	}
	for _, member := range s.Members {
		view, err := r.state.sessionStore.(sessions.ViewStore).ReadView(context.Background(), sessions.ViewTarget{ID: member.ID}, "")
		if err != nil {
			t.Fatal(err)
		}
		if view.Metadata.SwarmID != r.state.swarm.ID || view.Metadata.Parent != "parent-work" || view.Metadata.Description != "explore" {
			t.Fatalf("metadata: %+v", view.Metadata)
		}
		session, err := r.state.sessionStore.Acquire(context.Background(), member.Name, sessions.AcquireOptions{ExistingOnly: true, ExpectedID: member.ID})
		if err != nil {
			t.Fatal(err)
		}
		history := testSessionHistory(t, session)
		session.Close()
		foundIdentity := false
		for _, message := range history {
			if message.ToolName == "list_agents" {
				if !strings.Contains(message.Content, `"self":"`+member.ID+`"`) {
					t.Fatalf("caller identity leaked: %s", message.Content)
				}
				foundIdentity = true
			}
		}
		if !foundIdentity {
			t.Fatal("member did not query its own roster")
		}
	}
}

func TestSwarmMemberApprovalsReachParentUI(t *testing.T) {
	for _, host := range []string{"turn", "idle screen"} {
		t.Run(host, func(t *testing.T) {
			model := &scriptedStreamLLM{responses: []messages.ChatMessage{spawnTestToolCall("list_agents", `{}`), spawnTestReply("gave up")}, failErr: errors.New("unexpected request")}
			r := newSwarmTestREPL(t, model, nil)
			parentUI := &denyingTurnUI{}
			ctx := context.Background()
			if host == "turn" {
				ctx = withParentTurnUI(ctx, parentUI)
			} else {
				r.state.setMemberUI(parentUI)
			}
			res, err := r.state.swarm.Spawn(ctx, subagent.Request{Task: "look", ReadOnly: true})
			if !errors.Is(err, swarm.ErrEmptyResult) {
				t.Fatalf("denied tool-only result must be incomplete: %v", err)
			}
			if parentUI.approvals != 1 || model.calls != 1 || res.Session == "" {
				t.Fatalf("approvals %d, model calls %d, result %+v", parentUI.approvals, model.calls, res)
			}
			state := waitSwarmIdle(t, r.state.swarm)
			member := state.Members[res.Session]
			if state.Executions[member.Execution].Status != "failed" || state.Tasks[member.Task].Status != "blocked" {
				t.Fatalf("denial reported a successful task: %+v", state)
			}
		})
	}
}
