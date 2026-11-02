package main

import (
	"context"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestInspectorLegacyOverviewMatchesAugmentedAssignment(t *testing.T) {
	for _, input := range []any{nil, map[string]any{"path": "main.go", "revision": float64(2)}} {
		t.Run(tools.Result(input), func(t *testing.T) {
			store := testOpenMemoryStore(t, nil)
			child := testAcquireSession(t, store, "legacy-child")
			id := child.(sessions.ViewIdentity).ViewID()
			brief := "Review the implementation"
			if input != nil {
				brief += "\n\nInput:\n" + tools.Result(input)
			}
			// Legacy members saved this augmented prompt without a transcript identity.
			brief += "\n\nCompletion: End your turn with the result; it completes when durably delivered to the parent or workflow."
			testAddMessages(t, child, []messages.ChatMessage{
				{Role: messages.MessageRoleUser, Content: brief},
				{Role: messages.MessageRoleAssistant, Content: "The legacy review is complete.", Reasoning: "I checked the affected paths."},
			})
			if err := child.Close(); err != nil {
				t.Fatal(err)
			}
			r := newTabTestREPL(t, store, "root")
			execution := &swarm.Execution{ID: "old-execution", Generation: 1, Status: "completed", InputSaved: true, Request: swarm.AgentRequest{Task: "Review the implementation", Input: input}}
			r.visibleTab().swarmSnapshot = &swarm.State{
				Members:    map[string]*swarm.Member{id: {ID: id, Execution: execution.ID}},
				Executions: map[string]*swarm.Execution{execution.ID: execution},
			}
			r.inspect(viewTarget{session: sessions.ViewTarget{ID: id}})
			v := waitInspector(t, r, 140)
			text := overviewText(renderAgentOverview(v.model, 100, r.agentOverviewState()))
			for _, want := range []string{"Result", "The legacy review is complete.", "Recent thinking", "I checked the affected paths."} {
				if !strings.Contains(text, want) {
					t.Fatalf("legacy overview omitted %q: %q", want, text)
				}
			}
			assertHidden := func() {
				t.Helper()
				text := overviewText(renderAgentOverview(v.model, 100, r.agentOverviewState()))
				if strings.Contains(text, "The legacy review is complete.") || strings.Contains(text, "I checked the affected paths.") {
					t.Fatalf("different execution inherited legacy result: %q", text)
				}
			}
			// Similar briefs or different structured input are not the same assignment.
			execution.Request.Task = "Review"
			assertHidden()
			execution.Request.Task = "Review the implementation"
			execution.Request.Input = map[string]any{"path": "other.go"}
			assertHidden()
			execution.Request.Input = input
			// Reusing the same brief cannot pair a queued replacement with the
			// previous execution's answer before its assignment is persisted.
			snapshot := r.visibleTab().swarmSnapshot
			next := &swarm.Execution{ID: "replacement", Generation: 1, Status: "queued", Request: execution.Request}
			snapshot.Executions[next.ID] = next
			snapshot.Members[id].Execution = next.ID
			assertHidden()
			snapshot.Members[id].Execution = execution.ID
			// A known stale identity must never fall back to the legacy text match.
			v.live.identified = true
			assertHidden()
			v.live.identity = inspectorLiveIdentity{execution.ID, execution.Generation + 1}
			assertHidden()
		})
	}
}

func TestInspectorSavedOverviewUsesDurableExecutionIdentity(t *testing.T) {
	ctx := context.Background()
	store := testOpenMemoryStore(t, nil)
	child := testAcquireSession(t, store, "child")
	if err := child.(sessions.CoordinationSession).UpdateCoordination(ctx, func(s *sessions.CoordinationState) error {
		s.Transcript = &sessions.TranscriptIdentity{Execution: "execution", Generation: 2}
		s.Append = []messages.ChatMessage{
			{Role: messages.MessageRoleUser, Content: "task\n\nCompletion: return through the assignment's completion path"},
			{Role: messages.MessageRoleAssistant, Content: "saved execution update"},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	// No live reader or runtime is available, as in a separate process.
	info, tail, status, err := readInspectorConversation(ctx, store.(sessions.ViewStore), sessions.ViewTarget{Name: "child"}, "", nil, inspectorLiveIdentity{})
	if err != nil || status.active || len(tail.Messages) != 0 || status.identity != (inspectorLiveIdentity{"execution", 2}) {
		t.Fatalf("saved identity: status=%+v err=%v", status, err)
	}
	r := newTabTestREPL(t, store, "root")
	execution := &swarm.Execution{ID: "execution", Generation: 2, Request: swarm.AgentRequest{Task: "task"}}
	r.visibleTab().swarmSnapshot = &swarm.State{Members: map[string]*swarm.Member{info.ID: {ID: info.ID, Execution: execution.ID}}, Executions: map[string]*swarm.Execution{execution.ID: execution}}
	target := viewTarget{session: sessions.ViewTarget{ID: info.ID}}
	r.inspect(target)
	m := prepareChildDisplay(info, &Config{}, 80)
	r.workspace().inspector.current = &viewInstance{target: target, model: m, view: conversationView{}, live: status, info: info}
	if text := overviewText(renderAgentOverview(m, 80, r.agentOverviewState())); !strings.Contains(text, "saved execution update") {
		t.Fatalf("saved-only overview omitted current update: %q", text)
	}
	// Identical brief text cannot override a known different generation.
	execution.Generation++
	execution.Request.Task = m.assignment
	if text := overviewText(renderAgentOverview(m, 80, r.agentOverviewState())); strings.Contains(text, "saved execution update") {
		t.Fatalf("new execution inherited preceding update: %q", text)
	}
	// The store returns metadata even when unchanged history is omitted.
	_, _, unchanged, err := readInspectorConversation(ctx, store.(sessions.ViewStore), sessions.ViewTarget{ID: info.ID}, info.Revision, nil, inspectorLiveIdentity{})
	if err != nil || unchanged.identity != status.identity {
		t.Fatalf("unchanged view lost identity: %+v %v", unchanged, err)
	}
	info.Sequence++
	reader := inspectorViewReadFunc(func(context.Context, sessions.ViewTarget, string) (*sessions.SessionView, error) { return info, nil })
	_, _, stale, err := readInspectorConversation(ctx, reader, sessions.ViewTarget{ID: info.ID}, "", nil, inspectorLiveIdentity{})
	if err != nil || stale.identity.execution != "" || !stale.identified {
		t.Fatalf("independent append reused checkpoint identity: %+v %v", stale, err)
	}
	info.History = append(info.History, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "independent appended answer"})
	m = prepareChildDisplay(info, &Config{}, 80)
	execution.Generation = 2
	execution.Request.Task = m.assignment
	r.workspace().inspector.current.model = m
	r.workspace().inspector.current.live = stale
	if text := overviewText(renderAgentOverview(m, 80, r.agentOverviewState())); strings.Contains(text, "independent appended answer") {
		t.Fatalf("invalidated identity fell back to matching task text: %q", text)
	}
}
