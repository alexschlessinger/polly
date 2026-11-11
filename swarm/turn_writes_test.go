package swarm

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
)

// turnWrite is one update a member made through its own session.
type turnWrite struct {
	appended  int
	execution bool // the update wrote the member's execution record
	waited    bool // the member waited for the update to be safe from a crash
}

// turnWriteStore hands the runtime member sessions that note what each of
// their updates wrote and whether the member waited for it.
type turnWriteStore struct {
	sessions.SessionStore
	t      *testing.T
	mu     sync.Mutex
	writes []turnWrite
	// waits are the waits for the updates so far, in order.
	waits []func(context.Context) error
}

func (s *turnWriteStore) Acquire(ctx context.Context, name string, opts sessions.AcquireOptions) (sessions.Session, error) {
	handle, err := s.SessionStore.Acquire(ctx, name, opts)
	if err != nil || !opts.ExistingOnly {
		return handle, err
	}
	return &turnWriteSession{coordinatedSession: handle.(coordinatedSession), store: s}, nil
}

func (s *turnWriteStore) seen() []turnWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]turnWrite(nil), s.writes...)
}

type turnWriteSession struct {
	coordinatedSession
	store *turnWriteStore
}

func (s *turnWriteSession) CoordinationRevision(ctx context.Context) (uint64, error) {
	return s.coordinatedSession.(sessions.CoordinationRevisionSession).CoordinationRevision(ctx)
}

func (s *turnWriteSession) UpdateCoordinationFrom(ctx context.Context, known uint64, fn func(*sessions.CoordinationState) error) error {
	var state *sessions.CoordinationState
	err := s.coordinatedSession.(sessions.CoordinationDeltaSession).UpdateCoordinationFrom(ctx, known, func(raw *sessions.CoordinationState) error {
		state = raw
		return fn(raw)
	})
	if err != nil {
		return err
	}
	store, written := s.store, state.Written
	store.mu.Lock()
	index, earlier := len(store.writes), store.waits
	store.writes = append(store.writes, turnWrite{appended: len(state.Append), execution: len(state.Changes["execution"]) > 0})
	store.waits = append(store.waits, written)
	store.mu.Unlock()
	if written == nil {
		return nil
	}
	state.Written = func(ctx context.Context) error {
		store.mu.Lock()
		store.writes[index].waited = true
		store.mu.Unlock()
		if err := written(ctx); err != nil {
			return err
		}
		// The store writes in order: every earlier update is written too, so
		// its wait has nothing left to wait for.
		over, cancel := context.WithCancel(context.Background())
		cancel()
		for i, wait := range earlier {
			if wait != nil && wait(over) != nil {
				store.t.Errorf("update %d was written before update %d", index, i)
			}
		}
		return nil
	}
	return nil
}

// memberTurnWrites runs a member that calls tool in each of two batches and
// then answers, and returns the updates it made through its own session. Each
// model call checks that the record counts the calls answered before it.
func memberTurnWrites(t *testing.T, tool, args string) []turnWrite {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var r *Runtime
	calls := 0
	model := modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		calls++
		s, err := r.State(ctx)
		if err != nil {
			t.Error(err)
			return answer("failed")
		}
		for _, e := range s.Executions {
			if e.Iterations != calls-1 || e.Usage.Samples != calls-1 {
				t.Errorf("call %d began with %d calls and %d usage samples recorded", calls, e.Iterations, e.Usage.Samples)
			}
		}
		reply := answer("done")
		if calls < 3 {
			reply = iterationTool("batch", tool, args)
		}
		reply.SetTokenUsage(10, 2)
		return reply
	})
	// A disk store logs an update before it is safe from a crash, which is
	// the wait a member makes or skips; a memory store has none to offer.
	r = runtimeTestOn(t, sessions.StoreConfig{Mode: sessions.ModeDisk, Path: filepath.Join(t.TempDir(), "polly.db")}, model, 1, 1, nil)
	store := &turnWriteStore{SessionStore: r.config.Store, t: t}
	r.config.Store = store
	result, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "review", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e := s.Executions[result.Execution]
	if e.Status != "completed" || e.Iterations != 3 || e.Usage.Samples != 3 || *e.Usage.InputTokens != 30 || len(e.Intent) != 0 || e.IntentAt != 0 {
		t.Fatalf("finished execution %+v", e)
	}
	return store.seen()
}

func wantTurnWrites(t *testing.T, got, want []turnWrite) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("member updates = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("member update %d = %+v, want %+v (all: %+v)", i, got[i], want[i], got)
		}
	}
}

// A member writes its execution record when it takes its assignment, when it
// journals a tool batch and when its slice ends. The request after a
// journaled batch appends that batch to the transcript and leaves the record
// alone: the journal already carried the count and usage.
//
// The member does not wait for the store to write a request: it waits for the
// journal before the batch runs, and for the slice's last checkpoint, and by
// then every earlier update is written.
func TestRequestAfterJournaledBatchLeavesTheExecutionRecordAlone(t *testing.T) {
	t.Parallel()
	// The assignment, then for each of two batches a request and a journal,
	// then the request that is answered and the end of the slice.
	wantTurnWrites(t, memberTurnWrites(t, "swarm_publish", `{"text":"finding"}`),
		[]turnWrite{{2, true, true}, {0, false, false}, {0, true, true}, {2, false, false}, {0, true, true}, {2, false, false}, {1, true, true}})
}

// A batch whose tools all only read has no effect a crash could leave
// uncertain, so the member neither journals it nor waits before it runs. The
// request that follows carries the count and usage instead, unwaited, and the
// member waits only for its assignment and for the end of its slice.
func TestBatchThatOnlyReadsIsNotJournaled(t *testing.T) {
	t.Parallel()
	wantTurnWrites(t, memberTurnWrites(t, "list_agents", `{}`),
		[]turnWrite{{2, true, true}, {0, false, false}, {2, true, false}, {2, true, false}, {1, true, true}})
}

// A journal is the batch a slice was running. One the transcript has moved
// past was saved with its results, so a resumed member is told only of the
// batch that was cut short, and of every batch a record without a mark names.
func TestResumeReportsOnlyTheBatchItsTranscriptLacks(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		mark        func(sequence int64) int64
		interrupted int
	}{
		"cut short": {func(sequence int64) int64 { return sequence }, 1},
		"saved":     {func(sequence int64) int64 { return sequence - 1 }, 0},
		"unmarked":  {func(int64) int64 { return 0 }, 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			calls, interrupted := 0, 0
			model := modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				if calls++; calls == 1 {
					return iterationTool("park", "wait_agent", `{}`)
				}
				for _, m := range req.Messages {
					if m.ToolCallID == "uncertain" && m.Content == llm.ToolInterruptedContent {
						interrupted++
					}
				}
				return answer("recovered")
			})
			r := runtimeTest(t, model, 1, 1)
			result, err := r.Spawn(ctx, subagent.Request{Label: "recovery", Task: "wait then recover", ReadOnly: true})
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
			if err := r.update(ctx, func(s *State) error {
				e := s.Executions[member.Execution]
				e.Intent = []messages.ChatMessage{iterationTool("uncertain", "list_agents", `{}`)}
				e.IntentAt = tc.mark(view.Sequence)
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
			if e.Status != "completed" || len(e.Intent) != 0 || e.IntentAt != 0 || calls != 2 || interrupted != tc.interrupted {
				t.Fatalf("execution %+v after %d calls, told of %d interrupted calls, want %d", e, calls, interrupted, tc.interrupted)
			}
		})
	}
}
