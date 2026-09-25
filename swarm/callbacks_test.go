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
)

func TestManagedRunsRejectHostPersistenceCallbacks(t *testing.T) {
	t.Parallel()
	for _, hook := range []string{"AdmitInput", "Checkpoint", "JournalToolBatch", "all"} {
		for _, member := range []bool{false, true} {
			name := "parent/" + hook
			if member {
				name = "member/" + hook
			}
			t.Run(name, func(t *testing.T) {
				var modelCalls, hostCalls atomic.Int32
				model := modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
					modelCalls.Add(1)
					return answer("done")
				})
				r := runtimeTest(t, model, 1, 1)
				cb := &llm.AgentCallbacks{BeforeFirstRequest: func(llm.ProjectionStats) error { hostCalls.Add(1); return nil }}
				if hook == "AdmitInput" || hook == "all" {
					cb.AdmitInput = func(context.Context) ([]messages.ChatMessage, error) { hostCalls.Add(1); return nil, nil }
				}
				if hook == "Checkpoint" || hook == "all" {
					cb.Checkpoint = func(context.Context, llm.AgentCheckpoint) error { hostCalls.Add(1); return nil }
				}
				if hook == "JournalToolBatch" || hook == "all" {
					cb.JournalToolBatch = func(context.Context, llm.AgentCheckpoint) error { hostCalls.Add(1); return nil }
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				var err error
				if member {
					r.config.Callbacks = func(context.Context, Member) *llm.AgentCallbacks { return cb }
					_, err = r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "review", ReadOnly: true})
				} else {
					agent := parentAgent(t, r, model, 2)
					_, err = r.RunParent(ctx, agent, &llm.CompletionRequest{}, cb, nil)
				}
				if !errors.Is(err, ErrCallbackOwnership) {
					t.Fatalf("error = %v, want callback ownership rejection", err)
				}
				for _, field := range []string{"AdmitInput", "Checkpoint", "JournalToolBatch"} {
					if (hook == field || hook == "all") && !strings.Contains(err.Error(), field) {
						t.Errorf("error %q omitted conflicting hook %s", err, field)
					}
				}
				if modelCalls.Load() != 0 || hostCalls.Load() != 0 {
					t.Fatalf("rejected run executed model=%d host=%d", modelCalls.Load(), hostCalls.Load())
				}
				if member {
					s, err := r.State(ctx)
					if err != nil {
						t.Fatal(err)
					}
					for _, e := range s.Executions {
						if e.InputSaved || e.Iterations != 0 || e.Status != "failed" {
							t.Fatalf("rejected callbacks consumed assignment: %+v", e)
						}
					}
				} else {
					if p := r.ParentState(nil); p.Busy || p.Display != "idle" {
						t.Fatalf("rejected callbacks changed parent lifecycle: %+v", p)
					}
					// Rejection must not leave the parent marked running.
					agent := parentAgent(t, r, model, 2)
					if _, err := r.RunParent(ctx, agent, &llm.CompletionRequest{}, nil, nil); err != nil {
						t.Fatalf("valid run after rejection: %v", err)
					}
				}
			})
		}
	}
}

func TestParentPreservesHostContinuationAndObservers(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"continue", "error", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls, continuations, usages, completions int
			model := modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				calls++
				if calls == 2 && req.Messages[len(req.Messages)-1].Content != "host continuation" {
					t.Error("host continuation was not passed to the model")
				}
				return answer("done")
			})
			r := runtimeTest(t, model, 1, 1)
			agent := parentAgent(t, r, model, 3)
			hostErr := errors.New("host stopped")
			cb := &llm.AgentCallbacks{
				OnIterationUsage: func(int, int, int) { usages++ },
				OnComplete:       func(*messages.ChatMessage) { completions++ },
				ContinueAfterFinal: func(context.Context, *messages.ChatMessage) ([]messages.ChatMessage, error) {
					continuations++
					if continuations > 1 {
						return nil, nil
					}
					switch outcome {
					case "continue":
						return messages.User("host continuation"), nil
					case "error":
						return nil, hostErr
					default:
						cancel()
						return nil, nil
					}
				},
			}
			resp, err := r.RunParent(ctx, agent, &llm.CompletionRequest{}, cb, nil)
			wantCalls, wantComplete := 1, 0
			var wantErr error
			switch outcome {
			case "continue":
				wantCalls, wantComplete = 2, 1
			case "error":
				wantErr = hostErr
			case "cancel":
				wantErr = context.Canceled
			}
			if !errors.Is(err, wantErr) || calls != wantCalls || continuations != wantCalls || usages != wantCalls || completions != wantComplete {
				t.Fatalf("err=%v model=%d continuations=%d usages=%d completions=%d", err, calls, continuations, usages, completions)
			}
			if resp == nil || resp.PersistedMessages != len(resp.AllMessages) {
				t.Fatalf("runtime checkpoint was lost: %+v", resp)
			}
			if cb.AdmitInput != nil || cb.Checkpoint != nil || cb.JournalToolBatch != nil || cb.BeforeToolExecute != nil || cb.OnToolEnd != nil {
				t.Fatal("runtime bindings mutated the host callbacks")
			}
		})
	}
}

func TestParentHostContinuationPrecedesSettlement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var calls, continuations int
	model := modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		calls++
		if calls == 2 && req.Messages[len(req.Messages)-1].Content != "host continuation" {
			t.Error("settlement displaced host continuation")
		}
		if calls == 3 && !strings.Contains(req.Messages[len(req.Messages)-1].Content, "Coordination is still outstanding") {
			t.Error("host continuation bypassed settlement")
		}
		return answer("provisional")
	})
	r := runtimeTest(t, model, 1, 1)
	if _, err := r.CreateTask(ctx, "pending work", "review", nil, ""); err != nil {
		t.Fatal(err)
	}
	cb := &llm.AgentCallbacks{ContinueAfterFinal: func(context.Context, *messages.ChatMessage) ([]messages.ChatMessage, error) {
		continuations++
		if continuations == 1 {
			return messages.User("host continuation"), nil
		}
		return nil, nil
	}}
	_, err := r.RunParent(ctx, parentAgent(t, r, model, 5), &llm.CompletionRequest{}, cb, nil)
	if !errors.Is(err, errSettlementBlocked) || calls != 3 || continuations != 3 {
		t.Fatalf("err=%v model=%d continuations=%d", err, calls, continuations)
	}
}

func TestMemberPreservesHostBatchHooksAndReceipts(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"continue", "stop", "stop while parking"} {
		t.Run(outcome, func(t *testing.T) {
			var calls, before, after, receipts, usages atomic.Int32
			tool := "list_agents"
			if outcome == "stop while parking" {
				tool = "wait_agent"
			}
			model := modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
				if calls.Add(1) == 1 {
					return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
						ToolCalls: []messages.ChatMessageToolCall{{ID: "call", Name: tool, Arguments: `{}`}}}
				}
				return answer("done")
			})
			r := runtimeTest(t, model, 1, 1)
			hostErr := errors.New("host batch stopped")
			cb := &llm.AgentCallbacks{
				BeforeToolBatch:  func(context.Context, []messages.ChatMessageToolCall) error { before.Add(1); return nil },
				OnIterationUsage: func(int, int, int) { usages.Add(1) },
				OnToolResult: func(_ messages.ChatMessageToolCall, result messages.ChatMessage) {
					if succeeded, known := result.ToolSucceeded(); !known || !succeeded {
						t.Errorf("unexpected tool receipt: %+v", result)
					}
					receipts.Add(1)
				},
				AfterToolBatch: func(context.Context) error {
					after.Add(1)
					if outcome != "continue" {
						return hostErr
					}
					return nil
				},
			}
			r.config.Callbacks = func(context.Context, Member) *llm.AgentCallbacks { return cb }
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "review", ReadOnly: true})
			wantCalls := int32(2)
			var wantErr error
			if outcome != "continue" {
				wantCalls, wantErr = 1, hostErr
			}
			if !errors.Is(err, wantErr) || calls.Load() != wantCalls || before.Load() != 1 || after.Load() != 1 || receipts.Load() != 1 || usages.Load() != wantCalls {
				t.Fatalf("err=%v model=%d before=%d after=%d receipts=%d usages=%d", err, calls.Load(), before.Load(), after.Load(), receipts.Load(), usages.Load())
			}
			s, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			e := s.Executions[result.Execution]
			if e == nil || len(e.Intent) != 0 || e.Status == "waiting" {
				t.Fatalf("host error lost checkpoint or parked: %+v", e)
			}
			if cb.AdmitInput != nil || cb.Checkpoint != nil || cb.JournalToolBatch != nil || cb.BeforeToolExecute != nil || cb.ContinueAfterFinal != nil {
				t.Fatal("runtime bindings mutated the host callbacks")
			}
		})
	}
}
