package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
)

// A local-only provider makes fanout assertions independent of poll timing.
// Production source/remote behavior is covered by the notification API tests.
func localWaitNotifications(t *testing.T, r *Runtime) *changeHub {
	t.Helper()
	hub := newChangeHub()
	r.notifications = &Notifications{hub: hub, source: &notificationSource{root: r.ID, durable: true}}
	t.Cleanup(hub.Close)
	return hub
}

type waitReadBarrier struct {
	ctx    context.Context
	cancel context.CancelFunc
	reads  chan int
	resume chan struct{}
	done   chan error
}

func startWaitReadBarrier(t *testing.T, r *Runtime, root sessions.CoordinationSession, wait func(context.Context) error) *waitReadBarrier {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	b := &waitReadBarrier{ctx: ctx, cancel: cancel, reads: make(chan int), resume: make(chan struct{}), done: make(chan error, 1)}
	handle := &semanticWaitSession{CoordinationSession: root, CoordinationRecords: root.(sessions.CoordinationRecords)}
	handle.after = func(n int) {
		select {
		case b.reads <- n:
		case <-ctx.Done():
			return
		}
		select {
		case <-b.resume:
		case <-ctx.Done():
		}
	}
	r.parent = handle
	finished := make(chan struct{})
	t.Cleanup(func() { cancel(); <-finished })
	go func() {
		defer close(finished)
		b.done <- wait(ctx)
	}()
	return b
}

func (b *waitReadBarrier) read(t *testing.T, want int) {
	t.Helper()
	select {
	case got := <-b.reads:
		if got != want {
			t.Fatalf("logical read = %d, want %d", got, want)
		}
	case err := <-b.done:
		t.Fatalf("wait ended before read %d: %v", want, err)
	case <-b.ctx.Done():
		t.Fatalf("wait did not reach read %d: %v", want, b.ctx.Err())
	}
}

func (b *waitReadBarrier) advance(t *testing.T) {
	t.Helper()
	select {
	case b.resume <- struct{}{}:
	case <-b.ctx.Done():
		t.Fatal(b.ctx.Err())
	}
}

func (b *waitReadBarrier) result(t *testing.T, want error) {
	t.Helper()
	select {
	case err := <-b.done:
		if !errors.Is(err, want) {
			t.Fatalf("wait error = %v, want %v", err, want)
		}
	case n := <-b.reads:
		t.Fatalf("unexpected logical read %d before completion", n)
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not finish")
	}
}

func commitWaitRecord(t *testing.T, r *Runtime, root sessions.CoordinationSession, kind, id string, value any) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := root.(sessions.CoordinationRecords).UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error {
		return tx.Put(sessions.RecordKey{Kind: kind, ID: id}, payload)
	})
	if err != nil {
		t.Fatal(err)
	}
	r.publishChanges(commit.Changed)
}

func assertWaitScopes(t *testing.T, hub *changeHub, scopes []sessions.RevisionScope, hinted bool) {
	t.Helper()
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.subscriptions) != 1 {
		t.Fatalf("subscriptions = %d, want 1", len(hub.subscriptions))
	}
	for sub := range hub.subscriptions {
		if len(sub.scopes) != len(scopes) {
			t.Fatalf("scopes = %v, want %v", sub.scopes, scopes)
		}
		for _, scope := range scopes {
			if _, ok := sub.scopes[scope]; !ok {
				t.Fatalf("missing scope %v in %v", scope, sub.scopes)
			}
		}
		if got := len(sub.changes) != 0; got != hinted {
			t.Fatalf("hint = %v, want %v", got, hinted)
		}
	}
}

func TestNotificationInvocationLocalScopeIsolation(t *testing.T) {
	t.Parallel()
	r, root := lifecycleRecordFixture(t, 1, false)
	hub := localWaitNotifications(t, r)
	i := &invocation{member: "selected", waitState: "selected:1:running"}
	b := startWaitReadBarrier(t, r, root, func(ctx context.Context) error { return r.waitInvocation(ctx, i) })
	b.read(t, 1)
	b.advance(t)
	b.read(t, 2) // discovering the task scope requires a second read
	scopes := append(invocationWaitScopes(i.member), waitRecordScope("task", "selected"))
	assertWaitScopes(t, hub, scopes, false)
	for _, scope := range []sessions.RevisionScope{
		{Index: sessions.CoordinationIndexRecords}, ActivityScope(),
		waitRecordScope("task", "other"), waitRecordScope("member", "other"),
		{Index: sessions.CoordinationIndexMailPending, Key: "other"},
		{Index: sessions.CoordinationIndexPublicationPending, Key: "other"},
	} {
		hub.Publish([]sessions.RevisionScope{scope})
		assertWaitScopes(t, hub, scopes, false)
	}
	// An actual unrelated commit also leaves the logical read parked.
	commitWaitRecord(t, r, root, "mail", "other", &Mail{ID: "other", To: "other", Text: "not selected"})
	assertWaitScopes(t, hub, scopes, false)
	commitWaitRecord(t, r, root, "mail", "selected-new", &Mail{ID: "selected-new", To: "selected", Text: "ready"})
	assertWaitScopes(t, hub, scopes, true)
	b.advance(t)
	b.read(t, 3)
	b.advance(t)
	b.result(t, nil)
	if i.waitState != "selected:1:running" || i.err != nil {
		t.Fatalf("wait mutated invocation: %+v", i)
	}
}

func TestNotificationInvocationReplacesMissingAssignment(t *testing.T) {
	t.Parallel()
	r, root := lifecycleRecordFixture(t, 0, false)
	hub := localWaitNotifications(t, r)
	commitWaitRecord(t, r, root, "member", "selected", &Member{ID: "selected", Task: "absent-before"})
	i := &invocation{member: "selected", waitState: ""}
	b := startWaitReadBarrier(t, r, root, func(ctx context.Context) error { return r.waitInvocation(ctx, i) })
	b.read(t, 1)
	b.advance(t)
	b.read(t, 2)
	commitWaitRecord(t, r, root, "member", "selected", &Member{ID: "selected", Task: "absent-after"})
	b.advance(t)
	b.read(t, 3)
	// The newly discovered assignment changes after its read but before Replace.
	// No old scope can hint this commit; the mandatory reread must find it.
	commitWaitRecord(t, r, root, "task", "absent-after", &Task{ID: "absent-after", Revision: 1, Status: "running"})
	b.advance(t)
	b.read(t, 4)
	assertWaitScopes(t, hub, append(invocationWaitScopes("selected"), waitRecordScope("task", "absent-after")), false)
	b.advance(t)
	b.result(t, nil)
}

func TestNotificationInvocationReplacesMissingDependency(t *testing.T) {
	t.Parallel()
	r, root := lifecycleRecordFixture(t, 0, false)
	hub := localWaitNotifications(t, r)
	commitWaitRecord(t, r, root, "task", "selected", &Task{ID: "selected", Revision: 1, Status: "running", Dependencies: []string{"old-missing"}})
	i := &invocation{member: "selected", waitState: "selected:1:running"}
	b := startWaitReadBarrier(t, r, root, func(ctx context.Context) error { return r.waitInvocation(ctx, i) })
	b.read(t, 1)
	b.advance(t)
	b.read(t, 2)
	commitWaitRecord(t, r, root, "task", "selected", &Task{ID: "selected", Revision: 1, Status: "running", Dependencies: []string{"new-missing", "new-missing"}})
	b.advance(t)
	b.read(t, 3)
	b.advance(t)
	b.read(t, 4)
	scopes := append(invocationWaitScopes("selected"), waitRecordScope("task", "selected"), waitRecordScope("task", "new-missing"))
	assertWaitScopes(t, hub, scopes, false)
	commitWaitRecord(t, r, root, "task", "old-missing", &Task{ID: "old-missing", Revision: 1, Status: "done"})
	assertWaitScopes(t, hub, scopes, false)
	commitWaitRecord(t, r, root, "task", "new-missing", &Task{ID: "new-missing", Revision: 1, Status: "done"})
	assertWaitScopes(t, hub, scopes, true)
	b.advance(t)
	b.read(t, 5)
	b.advance(t)
	b.result(t, nil)
}

func TestNotificationInvocationPublicationAudience(t *testing.T) {
	t.Parallel()
	for _, move := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "execution-run-move"}[move], func(t *testing.T) {
			t.Parallel()
			r, root := lifecycleRecordFixture(t, 0, false)
			hub := localWaitNotifications(t, r)
			i := &invocation{member: "selected", waitState: "selected:1:running"}
			b := startWaitReadBarrier(t, r, root, func(ctx context.Context) error { return r.waitInvocation(ctx, i) })
			b.read(t, 1)
			b.advance(t)
			b.read(t, 2)
			scopes := append(invocationWaitScopes("selected"), waitRecordScope("task", "selected"))
			commitWaitRecord(t, r, root, "publication", "hidden", &Publication{ID: "hidden", Author: "other", Run: "different", Text: "not visible", Posted: time.Now().UTC()})
			assertWaitScopes(t, hub, scopes, false)
			if move {
				// PublicationPending tracks execution/run audience itself. The wait
				// must not subscribe to execution or run records to notice this.
				commitWaitRecord(t, r, root, "execution", "selected", &Execution{ID: "selected", Member: "selected", Run: "different", Status: "running"})
			} else {
				commitWaitRecord(t, r, root, "publication", "visible", &Publication{ID: "visible", Author: "other", Run: "run", Text: "visible", Posted: time.Now().UTC()})
			}
			assertWaitScopes(t, hub, scopes, true)
			b.advance(t)
			b.read(t, 3)
			b.advance(t)
			b.result(t, nil)
		})
	}
}

func TestNotificationInvocationDeadlineCancellationAndShutdown(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"deadline", "cancel", "shutdown", "ready-shutdown"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			r, root := lifecycleRecordFixture(t, 0, false)
			hub := localWaitNotifications(t, r)
			i := &invocation{member: "selected", waitState: "selected:1:running"}
			if mode == "deadline" {
				i.waitUntil = time.Now().Add(-time.Second)
			}
			if mode == "ready-shutdown" {
				commitWaitRecord(t, r, root, "mail", "ready", &Mail{ID: "ready", To: "selected"})
			}
			b := startWaitReadBarrier(t, r, root, func(ctx context.Context) error { return r.waitInvocation(ctx, i) })
			b.read(t, 1)
			if mode != "ready-shutdown" {
				b.advance(t)
				b.read(t, 2)
			}
			switch mode {
			case "cancel":
				b.cancel()
			case "shutdown", "ready-shutdown":
				hub.Close()
				b.advance(t)
			case "deadline":
				b.advance(t)
			}
			want := error(context.Canceled)
			if mode == "deadline" {
				want = nil
			}
			b.result(t, want)
		})
	}
}

func TestNotificationInvocationFutureDeadline(t *testing.T) {
	t.Parallel()
	r, _ := lifecycleRecordFixture(t, 0, false)
	hub := localWaitNotifications(t, r)
	i := &invocation{member: "selected", waitState: "selected:1:running", waitUntil: time.Now().Add(25 * time.Millisecond)}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := r.waitInvocation(ctx, i); err != nil {
		t.Fatal(err)
	}
	if time.Now().Before(i.waitUntil) {
		t.Fatal("wait returned before deadline without an event")
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.subscriptions) != 0 || len(hub.scopes) != 0 {
		t.Fatal("deadline leaked subscription scopes")
	}
}

func TestNotificationParentExactScopesAndActivity(t *testing.T) {
	t.Parallel()
	r, root := lifecycleRecordFixture(t, 0, false)
	hub := localWaitNotifications(t, r)
	r.active["selected"] = &invocation{}
	var wake parentWake
	b := startWaitReadBarrier(t, r, root, func(ctx context.Context) (err error) { wake, err = r.waitParent(ctx); return err })
	b.read(t, 1)
	b.advance(t)
	b.read(t, 2)
	scopes := []sessions.RevisionScope{{Index: sessions.CoordinationIndexSemanticNews}, {Index: sessions.CoordinationIndexSemanticMail, Key: r.ID}, ActivityScope()}
	assertWaitScopes(t, hub, scopes, false)
	commitWaitRecord(t, r, root, "publication", "unrelated", &Publication{ID: "unrelated", Author: "other", Run: "run", Text: "not parent news", Posted: time.Now().UTC()})
	commitWaitRecord(t, r, root, "mail", "unrelated", &Mail{ID: "unrelated", To: "selected", Text: "not parent mail"})
	assertWaitScopes(t, hub, scopes, false)
	r.mu.Lock()
	delete(r.active, "selected")
	r.mu.Unlock()
	r.publishActivity()
	assertWaitScopes(t, hub, scopes, true)
	b.advance(t)
	b.read(t, 3)
	b.advance(t)
	b.result(t, nil)
	if !wake.Idle || wake.News {
		t.Fatalf("wake = %+v, want idle without news", wake)
	}
}

func TestNotificationParentReservationDuringRead(t *testing.T) {
	t.Parallel()
	r, root := lifecycleRecordFixture(t, 0, false)
	localWaitNotifications(t, r)
	var wake parentWake
	b := startWaitReadBarrier(t, r, root, func(ctx context.Context) (err error) { wake, err = r.waitParent(ctx); return err })
	b.read(t, 1)
	b.advance(t)
	b.read(t, 2)
	r.mu.Lock()
	r.active["reserved"] = &invocation{}
	r.mu.Unlock()
	r.publishActivity()
	b.advance(t)
	b.read(t, 3) // not an Idle return based on the pre-read active count
	b.cancel()
	b.result(t, context.Canceled)
	if wake.Idle || wake.News {
		t.Fatalf("canceled wake = %+v", wake)
	}
}

func TestNotificationParentSemanticNewsAndMail(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"news", "mail", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			r, root := lifecycleRecordFixture(t, 0, false)
			hub := localWaitNotifications(t, r)
			r.active["selected"] = &invocation{}
			var wake parentWake
			b := startWaitReadBarrier(t, r, root, func(ctx context.Context) (err error) { wake, err = r.waitParent(ctx); return err })
			b.read(t, 1)
			b.advance(t)
			b.read(t, 2)
			switch mode {
			case "news":
				commitWaitRecord(t, r, root, "task", "news", &Task{ID: "news", Revision: 1, Status: "blocked"})
			case "mail":
				commitWaitRecord(t, r, root, "mail", "news", &Mail{ID: "news", To: r.ID, Text: "parent mail"})
			case "shutdown":
				hub.Close()
			}
			b.advance(t)
			if mode == "shutdown" {
				b.result(t, context.Canceled)
				return
			}
			b.read(t, 3)
			b.advance(t)
			b.result(t, nil)
			if !wake.News || wake.Idle {
				t.Fatalf("wake = %+v, want news without idle", wake)
			}
		})
	}
}

func TestNotificationSettlementSubscription(t *testing.T) {
	t.Parallel()
	r, _ := lifecycleRecordFixture(t, 0, false)
	hub := localWaitNotifications(t, r)
	sub, err := r.subscribeSettlement(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	scopes := []sessions.RevisionScope{{Index: sessions.CoordinationIndexRecords}, ActivityScope()}
	assertWaitScopes(t, hub, scopes, false)
	hub.Publish([]sessions.RevisionScope{{Index: sessions.CoordinationIndexPublicationPending, Key: "other"}})
	assertWaitScopes(t, hub, scopes, false)
	for _, scope := range scopes {
		hub.Publish([]sessions.RevisionScope{scope})
		assertWaitScopes(t, hub, scopes, true)
		if err := beforeNotificationRead(t.Context(), sub); err != nil {
			t.Fatal(err)
		}
		assertWaitScopes(t, hub, scopes, false)
	}
	hub.Close()
	if err := waitForNotification(t.Context(), sub, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown error = %v", err)
	}
}
