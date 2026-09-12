package swarm

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func TestRenamedMemberContinuesByStableIdentity(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "continue", true: "resume intent"}[resume], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var calls atomic.Int32
			r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
				calls.Add(1)
				return answer("done")
			}), 1, 3)
			first, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "investigate", ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			s, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			member := s.Members[first.Session]
			oldName := member.Name
			child, err := r.config.Store.Acquire(ctx, oldName, sessions.AcquireOptions{ExpectedID: member.ID, ExistingOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := child.Rename(ctx, "readable-member-name"); err != nil {
				child.Close()
				t.Fatal(err)
			}
			if err := child.Close(); err != nil {
				t.Fatal(err)
			}
			// The old handle can belong to another live session. The runtime
			// must neither acquire it nor wait for its lease to be released.
			reused, err := r.config.Store.Acquire(ctx, oldName, sessions.AcquireOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer reused.Close()
			if resume {
				if err := r.update(ctx, func(s *State) error {
					m := s.Members[first.Session]
					e := s.Executions[m.Execution]
					e.Status = "paused"
					s.Tasks[m.Task].Status = "blocked"
					e.Intent = []messages.ChatMessage{{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
						ToolCalls: []messages.ChatMessageToolCall{{ID: "uncertain", Name: "not_replayed", Arguments: `{}`}}}}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if err := r.Resume(ctx, first.Session, 0); err != nil {
					t.Fatal(err)
				}
				waitWorkflowIdle(t, r)
			} else if _, err := r.Agent(ctx, "", AgentRequest{Session: first.Session, Task: "continue investigation"}); err != nil {
				t.Fatalf("stable member could not continue after rename: %v", err)
			}
			s, err = r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 || s.Members[first.Session].Name != "readable-member-name" || s.Executions[s.Members[first.Session].Execution].Status != "completed" {
				t.Fatalf("renamed member did not complete: calls=%d member=%+v", calls.Load(), s.Members[first.Session])
			}
			history, err := reused.GetHistory(ctx)
			if err != nil || len(history) != 0 {
				t.Fatalf("reused name received the old member's history: %+v, %v", history, err)
			}
		})
	}
}

func TestDeletedMemberCannotResumeThroughReusedHandle(t *testing.T) {
	ctx := context.Background()
	r := runtimeTest(t, nilModel(), 1, 3)
	first, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "investigate", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	name := s.Members[first.Session].Name
	if err := r.config.Store.Delete(ctx, name); err != nil {
		t.Fatal(err)
	}
	reused, err := r.config.Store.Acquire(ctx, name, sessions.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reused.Close()
	if _, err := r.Agent(ctx, "", AgentRequest{Session: first.Session, Task: "continue"}); !errors.Is(err, sessions.ErrSessionNotFound) {
		t.Fatalf("expected missing stable identity, got %v", err)
	}
	history, err := reused.GetHistory(ctx)
	if err != nil || len(history) != 0 {
		t.Fatalf("reused handle received old work: %+v, %v", history, err)
	}
}
