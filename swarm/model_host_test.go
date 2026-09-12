package swarm

import (
	"context"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestMemberRouteInheritanceAndPersistedIdentity(t *testing.T) {
	requests := make(chan llm.CompletionRequest, 3)
	model := modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		requests <- *req
		return answer("done")
	})
	r := runtimeTest(t, model, 1, 4)
	r.UpdateDefaults(llm.CompletionRequest{Model: "openrouter/org/m", ModelHost: "parent-host"}, llm.AgentConfig{MaxIterations: 2}, nil)
	for _, tc := range []struct {
		request AgentRequest
		host    string
	}{
		{AgentRequest{Label: "Test agent", Task: "inherit", ReadOnly: true}, "parent-host"},
		{AgentRequest{Label: "Test agent", Task: "new model", ReadOnly: true, Model: "openrouter/org/other"}, ""},
		{AgentRequest{Label: "Test agent", Task: "explicit route", ReadOnly: true, Model: "openrouter/org/other", ModelHost: "other-host"}, "other-host"},
	} {
		result, err := r.Agent(context.Background(), "", tc.request)
		if err != nil {
			t.Fatal(err)
		}
		req := <-requests
		if req.ModelHost != tc.host {
			t.Fatalf("request pin %q, want %q", req.ModelHost, tc.host)
		}
		state, err := r.State(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, m := range state.Members {
			if m.Name == result.Session || m.ID == result.Session {
				found = true
				if m.ModelHost != tc.host {
					t.Fatal("member pin not persisted")
				}
			}
		}
		if !found {
			t.Fatalf("missing member %s", result.Session)
		}
	}
}
