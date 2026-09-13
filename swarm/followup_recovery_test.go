package swarm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

// A one-shot store boundary fault, selected by durable state transition rather
// than a brittle transaction count. before runs inside the transaction; after
// runs only once that transaction has committed.
type refreshStoreFault struct {
	sessions.CoordinationSession
	mu     sync.Mutex
	match  func(*State, *State) bool
	before func(*State) error
	after  func() error
	reads  atomic.Int32
}

func (f *refreshStoreFault) ReadCoordination(ctx context.Context) (*sessions.CoordinationState, error) {
	if f.reads.CompareAndSwap(1, 0) {
		return nil, errors.New("injected confirmation read failure")
	}
	return f.CoordinationSession.ReadCoordination(ctx)
}

func (f *refreshStoreFault) UpdateCoordination(ctx context.Context, fn func(*sessions.CoordinationState) error) error {
	var after func() error
	err := f.CoordinationSession.UpdateCoordination(ctx, func(raw *sessions.CoordinationState) error {
		prior, err := decodeState(raw)
		if err != nil {
			return err
		}
		if err := fn(raw); err != nil {
			return err
		}
		next, err := decodeState(raw)
		if err != nil {
			return err
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.match == nil || !f.match(prior, next) {
			return nil
		}
		f.match = nil
		after = f.after
		if f.before != nil {
			if err := f.before(next); err != nil {
				return err
			}
			return encodeState(raw, next)
		}
		return nil
	})
	if err == nil && after != nil {
		return after()
	}
	return err
}

func refreshTransition(stage string) func(*State, *State) bool {
	return func(before, after *State) bool {
		switch stage {
		case "selection":
			return len(after.Followups) > len(before.Followups)
		case "release_mark":
			for id, c := range after.Contexts {
				if old := before.Contexts[id]; old != nil && old.Release == "" && c.Release == WorkspaceReleasing {
					return true
				}
			}
		case "release_receipt":
			return len(after.Contexts) < len(before.Contexts)
		case "allocation":
			return len(after.Contexts) > len(before.Contexts)
		case "launch":
			return len(after.Executions) > len(before.Executions)
		}
		return false
	}
}

func settledRefreshWorker(t *testing.T, r *Runtime) AgentResult {
	t.Helper()
	a, err := r.Agent(context.Background(), "", AgentRequest{TaskName: "worker", Label: "Worker", Task: "inspect", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	admitParent(t, r)
	return a
}

func TestRefreshPreparationStorageFailures(t *testing.T) {
	for _, stage := range []string{"selection", "release_mark", "release_receipt", "allocation", "launch"} {
		for _, committed := range []bool{false, true} {
			t.Run(stage+map[bool]string{false: "-rollback", true: "-lost-reply"}[committed], func(t *testing.T) {
				r := scratchRuntime(t, doneModel(), true)
				suspendAutoRelease(t, r)
				ctx := context.Background()
				a := settledRefreshWorker(t, r)
				before, _ := r.read(ctx)
				fault := &refreshStoreFault{CoordinationSession: r.parent, match: refreshTransition(stage)}
				if committed {
					fault.after = func() error { return errors.New("lost commit reply") }
				} else {
					fault.before = func(*State) error { return errors.New("injected storage rollback") }
				}
				r.parent = fault
				mail, err := r.followupTask(ctx, a.Session, "refresh", "fault", true)
				if stage == "launch" && committed {
					if err != nil {
						t.Fatal("committed launch was not recovered:", err)
					}
					awaitIdle(t, r, ctx)
					again, err := r.followupTask(ctx, a.Session, "refresh", "fault", true)
					if err != nil || mail.ID != again.ID {
						t.Fatal("committed launch replay changed receipt")
					}
					s, _ := r.read(ctx)
					if len(s.Executions) != 2 || len(s.Tasks) != 2 {
						t.Fatal("committed launch duplicated")
					}
					return
				}
				if err == nil {
					t.Fatal("fault went unreported")
				}
				r.wakeIdleMember(a.Session)
				after, readErr := r.read(ctx)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if !reflect.DeepEqual(before.Tasks, after.Tasks) || !reflect.DeepEqual(before.Executions, after.Executions) || pendingFollowup(after, a.Session) {
					t.Fatal("failed preparation left runnable work")
				}
				for id, f := range after.Followups {
					if f.Phase != "failed" || after.Messages[id].Start || after.Messages[id].StartError == "" {
						t.Fatalf("failure was not durable: %+v %+v", f, after.Messages[id])
					}
					if _, err := r.followupTask(ctx, a.Session, "refresh", "fault", true); err == nil {
						t.Fatal("recorded failure silently retried")
					}
				}
			})
		}
	}
}

func TestOrdinaryFollowupLostSelectionReplyDisablesStartup(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "steering"}[active], func(t *testing.T) {
			entered := make(chan struct{})
			r := runtimeTest(t, modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
				close(entered)
				if active {
					<-ctx.Done()
				}
				return answer("done")
			}), 1, 3)
			suspendAutoRelease(t, r)
			ctx := context.Background()
			i, err := r.start(ctx, "", AgentRequest{TaskName: "worker", Label: "Worker", Task: "inspect", ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			<-entered
			if !active {
				awaitIdle(t, r, ctx)
				admitParent(t, r)
			}
			r.parent = &refreshStoreFault{CoordinationSession: r.parent, match: refreshTransition("selection"), after: func() error { return errors.New("lost selection reply") }}
			if _, err := r.FollowupTask(ctx, i.member, "inspect again", "ordinary-failure"); err == nil {
				t.Fatal("lost reply was not reported")
			}
			s, _ := r.read(ctx)
			if pendingFollowup(s, i.member) || len(s.Executions) != 1 || len(s.Tasks) != 1 {
				t.Fatal("failed selection left startup intent")
			}
			for id, f := range s.Followups {
				if f.Phase != "failed" || s.Messages[id].StartError == "" {
					t.Fatal("missing durable refusal")
				}
			}
		})
	}
}

func TestRefreshUnconfirmedLaunchIsPaused(t *testing.T) {
	r := scratchRuntime(t, doneModel(), true)
	suspendAutoRelease(t, r)
	a := settledRefreshWorker(t, r)
	fault := &refreshStoreFault{CoordinationSession: r.parent, match: refreshTransition("launch")}
	fault.after = func() error { fault.reads.Store(1); return errors.New("lost commit reply") }
	r.parent = fault
	ctx := context.Background()
	if _, err := r.followupTask(ctx, a.Session, "refresh", "ambiguous", true); err == nil {
		t.Fatal("failed confirmation not reported")
	}
	r.wakeIdleMember(a.Session)
	s, _ := r.read(ctx)
	e := s.Executions[s.Members[a.Session].Execution]
	if e.ID == a.Execution || e.Status != "paused" || pendingFollowup(s, a.Session) || r.HasActive() {
		t.Fatalf("ambiguous launch was runnable: %+v", e)
	}
	if _, err := r.FollowupTask(ctx, a.Session, "Resume the committed assignment", "resume"); err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, r, ctx)
	s, _ = r.read(ctx)
	if len(s.Executions) != 2 || s.Executions[e.ID].Generation != 2 {
		t.Fatal("resume replaced the committed execution")
	}
}

func reopenRefreshRuntime(t *testing.T, r *Runtime) *Runtime {
	t.Helper()
	ctx := context.Background()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	durable := r.config.Store.(sessions.DurableStore)
	mode, db := durable.Location()
	if mode != sessions.ModeDisk {
		db = filepath.Join(t.TempDir(), "refresh.db")
	}
	if err := durable.Promote(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := r.config.Parent.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.config.Store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeDisk, Path: db})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.Acquire(ctx, "parent", sessions.AcquireOptions{ExpectedID: r.ID, ExistingOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	config := r.config
	config.Store, config.Parent = store, parent
	recovered, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { recovered.Close(); parent.Close(); store.Close() })
	suspendAutoRelease(t, recovered)
	return recovered
}

func TestRefreshInterruptedPreparationRetriesPinnedCapture(t *testing.T) {
	for _, stage := range []string{"selection", "release_mark", "release_receipt", "allocation"} {
		t.Run(stage, func(t *testing.T) {
			r := scratchRuntime(t, doneModel(), true)
			suspendAutoRelease(t, r)
			a := settledRefreshWorker(t, r)
			writeRefreshFile(t, r.config.Root, "version", "selected")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r.parent = &refreshStoreFault{CoordinationSession: r.parent, match: refreshTransition(stage), after: func() error { cancel(); return nil }}
			if _, err := r.followupTask(ctx, a.Session, "refresh", "retry", true); !errors.Is(err, context.Canceled) {
				t.Fatalf("expected cancellation: %v", err)
			}
			ctx = context.Background()
			s, _ := r.read(ctx)
			var id string
			for key, f := range s.Followups {
				id = key
				if f.Phase != "interrupted" || f.Execution != "" || s.Tasks[f.Task] != nil || s.Messages[key].Start {
					t.Fatalf("interrupted preparation became runnable: %+v", f)
				}
			}
			pinned := s.Followups[id].Base
			writeRefreshFile(t, r.config.Root, "version", "newer parent")
			// Persist a process-crash state, not just the graceful error state.
			if err := r.update(ctx, func(s *State) error { s.Followups[id].Phase = "preparing"; return nil }); err != nil {
				t.Fatal(err)
			}
			r = reopenRefreshRuntime(t, r)
			if err := r.prepare(ctx); err != nil {
				t.Fatal(err)
			}
			r.wakeIdleMember(a.Session)
			s, _ = r.read(ctx)
			if len(s.Executions) != 1 || len(inbox(s, a.Session, true)) != 0 || s.Followups[id].Phase != "interrupted" {
				t.Fatal("restart launched or admitted incomplete refresh")
			}
			mail, err := r.followupTask(ctx, a.Session, "refresh", "retry", true)
			if err != nil || mail.ID != id {
				t.Fatalf("explicit retry failed: %+v %v", mail, err)
			}
			awaitIdle(t, r, ctx)
			s, _ = r.read(ctx)
			f := s.Followups[id]
			c := s.Contexts[s.Members[a.Session].Context]
			if f.Base != pinned || len(s.Executions) != 2 || s.Tasks[f.Task].Follows != a.Task || refreshGit(t, c.Root, "show", snapshotCommit(s, pinned)+":version") != "selected" {
				t.Fatal("retry recaptured parent or changed identity")
			}
			admitParent(t, r)
			original := followupView(s, mail)
			r = reopenRefreshRuntime(t, r)
			again, err := r.followupTask(ctx, a.Session, "refresh", "retry", true)
			if err != nil {
				t.Fatal(err)
			}
			view, err := r.followupResult(ctx, again)
			if err != nil || !reflect.DeepEqual(view, original) {
				t.Fatal("restart changed committed receipt")
			}
		})
	}
}

func TestRefreshRestartAfterLaunchUsesPausedRecovery(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{})
	r := scratchRuntime(t, modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 2 {
			close(entered)
			<-ctx.Done()
		}
		return answer("done")
	}), true)
	suspendAutoRelease(t, r)
	a := settledRefreshWorker(t, r)
	writeRefreshFile(t, r.config.Root, "version", "selected")
	ctx := context.Background()
	mail, err := r.followupTask(ctx, a.Session, "refresh", "committed", true)
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	s, _ := r.read(ctx)
	f := *s.Followups[mail.ID]
	writeRefreshFile(t, r.config.Root, "version", "later parent")
	r = reopenRefreshRuntime(t, r)
	again, err := r.followupTask(ctx, a.Session, "refresh", "committed", true)
	if err != nil || again.ID != mail.ID {
		t.Fatalf("committed receipt lost after restart: %+v %v", again, err)
	}
	r.wakeIdleMember(a.Session)
	s, _ = r.read(ctx)
	if calls.Load() != 2 || len(s.Executions) != 2 || s.Executions[f.Execution].Status != "paused" || s.Tasks[f.Task].Status != "blocked" {
		t.Fatal("restart or receipt replay launched a paused assignment")
	}
	if _, err := r.FollowupTask(ctx, a.Session, "Continue the interrupted assignment", "resume"); err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, r, ctx)
	s, _ = r.read(ctx)
	if calls.Load() != 3 || len(s.Executions) != 2 || s.Executions[f.Execution].Generation != 2 || !reflect.DeepEqual(*s.Followups[mail.ID], f) {
		t.Fatal("explicit resume replaced launch identity or replayed work")
	}
	c := s.Contexts[s.Members[a.Session].Context]
	if got, _ := os.ReadFile(filepath.Join(c.Root, "version")); string(got) != "selected" {
		t.Fatalf("paused resume recaptured parent: %q", got)
	}
}

func TestRefreshRechecksRevisionAndCapture(t *testing.T) {
	for _, when := range []string{"selection", "allocation"} {
		for _, change := range []string{"revision", "capture"} {
			t.Run(when+"-"+change, func(t *testing.T) {
				r := scratchRuntime(t, doneModel(), true)
				suspendAutoRelease(t, r)
				a := settledRefreshWorker(t, r)
				r.parent = &refreshStoreFault{CoordinationSession: r.parent, match: refreshTransition(when), before: func(s *State) error {
					if change == "revision" {
						s.Tasks[a.Task].Revision++
					} else {
						for _, f := range s.Followups {
							delete(s.Snapshots, f.Base)
						}
					}
					return nil
				}}
				if _, err := r.followupTask(context.Background(), a.Session, "refresh", "stale", true); err == nil {
					t.Fatal("changed authority accepted")
				}
				s, _ := r.read(context.Background())
				if len(s.Executions) != 1 || len(s.Tasks) != 1 || pendingFollowup(s, a.Session) {
					t.Fatal("stale refresh queued a task")
				}
			})
		}
	}
}

func TestRefreshFilesystemFailures(t *testing.T) {
	for _, failure := range []string{"capture", "allocation", "release"} {
		t.Run(failure, func(t *testing.T) {
			r := scratchRuntime(t, doneModel(), true)
			suspendAutoRelease(t, r)
			a := settledRefreshWorker(t, r)
			ctx := context.Background()
			s, _ := r.read(ctx)
			c := s.Contexts[a.Context]
			manager, _ := r.manager(ctx)
			switch failure {
			case "capture":
				writeRefreshFile(t, r.config.Root, ".gitattributes", "* filter=untrusted\n")
			case "allocation":
				manager.Slots = nil
			case "release":
				writeRefreshFile(t, filepath.Dir(c.Root), "owner", "different-owner")
			}
			if _, err := r.followupTask(ctx, a.Session, "refresh", "filesystem", true); err == nil {
				t.Fatal("filesystem failure ignored")
			}
			r.wakeIdleMember(a.Session)
			s, _ = r.read(ctx)
			if len(s.Executions) != 1 || len(s.Tasks) != 1 || pendingFollowup(s, a.Session) {
				t.Fatal("filesystem failure left runnable work")
			}
			if failure != "allocation" {
				if _, err := os.Stat(c.Root); err != nil {
					t.Fatal("refused release removed worker files")
				}
			}
		})
	}
}

func TestConcurrentRefreshAndMaintenance(t *testing.T) {
	for _, action := range []string{"duplicate", "other_followup", "release", "forget", "close"} {
		t.Run(action, func(t *testing.T) {
			var calls atomic.Int32
			entered, proceed := make(chan struct{}), make(chan struct{})
			r := scratchRuntime(t, modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
				if calls.Add(1) > 1 {
					<-ctx.Done() // keep the committed worker active during the race
				}
				return answer("done")
			}), true)
			suspendAutoRelease(t, r)
			a := settledRefreshWorker(t, r)
			r.parent = &refreshStoreFault{CoordinationSession: r.parent, match: refreshTransition("selection"), after: func() error { close(entered); <-proceed; return nil }}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			first, second := make(chan error, 1), make(chan error, 1)
			go func() { _, err := r.followupTask(ctx, a.Session, "refresh", "race", true); first <- err }()
			<-entered
			go func() {
				var err error
				switch action {
				case "duplicate":
					_, err = r.followupTask(ctx, a.Session, "refresh", "race", true)
				case "other_followup":
					_, err = r.followupTask(ctx, a.Session, "another refresh", "other", true)
				case "release":
					_, err = r.releaseWorkspace(ctx, a.Context)
				case "forget":
					err = r.Forget(ctx)
				case "close":
					err = r.Close()
				}
				second <- err
			}()
			close(proceed)
			select {
			case err := <-first:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("refresh deadlocked with maintenance")
			}
			select {
			case err := <-second:
				if (action == "forget" || action == "other_followup") != (err != nil) {
					t.Fatalf("%s: %v", action, err)
				}
			case <-ctx.Done():
				t.Fatal("maintenance deadlocked with refresh")
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			s, _ := r.read(context.Background())
			if len(s.Executions) != 2 || len(s.Tasks) != 2 {
				t.Fatal("race lost or duplicated launch")
			}
		})
	}
}

func TestFollowupProvenanceOmittedAfterForget(t *testing.T) {
	r := scratchRuntime(t, doneModel(), true)
	suspendAutoRelease(t, r)
	a := settledRefreshWorker(t, r)
	mail, err := r.followupTask(context.Background(), a.Session, "refresh", "forget-result", true)
	if err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, r, context.Background())
	admitParent(t, r)
	if err := r.Forget(context.Background()); err != nil {
		t.Fatal(err)
	}
	v, err := r.followupResult(context.Background(), mail)
	if err != nil || v.BaseCommit != "" || v.BaseOrigin != "parent" || strings.Contains(v.Note, "HEAD") {
		t.Fatalf("invented unavailable provenance: %+v %v", v, err)
	}
}
