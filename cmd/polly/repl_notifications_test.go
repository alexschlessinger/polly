package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
)

type replTestChanges struct {
	changes chan struct{}
	done    chan struct{}
	once    sync.Once
	scopes  []sessions.RevisionScope
}

func newREPLTestChanges(scopes []sessions.RevisionScope) *replTestChanges {
	return &replTestChanges{changes: make(chan struct{}, 1), done: make(chan struct{}), scopes: scopes}
}
func (s *replTestChanges) Changes() <-chan struct{} { return s.changes }
func (s *replTestChanges) Done() <-chan struct{}    { return s.done }
func (s *replTestChanges) Close()                   { s.once.Do(func() { close(s.done) }) }
func (s *replTestChanges) publish(scope sessions.RevisionScope) {
	if !slices.Contains(s.scopes, scope) {
		return
	}
	select {
	case s.changes <- struct{}{}:
	default:
	}
}

func installREPLTestWatch(tab *replTab, watch *replChangeWatch, scopes []sessions.RevisionScope) *replTestChanges {
	if tab.notifications == nil {
		tab.notifications = &replNotificationSource{root: tab.viewID(), store: tab.state.sessionStore, runtime: tab.state.swarm, watches: make(map[*replChangeWatch]struct{})}
	}
	sub := newREPLTestChanges(scopes)
	*watch = replChangeWatch{source: tab.notifications, sub: sub, initial: true}
	tab.notifications.watches[watch] = struct{}{}
	return sub
}

func TestREPLChangeWatchCoalescesBeforeRead(t *testing.T) {
	scope := sessions.RevisionScope{Index: sessions.CoordinationIndexDisplay}
	sub := newREPLTestChanges([]sessions.RevisionScope{scope})
	w := replChangeWatch{sub: sub, initial: true}
	if !w.take(false) || w.take(false) {
		t.Fatal("initial read not mandatory or unchanged view stayed dirty")
	}
	for range 50 {
		sub.publish(scope)
	}
	if !w.take(false) || len(sub.changes) != 0 {
		t.Fatal("hints were not drained before dispatch")
	}
	// A write after dispatch must survive the old result landing. Consumers do
	// not drain at completion; the next read takes this one coalesced hint.
	sub.publish(scope)
	if !w.pending() || !w.take(false) || w.take(false) {
		t.Fatal("in-flight change lost or repeated")
	}
	if !w.take(true) {
		t.Fatal("manual refresh was suppressed")
	}
	sub.Close()
	if !w.take(false) {
		t.Fatal("closed source became permanently stale")
	}
}

type replNotificationGate struct{ entered, release chan struct{} }
type replNotificationSession struct {
	sessions.Session
	sessions.CoordinationSession
	sessions.CoordinationRecords
	sessions.CoordinationCapabilityReporter
	reads atomic.Int64
	gate  atomic.Pointer[replNotificationGate]
	fail  atomic.Bool
}

func (s *replNotificationSession) ReadRecords(ctx context.Context, fn func(sessions.CoordinationReader) error) error {
	s.reads.Add(1)
	if gate := s.gate.Swap(nil); gate != nil {
		close(gate.entered)
		select {
		case <-gate.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if s.fail.Load() {
		return errors.New("notification test source unavailable")
	}
	return s.CoordinationRecords.ReadRecords(ctx, fn)
}

func newNotificationREPL(t *testing.T) (*managedREPL, *replNotificationSession) {
	t.Helper()
	var parent *replNotificationSession
	r := newSwarmTestREPL(t, integrationModel(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return spawnTestReply("done") }), func(c *swarm.Config) {
		parent = &replNotificationSession{Session: c.Parent, CoordinationSession: c.Parent.(sessions.CoordinationSession), CoordinationRecords: c.Parent.(sessions.CoordinationRecords), CoordinationCapabilityReporter: c.Parent.(sessions.CoordinationCapabilityReporter)}
		c.Parent = parent
	})
	_, err := parent.UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error {
		return tx.Put(sessions.RecordKey{Kind: "format", ID: "swarm"}, json.RawMessage(`{"version":2}`))
	})
	if err != nil {
		t.Fatal(err)
	}
	parent.reads.Store(0)
	return r, parent
}

func waitNotificationOverview(t *testing.T, r *managedREPL) {
	t.Helper()
	for r.visibleTab().swarmLoading {
		runUITask(t, r)
	}
}

func TestREPLOverviewScopedReadsAndInflightDirty(t *testing.T) {
	r, backend := newNotificationREPL(t)
	tab := r.visibleTab()
	sub := installREPLTestWatch(tab, &tab.swarmChanges, swarmOverviewScopes())
	refresh := func() {
		tab.swarmRefreshAt = time.Unix(1, 0) // due, not a manual force
		r.refreshSwarmActivities()
		waitNotificationOverview(t, r)
	}
	refresh()
	initial := backend.reads.Load()
	if initial == 0 || tab.swarmDisplay == nil {
		t.Fatal("initial overview not read")
	}
	for range 20 {
		sub.publish(sessions.RevisionScope{Index: sessions.CoordinationIndexKind, Key: "publication"})
		refresh()
	}
	if got := backend.reads.Load(); got != initial {
		t.Fatalf("unchanged/unrelated frames read storage: %d -> %d", initial, got)
	}
	sub.publish(sessions.RevisionScope{Index: sessions.CoordinationIndexDisplay})
	gate := &replNotificationGate{entered: make(chan struct{}), release: make(chan struct{})}
	backend.gate.Store(gate)
	tab.swarmRefreshAt = time.Unix(1, 0)
	r.refreshSwarmActivities()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("relevant overview hint did not read")
	}
	sub.publish(swarm.ActivityScope())
	close(gate.release)
	waitNotificationOverview(t, r)
	landed := backend.reads.Load()
	if !tab.swarmChanges.pending() {
		t.Fatal("landing consumed in-flight hint")
	}
	refresh()
	if backend.reads.Load() <= landed {
		t.Fatal("in-flight activity did not cause another read")
	}
	sub.Close()
	tab.closeNotifications()
}

func TestREPLInspectorScopedReadsLayoutAndCleanup(t *testing.T) {
	r, backend := newNotificationREPL(t)
	tab := r.visibleTab()
	target := tabViewTarget(tab)
	target.kind, target.item = swarmViewKind, "members"
	r.inspect(target)
	v := &viewInstance{target: target, view: viewFor(target.kind)}
	r.workspace().inspector.current = v
	sub := installREPLTestWatch(tab, &v.changes, swarmInspectorScopes())
	waitInspector(t, r, 140)
	initial := backend.reads.Load()
	if initial == 0 || v.revision == "" {
		t.Fatal("initial selected page not read")
	}
	for range 20 {
		r.inspectorRefreshAt = time.Unix(1, 0)
		r.refreshInspector(140)
		if v.loading {
			t.Fatal("unchanged inspector scheduled a read")
		}
	}
	r.inspectorRefreshAt = time.Unix(1, 0)
	waitInspector(t, r, 100)
	if backend.reads.Load() != initial {
		t.Fatal("unchanged inspector or resize read storage")
	}
	sub.publish(sessions.RevisionScope{Index: sessions.CoordinationIndexRecords})
	r.inspectorRefreshAt = time.Unix(1, 0)
	waitInspector(t, r, 100)
	if backend.reads.Load() <= initial {
		t.Fatal("records change did not refresh selected page")
	}
	beforeForce := backend.reads.Load()
	r.inspectorRefreshAt = time.Time{}
	waitInspector(t, r, 100)
	if backend.reads.Load() <= beforeForce {
		t.Fatal("manual force did not refresh")
	}
	target.item = "tasks"
	r.inspect(target)
	select {
	case <-sub.Done():
	default:
		t.Fatal("section switch kept old subscription")
	}
	v = &viewInstance{target: target, view: viewFor(target.kind)}
	r.workspace().inspector.current = v
	sub = installREPLTestWatch(tab, &v.changes, swarmInspectorScopes())
	waitInspector(t, r, 100)
	r.closeInspector()
	select {
	case <-sub.Done():
	default:
		t.Fatal("closing inspector kept subscription")
	}
}

func landNotificationContext(t *testing.T, r *managedREPL, d *contextPopover) {
	t.Helper()
	select {
	case result := <-d.pending:
		d.pending <- result
		r.refreshContextPopover(time.Now())
	case <-time.After(5 * time.Second):
		t.Fatal("context read did not finish")
	}
}

func TestREPLContextUsageScopedReadsKeepTranscriptClock(t *testing.T) {
	r, backend := newNotificationREPL(t)
	tab := r.visibleTab()
	ctx, cancel := context.WithCancel(t.Context())
	d := &contextPopover{ctx: ctx, cancel: cancel, owner: r.state, reader: newContextSnapshotReader(nil)}
	r.model.modal = &replModal{context: d}
	sub := installREPLTestWatch(tab, &d.usageChanges, swarmUsageScopes())
	r.startContextRead(d)
	landNotificationContext(t, r, d)
	initial := backend.reads.Load()
	if initial == 0 {
		t.Fatal("initial usage not read")
	}
	testAddMessages(t, r.state.session, messages.User("transcript clock still runs"))
	for range 20 {
		sub.publish(sessions.RevisionScope{Index: sessions.CoordinationIndexKind, Key: "publication"})
		r.startContextRead(d)
		landNotificationContext(t, r, d)
	}
	if backend.reads.Load() != initial || d.snapshot.storedMessages == 0 {
		t.Fatalf("usage polled or transcript stopped: reads=%d initial=%d messages=%d", backend.reads.Load(), initial, d.snapshot.storedMessages)
	}
	backend.fail.Store(true)
	sub.publish(sessions.RevisionScope{Index: sessions.CoordinationIndexKind, Key: "execution"})
	r.startContextRead(d)
	landNotificationContext(t, r, d)
	if d.snapshot.usage.err == nil || !d.usageChanges.pending() {
		t.Fatal("usage failure hidden or not retryable")
	}
	backend.fail.Store(false)
	r.startContextRead(d)
	landNotificationContext(t, r, d)
	if d.snapshot.usage.err != nil {
		t.Fatal("usage retry did not clear error")
	}
	r.closeModal()
	select {
	case <-sub.Done():
	default:
		t.Fatal("modal close retained subscription")
	}
	if d.ctx.Err() == nil {
		t.Fatal("modal close did not cancel read context")
	}
}

func TestREPLSavedNotificationSourceSharedAndCanceled(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "saved-root")
	tab := r.visibleTab()
	source, err := tab.notificationSource(r.work.ctx)
	if err != nil || source.saved == nil {
		t.Fatalf("saved source: %v", err)
	}
	var overview, inspector replChangeWatch
	if err := overview.bind(r.work.ctx, source, swarmOverviewScopes()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(r.work.ctx)
	defer cancel()
	again, err := tab.notificationSource(r.work.ctx)
	if err != nil || again != source {
		t.Fatal("views did not share their saved-root manager")
	}
	if err := inspector.bind(ctx, again, swarmInspectorScopes()); err != nil {
		t.Fatal(err)
	}
	inspectorSub := inspector.sub
	cancel()
	select {
	case <-inspectorSub.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not close selected subscription")
	}
	select {
	case <-overview.sub.Done():
		t.Fatal("one canceled view stopped its sibling")
	default:
	}
	overviewSub := overview.sub
	tab.closeNotifications()
	select {
	case <-overviewSub.Done():
	default:
		t.Fatal("tab cleanup did not close shared source")
	}
}

func TestREPLInspectorInflightAndTabReplacement(t *testing.T) {
	r, backend := newNotificationREPL(t)
	tab := r.visibleTab()
	target := tabViewTarget(tab)
	target.kind, target.item = swarmViewKind, "members"
	r.inspect(target)
	v := &viewInstance{target: target, view: viewFor(target.kind)}
	r.workspace().inspector.current = v
	sub := installREPLTestWatch(tab, &v.changes, swarmInspectorScopes())
	waitInspector(t, r, 140)
	gate := &replNotificationGate{entered: make(chan struct{}), release: make(chan struct{})}
	backend.gate.Store(gate)
	sub.publish(sessions.RevisionScope{Index: sessions.CoordinationIndexRecords})
	r.inspectorRefreshAt = time.Unix(1, 0)
	r.refreshInspector(140)
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("selected read did not start")
	}
	sub.publish(swarm.ActivityScope())
	close(gate.release)
	runUITask(t, r)
	before := backend.reads.Load()
	r.inspectorRefreshAt = time.Unix(1, 0)
	waitInspector(t, r, 140)
	if backend.reads.Load() <= before {
		t.Fatal("selected read lost in-flight activity")
	}
	ctx, cancel := context.WithCancel(t.Context())
	d := &contextPopover{ctx: ctx, cancel: cancel}
	tab.model.modal = &replModal{context: d}
	usageSub := installREPLTestWatch(tab, &d.usageChanges, swarmUsageScopes())
	other := &replTab{name: "other", model: newReplModel()}
	r.tabs = append(r.tabs, other)
	r.showTab(len(r.tabs) - 1)
	for _, closed := range []*replTestChanges{sub, usageSub} {
		select {
		case <-closed.Done():
		default:
			t.Fatal("tab switch retained inspector or modal subscription")
		}
	}
	if d.ctx.Err() == nil || tab.model.modal != nil {
		t.Fatal("hidden context modal remained alive")
	}
	r.showTab(0)
	if tab.notifications == nil || tab.notifications.closed {
		t.Fatal("tab switch closed the shared overview source")
	}
}

func TestREPLNoncomparableStoreKeepsConservativeReads(t *testing.T) {
	r, backend := newNotificationREPL(t)
	tab := r.visibleTab()
	tab.state.sessionStore = pickerListingStore{SessionStore: tab.state.sessionStore, list: func(context.Context) ([]sessions.SessionSummary, error) { return nil, nil }}
	for range 2 {
		tab.swarmRefreshAt = time.Unix(1, 0)
		r.refreshSwarmActivities()
		waitNotificationOverview(t, r)
		if tab.swarmDisplay == nil || tab.swarmReadError != "" || tab.notifications != nil {
			t.Fatal("noncomparable store lost its fallback or churned managers")
		}
	}
	if backend.reads.Load() < 2 {
		t.Fatal("unknown store identity suppressed fallback reads")
	}
	tab.swarmRefreshAt = time.Unix(1, 0)
	if !r.needsTick() {
		t.Fatal("idle fallback stopped observing the source")
	}
}

func TestREPLNotificationIdentityAndTabCleanup(t *testing.T) {
	r, _ := newNotificationREPL(t)
	tab := r.visibleTab()
	sub := installREPLTestWatch(tab, &tab.swarmChanges, swarmOverviewScopes())
	source := tab.notifications
	tab.name = "renamed"
	if !source.matches(tab) {
		t.Fatal("display rename changed stable source identity")
	}
	previous := tab.state
	defer func() { tab.state = previous }()
	tab.state = &conversationState{session: previous.session, sessionStore: previous.sessionStore, swarm: previous.swarm}
	if !source.matches(tab) {
		t.Fatal("conversation wrapper replacement changed stable identity")
	}
	// Replacing the backend, even for the same root, must retire the old source.
	tab.state.sessionStore = &gatedChildViewStore{SessionStore: tab.state.sessionStore}
	if source.matches(tab) {
		t.Fatal("store replacement reused old notifications")
	}
	tab.closeNotifications()
	select {
	case <-sub.Done():
	default:
		t.Fatal("tab cleanup retained overview subscription")
	}
	if len(source.watches) != 0 || !source.closed {
		t.Fatal("tab cleanup retained source consumers")
	}
}
