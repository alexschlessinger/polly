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

// statusWrite is one change the runtime made to an execution's status through
// the parent's session.
type statusWrite struct {
	status   string
	volatile bool // the store was told it need not write the change
	waited   bool // the store handed back a wait for the change
}

// statusWatch is a parent session that notes every status the runtime gives
// an execution through it.
type statusWatch struct {
	coordinatedSession
	mu     sync.Mutex
	writes []statusWrite
}

func (w *statusWatch) CoordinationRevision(ctx context.Context) (uint64, error) {
	return w.coordinatedSession.(sessions.CoordinationRevisionSession).CoordinationRevision(ctx)
}

func (w *statusWatch) UpdateCoordinationFrom(ctx context.Context, known uint64, fn func(*sessions.CoordinationState) error) error {
	var state *sessions.CoordinationState
	err := w.coordinatedSession.(sessions.CoordinationDeltaSession).UpdateCoordinationFrom(ctx, known, func(raw *sessions.CoordinationState) error {
		state = raw
		return fn(raw)
	})
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, payload := range state.Changes["execution"] {
		var e Execution
		if json.Unmarshal(payload, &e) == nil {
			w.writes = append(w.writes, statusWrite{e.Status, state.Volatile["execution"][id], state.Written != nil})
		}
	}
	return nil
}

func (w *statusWatch) seen() []statusWrite {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]statusWrite(nil), w.writes...)
}

// storedStatus reads an execution's status as the database holds it, through
// a store handle of its own.
func storedStatus(t *testing.T, ctx context.Context, path, root, execution string) string {
	t.Helper()
	other, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeDisk, Path: path})
	if err != nil {
		t.Error(err)
		return ""
	}
	defer other.Close()
	view, err := other.ReadCoordinationView(ctx, root)
	if err != nil {
		t.Error(err)
		return ""
	}
	var e Execution
	if err := json.Unmarshal(view.Records["execution"][execution], &e); err != nil {
		t.Error(err)
	}
	return e.Status
}

// That a member is queued for a slot or running says what it is doing at the
// moment. The runtime's own readers see it at once. The store is told it
// need not write it, and nobody waits for it: the database holds the status
// the member last saved with something else, until the outcome is recorded.
func TestQueuedAndRunningAreNotWrittenForTheirOwnSake(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "polly.db")
	var r *Runtime
	var watch *statusWatch
	calls := 0
	model := modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		calls++
		s, err := r.State(ctx)
		if err != nil {
			t.Error(err)
			return answer("failed")
		}
		for id, e := range s.Executions {
			if e.Status != "running" {
				t.Errorf("call %d: the runtime says its member is %s", calls, e.Status)
			}
			if calls == 2 {
				// The member parked, was woken and runs again. The database
				// has what its last slice saved when it ended.
				if err := r.config.Store.(sessions.SyncStore).Sync(ctx); err != nil {
					t.Error(err)
				}
				if stored := storedStatus(t, ctx, path, r.ID, id); stored != "waiting" {
					t.Errorf("call 2: the database says the member is %s", stored)
				}
			}
		}
		if calls == 1 {
			return iterationTool("park", "wait_agent", `{}`)
		}
		return answer("done")
	})
	r = runtimeTestOn(t, sessions.StoreConfig{Mode: sessions.ModeDisk, Path: path}, model, 1, 1, func(parent sessions.Session) sessions.Session {
		watch = &statusWatch{coordinatedSession: parent.(coordinatedSession)}
		return watch
	})
	result, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "wait, then answer", ReadOnly: true})
	if err != nil || !result.Yielded {
		t.Fatalf("member did not park: %+v %v", result, err)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	execution := s.Members[result.Session].Execution
	if e := s.Executions[execution]; e.Status != "waiting" {
		t.Fatalf("a parked member is %s", e.Status)
	}
	if _, err = r.Send(ctx, r.ID, result.Session, "info", "", "go on"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-result.Done:
	case <-ctx.Done():
		t.Fatal("member did not finish")
	}
	if err := r.config.Store.(sessions.SyncStore).Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if stored := storedStatus(t, ctx, path, r.ID, execution); stored != "completed" {
		t.Fatalf("the database says a finished member is %s", stored)
	}
	volatile := map[string]int{}
	for _, write := range watch.seen() {
		switch {
		case write.volatile && write.waited:
			t.Fatalf("a change the store need not write handed back a wait: %+v", write)
		case write.volatile:
			volatile[write.status]++
		case write.status == "running":
			t.Fatalf("running was written for its own sake: %+v", watch.seen())
		}
	}
	// Two slices began, and one wake queued the member in between.
	if volatile["running"] != 2 || volatile["queued"] != 1 || len(volatile) != 2 {
		t.Fatalf("volatile status changes = %v, all changes %+v", volatile, watch.seen())
	}
}

// A record that a restart finds live may trail its member's transcript: the
// calls whose tools only read were counted in memory. The transcript is saved
// with the counts at every boundary, and the member's next slice takes them
// from there.
func TestRestartTakesCountsFromTheTranscript(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	calls := 0
	model := modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		calls++
		reply := answer("recovered")
		switch calls {
		case 1, 2:
			reply = iterationTool("look", "list_agents", `{}`)
		case 3:
			reply = iterationTool("park", "wait_agent", `{}`)
		}
		reply.SetTokenUsage(10, 2)
		return reply
	})
	r := runtimeTest(t, model, 1, 1)
	result, err := r.Spawn(ctx, subagent.Request{Label: "recovery", Task: "look twice, wait, then answer", ReadOnly: true})
	if err != nil || !result.Yielded {
		t.Fatalf("member did not park: %+v %v", result, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	member := s.Members[result.Session]
	view, err := r.config.Store.(sessions.ViewStore).ReadView(ctx, sessions.ViewTarget{ID: member.ID}, "")
	if err != nil {
		t.Fatal(err)
	}
	var saved executionCounts
	if note := view.Metadata.Transcript; note == nil || json.Unmarshal([]byte(note.Counts), &saved) != nil || saved.Iterations != 3 || saved.Usage.Samples != 3 {
		t.Fatalf("the transcript was saved with the counts %+v", view.Metadata.Transcript)
	}
	// What a crash during the first slice would have left in the database:
	// the record as the assignment wrote it, with no call counted.
	if err := r.update(ctx, func(s *State) error {
		e := s.Executions[member.Execution]
		e.Status, e.Iterations, e.Usage, e.Uncounted = "running", 0, Usage{}, false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fresh, err := New(r.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fresh.Close() })
	if err := fresh.Resume(ctx, result.Session, 0); err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, fresh, ctx)
	s, err = fresh.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e := s.Executions[member.Execution]
	if e.Status != "completed" || calls != 4 || e.Iterations != 4 || e.Usage.Samples != 4 || *e.Usage.InputTokens != 40 || e.Uncounted {
		t.Fatalf("after %d calls the execution is %+v", calls, e)
	}
}
