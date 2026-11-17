package swarm

import (
	"context"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
)

// A new member's session starts as the runtime describes it: titled and
// described by its label, bound to the swarm and its context, and without
// the system prompt the store gives sessions by default.
func TestNewMemberSessionStartsAsDescribed(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	model := modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") })
	config := sessions.StoreConfig{Mode: sessions.ModeMemory, DefaultMetadata: &sessions.Metadata{SystemPrompt: "launch prompt"}}
	r := runtimeTestOn(t, config, model, 1, 1, nil)
	result, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "answer", ReadOnly: true, CallID: "call-7"})
	if err != nil {
		t.Fatal(err)
	}
	<-result.Done
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	member := s.Members[result.Session]
	view, err := r.config.Store.(sessions.ViewStore).ReadView(ctx, sessions.ViewTarget{ID: member.ID}, "")
	if err != nil {
		t.Fatal(err)
	}
	md := view.Metadata
	if md.Title != "Test agent" || md.TitleSource != sessions.TitleSourceAgent || md.Description != "Test agent" ||
		md.SpawnCallID != "call-7" || md.SwarmID != r.ID || md.ExecutionContext != member.Context || md.Name != member.Name {
		t.Fatalf("a new member's settings = %+v", md)
	}
	for _, message := range view.History {
		if message.Content == "launch prompt" {
			t.Fatalf("a new member's transcript holds the store's default prompt: %+v", view.History)
		}
	}
}
