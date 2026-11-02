package workflow

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
)

type recordingHost struct {
	hostFunc
	save func(context.Context, Report) error
}

func (h recordingHost) SaveWorkflow(ctx context.Context, report Report) error {
	return h.save(ctx, report)
}

func TestWorkflowCheckpointCancellation(t *testing.T) {
	for _, checkpoint := range []string{"initial", "intent", "completion"} {
		for _, fault := range []string{"canceled", "wrapped_canceled", "save_failed", "joined_canceled", "live_canceled", "unrelated_timeout"} {
			t.Run(checkpoint+"/"+fault, func(t *testing.T) {
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				blocker := &Error{Code: "workflow_blocked", Message: "missing prerequisite", Session: "worker", Result: map[string]any{"task": "task", "revision": float64(2)}}
				writeErr := errors.New("checkpoint storage failed")
				var calls atomic.Int32
				var saved []Report
				injected := false
				host := recordingHost{
					hostFunc: func(context.Context, Operation) (any, error) {
						calls.Add(1)
						return "receipt", nil
					},
					save: func(c context.Context, report Report) error {
						saved = append(saved, report)
						if report.Status != "running" {
							if c.Err() != nil {
								t.Errorf("terminal checkpoint used canceled context: %v", c.Err())
							}
							if _, ok := c.Deadline(); !ok {
								t.Error("terminal checkpoint has no timeout")
							}
							return nil
						}
						phase := "initial"
						if len(report.Steps) > 0 {
							phase = "intent"
							if report.Steps[0].Status == "completed" {
								phase = "completion"
							}
						}
						if phase != checkpoint {
							return nil
						}
						if injected {
							t.Error("failed checkpoint was retried")
						}
						injected = true
						if fault != "live_canceled" {
							cancel(blocker)
						}
						switch fault {
						case "wrapped_canceled":
							return fmt.Errorf("checkpoint: %w", c.Err())
						case "save_failed":
							return writeErr
						case "joined_canceled":
							return fmt.Errorf("checkpoint: %w", errors.Join(c.Err(), writeErr))
						case "unrelated_timeout":
							return context.DeadlineExceeded
						default:
							return context.Canceled
						}
					},
				}
				r := Runner{Host: host}
				report, err := r.Run(ctx, script(`try { return await polly.agent({}); } catch (e) { return "swallowed"; }`), map[string]any{})
				var failure *Error
				if !injected || !errors.As(err, &failure) || report == nil || report.Error == nil || report.Output != nil {
					t.Fatalf("checkpoint failure lost: injected=%v report=%+v err=%v", injected, report, err)
				}
				wantCode := "workflow_blocked"
				switch fault {
				case "save_failed":
					wantCode = "host_failed"
					if !errors.Is(err, writeErr) {
						t.Fatalf("cancellation hid storage error: %v", err)
					}
				case "joined_canceled":
					wantCode = "canceled"
					if !errors.Is(err, writeErr) || !errors.Is(err, context.Canceled) {
						t.Fatalf("cancellation hid joined storage error: %v", err)
					}
				case "live_canceled":
					wantCode = "canceled"
				case "unrelated_timeout":
					wantCode = "timeout"
				}
				if failure.Code != wantCode || report.Error.Code != wantCode || failure.Report != report {
					t.Fatalf("wrong checkpoint failure: report=%+v error=%+v", report.Error, failure)
				}
				if wantCode == "workflow_blocked" {
					for _, got := range []*Error{failure, report.Error} {
						if got.Session != blocker.Session || got.Message != blocker.Message || !reflect.DeepEqual(got.Result, blocker.Result) {
							t.Fatalf("blocker provenance lost: %+v", got)
						}
					}
				}
				wantStatus := "interrupted"
				if fault == "live_canceled" {
					wantStatus = "failed"
				}
				if report.Status != wantStatus || !reflect.DeepEqual(saved[len(saved)-1], *report) {
					t.Fatalf("terminal report not preserved: report=%+v saved=%+v", report, saved[len(saved)-1])
				}
				wantCalls := int32(0)
				if checkpoint == "completion" {
					wantCalls = 1
					if report.Steps[0].Status != "completed" || report.Steps[0].Value != "receipt" {
						t.Fatalf("completed receipt lost: %+v", report.Steps)
					}
				}
				if calls.Load() != wantCalls {
					t.Fatalf("host calls=%d, want %d", calls.Load(), wantCalls)
				}
			})
		}
	}
}

func TestWorkflowTerminalSaveFailurePreservesBothErrors(t *testing.T) {
	for _, writeErr := range []error{errors.New("terminal storage failed"), context.Canceled, context.DeadlineExceeded} {
		t.Run(writeErr.Error(), func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			blocker := &Error{Code: "workflow_blocked", Message: "missing prerequisite", Session: "worker", Result: map[string]any{"task": "task"}}
			host := recordingHost{
				hostFunc: func(context.Context, Operation) (any, error) {
					cancel(blocker)
					return nil, blocker
				},
				save: func(c context.Context, report Report) error {
					if report.Status == "running" {
						return nil
					}
					if c.Err() != nil {
						t.Errorf("terminal checkpoint used canceled context: %v", c.Err())
					}
					return writeErr
				},
			}
			r := Runner{Host: host}
			report, err := r.Run(ctx, script(`return await polly.agent({});`), map[string]any{})
			var failure *Error
			if !errors.As(err, &failure) || !errors.Is(err, writeErr) || !errors.Is(err, blocker) {
				t.Fatalf("terminal write or blocker error lost: %v", err)
			}
			if report.Status != "failed" || report.Error.Code != blocker.Code || failure.Code != blocker.Code || failure.Session != blocker.Session || !reflect.DeepEqual(failure.Result, blocker.Result) {
				t.Fatalf("terminal failure lost blocker provenance: report=%+v error=%+v", report, failure)
			}
			wantMessage := errors.Join(blocker, writeErr).Error()
			if report.Error.Message != wantMessage || failure.Message != wantMessage {
				t.Fatalf("terminal write failure hidden: report=%+v error=%+v", report.Error, failure)
			}
		})
	}
}
