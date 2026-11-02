package main

import (
	"context"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
)

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
