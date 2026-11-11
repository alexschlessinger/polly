package swarm

import (
	"context"
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
}

// turnWriteStore hands the runtime member sessions that note what each of
// their updates wrote.
type turnWriteStore struct {
	sessions.SessionStore
	mu     sync.Mutex
	writes []turnWrite
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
	var write turnWrite
	err := s.coordinatedSession.(sessions.CoordinationDeltaSession).UpdateCoordinationFrom(ctx, known, func(state *sessions.CoordinationState) error {
		if err := fn(state); err != nil {
			return err
		}
		write = turnWrite{appended: len(state.Append), execution: len(state.Changes["execution"]) > 0}
		return nil
	})
	if err == nil {
		s.store.mu.Lock()
		s.store.writes = append(s.store.writes, write)
		s.store.mu.Unlock()
	}
	return err
}

// A member writes its execution record when it takes its assignment, when it
// journals a tool batch and when its slice ends. The request between two
// batches appends the last batch to the transcript and leaves the record
// alone, and the journal that follows carries the count and usage so far.
func TestRequestBetweenToolBatchesLeavesTheExecutionRecordAlone(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var r *Runtime
	calls := 0
	model := modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		calls++
		// The request for this call is saved, and no journal has followed it.
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
			reply = iterationTool("list", "list_agents", `{}`)
		}
		reply.SetTokenUsage(10, 2)
		return reply
	})
	r = runtimeTest(t, model, 1, 1)
	store := &turnWriteStore{SessionStore: r.config.Store}
	r.config.Store = store
	result, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "review", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	// The assignment, then for each of two batches a request and a journal,
	// then the request that is answered and the end of the slice.
	want := []turnWrite{{2, true}, {0, false}, {0, true}, {2, false}, {0, true}, {2, false}, {1, true}}
	got := store.seen()
	if len(got) != len(want) {
		t.Fatalf("member updates = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("member update %d = %+v, want %+v (all: %+v)", i, got[i], want[i], got)
		}
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e := s.Executions[result.Execution]
	if e.Status != "completed" || e.Iterations != 3 || e.Usage.Samples != 3 || *e.Usage.InputTokens != 30 || len(e.Intent) != 0 || e.IntentAt != 0 {
		t.Fatalf("finished execution %+v", e)
	}
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
