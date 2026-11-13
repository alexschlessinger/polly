package swarm

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
)

// finishGate is a parent session that holds the wait for the write of every
// edit that completes an execution until want of them are waiting.
type finishGate struct {
	coordinatedSession
	t    *testing.T
	want int
	// held runs once, when the last of them has arrived and all still wait.
	held func()

	mu      sync.Mutex
	waiting int
	all     chan struct{}
}

func (g *finishGate) CoordinationRevision(ctx context.Context) (uint64, error) {
	return g.coordinatedSession.(sessions.CoordinationRevisionSession).CoordinationRevision(ctx)
}

func (g *finishGate) UpdateCoordinationFrom(ctx context.Context, known uint64, fn func(*sessions.CoordinationState) error) error {
	var state *sessions.CoordinationState
	err := g.coordinatedSession.(sessions.CoordinationDeltaSession).UpdateCoordinationFrom(ctx, known, func(raw *sessions.CoordinationState) error {
		state = raw
		return fn(raw)
	})
	if err != nil {
		return err
	}
	completes := false
	for _, payload := range state.Changes["execution"] {
		var e Execution
		completes = completes || json.Unmarshal(payload, &e) == nil && e.Status == "completed"
	}
	written := state.Written
	if !completes || written == nil {
		return nil
	}
	state.Written = func(ctx context.Context) error {
		g.mu.Lock()
		g.waiting++
		last := g.waiting == g.want
		g.mu.Unlock()
		if last {
			g.held()
			close(g.all)
		}
		select {
		case <-g.all:
		case <-time.After(3 * time.Second):
			g.t.Errorf("a finish waited for its write while the others could not begin")
		}
		return written(ctx)
	}
	return nil
}

// Members that finish at once wait for their writes together, and an
// operation of the parent's stays out until the last of them is written.
func TestFinishesWaitForTheirWritesTogether(t *testing.T) {
	t.Parallel()
	const members = 3
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Starting a member is an operation of the parent's, so every member is
	// started before the first may finish.
	started := make(chan struct{})
	model := modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		select {
		case <-started:
		case <-ctx.Done():
		}
		return answer("done")
	})
	var r *Runtime
	var gate *finishGate
	// A disk store has a write to wait for; a memory store has none.
	r = runtimeTestOn(t, sessions.StoreConfig{Mode: sessions.ModeDisk, Path: filepath.Join(t.TempDir(), "polly.db")}, model, members, members, func(parent sessions.Session) sessions.Session {
		gate = &finishGate{coordinatedSession: parent.(coordinatedSession), t: t, want: members, all: make(chan struct{})}
		gate.held = func() {
			if r.parentTools.TryLock() {
				r.parentTools.Unlock()
				t.Errorf("an operation of the parent's could begin while finishes waited for their writes")
			}
		}
		return gate
	})
	done := make([]<-chan struct{}, members)
	for n := range done {
		result, err := r.Spawn(ctx, subagent.Request{Label: "Worker", Task: "work", ReadOnly: true, Background: true})
		if err != nil {
			t.Fatal(err)
		}
		done[n] = result.Done
	}
	close(started)
	for _, ch := range done {
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatal("a member did not finish")
		}
	}
	gate.mu.Lock()
	waited := gate.waiting
	gate.mu.Unlock()
	if waited != members {
		t.Fatalf("%d finishes waited for a write, want %d", waited, members)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for id, e := range s.Executions {
		if e.Status != "completed" {
			t.Fatalf("execution %s is %s", id, e.Status)
		}
	}
	// With the finishes over, the parent's operations run again.
	if !r.parentTools.TryLock() {
		t.Fatal("the finishes left parentTools held")
	}
	r.parentTools.Unlock()
}
