package swarm

import (
	"context"
	"errors"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func TestMemberTranscriptIdentityTracksAcceptedAssignment(t *testing.T) {
	t.Parallel()
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "rejected"}[rejected], func(t *testing.T) {
			r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
				if rejected {
					t.Error("rejected assignment reached provider")
				}
				return answer("saved result")
			}), 1, 1)
			if rejected {
				r.config.Callbacks = func(context.Context, Member) *llm.AgentCallbacks {
					return &llm.AgentCallbacks{BeforeFirstRequest: func(llm.ProjectionStats) error { return errors.New("gate rejected") }}
				}
			}
			ctx := context.Background()
			result, err := r.Agent(ctx, "", AgentRequest{Label: "Research", Task: "inspect", ReadOnly: true, Review: true})
			if (err != nil) != rejected {
				t.Fatalf("assignment result: %+v %v", result, err)
			}
			state, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			member := state.Members[result.Session]
			execution := state.Executions[member.Execution]
			view, err := r.config.Store.(sessions.ViewStore).ReadView(ctx, sessions.ViewTarget{ID: member.ID}, "")
			if err != nil {
				t.Fatal(err)
			}
			if rejected {
				if view.Metadata.Transcript != nil {
					t.Fatalf("rejected assignment claimed the saved conversation: %+v", view.Metadata.Transcript)
				}
			} else if view.Metadata.Transcript == nil || *view.Metadata.Transcript != (sessions.TranscriptIdentity{Execution: execution.ID, Generation: execution.Generation, Sequence: view.Sequence}) {
				t.Fatalf("checkpoint did not bind actual saved result: %+v sequence=%d execution=%+v", view.Metadata.Transcript, view.Sequence, execution)
			}
		})
	}
}
