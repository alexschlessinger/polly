package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

// deltaRecordingHost is a functional, atomic in-memory recorder, not a native
// persistence capability. It validates and merges deltas to exercise the same
// boundary a record-backed host will implement. It intentionally also implements
// Recorder so tests detect accidentally preferring the full-report path.
type deltaRecordingHost struct {
	hostFunc
	mu            sync.Mutex
	report        Report
	deltas        []ReportDelta
	onSave        func(context.Context, Report) error
	commitOnError bool
}

func (h *deltaRecordingHost) SaveWorkflow(context.Context, Report) error {
	return errors.New("runner used full Recorder despite DeltaRecorder")
}

func (h *deltaRecordingHost) SaveWorkflowDelta(ctx context.Context, d ReportDelta) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if d.Header.ID == "" || (h.report.ID != "" && d.Header.ID != h.report.ID) {
		return errors.New("invalid workflow identity")
	}
	if d.TotalSteps < len(h.report.Steps) {
		return errors.New("step count decreased")
	}
	steps := make([]Step, d.TotalSteps)
	copy(steps, h.report.Steps)
	for index, step := range d.Steps {
		if index < 0 || index >= d.TotalSteps {
			return errors.New("step index out of range")
		}
		steps[index] = step
	}
	for i := len(h.report.Steps); i < d.TotalSteps; i++ {
		if _, ok := d.Steps[i]; !ok {
			return errors.New("gap in step history")
		}
	}
	data, err := json.Marshal(d.Header)
	if err != nil {
		return err
	}
	var next Report
	if err := json.Unmarshal(data, &next); err != nil {
		return err
	}
	next.Steps = steps
	if h.report.Run != "" {
		next.Run = h.report.Run
	}
	next.Acknowledged = next.Acknowledged || h.report.Acknowledged
	h.deltas = append(h.deltas, d)
	if h.onSave != nil {
		err = h.onSave(ctx, next)
	}
	if err == nil || h.commitOnError {
		h.report = next
	}
	return err
}

func (h *deltaRecordingHost) snapshot() Report {
	h.mu.Lock()
	defer h.mu.Unlock()
	copy, err := cloneReport(h.report)
	if err != nil {
		panic(err)
	}
	return copy
}

func checkpointRecorder(h recordingHost, delta bool) Host {
	if delta {
		return &deltaRecordingHost{hostFunc: h.hostFunc, onSave: h.save}
	}
	return h
}

type countedWorkflowReceipt struct{ encodes *atomic.Int32 }

func (r countedWorkflowReceipt) MarshalJSON() ([]byte, error) {
	r.encodes.Add(1)
	return []byte(`{"receipt":"committed"}`), nil
}

func TestRuntimeStorageScaleWorkflowDeltas(t *testing.T) {
	for _, count := range []int{8, 128} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			var encodes atomic.Int32
			host := &deltaRecordingHost{hostFunc: func(context.Context, Operation) (any, error) {
				return countedWorkflowReceipt{encodes: &encodes}, nil
			}}
			runner := Runner{Host: host}
			report, err := runner.Run(context.Background(), script(fmt.Sprintf(`for(let i=0;i<%d;i++) await polly.agent({i}); return "done";`, count)), map[string]any{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(*report, host.snapshot()) {
				t.Fatal("delta reconstruction differs from full final report")
			}
			// Each receipt is encoded once in its completion delta, once for
			// the VM, and once for the full returned report, independent of
			// the amount of previously completed history.
			if got := encodes.Load(); got != int32(3*count) {
				t.Fatalf("receipt encodes=%d, want %d", got, 3*count)
			}
			if len(host.deltas) != 2*count+2 {
				t.Fatalf("checkpoint count=%d", len(host.deltas))
			}
			for i, delta := range host.deltas {
				if i == 0 || i == len(host.deltas)-1 {
					if len(delta.Steps) != 0 {
						t.Fatalf("header checkpoint includes history: %+v", delta)
					}
					continue
				}
				index := (i - 1) / 2
				step, ok := delta.Steps[index]
				if !ok || len(delta.Steps) != 1 || delta.TotalSteps != index+1 || step.ID != fmt.Sprintf("%s/%d", report.ID, index+1) {
					t.Fatalf("checkpoint %d: %+v", i, delta)
				}
			}
			t.Logf("steps=%d checkpoints=%d receipt encodes=%d; full final report remains O(steps)", count, len(host.deltas), encodes.Load())
		})
	}
}

func TestRuntimeStorageScaleWorkflowHelperLimits(t *testing.T) {
	if DefaultMaxHostCalls != 4096 {
		t.Fatalf("default host-call budget=%d, want 4096", DefaultMaxHostCalls)
	}
	for _, tc := range []struct {
		options string
		want    float64
	}{
		{"{}", 8},
		{"{concurrency:256}", 256},
		{"{concurrency:257}", 0},
	} {
		host := &deltaRecordingHost{hostFunc: func(context.Context, Operation) (any, error) {
			t.Error("pure JS branches dispatched a host effect")
			return nil, nil
		}}
		runner := Runner{Host: host}
		report, err := runner.Run(context.Background(), script(`let active=0, peak=0;
await polly.parallel(Array.from({length:300}), async () => {
 active++; peak=Math.max(active,peak); await Promise.resolve(); active--;
}, `+tc.options+`); return peak;`), map[string]any{})
		if tc.want == 0 {
			if err == nil {
				t.Fatal("helper accepted concurrency above 256")
			}
		} else if err != nil || report.Output != tc.want {
			t.Fatalf("options=%s report=%+v err=%v", tc.options, report, err)
		}
		if len(report.Steps) != 0 {
			t.Fatal("JS helper branches counted as host executions")
		}
	}
	// These are JS branch/helper bounds, not runtime execution-slot admission.
	// The runner still retains a full report plus its bounded completion mailbox;
	// deltas remove repeated history encoding, not the requested final history.
}

func TestWorkflowDeltaFixtureRejectsInvalidHistoryAtomically(t *testing.T) {
	for _, delta := range []ReportDelta{
		{TotalSteps: -1},
		{TotalSteps: 1},
		{TotalSteps: 1, Steps: map[int]Step{-1: {}}},
		{TotalSteps: 1, Steps: map[int]Step{1: {}}},
		{TotalSteps: 2, Steps: map[int]Step{1: {}}},
	} {
		host := &deltaRecordingHost{}
		delta.Header.ID = "workflow"
		if err := host.SaveWorkflowDelta(context.Background(), delta); err == nil {
			t.Fatalf("accepted invalid delta: %+v", delta)
		}
		if host.report.ID != "" || len(host.report.Steps) != 0 {
			t.Fatal("invalid delta changed stored report")
		}
	}
}

func TestWorkflowDeltaCopiesOnlyRequestedSteps(t *testing.T) {
	state := Report{ID: "workflow", Input: map[string]any{"input": "original"}, Steps: []Step{
		{Operation: Operation{Args: map[string]any{"arg": "original"}}, Value: map[string]any{"receipt": "original"}},
		{Value: make(chan int)}, // An unrequested old value must not be encoded.
	}}
	delta, err := cloneReportDelta(state, map[int]struct{}{0: {}})
	if err != nil {
		t.Fatal(err)
	}
	delta.Header.Input.(map[string]any)["input"] = "changed"
	delta.Steps[0].Args["arg"] = "changed"
	delta.Steps[0].Value.(map[string]any)["receipt"] = "changed"
	if state.Input.(map[string]any)["input"] != "original" || state.Steps[0].Args["arg"] != "original" || state.Steps[0].Value.(map[string]any)["receipt"] != "original" {
		t.Fatal("recorder mutation changed runner state")
	}
	if _, err := cloneReportDelta(state, map[int]struct{}{1: {}}); err == nil {
		t.Fatal("changed step was not JSON validated")
	}
	data, err := json.Marshal(delta.Header)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["steps"]; ok {
		t.Fatal("delta header contains steps")
	}
}
