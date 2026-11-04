package main

import (
	"context"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func TestInspectorSavedViewKeepsDurableExecutionIdentity(t *testing.T) {
	ctx := context.Background()
	store := testOpenMemoryStore(t, nil)
	child := testAcquireSession(t, store, "child")
	if err := child.(sessions.CoordinationSession).UpdateCoordination(ctx, func(s *sessions.CoordinationState) error {
		s.Transcript = &sessions.TranscriptIdentity{Execution: "execution", Generation: 2}
		s.Append = []messages.ChatMessage{
			{Role: messages.MessageRoleUser, Content: "task"},
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
	// The store returns metadata even when unchanged history is omitted.
	_, _, unchanged, err := readInspectorConversation(ctx, store.(sessions.ViewStore), sessions.ViewTarget{ID: info.ID}, info.Revision, nil, inspectorLiveIdentity{})
	if err != nil || unchanged.identity != status.identity {
		t.Fatalf("unchanged view lost identity: %+v %v", unchanged, err)
	}
	// An append the checkpoint does not cover invalidates its identity rather
	// than carrying the execution forward.
	info.Sequence++
	reader := inspectorViewReadFunc(func(context.Context, sessions.ViewTarget, string) (*sessions.SessionView, error) { return info, nil })
	_, _, stale, err := readInspectorConversation(ctx, reader, sessions.ViewTarget{ID: info.ID}, "", nil, inspectorLiveIdentity{})
	if err != nil || stale.identity.execution != "" || !stale.identified {
		t.Fatalf("independent append reused checkpoint identity: %+v %v", stale, err)
	}
}
