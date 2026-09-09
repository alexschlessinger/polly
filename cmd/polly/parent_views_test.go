package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestSwarmMembersTextLeadsWithParent(t *testing.T) {
	s := &swarm.State{Members: map[string]*swarm.Member{"m": {ID: "m", Name: "worker"}}}
	parent := swarm.AgentPresentation{Lifecycle: swarm.LifecycleIdle, Detail: "awaiting review", Display: "idle · awaiting review"}
	if text := swarmInspectorText(s, &parent, "members"); !strings.HasPrefix(text, "Parent — idle · awaiting review\n\n1 members") {
		t.Fatalf("members text: %q", text)
	}
	if text := swarmInspectorText(s, nil, "members"); strings.Contains(text, "Parent —") {
		t.Fatalf("read-only view invented a parent: %q", text)
	}
	if text := swarmInspectorText(s, &parent, "tasks"); strings.Contains(text, "Parent —") {
		t.Fatalf("task section carries the parent: %q", text)
	}
}

// A parent turn's outcome reaches the status row, the picker's root row and
// the root's inspector header; a session that never delegated shows nothing.
func TestParentLifecycleFollowsTurnOutcome(t *testing.T) {
	for _, tc := range []struct {
		name         string
		task, cancel bool
		want, agents string
		listed       bool
	}{
		{name: "no swarm", want: "idle"},
		{name: "blocked", task: true, want: "paused · blocked", agents: "swarm paused · blocked", listed: true},
		{name: "interrupted", task: true, cancel: true, want: "paused · interrupted", agents: "swarm paused · interrupted", listed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			model := integrationModel(func(mctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
				if tc.cancel {
					cancel()
					<-mctx.Done()
				}
				return spawnTestReply("done")
			})
			r := newSwarmTestREPL(t, model, nil)
			if tc.task {
				if _, err := r.state.swarm.CreateTask(context.Background(), "pending work", "review", nil, ""); err != nil {
					t.Fatal(err)
				}
			}
			_, err := executeTurnWithUserMessage(ctx, r.config, r.state, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "go"}, nil, nil, &collectingTurnUI{}, false)
			if (err == nil) != (tc.want == "idle") {
				t.Fatalf("turn error: %v", err)
			}
			s := waitSwarmIdle(t, r.state.swarm)
			if p := r.state.swarm.ParentState(s); p.Display != tc.want || p.Busy {
				t.Fatalf("parent after the turn: %+v", p)
			}
			refreshPickerSwarm(t, r)
			r.model.mu.Lock()
			r.openSessionsPicker()
			item := pickerItem(t, r.model.modal, r.visibleTab().viewID())
			agents, _ := r.agentsStatus()
			r.model.mu.Unlock()
			if agents != tc.agents {
				t.Fatalf("status row agents = %q, want %q", agents, tc.agents)
			}
			if strings.Contains(item.label, tc.want) != tc.listed {
				t.Fatalf("picker root row: %+v", item)
			}
			r.inspect(viewTarget{session: sessions.ViewTarget{ID: r.visibleTab().viewID(), Name: r.visibleTab().name}})
			waitInspector(t, r, 180)
			header := plainStyledText(r.inspectorHeader(180, 20, 0, 0).text)
			if strings.Contains(header, tc.want) != tc.listed {
				t.Fatalf("root inspector header: %q", header)
			}
			if r.state.swarm.HasActive() {
				t.Fatal("display work woke an agent")
			}
		})
	}
}

// Reading views repaint from records; they never wake a member or ask the model.
func TestSwarmDisplayRefreshDoesNotWakeMembers(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{}, 1)
	r := newSwarmTestREPL(t, integrationModel(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		calls.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		return spawnTestReply("done")
	}), nil)
	r.runTabCommand("/spawn --read-only inspect the fixture")
	runUITask(t, r)
	<-started
	s := waitSwarmIdle(t, r.state.swarm)
	before := calls.Load()
	var member string
	for id := range s.Members {
		member = id
	}
	for range 3 {
		refreshPickerSwarm(t, r)
		r.model.mu.Lock()
		r.openSessionsPicker()
		pickerItem(t, r.model.modal, member)
		r.agentsStatus()
		r.model.mu.Unlock()
		parent := r.state.swarm.ParentState(s)
		swarmInspectorText(s, &parent, "members")
		swarmInspectorText(s, &parent, "tasks")
	}
	after := waitSwarmIdle(t, r.state.swarm)
	if calls.Load() != before || len(after.Executions) != 1 || r.state.swarm.HasActive() {
		t.Fatalf("display refresh changed the swarm: calls %d→%d executions=%d", before, calls.Load(), len(after.Executions))
	}
	if p := swarm.MemberState(after, after.Members[member]); p.Display != "idle · awaiting review" {
		t.Fatalf("member after refreshes: %+v", p)
	}
}

// A root whose records predate the format record does not open.
func TestOpenFailsWhenSwarmFormatUnsupported(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "parent-work")
	state := r.state
	state.settings = Settings{Model: "test/model", MaxTokens: 128, MaxIterations: 10}
	state.toolRegistry = tools.NewToolRegistry(nil, tools.WithUnsafeNoSandbox())
	ctx := context.Background()
	if err := state.session.(sessions.CoordinationSession).UpdateCoordination(ctx, func(s *sessions.CoordinationState) error {
		s.Records["member"] = map[string]json.RawMessage{"m": json.RawMessage(`{"id":"m","status":"idle"}`)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	err := registerSwarm(state, r.config, integrationModel(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return spawnTestReply("unused") }))
	if !errors.Is(err, swarm.ErrUnsupportedFormat) || state.swarm != nil {
		t.Fatalf("legacy root registered a swarm: %v", err)
	}
	if !strings.Contains(err.Error(), "no format record") {
		t.Fatalf("error does not name the cause: %v", err)
	}
}
