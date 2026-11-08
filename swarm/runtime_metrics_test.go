package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestRuntimeStorageScaleCodecObservations(t *testing.T) {
	t.Parallel()
	r := runtimemetrics.New()
	ctx := runtimemetrics.WithRecorder(t.Context(), r)
	raw := &sessions.CoordinationState{Records: map[string]map[string]json.RawMessage{
		"format":  {"swarm": json.RawMessage(`{"version":2}`)},
		"member":  {"m": json.RawMessage(`{"id":"m","name":"worker"}`)},
		"unknown": {"not-decoded": json.RawMessage(`{}`)},
	}}
	state, err := decodeStateContext(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := encodeStateContext(ctx, raw, state); err != nil {
		t.Fatal(err)
	}
	s := r.Snapshot()
	if s.Count(runtimemetrics.RecordsDecoded) != 2 || s.Count(runtimemetrics.RecordsEncoded) != 2 || s.Timing(runtimemetrics.StateDecode).Count != 1 || s.Timing(runtimemetrics.StateEncode).Count != 1 {
		t.Fatalf("codec observations include unconverted records: %+v", s)
	}
	disabled := runtimemetrics.WithRecorder(ctx, nil)
	if _, err := decodeStateContext(disabled, raw); err != nil {
		t.Fatal(err)
	}
	if err := encodeStateContext(disabled, raw, state); err != nil {
		t.Fatal(err)
	}
	if r.Snapshot() != s {
		t.Fatal("disabled codecs reached recorder")
	}
	raw.Records["member"]["m"] = json.RawMessage(`{"id":1}`)
	if _, err := decodeStateContext(ctx, raw); err == nil {
		t.Fatal("malformed member accepted")
	}
	if got := r.Snapshot(); got.Count(runtimemetrics.RecordsDecoded) != 3 || got.Timing(runtimemetrics.StateDecode).Count != 2 {
		t.Fatal("failed decode lost actual format conversion or counted invalid member")
	}
}

func TestRuntimeStorageScaleExecutionPhases(t *testing.T) {
	t.Parallel()
	runtime := runtimeTest(t, doneModel(), 1, 3)
	r := runtimemetrics.New()
	ctx, cancel := context.WithTimeout(runtimemetrics.WithRecorder(t.Context(), r), 10*time.Second)
	defer cancel()
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err := runtime.start(canceled, "", AgentRequest{Label: "canceled", Task: "never run", ReadOnly: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled admission: %v", err)
	}

	// Occupy the actual capacity slot. A queued invocation canceled before it
	// obtains that slot must never count as a running slice.
	runtime.slots <- struct{}{}
	queued, err := runtime.start(ctx, "", AgentRequest{Label: "queued", Task: "never run", ReadOnly: true})
	if err != nil {
		<-runtime.slots
		t.Fatal(err)
	}
	queued.cancel()
	select {
	case <-queued.done:
	case <-ctx.Done():
		<-runtime.slots
		t.Fatal("queued cancellation did not settle")
	}
	<-runtime.slots
	before := r.Snapshot()
	if before.Count(runtimemetrics.ExecutionsStarted) != 0 || before.Count(runtimemetrics.ExecutionsCompleted) != 0 || before.Timing(runtimemetrics.Execution).Count != 0 {
		t.Fatal("queued/canceled work counted as executing")
	}
	result, err := runtime.Spawn(ctx, subagent.Request{Label: "running", Task: "finish", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Done != nil {
		select {
		case <-result.Done:
		case <-ctx.Done():
			t.Fatal("running slice did not settle")
		}
	}
	s := r.Snapshot()
	if s.Count(runtimemetrics.ExecutionsStarted) != 1 || s.Count(runtimemetrics.ExecutionsCompleted) != 1 || s.Timing(runtimemetrics.Execution).Count != 1 || s.Timing(runtimemetrics.Admission).Count != 3 {
		t.Fatalf("phase observations: %+v", s)
	}
	t.Log("observed one running slice entry/exit; counts are not unique logical executions or useful completed work")
}

func TestRuntimeStorageScaleExecutionPreparation(t *testing.T) {
	t.Parallel()
	failure := errors.New("host preparation rejected")
	for _, test := range []struct {
		name       string
		selection  []string
		prepareErr error
		wantError  string
		wantSlices uint64
	}{
		{name: "preparation rejection", prepareErr: failure, wantError: failure.Error()},
		{name: "final tool selection rejection", selection: []string{"missing_host_tool"}, wantError: `required tool "missing_host_tool" cannot honor execution context`},
		{name: "prepared host tool", selection: []string{"host_tool"}, wantSlices: 1},
		{name: "tools disabled", selection: []string{}, wantSlices: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var modelCalls, prepareCalls, callbackCalls, beforeCalls, completeCalls atomic.Int32
			model := modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
				modelCalls.Add(1)
				return answer("done")
			})
			runtime := runtimeWithToolset(t, runtimeTest(t, model, 1, 1), &independentToolset{}, nil)
			r := runtimemetrics.New()
			ctx, cancel := context.WithTimeout(runtimemetrics.WithRecorder(t.Context(), r), 5*time.Second)
			defer cancel()
			runtime.config.Callbacks = func(context.Context, Member) *llm.AgentCallbacks {
				callbackCalls.Add(1)
				return &llm.AgentCallbacks{
					BeforeFirstRequest: func(llm.ProjectionStats) error { beforeCalls.Add(1); return nil },
					OnComplete:         func(*messages.ChatMessage) { completeCalls.Add(1) },
				}
			}
			runtime.config.PrepareMember = func(ctx context.Context, _ sessions.Session, registry *tools.ToolRegistry) (string, error) {
				prepareCalls.Add(1)
				// The observational boundary must not move the durable lifecycle
				// transition or count time spent preparing a member.
				state, err := runtime.State(ctx)
				if err != nil {
					return "", err
				}
				for _, e := range state.Executions {
					if e.Status != "running" || e.Started.IsZero() {
						t.Errorf("preparation lifecycle: %+v", e)
					}
				}
				s := r.Snapshot()
				if s.Count(runtimemetrics.ExecutionsStarted) != 0 || s.Count(runtimemetrics.ExecutionsCompleted) != 0 || s.Timing(runtimemetrics.Execution).Count != 0 {
					t.Error("preparation reached the execution metrics boundary")
				}
				if registry != nil {
					registry.Register(&tools.Func{Name: "host_tool", Desc: "host", Run: func(context.Context, tools.Args) (string, error) { return "host", nil }})
				}
				return "", test.prepareErr
			}
			_, err := runtime.Spawn(ctx, subagent.Request{Label: "preparation", Task: "finish if prepared", ReadOnly: true, Tools: test.selection})
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || err.Error() != test.wantError {
				t.Fatalf("pre-execution error = %v, want %q", err, test.wantError)
			}
			if test.prepareErr != nil && !errors.Is(err, test.prepareErr) {
				t.Fatalf("preparation error identity changed: %v", err)
			}
			if prepareCalls.Load() != 1 || callbackCalls.Load() != 1 || modelCalls.Load() != int32(test.wantSlices) || beforeCalls.Load() != int32(test.wantSlices) || completeCalls.Load() != int32(test.wantSlices) {
				t.Fatalf("calls: prepare=%d callbacks=%d model=%d before=%d complete=%d", prepareCalls.Load(), callbackCalls.Load(), modelCalls.Load(), beforeCalls.Load(), completeCalls.Load())
			}
			s := r.Snapshot()
			if s.Count(runtimemetrics.ExecutionsStarted) != test.wantSlices || s.Count(runtimemetrics.ExecutionsCompleted) != test.wantSlices || s.Timing(runtimemetrics.Execution).Count != test.wantSlices || s.Timing(runtimemetrics.Admission).Count != 1 {
				t.Fatalf("preparation observations: starts=%d exits=%d timings=%d admission=%d", s.Count(runtimemetrics.ExecutionsStarted), s.Count(runtimemetrics.ExecutionsCompleted), s.Timing(runtimemetrics.Execution).Count, s.Timing(runtimemetrics.Admission).Count)
			}
			state, err := runtime.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			wantStatus := "completed"
			if test.wantError != "" {
				wantStatus = "failed"
			}
			for _, e := range state.Executions {
				if e.Status != wantStatus || e.Started.IsZero() || e.Ended.IsZero() {
					t.Fatalf("settled lifecycle: %+v", e)
				}
				if test.wantError != "" && (e.InputSaved || e.Iterations != 0 || e.Error != test.wantError) {
					t.Fatalf("pre-execution failure changed: %+v", e)
				}
			}
		})
	}
}

func TestRuntimeStorageScaleExecutionYieldCountsSlices(t *testing.T) {
	t.Parallel()
	var modelCalls atomic.Int32
	model := modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		if modelCalls.Add(1) == 1 {
			return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "wait", Name: "wait_agent", Arguments: `{}`}}}
		}
		return answer("done")
	})
	runtime := runtimeTest(t, model, 1, 1)
	r := runtimemetrics.New()
	ctx, cancel := context.WithTimeout(runtimemetrics.WithRecorder(t.Context(), r), 5*time.Second)
	defer cancel()
	result, err := runtime.Spawn(ctx, subagent.Request{Label: "yield", Task: "wait then finish", ReadOnly: true})
	if err != nil || !result.Yielded {
		t.Fatalf("spawn: %+v %v", result, err)
	}
	s := r.Snapshot()
	if s.Count(runtimemetrics.ExecutionsStarted) != 1 || s.Count(runtimemetrics.ExecutionsCompleted) != 1 || s.Timing(runtimemetrics.Execution).Count != 1 {
		t.Fatal("yield did not finish the first slice's observations")
	}
	if _, err := runtime.Send(ctx, runtime.ID, result.Session, "info", "", "resume"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-result.Done:
	case <-ctx.Done():
		t.Fatal("slice failed to resume")
	}
	state, err := runtime.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s = r.Snapshot()
	if s.Count(runtimemetrics.ExecutionsStarted) != 2 || s.Count(runtimemetrics.ExecutionsCompleted) != 2 || s.Timing(runtimemetrics.Execution).Count != 2 || s.Timing(runtimemetrics.Admission).Count != 1 {
		t.Fatalf("two-slice observations: starts=%d exits=%d timings=%d admission=%d", s.Count(runtimemetrics.ExecutionsStarted), s.Count(runtimemetrics.ExecutionsCompleted), s.Timing(runtimemetrics.Execution).Count, s.Timing(runtimemetrics.Admission).Count)
	}
	if len(state.Executions) != 1 || modelCalls.Load() != 2 {
		t.Fatalf("logical execution/model calls: %d/%d", len(state.Executions), modelCalls.Load())
	}
	for _, e := range state.Executions {
		if e.Status != "completed" || e.Iterations != 2 {
			t.Fatalf("resumed lifecycle: %+v", e)
		}
	}
}
