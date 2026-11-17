package swarm

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
)

// statusWrite is one change the runtime made to an execution's record.
type statusWrite struct {
	execution string
	status    string
	volatile  bool // the store was told it need not write the change
	waited    bool // the store handed back a wait for the edit
	became    bool // the execution was last seen with another status
	rides     bool // the edit saves something of a transcript as well
}

// statusLog is every status the runtime gave an execution, in order.
type statusLog struct {
	mu     sync.Mutex
	writes []statusWrite
	last   map[string]string
}

func (l *statusLog) note(state *sessions.CoordinationState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = map[string]string{}
	}
	for id, payload := range state.Changes["execution"] {
		var e Execution
		if json.Unmarshal(payload, &e) != nil {
			continue
		}
		l.writes = append(l.writes, statusWrite{
			execution: id, status: e.Status, volatile: state.Volatile["execution"][id], waited: state.Written != nil,
			became: l.last[id] != e.Status, rides: len(state.Append) > 0 || state.Transcript != nil,
		})
		l.last[id] = e.Status
	}
}

func (l *statusLog) seen() []statusWrite {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]statusWrite(nil), l.writes...)
}

// statusWatch is a session that notes every status the runtime gives an
// execution through it.
type statusWatch struct {
	coordinatedSession
	log *statusLog
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
	if err == nil {
		w.log.note(state)
	}
	return err
}

func (w *statusWatch) seen() []statusWrite { return w.log.seen() }

// statusWatchStore hands the runtime member sessions that note what they
// write in log, as the parent's statusWatch does.
type statusWatchStore struct {
	memberStore
	log *statusLog
}

func (s *statusWatchStore) watched(handle sessions.Session, err error) (sessions.Session, error) {
	if err != nil {
		return nil, err
	}
	return &statusWatch{coordinatedSession: handle.(coordinatedSession), log: s.log}, nil
}

func (s *statusWatchStore) Acquire(ctx context.Context, name string, opts sessions.AcquireOptions) (sessions.Session, error) {
	return s.watched(s.memberStore.Acquire(ctx, name, opts))
}

func (s *statusWatchStore) Create(ctx context.Context, name string, opts sessions.AcquireOptions, configure func(*sessions.Metadata)) (sessions.Session, error) {
	return s.watched(s.memberStore.Create(ctx, name, opts, configure))
}

func (s *statusWatchStore) Sync(ctx context.Context) error {
	return s.memberStore.(sessions.SyncStore).Sync(ctx)
}

// watchStatuses has r note what its members' sessions write in the log of
// watch, which wraps its parent.
func watchStatuses(r *Runtime, watch *statusWatch) {
	r.config.Store = &statusWatchStore{memberStore: r.config.Store.(memberStore), log: watch.log}
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

// That a member is running says what it is doing at the moment. The runtime's
// own readers see it at once. Its slice's first request says so, and the
// store is told it need not write the record for that: the database holds the
// status the member last saved with something else, until the outcome is
// recorded. A member that wakes to a free slot is never queued.
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
		watch = &statusWatch{coordinatedSession: parent.(coordinatedSession), log: &statusLog{}}
		return watch
	})
	watchStatuses(r, watch)
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
	began := 0
	for _, write := range watch.seen() {
		switch {
		case write.volatile && write.waited && !write.rides:
			t.Fatalf("a change the store need not write handed back a wait: %+v", write)
		case !write.became:
		case write.status == "running":
			if !write.volatile || !write.rides {
				t.Fatalf("running was written for its own sake: %+v in %+v", write, watch.seen())
			}
			began++
		case write.status == "queued" && write.volatile:
			t.Fatalf("a member that woke to a free slot was queued: %+v", watch.seen())
		}
	}
	// Two slices began, each with a request that said so.
	if began != 2 {
		t.Fatalf("%d requests said the member is running, want 2: %+v", began, watch.seen())
	}
}

// A member that wakes while every slot is taken is queued until one is free.
// The runtime's own readers see that at once; the store need not write it,
// and nobody waits for it.
func TestWokenMemberIsQueuedWhileItWaitsForASlot(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	holding, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	parked := false
	model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		for _, message := range req.Messages {
			if strings.Contains(message.Content, "hold the slot") {
				once.Do(func() { close(holding) })
				select {
				case <-release:
				case <-ctx.Done():
				}
				return answer("held")
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if !parked {
			parked = true
			return iterationTool("park", "wait_agent", `{}`)
		}
		return answer("done")
	})
	var watch *statusWatch
	r := runtimeTestOn(t, sessions.StoreConfig{Mode: sessions.ModeDisk, Path: filepath.Join(t.TempDir(), "polly.db")}, model, 1, 2, func(parent sessions.Session) sessions.Session {
		watch = &statusWatch{coordinatedSession: parent.(coordinatedSession), log: &statusLog{}}
		return watch
	})
	watchStatuses(r, watch)
	waiter, err := r.Spawn(ctx, subagent.Request{Label: "Waiter", Task: "wait, then answer", ReadOnly: true})
	if err != nil || !waiter.Yielded {
		t.Fatalf("member did not park: %+v %v", waiter, err)
	}
	holder, err := r.Spawn(ctx, subagent.Request{Label: "Holder", Task: "hold the slot", ReadOnly: true, Background: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-holding:
	case <-ctx.Done():
		t.Fatal("the second member did not take the slot")
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	execution := s.Members[waiter.Session].Execution
	if _, err = r.Send(ctx, r.ID, waiter.Session, "info", "", "go on"); err != nil {
		t.Fatal(err)
	}
	for status := ""; status != "queued"; {
		if ctx.Err() != nil {
			t.Fatalf("a member woken with no slot free is %s", status)
		}
		time.Sleep(5 * time.Millisecond)
		if s, err = r.State(ctx); err != nil {
			t.Fatal(err)
		}
		status = s.Executions[execution].Status
	}
	close(release)
	for _, done := range []<-chan struct{}{holder.Done, waiter.Done} {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("a member did not finish")
		}
	}
	queued, ran := 0, false
	for _, write := range watch.seen() {
		if write.execution != execution || !write.became {
			continue
		}
		switch write.status {
		case "queued":
			// The first is the member's start, which is saved.
			if queued++; queued == 2 && (!write.volatile || write.waited) {
				t.Fatalf("queued was written for its own sake: %+v", write)
			}
		case "running":
			ran = queued == 2
		}
	}
	if queued != 2 || !ran {
		t.Fatalf("the member was queued %d times and then ran = %v: %+v", queued, ran, watch.seen())
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
