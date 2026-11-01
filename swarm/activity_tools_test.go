package swarm

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestLiveToolActivitySnapshotsIndependent(t *testing.T) {
	t.Parallel()
	i := &invocation{id: "execution", generation: 3}
	r := &Runtime{active: map[string]*invocation{"worker": i}}
	calls := []messages.ChatMessageToolCall{
		{ID: "first", Name: "bash", Arguments: `{"command":"go test ./..."}`},
		{ID: "second", Name: "read_file", Arguments: `{"path":"swarm/activity.go"}`},
	}
	original := slices.Clone(calls)
	starts := 0
	cb := &llm.AgentCallbacks{OnToolStart: func(got []messages.ChatMessageToolCall) {
		starts++
		// The observer runs after publication and outside activity locks.
		if snapshot := r.LiveActivities()["worker"]; len(snapshot.Tools) != len(original) {
			t.Fatalf("host callback preceded activity update: %+v", snapshot)
		}
		got[0].Arguments = "changed by host callback"
	}}
	i.bindActivity(cb, 0, 0)
	before := time.Now()
	cb.OnToolStart(calls)
	after := time.Now()
	first := r.LiveActivities()["worker"]
	if starts != 1 || first.Execution != i.id || first.Generation != i.generation || first.Phase != "tools" || len(first.Tools) != 2 {
		t.Fatalf("tool batch not published/chained: starts=%d snapshot=%+v", starts, first)
	}
	for n, tool := range first.Tools {
		if tool.Call != original[n] || tool.Started.Before(before) || tool.Started.After(after) || !tool.Started.Equal(first.Since) {
			t.Fatalf("tool %d lost call/start data: %+v", n, tool)
		}
	}
	// Neither the caller's original input nor a returned snapshot aliases the
	// runtime's values, including the nested call's argument string.
	calls[1].Name = "changed by input owner"
	first.Tools[0].Call.Arguments = "changed by snapshot consumer"
	first.Tools[1].Started = time.Time{}
	second := r.LiveActivities()["worker"]
	if second.Tools[0].Call != original[0] || second.Tools[1].Call != original[1] || second.Tools[1].Started.IsZero() {
		t.Fatalf("caller mutated runtime tools: %+v", second.Tools)
	}
	cb.OnToolEnd(original[0], "private result is not activity", time.Second, nil)
	current := r.LiveActivities()["worker"]
	if len(current.Tools) != 1 || current.Tools[0].Call != original[1] {
		t.Fatalf("settled call not removed: %+v", current.Tools)
	}
	if len(second.Tools) != 2 || second.Tools[0].Call != original[0] || second.Tools[1].Call != original[1] {
		t.Fatalf("settlement mutated previous snapshot: %+v", second.Tools)
	}
	current.Tools[0].Call.ID = "changed after deletion"
	if got := r.LiveActivities()["worker"].Tools[0].Call; got != original[1] {
		t.Fatalf("post-settlement snapshot aliases runtime: %+v", got)
	}
}

func TestLiveToolActivityRepeatedProviderIDsCompleteByHostIdentity(t *testing.T) {
	t.Parallel()
	i := &invocation{}
	r := &Runtime{active: map[string]*invocation{"worker": i}}
	cb := &llm.AgentCallbacks{}
	i.bindActivity(cb, 0, 0)
	first := messages.ChatMessageToolCall{ID: "repeat", DisplayID: "first", Name: "read_file"}
	second := messages.ChatMessageToolCall{ID: "repeat", DisplayID: "second", Name: "read_file"}
	cb.OnToolStart([]messages.ChatMessageToolCall{first, second})
	cb.OnToolEnd(messages.ChatMessageToolCall{ID: "repeat", DisplayID: "unrelated"}, "stale", 0, nil)
	if tools := r.LiveActivities()["worker"].Tools; len(tools) != 2 {
		t.Fatalf("unrelated host identity removed a sibling: %+v", tools)
	}
	cb.OnToolEnd(second, "second result", 0, nil)
	if tools := r.LiveActivities()["worker"].Tools; len(tools) != 1 || tools[0].Call != first {
		t.Fatalf("reverse completion removed the wrong sibling: %+v", tools)
	}
	cb.OnToolEnd(first, "first result", 0, nil)
	if tools := r.LiveActivities()["worker"].Tools; tools != nil {
		t.Fatalf("completed batch retained tools: %+v", tools)
	}
}

func TestLiveToolActivityParallelEndsChainCallbacks(t *testing.T) {
	t.Parallel()
	i := &invocation{}
	r := &Runtime{active: map[string]*invocation{"worker": i}}
	const count = 32
	calls := make([]messages.ChatMessageToolCall, count)
	for n := range calls {
		calls[n] = messages.ChatMessageToolCall{ID: fmt.Sprint(n), Name: "bash", Arguments: fmt.Sprintf(`{"command":"work %d"}`, n)}
	}
	failure := errors.New("tool failed")
	var starts, ends atomic.Int32
	seen := make(chan messages.ChatMessageToolCall, count)
	cb := &llm.AgentCallbacks{
		OnToolStart: func(got []messages.ChatMessageToolCall) {
			starts.Add(1)
			if !slices.Equal(got, calls) {
				t.Errorf("host start callback got different batch: %+v", got)
			}
		},
		OnToolEnd: func(call messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
			ends.Add(1)
			if result != "private output "+call.ID || duration != 2*time.Second || err != failure {
				t.Errorf("host end callback arguments changed: call=%+v result=%q duration=%v err=%v", call, result, duration, err)
			}
			// Exercise snapshot reads concurrently with other tool completions,
			// and verify the host callback is never called under an activity lock.
			for _, tool := range r.LiveActivities()["worker"].Tools {
				if tool.Call.ID == call.ID {
					t.Errorf("host end callback preceded removal of %s", call.ID)
				}
			}
			seen <- call
		},
	}
	i.bindActivity(cb, 0, 0)
	cb.OnToolStart(calls)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, call := range calls {
		wg.Go(func() {
			<-start
			cb.OnToolEnd(call, "private output "+call.ID, 2*time.Second, failure)
		})
	}
	close(start)
	wg.Wait()
	close(seen)
	if a := r.LiveActivities()["worker"]; a.Tools != nil || starts.Load() != 1 || ends.Load() != count {
		t.Fatalf("parallel completions lost updates/callbacks: tools=%+v starts=%d ends=%d", a.Tools, starts.Load(), ends.Load())
	}
	completed := make(map[string]bool)
	for call := range seen {
		if completed[call.ID] {
			t.Fatalf("duplicate host completion: %+v", call)
		}
		completed[call.ID] = true
	}
	if len(completed) != count {
		t.Fatalf("host received only %d distinct completions", len(completed))
	}
}

func TestLiveToolActivityResetsAndRejectsStaleCallbacks(t *testing.T) {
	t.Parallel()
	i := &invocation{}
	r := &Runtime{active: map[string]*invocation{"worker": i}}
	old := messages.ChatMessageToolCall{ID: "old", Name: "bash"}
	current := messages.ChatMessageToolCall{ID: "current", Name: "read_file", Arguments: `{"path":"README.md"}`}
	cb := &llm.AgentCallbacks{}
	i.bindActivity(cb, 0, 0)
	cb.OnModelRequest(0, 0)
	cb.OnToolStart([]messages.ChatMessageToolCall{old})
	cb.OnModelRequest(0, 1)
	if a := r.LiveActivities()["worker"]; a.Tools != nil || a.Attempt != 2 || a.Phase != "requesting" {
		t.Fatalf("new request retained stale tool activity: %+v", a)
	}
	cb.OnToolStart([]messages.ChatMessageToolCall{current})
	cb.OnToolEnd(old, "late result", 0, nil)
	if tools := r.LiveActivities()["worker"].Tools; len(tools) != 1 || tools[0].Call != current {
		t.Fatalf("unmatched completion removed current tool: %+v", tools)
	}
	cb.OnAttemptAborted(0, 1, errors.New("aborted"))
	if a := r.LiveActivities()["worker"]; a.Tools != nil || a.Phase != "preparing" {
		t.Fatalf("abort retained active tools: %+v", a)
	}
	cb.OnToolStart([]messages.ChatMessageToolCall{old})
	next := &llm.AgentCallbacks{}
	i.bindActivity(next, 0, 0)
	if a := r.LiveActivities()["worker"]; a.Tools != nil {
		t.Fatalf("new invocation retained previous tool batch: %+v", a.Tools)
	}
	next.OnModelRequest(0, 0)
	next.OnToolStart([]messages.ChatMessageToolCall{current})
	// Even a matching call ID cannot let a previous invocation's callbacks
	// mutate the new invocation's active tools.
	cb.OnToolStart([]messages.ChatMessageToolCall{old})
	cb.OnToolEnd(current, "stale result", 0, nil)
	cb.OnAttemptAborted(0, 1, errors.New("stale abort"))
	if a := r.LiveActivities()["worker"]; len(a.Tools) != 1 || a.Tools[0].Call != current || a.Phase != "tools" {
		t.Fatalf("stale callbacks changed current activity: %+v", a)
	}
}

func TestLiveToolActivityWaitingAndMissingIDs(t *testing.T) {
	t.Parallel()
	i := &invocation{}
	r := &Runtime{active: map[string]*invocation{"worker": i}}
	cb := &llm.AgentCallbacks{}
	i.bindActivity(cb, 0, 0)
	wait := messages.ChatMessageToolCall{ID: "wait", Name: "wait_agent"}
	cb.OnToolStart([]messages.ChatMessageToolCall{wait})
	if a := r.LiveActivities()["worker"]; a.Phase != "waiting" || len(a.Tools) != 1 || a.Tools[0].Call != wait {
		t.Fatalf("waiting phase lost its active call: %+v", a)
	}
	first := messages.ChatMessageToolCall{Name: "read_file", Arguments: `{"path":"first.go"}`}
	second := messages.ChatMessageToolCall{Name: "read_file", Arguments: `{"path":"second.go"}`}
	cb.OnToolStart([]messages.ChatMessageToolCall{first, second})
	cb.OnToolEnd(second, "second result", 0, nil)
	if tools := r.LiveActivities()["worker"].Tools; len(tools) != 1 || tools[0].Call != first {
		t.Fatalf("missing ID matched the wrong call: %+v", tools)
	}
	cb.OnToolEnd(first, "first result", 0, nil)
	if tools := r.LiveActivities()["worker"].Tools; tools != nil {
		t.Fatalf("settled batch retained tools: %+v", tools)
	}
}
