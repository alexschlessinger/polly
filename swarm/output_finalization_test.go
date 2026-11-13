package swarm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestOutputsSurviveProvisionalFinal(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"unchanged", "updated", "iteration_pause"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var r *Runtime
			var path string
			calls := 0
			original := []byte("original output")
			want := original
			model := modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
				calls++
				if calls == 1 {
					s, err := r.State(ctx)
					if err != nil {
						t.Fatal(err)
					}
					path = filepath.Join(tools.OutputDir(onlyContext(t, s).Scratch), "result.bin")
					if err := os.WriteFile(path, original, 0o600); err != nil {
						t.Fatal(err)
					}
					if _, err := r.FollowupTask(ctx, "worker", "Check the output once more", "followup"); err != nil {
						t.Fatal(err)
					}
					return answer("provisional result")
				}
				if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, original) {
					t.Fatalf("continuation lost original output: %q, %v", data, err)
				}
				if name == "updated" {
					want = []byte("corrected output")
					if err := os.WriteFile(path, want, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return answer("final result")
			})
			r = scratchRuntime(t, model, false)
			suspendAutoRelease(t, r)
			request := AgentRequest{TaskName: "worker", Label: "Worker", Task: "produce file", ReadOnly: true}
			if name == "iteration_pause" {
				request.MaxIterations = 1
			}
			result, err := r.Agent(ctx, "", request)
			if name == "iteration_pause" {
				if !llm.IsIterationLimit(err) {
					t.Fatalf("expected iteration pause, got %v", err)
				}
				if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, original) {
					t.Fatalf("pause lost original output: %q, %v", data, err)
				}
				if err := r.ResumeWithIterations(ctx, result.Session, 1); err != nil {
					t.Fatal(err)
				}
				awaitIdle(t, r, ctx)
			} else if err != nil {
				t.Fatal(err)
			}
			s, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			e := s.Executions[s.Members[result.Session].Execution]
			if calls != 2 || e.Status != "completed" || e.Result == nil {
				t.Fatalf("calls=%d execution=%+v", calls, e)
			}
			assertFinalizedOutput(t, r, *e.Result, path, want)
			if name != "iteration_pause" && len(result.Outputs) != 1 {
				t.Fatalf("caller lost outputs: %+v", result)
			}
		})
	}
}

// Fail the selected transaction once, after its callback has prepared the write,
// so a failed commit cannot be mistaken for durable output delivery.
type outputCommitFailure struct {
	sessions.CoordinationSession
	stage string
	err   error
	fired atomic.Bool
}

func (s *outputCommitFailure) UpdateCoordination(ctx context.Context, fn func(*sessions.CoordinationState) error) error {
	return s.CoordinationSession.UpdateCoordination(ctx, func(raw *sessions.CoordinationState) error {
		if err := fn(raw); err != nil {
			return err
		}
		fail := s.stage == "pin" && len(raw.Pins) > 0
		if s.stage == "finalization" {
			state, err := decodeState(raw)
			if err != nil {
				return err
			}
			for _, e := range state.Executions {
				if (e.Status == "completed" || e.Status == "failed") && e.Result != nil && len(e.Result.Outputs) > 0 {
					fail = true
				}
			}
		}
		if fail && s.fired.CompareAndSwap(false, true) {
			return s.err
		}
		return nil
	})
}

func TestOutputsSurviveCommitFailure(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"pin", "finalization"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var r *Runtime
			var path string
			data := []byte("retryable output")
			calls := 0
			model := modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
				calls++
				if calls == 1 {
					s, err := r.State(ctx)
					if err != nil {
						t.Fatal(err)
					}
					path = filepath.Join(tools.OutputDir(onlyContext(t, s).Scratch), "result.bin")
					if err := os.WriteFile(path, data, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return answer("result in result.bin")
			})
			r = scratchRuntime(t, model, false)
			suspendAutoRelease(t, r)
			fault := &outputCommitFailure{CoordinationSession: r.parent, stage: stage, err: errors.New("injected output " + stage + " failure")}
			setParent(r, fault)
			result, err := r.Agent(ctx, "", AgentRequest{TaskName: "worker", Label: "Worker", Task: "produce file", ReadOnly: true})
			if !errors.Is(err, fault.err) || !fault.fired.Load() {
				t.Fatalf("injected failure not observed: %v", err)
			}
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("failed %s lost original output: %q, %v", stage, got, err)
			}
			s, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Tasks[result.Task].Outputs) != 0 {
				t.Fatalf("failed %s committed outputs: %+v", stage, s.Tasks[result.Task])
			}
			if stage == "pin" {
				ref := artifacts.RefForBlob(artifacts.Blob{Data: data})
				if _, reader, err := r.parent.OpenPublishedArtifact(ctx, ref.ID); err == nil {
					reader.Close()
					t.Fatal("failed pin exposed output")
				}
				admitParent(t, r)
				if _, err := r.FollowupTask(ctx, "worker", "Retry the file handoff", ""); err != nil {
					t.Fatal(err)
				}
				awaitIdle(t, r, ctx)
				s, err = r.State(ctx)
				if err != nil {
					t.Fatal(err)
				}
				e := s.Executions[s.Members[result.Session].Execution]
				if calls != 2 || e.Status != "completed" || e.Result == nil {
					t.Fatalf("retry did not complete: calls=%d execution=%+v", calls, e)
				}
				assertFinalizedOutput(t, r, *e.Result, path, data)
			}
		})
	}
}

func assertFinalizedOutput(t *testing.T, r *Runtime, result AgentResult, path string, want []byte) {
	t.Helper()
	ctx := context.Background()
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantID := artifacts.RefForBlob(artifacts.Blob{Data: want}).ID
	for name, outputs := range map[string][]artifacts.Ref{"result": result.Outputs, "task": s.Tasks[result.Task].Outputs} {
		if len(outputs) != 1 || outputs[0].ID != wantID || outputs[0].Name != "result.bin" {
			t.Fatalf("%s outputs = %+v, want %s", name, outputs, wantID)
		}
	}
	_, reader, err := r.parent.OpenPublishedArtifact(ctx, wantID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if closeErr := reader.Close(); err != nil || closeErr != nil || !bytes.Equal(got, want) {
		t.Fatalf("final output bytes = %q, %v, %v", got, err, closeErr)
	}
	if entries, err := os.ReadDir(filepath.Dir(path)); err != nil || len(entries) != 0 {
		t.Fatalf("finalized output directory = %v, %v", entries, err)
	}
}
