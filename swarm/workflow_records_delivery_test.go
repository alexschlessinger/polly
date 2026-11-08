package swarm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
)

func seedWorkflowReceipt(t *testing.T, session *workflowRecordSession, status string) (AgentResult, workflow.Step) {
	t.Helper()
	result := AgentResult{Task: "task", Session: "member", Execution: "execution", Revision: 2}
	repo := repositoryFor(t, session)
	if _, err := repo.Update(t.Context(), func(tx *recordTx) error {
		if err := tx.PutMember(&Member{ID: "member", Label: "Reader", ReadOnly: true, Execution: "execution", Task: "task"}); err != nil {
			return err
		}
		if err := tx.PutExecution(&Execution{ID: "execution", Member: "member", Run: "run", Status: status, Workflow: "w", Request: AgentRequest{CallID: "w/1"}, Result: &result}); err != nil {
			return err
		}
		return tx.PutTask(&Task{ID: "task", Run: "run", Owner: "member", Execution: "execution", Revision: 2, Requirement: RequirementDelivered, Status: "running", Result: "original result"})
	}); err != nil {
		t.Fatal(err)
	}
	return result, workflow.Step{Operation: workflow.Operation{ID: "w/1", Kind: "agent"}, Status: "completed", Value: result}
}

func TestWorkflowRecordsReceiptIdentity(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		for _, kind := range []string{"agent", "followup"} {
			for _, corrupt := range []string{"none", "task", "member", "execution", "revision", "workflow", "call", "saved", "status", "requirement"} {
				t.Run(fmt.Sprintf("legacy=%t/%s/%s", legacy, kind, corrupt), func(t *testing.T) {
					r, session := workflowRecordsRuntime(t, legacy)
					h := workflowRecordsHeader()
					if err := r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: h}); err != nil {
						t.Fatal(err)
					}
					result, step := seedWorkflowReceipt(t, session, "completed")
					step.Kind = kind
					switch corrupt {
					case "task":
						result.Task = "other"
					case "member":
						result.Session = "other"
					case "execution":
						result.Execution = "other"
					case "revision":
						result.Revision++
					case "call":
						step.ID = "w/other"
					case "workflow", "saved", "status", "requirement":
						if _, err := repositoryFor(t, session).Update(t.Context(), func(tx *recordTx) error {
							e, err := tx.Execution("execution")
							if err != nil {
								return err
							}
							switch corrupt {
							case "workflow":
								e.Workflow = "other"
							case "saved":
								e.Result.Revision++
							case "status":
								e.Status = "running"
							case "requirement":
								task, err := tx.Task("task")
								if err != nil {
									return err
								}
								task.Requirement = RequirementReviewed
								if err := tx.PutTask(task); err != nil {
									return err
								}
							}
							return tx.PutExecution(e)
						}); err != nil {
							t.Fatal(err)
						}
					}
					step.Value = result
					if err := r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: h, TotalSteps: 1, Steps: map[int]workflow.Step{0: step}}); err != nil {
						t.Fatal(err)
					}
					task := workflowRecordsState(t, session).Tasks["task"]
					if got := task.Status == "done"; got != (corrupt == "none") {
						t.Fatalf("delivery=%t task=%+v", got, task)
					}
					if corrupt == "none" && (task.Delivery == nil || task.Delivery.Via != "workflow_step" || task.Delivery.Ref != step.ID) {
						t.Fatalf("receipt=%+v", task.Delivery)
					}
				})
			}
		}
	}
}

func TestWorkflowRecordsStepDeliveryRaces(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			r, session := workflowRecordsRuntime(t, legacy)
			h := workflowRecordsHeader()
			if err := r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: h}); err != nil {
				t.Fatal(err)
			}
			_, step := seedWorkflowReceipt(t, session, "running")
			delta := workflow.ReportDelta{Header: h, TotalSteps: 1, Steps: map[int]workflow.Step{0: step}}
			if err := r.SaveWorkflowDelta(t.Context(), delta); err != nil {
				t.Fatal(err)
			}
			if workflowRecordsState(t, session).Tasks["task"].Delivery != nil {
				t.Fatal("running execution delivered")
			}
			if _, err := repositoryFor(t, session).Update(t.Context(), func(tx *recordTx) error {
				e, err := tx.Execution("execution")
				if err != nil {
					return err
				}
				e.Status = "completed"
				return tx.PutExecution(e)
			}); err != nil {
				t.Fatal(err)
			}
			// An identical supplied step must recheck the receipt after completion.
			if err := r.SaveWorkflowDelta(t.Context(), delta); err != nil {
				t.Fatal(err)
			}
			before := workflowRecordsState(t, session).Tasks["task"].Delivery
			if before == nil {
				t.Fatal("identical step failed to deliver completed execution")
			}
			if err := r.SaveWorkflowDelta(t.Context(), delta); err != nil {
				t.Fatal(err)
			}
			after := workflowRecordsState(t, session).Tasks["task"].Delivery
			if *before != *after {
				t.Fatal("idempotent step replaced receipt timestamp")
			}
			// Reset a fresh task, then race two durable delivery routes. Neither may
			// overwrite the winner's proof, nor undo done status.
			seedWorkflowReceipt(t, session, "completed")
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			var saveErr, mailErr error
			go func() { defer wg.Done(); <-start; saveErr = r.SaveWorkflowDelta(t.Context(), delta) }()
			go func() {
				defer wg.Done()
				<-start
				mailErr = session.CoordinationSession.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error {
					s, err := decodeState(raw)
					if err != nil {
						return err
					}
					recordDelivery(s, &Mail{ID: "mail-proof", Task: "task", Revision: 2, Execution: "execution"})
					return encodeState(raw, s)
				})
			}()
			close(start)
			wg.Wait()
			if saveErr != nil || mailErr != nil {
				t.Fatalf("step=%v mail=%v", saveErr, mailErr)
			}
			task := workflowRecordsState(t, session).Tasks["task"]
			if task.Status != "done" || task.Delivery == nil || task.Delivery.Via != "mail" && task.Delivery.Via != "workflow_step" {
				t.Fatalf("race lost delivery: %+v", task)
			}
			winner := *task.Delivery
			if err := r.SaveWorkflowDelta(t.Context(), delta); err != nil {
				t.Fatal(err)
			}
			if *workflowRecordsState(t, session).Tasks["task"].Delivery != winner {
				t.Fatal("later save replaced winning receipt")
			}
		})
	}
}

func TestWorkflowRecordsTerminalRepairsAndAtomicReceipt(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		for _, delivered := range []bool{false, true} {
			t.Run(fmt.Sprintf("legacy=%t/delivered=%t", legacy, delivered), func(t *testing.T) {
				r, session := workflowRecordsRuntime(t, legacy)
				h := workflowRecordsHeader()
				if err := r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: h}); err != nil {
					t.Fatal(err)
				}
				_, step := seedWorkflowReceipt(t, session, "completed")
				h.Status = "failed"
				delta := workflow.ReportDelta{Header: h}
				if delivered {
					delta.TotalSteps = 1
					delta.Steps = map[int]workflow.Step{0: step}
				}
				session.veto = errors.New("veto terminal receipt")
				if err := r.SaveWorkflowDelta(t.Context(), delta); err == nil {
					t.Fatal("veto succeeded")
				}
				if s := workflowRecordsState(t, session); s.Tasks["task"].Status != "running" || len(s.Messages) != 0 || s.Workflows["w"].Status != "running" {
					t.Fatal("receipt/status/mail not rolled back atomically")
				}
				session.veto = nil
				if err := r.SaveWorkflowDelta(t.Context(), delta); err != nil {
					t.Fatal(err)
				}
				s := workflowRecordsState(t, session)
				want := 2
				if delivered {
					want = 1
				}
				if len(s.Messages) != want {
					t.Fatalf("terminal notices=%d want=%d", len(s.Messages), want)
				}
				for _, mail := range s.Messages {
					if mail.Workflow == "w" && mail.Text != workflowTerminalText(s, s.Workflows["w"]) {
						t.Fatalf("terminal text changed: %q", mail.Text)
					}
				}
				if delivered {
					if s.Tasks["task"].Status != "done" {
						t.Fatal("terminal step did not deliver")
					}
				} else if resultNotice(s, s.Tasks["task"]) == nil {
					t.Fatal("terminal save stranded suppressed completion")
				}
			})
		}
	}
}

func TestWorkflowRecordsTerminalAndAcknowledgementRaces(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			r, session := workflowRecordsRuntime(t, legacy)
			h := workflowRecordsHeader()
			if err := r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: h}); err != nil {
				t.Fatal(err)
			}
			h.Status = "completed"
			delta := workflow.ReportDelta{Header: h}
			var wg sync.WaitGroup
			errors := make(chan error, 8)
			for range 8 {
				wg.Go(func() { errors <- r.SaveWorkflowDelta(t.Context(), delta) })
			}
			wg.Wait()
			for range 8 {
				if err := <-errors; err != nil {
					t.Fatal(err)
				}
			}
			state := workflowRecordsState(t, session)
			if len(state.Messages) != 1 {
				t.Fatalf("concurrent terminal saves produced %d notices", len(state.Messages))
			}
			wg.Go(func() { errors <- r.SaveWorkflowDelta(t.Context(), delta) })
			wg.Go(func() {
				errors <- session.CoordinationSession.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error {
					state, err := decodeState(raw)
					if err != nil {
						return err
					}
					for _, mail := range state.Messages {
						mail.Delivered = true
						recordDelivery(state, mail)
					}
					return encodeState(raw, state)
				})
			})
			wg.Wait()
			for range 2 {
				if err := <-errors; err != nil {
					t.Fatal(err)
				}
			}
			state = workflowRecordsState(t, session)
			if !state.Workflows["w"].Acknowledged || len(state.Messages) != 1 {
				t.Fatal("concurrent save lost parent acknowledgement")
			}
		})
	}
}

type workflowPersistenceHost struct {
	*Runtime
	calls       atomic.Int32
	intentError error
	call        func(context.Context, workflow.Operation) (any, error)
}

func (h *workflowPersistenceHost) Call(ctx context.Context, op workflow.Operation) (any, error) {
	h.calls.Add(1)
	if h.call != nil {
		return h.call(ctx, op)
	}
	return map[string]any{"task": "opaque", "session": "opaque"}, nil
}
func (h *workflowPersistenceHost) SaveWorkflow(context.Context, workflow.Report) error {
	return errors.New("runner used full recorder instead of native delta")
}
func (h *workflowPersistenceHost) SaveWorkflowDelta(ctx context.Context, delta workflow.ReportDelta) error {
	err := h.Runtime.SaveWorkflowDelta(ctx, delta)
	if err == nil && h.intentError != nil && delta.Header.Status == "running" {
		for _, step := range delta.Steps {
			if step.Status == "running" {
				return h.intentError
			}
		}
	}
	return err
}

func TestWorkflowRecordsRunnerEndToEnd(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		for _, uncertain := range []bool{false, true} {
			t.Run(fmt.Sprintf("legacy=%t/uncertain=%t", legacy, uncertain), func(t *testing.T) {
				r, session := workflowRecordsRuntime(t, legacy)
				host := &workflowPersistenceHost{Runtime: r}
				if uncertain {
					host.intentError = errors.New("intent committed, response lost")
				}
				runner := workflow.Runner{Host: host, Config: workflow.Config{RunID: "w"}}
				report, err := runner.Run(t.Context(), `polly.workflow("native",polly.schema.obj({}),async()=>{await polly.log("first");return await polly.log("second");});`, map[string]any{})
				if uncertain {
					if err == nil || !strings.Contains(err.Error(), host.intentError.Error()) || host.calls.Load() != 0 {
						t.Fatalf("uncertain intent replayed effect: calls=%d err=%v", host.calls.Load(), err)
					}
				} else if err != nil || host.calls.Load() != 2 || report == nil || report.Status != "completed" {
					t.Fatalf("runner failed: report=%+v calls=%d err=%v", report, host.calls.Load(), err)
				}
				state := workflowRecordsState(t, session)
				if report == nil || state.Workflows["w"].Status != report.Status || len(state.Workflows["w"].Steps) != len(report.Steps) || len(state.Messages) != 1 {
					t.Fatalf("runner persistence diverged: report=%+v saved=%+v", report, state.Workflows["w"])
				}
			})
		}
	}
}
