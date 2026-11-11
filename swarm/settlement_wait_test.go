package swarm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
)

func TestSettleWaitsWithoutCoordinationWrites(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	var parent *countingSession
	r := runtimeTestWithParent(t, modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return answer("done")
	}), 1, 1, func(s sessions.Session) sessions.Session {
		parent = newCountingSession(s)
		return parent
	})
	suspendAutoRelease(t, r)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	child, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "inspect", ReadOnly: true, Background: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("member never entered its model call")
	}

	before := parent.updates.Load()
	waitCtx, stop := context.WithTimeout(ctx, 200*time.Millisecond)
	defer stop()
	if err := r.Settle(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("settlement did not wait for the running member: %v", err)
	}
	if writes := parent.updates.Load() - before; writes != 0 {
		t.Errorf("settlement wrote coordination %d times while the model was waiting", writes)
	}

	close(release)
	select {
	case <-child.Done:
	case <-ctx.Done():
		t.Fatal("member did not finish after its model call")
	}
	awaitIdle(t, r, ctx)
	if err := r.Settle(ctx); err == nil || !strings.Contains(err.Error(), "awaits delivery") {
		t.Fatalf("completed result lost its delivery obligation: %v", err)
	}
	admitParent(t, r)
	if err := r.Settle(ctx); err != nil {
		t.Fatalf("delivered result did not settle: %v", err)
	}
}

func TestWaitPathsRepairNoticesOnlyWhenMissing(t *testing.T) {
	t.Parallel()
	for _, settle := range []bool{false, true} {
		t.Run(map[bool]string{false: "wait_agent", true: "settle"}[settle], func(t *testing.T) {
			var parent *countingSession
			r := runtimeTestWithParent(t, doneModel(), 1, 1, func(s sessions.Session) sessions.Session {
				parent = newCountingSession(s)
				return parent
			})
			suspendAutoRelease(t, r)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			result, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "inspect", ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			awaitIdle(t, r, ctx)
			check := func(wantWrites int64, delivered bool) {
				t.Helper()
				before := parent.updates.Load()
				var err error
				if settle {
					err = r.Settle(ctx)
				} else {
					_, err = r.waitParent(ctx)
				}
				if settle && !delivered {
					if err == nil || !strings.Contains(err.Error(), "awaits delivery") {
						t.Fatalf("missing delivery blocker: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if writes := parent.updates.Load() - before; writes != wantWrites {
					t.Errorf("coordination writes = %d, want %d", writes, wantWrites)
				}
			}
			check(0, false)
			if err := r.update(ctx, func(s *State) error {
				for id, mail := range s.Messages {
					if mail.Task == result.Task {
						delete(s.Messages, id)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			check(1, false)
			check(0, false)
			s, err := r.read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(inbox(s, r.ID, true)) != 1 || resultNotice(s, s.Tasks[result.Task]) == nil {
				t.Fatal("repair did not post exactly one result notice")
			}
			admitParent(t, r)
			check(0, true)
		})
	}
}

// Settlement derives its facts after its repairs commit, while members go on
// editing: the state it keeps has to be one no later edit writes.
func TestSettlementStateIsNotTheOneEditsWrite(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for name, prepare := range map[string]func(t *testing.T) (*Runtime, func(*State) bool){
		// A finished task whose notice is gone gets one inside an edit.
		"notice": func(t *testing.T) (*Runtime, func(*State) bool) {
			r := runtimeTest(t, doneModel(), 1, 1)
			result, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "inspect", ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			awaitIdle(t, r, ctx)
			if err := r.update(ctx, func(s *State) error {
				for id, mail := range s.Messages {
					if mail.Task == result.Task {
						delete(s.Messages, id)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			return r, func(s *State) bool { return resultNotice(s, s.Tasks[result.Task]) != nil }
		},
		// A saved acceptance of an unchanged submission is completed inside one.
		"unchanged": func(t *testing.T) (*Runtime, func(*State) bool) {
			r, _, ref := noEditResult(t, false)
			if err := r.update(ctx, func(s *State) error {
				s.Tasks[ref.Task].AcceptedRevision = ref.Revision
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			return r, func(s *State) bool { return s.Tasks[ref.Task].Status == "done" }
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, repaired := prepare(t)
			suspendAutoRelease(t, r)
			s, err := r.settlementState(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !repaired(s) {
				t.Fatal("settlement repaired nothing, so it kept no edit's state")
			}
			tasks, mail := len(s.Tasks), len(s.Messages)
			// A member's edit lands while settlement reads what it kept.
			done := make(chan error, 1)
			go func() {
				done <- r.edit(ctx, func(current *State) error {
					put(current, "task", &current.Tasks, "later", &Task{ID: "later", Status: "pending", Revision: 1})
					put(current, "mail", &current.Messages, "later", &Mail{ID: "later", From: "later", To: r.ID, Kind: "info"})
					return nil
				})
			}()
			for editing := true; editing; {
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
					editing = false
				default:
				}
				deriveFacts(s, r.ID)
			}
			if len(s.Tasks) != tasks || len(s.Messages) != mail {
				t.Fatalf("a later edit wrote the state settlement kept: %d tasks and %d mail, were %d and %d", len(s.Tasks), len(s.Messages), tasks, mail)
			}
		})
	}
}
