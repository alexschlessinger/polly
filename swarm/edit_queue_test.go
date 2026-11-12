package swarm

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// heldEdit starts an edit that holds the family's state until the returned
// release is called, and returns once it does.
func heldEdit(t *testing.T, r *Runtime) (release func()) {
	t.Helper()
	holding, let, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- r.edit(context.Background(), func(*State) error {
			close(holding)
			<-let
			return nil
		})
	}()
	select {
	case <-holding:
	case err := <-done:
		t.Fatalf("the holding edit did not run: %v", err)
	}
	return func() {
		close(let)
		if err := <-done; err != nil {
			t.Errorf("the holding edit: %v", err)
		}
	}
}

// awaitQueuedEdits returns once want edits wait for the family's state.
func awaitQueuedEdits(t *testing.T, r *Runtime, want int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(200 * time.Microsecond) {
		r.edits.mu.Lock()
		waiting := len(r.edits.waiting)
		r.edits.mu.Unlock()
		if waiting == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d edits wait for the state, want %d", waiting, want)
		}
	}
}

// Edits that queue up behind one that holds the family's state run in the
// order they asked, however many callers take a turn at running them, and
// each caller gets what its own edit came to.
func TestQueuedEditsRunInOrderForTheirOwnCallers(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, idleModel(), 1, 1)
	ctx := context.Background()
	task, err := r.CreateTask(ctx, "task", "criteria", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	// More than two batches, so the lead passes on with edits still waiting.
	const queued = 2*editBatch + 5
	release := heldEdit(t, r)
	var order []int
	results := make([]chan error, queued)
	for n := range results {
		results[n] = make(chan error, 1)
		go func() {
			results[n] <- r.edit(ctx, func(s *State) error {
				if r.editing.TryRLock() {
					r.editing.RUnlock()
					t.Errorf("edit %d ran while a reader could look at the state", n)
				}
				order = append(order, n)
				s.ownTask(task.ID).Description += fmt.Sprintf(" %d", n)
				if n%3 == 0 {
					return fmt.Errorf("refused %d", n)
				}
				return nil
			})
		}()
		awaitQueuedEdits(t, r, n+1)
	}
	release()
	want := "task"
	for n, result := range results {
		var err error
		select {
		case err = <-result:
		case <-time.After(10 * time.Second):
			t.Fatalf("edit %d did not return", n)
		}
		if n%3 == 0 {
			if err == nil || err.Error() != fmt.Sprintf("refused %d", n) {
				t.Fatalf("edit %d returned %v, want its own refusal", n, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("edit %d: %v", n, err)
		}
		want += fmt.Sprintf(" %d", n)
	}
	for n, ran := range order {
		if ran != n {
			t.Fatalf("edits ran in the order %v", order)
		}
	}
	if len(order) != queued {
		t.Fatalf("%d edits ran, want %d", len(order), queued)
	}
	if got := sameAsStored(t, r).Tasks[task.ID].Description; got != want {
		t.Fatalf("the edits left %q, want %q", got, want)
	}
	// Nobody leads once the queue is empty, and the next edit leads itself.
	r.edits.mu.Lock()
	led, waiting := r.edits.led, len(r.edits.waiting)
	r.edits.mu.Unlock()
	if led || waiting != 0 {
		t.Fatalf("after the edits: led = %v with %d waiting", led, waiting)
	}
	if err := r.edit(ctx, func(s *State) error { s.ownTask(task.ID).Description = "after"; return nil }); err != nil {
		t.Fatal(err)
	}
}

// An edit that panics while another caller runs it panics in the caller that
// made it, and the edits around it are unharmed.
func TestQueuedEditPanicsInItsOwnCaller(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, idleModel(), 1, 1)
	ctx := context.Background()
	task, err := r.CreateTask(ctx, "task", "criteria", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	release := heldEdit(t, r)
	panicked, after := make(chan any, 1), make(chan error, 1)
	go func() {
		defer func() { panicked <- recover() }()
		_ = r.edit(ctx, func(*State) error { panic("an edit's own panic") })
	}()
	awaitQueuedEdits(t, r, 1)
	go func() {
		after <- r.edit(ctx, func(s *State) error { s.ownTask(task.ID).Description = "after"; return nil })
	}()
	awaitQueuedEdits(t, r, 2)
	release()
	for _, wait := range []func() error{
		func() error {
			if got := <-panicked; got != "an edit's own panic" {
				return fmt.Errorf("the edit's caller recovered %v", got)
			}
			return nil
		},
		func() error { return <-after },
	} {
		done := make(chan error, 1)
		go func() { done <- wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("an edit behind a panic did not return")
		}
	}
	if got := sameAsStored(t, r).Tasks[task.ID].Description; got != "after" {
		t.Fatalf("the edit behind the panic left %q", got)
	}
}

// The diagnostics of an edit name the code that made it, also when another
// caller runs it.
func TestEditSiteNamesTheCaller(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, queued := range []bool{false, true} {
		// The edit leaves a record changed where every reader sees it, so each
		// case has a family of its own.
		r := runtimeTest(t, idleModel(), 1, 1)
		task, err := r.CreateTask(ctx, "task", "criteria", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		release := func() {}
		if queued {
			release = heldEdit(t, r)
		}
		panicked := make(chan any, 1)
		go func() {
			defer func() { panicked <- recover() }()
			_ = r.edit(ctx, func(s *State) error { s.Tasks[task.ID].Description += " in place"; return nil })
		}()
		if queued {
			awaitQueuedEdits(t, r, 1)
		}
		release()
		var got any
		select {
		case got = <-panicked:
		case <-time.After(10 * time.Second):
			t.Fatal("the edit did not return")
		}
		if message, _ := got.(string); !strings.Contains(message, "the edit at edit_queue_test.go:") {
			t.Fatalf("queued = %v: the edit panicked with %v", queued, got)
		}
	}
}
