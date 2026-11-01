package swarm

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
)

// Scope faults to the BlockTask call's context so concurrent worker checkpoints
// cannot consume them. The wrapped parent still commits through real SQLite.
type blockFaultKey struct{}
type blockStoreFault struct {
	writeErr, readErr error
	rollback          bool
	alter             func(*State)
	afterCommit       func()
	writes, callbacks atomic.Int32
	reads             atomic.Int32
}
type blockFaultSession struct{ *countingSession }

func (s *blockFaultSession) UpdateCoordination(ctx context.Context, fn func(*sessions.CoordinationState) error) error {
	fault, _ := ctx.Value(blockFaultKey{}).(*blockStoreFault)
	if fault == nil {
		return s.countingSession.UpdateCoordination(ctx, fn)
	}
	fault.writes.Add(1)
	err := s.countingSession.UpdateCoordination(ctx, func(raw *sessions.CoordinationState) error {
		fault.callbacks.Add(1)
		if err := fn(raw); err != nil {
			return err
		}
		if fault.rollback {
			return fault.writeErr
		}
		return nil
	})
	if err != nil {
		return err
	}
	if fault.alter != nil {
		// A separate committed change makes the receipt stale before the
		// original update reports its ambiguous outcome.
		if err := s.coord.UpdateCoordination(ctx, func(raw *sessions.CoordinationState) error {
			state, err := decodeState(raw)
			if err != nil {
				return err
			}
			fault.alter(state)
			return encodeState(raw, state)
		}); err != nil {
			return err
		}
	}
	if fault.afterCommit != nil {
		fault.afterCommit()
	}
	return fault.writeErr
}

func (s *blockFaultSession) ReadCoordination(ctx context.Context) (*sessions.CoordinationState, error) {
	if fault, _ := ctx.Value(blockFaultKey{}).(*blockStoreFault); fault != nil {
		fault.reads.Add(1)
		if fault.readErr != nil {
			return nil, fault.readErr
		}
	}
	return s.countingSession.ReadCoordination(ctx)
}

func blockFaultParent(s sessions.Session) sessions.Session {
	return &blockFaultSession{countingSession: newCountingSession(s)}
}

func TestWorkflowBlockRecoversCommittedWriteErrorAndDrains(t *testing.T) {
	t.Parallel()
	for _, background := range []bool{false, true} {
		for _, canceledCaller := range []bool{false, true} {
			name := map[bool]string{false: "foreground", true: "background"}[background] + map[bool]string{false: "/lost_reply", true: "/canceled_caller"}[canceledCaller]
			t.Run(name, func(t *testing.T) {
				started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
				r := runtimeTestWithParent(t, modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
					for _, msg := range req.Messages {
						if msg.Role == messages.MessageRoleUser && strings.Contains(msg.Content, "running assignment") {
							close(started)
							<-ctx.Done()
							close(canceled)
							<-release
							return answer("unfinished work")
						}
					}
					return iterationTool("park", "wait_agent", `{}`)
				}), 2, 2, blockFaultParent)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				// Always release the model before runtime cleanup tries to join it.
				defer close(release)
				run, err := r.launchWorkflow(ctx, `polly.workflow("lost blocker reply",polly.schema.obj({}),async()=>
 polly.parallel(["running","parked"], label=>polly.research(label,label+" assignment"), {errors:"throw_after_all"}));`, map[string]any{}, background)
				if err != nil {
					t.Fatal(err)
				}
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("worker did not start")
				}
				s := awaitState(t, r, ctx, func(s *State) bool {
					for _, m := range s.Members {
						if e := s.Executions[m.Execution]; m.Label == "parked" && e != nil && e.Status == "waiting" {
							return true
						}
					}
					return false
				})
				var task *Task
				for _, m := range s.Members {
					if m.Label == "running" {
						task = s.Tasks[m.Task]
					}
				}
				callCtx, cancelCall := context.WithCancel(ctx)
				defer cancelCall()
				fault := &blockStoreFault{writeErr: errors.New("lost blocker commit reply")}
				if canceledCaller {
					fault.writeErr, fault.afterCommit = context.Canceled, cancelCall
				}
				callCtx = context.WithValue(callCtx, blockFaultKey{}, fault)
				if err := r.BlockTask(callCtx, task.Owner, task.ID, task.Revision, "parent decision needed"); err != fault.writeErr {
					t.Fatalf("write error changed: %v", err)
				}
				if fault.writes.Load() != 1 || fault.callbacks.Load() != 1 || fault.reads.Load() != 1 {
					t.Fatalf("blocker was replayed or not resolved: writes=%d callbacks=%d reads=%d", fault.writes.Load(), fault.callbacks.Load(), fault.reads.Load())
				}
				select {
				case <-canceled:
				case <-ctx.Done():
					t.Fatal("committed blocker did not cancel the running worker")
				}
				s, err = r.State(ctx)
				if err != nil {
					t.Fatal(err)
				}
				blocked := s.Tasks[task.ID]
				if blocked.Status != "blocked" || blocked.Revision != task.Revision+1 || blocked.Feedback != "parent decision needed" || blocked.AcceptedRevision != 0 {
					t.Fatalf("missing exact blocker receipt: %+v", blocked)
				}
				if s.Workflows[run.id].Status != "running" {
					t.Fatal("terminal report was saved before the worker drained")
				}
				// A later cancellation must not replace the recovered blocker cause.
				if err := r.CancelWorkflow(run.id); err != nil {
					t.Fatal(err)
				}
				select {
				case <-run.done:
					t.Fatal("workflow returned before its running worker drained")
				case <-time.After(30 * time.Millisecond):
				}
				t.Cleanup(func() {
					select {
					case <-run.done:
					case <-time.After(5 * time.Second):
						t.Fatal("workflow failed to return after draining")
					}
					var failure *workflow.Error
					if !errors.As(run.err, &failure) || failure.Code != "workflow_blocked" || failure.Session != task.Owner || run.report.Status != "interrupted" || r.HasActive() {
						t.Fatalf("committed blocker did not interrupt and drain: %+v, %v", run.report, run.err)
					}
					detail := run.report.Error.Result.(map[string]any)
					if detail["task"] != task.ID || detail["execution"] != task.Execution || detail["revision"] != float64(task.Revision+1) || detail["reason"] != "parent decision needed" {
						t.Fatalf("blocker provenance changed: %+v", run.report.Error)
					}
					s, err := r.State(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					for _, m := range s.Members {
						e, task := s.Executions[m.Execution], s.Tasks[m.Task]
						if e.Status != "paused" || m.Control == MemberControlStopped || task.Result != nil || task.AcceptedRevision != 0 {
							t.Fatalf("unfinished worker was stopped or accepted: %+v, %+v, %+v", m, e, task)
						}
					}
					assertWorkflowDelivery(t, r, false, false)
				})
			})
		}
	}
}

func TestWorkflowBlockWriteErrorRequiresExactReceipt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                   string
		rollback, stale, owner bool
		readErr                error
		alter                  func(*State)
	}{
		{name: "rollback_after_callback", rollback: true},
		{name: "stale_revision", stale: true},
		{name: "wrong_owner", owner: true},
		{name: "failed_receipt_read", readErr: errors.New("receipt read failed")},
		{name: "missing_task", alter: func(s *State) { delete(s.Tasks, "task") }},
		{name: "task_revision", alter: func(s *State) { s.Tasks["task"].Revision++ }},
		{name: "task_owner", alter: func(s *State) { s.Tasks["task"].Owner = "other" }},
		{name: "task_execution", alter: func(s *State) { s.Tasks["task"].Execution = "other" }},
		{name: "task_status", alter: func(s *State) { s.Tasks["task"].Status = "running" }},
		{name: "task_reason", alter: func(s *State) { s.Tasks["task"].Feedback = "another blocker" }},
		{name: "task_accepted", alter: func(s *State) { s.Tasks["task"].AcceptedRevision = 2 }},
		{name: "missing_member", alter: func(s *State) { delete(s.Members, "worker") }},
		{name: "member_task", alter: func(s *State) { s.Members["worker"].Task = "other" }},
		{name: "member_execution", alter: func(s *State) { s.Members["worker"].Execution = "other" }},
		{name: "member_controller", alter: func(s *State) { s.Members["worker"].Controller = "other" }},
		{name: "missing_execution", alter: func(s *State) { delete(s.Executions, "execution") }},
		{name: "execution_owner", alter: func(s *State) { s.Executions["execution"].Member = "other" }},
		{name: "execution_workflow", alter: func(s *State) { s.Executions["execution"].Workflow = "other" }},
		{name: "missing_workflow", alter: func(s *State) { delete(s.Workflows, "workflow") }},
		{name: "workflow_status", alter: func(s *State) { s.Workflows["workflow"].Status = "completed" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runtimeTestWithParent(t, doneModel(), 1, 1, blockFaultParent)
			ctx := context.Background()
			if err := r.update(ctx, func(s *State) error {
				s.Runs["run"] = &Run{ID: "run", Status: "running"}
				s.Tasks["task"] = &Task{ID: "task", Run: "run", Owner: "worker", Execution: "execution", Status: "running", Revision: 1}
				s.Members["worker"] = &Member{ID: "worker", AgentName: "/root/worker", Task: "task", Execution: "execution", Controller: "workflow"}
				s.Executions["execution"] = &Execution{ID: "execution", Run: "run", Member: "worker", Workflow: "workflow", Status: "running"}
				s.Workflows["workflow"] = &workflow.Report{ID: "workflow", Run: "run", Status: "running"}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			workflowCtx, cancel := context.WithCancelCause(ctx)
			defer cancel(nil)
			r.mu.Lock()
			r.workflowCancels["workflow"] = cancel
			r.mu.Unlock()
			fault := &blockStoreFault{writeErr: errors.New("uncertain blocker write"), readErr: tc.readErr, rollback: tc.rollback, alter: tc.alter}
			actor, revision := "worker", 1
			if tc.stale {
				revision++
			}
			if tc.owner {
				actor = "other"
			}
			err := r.BlockTask(context.WithValue(ctx, blockFaultKey{}, fault), actor, "task", revision, "blocked reason")
			wantReads := int32(1)
			if tc.stale || tc.owner {
				var failure *workflow.Error
				if !errors.As(err, &failure) || failure.Code != "stale_task" {
					t.Fatalf("validation error changed: %v", err)
				}
				wantReads = 0
			} else if err != fault.writeErr {
				t.Fatalf("write error changed: %v", err)
			}
			if fault.writes.Load() != 1 || fault.callbacks.Load() != 1 || fault.reads.Load() != wantReads {
				t.Fatalf("blocker was replayed or incorrectly resolved: writes=%d callbacks=%d reads=%d", fault.writes.Load(), fault.callbacks.Load(), fault.reads.Load())
			}
			if cause := context.Cause(workflowCtx); cause != nil {
				t.Fatalf("unconfirmed blocker canceled the workflow: %v", cause)
			}
			if tc.rollback || tc.stale || tc.owner {
				s, err := r.State(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if task := s.Tasks["task"]; task.Status != "running" || task.Revision != 1 || task.Feedback != "" {
					t.Fatalf("failed blocker changed its task: %+v", task)
				}
			}
		})
	}
}
