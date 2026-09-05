package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func TestChildViewCacheBudgetsAndUserRecency(t *testing.T) {
	c := childViewCache{maxBytes: 100, maxEntries: 3}
	entry := func(id string, used uint64, size int64) *cachedChildView {
		return &cachedChildView{info: &sessions.SessionView{ID: id}, used: used, bytes: size}
	}
	c.put(entry("a", c.visit(), 30))
	c.put(entry("b", c.visit(), 30))
	a := c.take("a")
	a.used = c.visit()
	c.put(a)
	c.put(entry("c", c.visit(), 30))
	c.put(entry("d", c.visit(), 30))
	if c.entries["b"] != nil || c.entries["a"] == nil || c.bytes != 90 {
		t.Fatalf("LRU: %+v, bytes %d", c.entries, c.bytes)
	}
	c.put(entry("completion", 0, 20))
	if c.entries["completion"] != nil || len(c.entries) != 3 {
		t.Fatal("completion displaced a viewed child")
	}
	c.put(entry("a", 0, 10))
	if c.entries["a"] != a {
		t.Fatal("late completion replaced viewed state")
	}
	c.put(entry("oversized", c.visit(), 101))
	if c.entries["oversized"] != nil || c.bytes > c.maxBytes {
		t.Fatal("oversized entry admitted")
	}
	c = childViewCache{}
	for i := 0; i < 8; i++ {
		c.put(entry(fmt.Sprint(i), 0, 1<<20))
	}
	if len(c.entries) != childViewProbationEntries || c.entries["0"] != nil || c.entries["7"] == nil {
		t.Fatal("completion pool is not bounded by admission order")
	}
}

func TestChildDisplayCacheDropsExecutionAndInputState(t *testing.T) {
	m := newReplModel()
	m.beginTurn("private draft")
	m.appendToolCallStart(agentCall("a", `{"label":"nested"}`))
	_, row := m.toolDisclosureRowForCall("a")
	row.agent.origin = &agentActivity{label: "live origin"}
	m.appendAssistant("**saved** text")
	m.finishAssistantBlock("")
	m.renderPendingMarkdown()
	m.transcriptRows(80)
	m.ed.setText("do not cache this")
	m.approval = &approvalState{reply: make(chan []bool)}
	m.attachmentSeq = 3
	m.attachments = map[int]composerAttachment{2: {Artifact: &artifacts.Ref{ID: "saved-image"}}, 3: {Path: "unsent-file"}}
	m.ambiguousAttachments = map[int]bool{1: true}
	view := childDisplayCopy(m)
	if view.busy || view.approval != nil || !view.ed.empty() || len(view.queue) > 0 || len(view.activeTools) > 0 || view.artifactStore != nil {
		t.Fatal("view retained execution/input state")
	}
	_, cachedRow := view.toolDisclosureRowForCall("a")
	if cachedRow.agent == row.agent || cachedRow.agent.origin != nil {
		t.Fatal("live agent references retained")
	}
	cachedRow.agent.label = "cached"
	if row.agent.label == "cached" {
		t.Fatal("cached activity aliases live model")
	}
	if !strings.Contains(plainStyledText(view.fullTranscript()), "saved") || childViewSize(view) <= int64(len(view.fullTranscript())) {
		t.Fatal("display or allocation estimate lost")
	}
	if view.attachmentSeq != 3 || view.attachments[2].Artifact == nil || view.attachments[3].Path != "" || !view.ambiguousAttachments[1] {
		t.Fatal("durable image registry lost or draft attachment cached")
	}
	if token := view.registerAttachment("new-file", "new"); token != "[image #4]" {
		t.Fatal("cache reused an image token")
	}
}

func savedChildViewFixture(t *testing.T) (*managedREPL, *sessions.SQLiteStore, *agentActivity) {
	t.Helper()
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "caller")
	ctx := context.Background()
	child, err := store.Acquire(ctx, "child", sessions.AcquireOptions{Parent: "caller"})
	if err != nil {
		t.Fatal(err)
	}
	if err := updateMetadata(ctx, child, func(md *sessions.Metadata) { md.SpawnCallID = "call"; md.SpawnOutcome = sessions.ReportFinished }); err != nil {
		t.Fatal(err)
	}
	if err := child.AddMessages(ctx, []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "inspect"}, {Role: messages.MessageRoleAssistant, Content: "The saved child answer."}}); err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	r.model.appendToolCallStart(agentCall("call", `{"label":"child"}`))
	_, row := r.model.toolDisclosureRowForCall("call")
	row.agent.session = "child"
	return r, store.(*sessions.SQLiteStore), row.agent
}

func clickChildView(t *testing.T, r *managedREPL, a *agentActivity) *replTab {
	t.Helper()
	r.model.mu.Lock()
	ok := r.requestChildViewLocked(a, "call")
	r.model.mu.Unlock()
	if !ok {
		t.Fatal("view navigation unavailable")
	}
	r.applyTabRequests()
	return r.visibleTab()
}

func driveChildView(t *testing.T, r *managedREPL, ready func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !ready() {
		select {
		case task := <-r.uiTasks:
			task()
			r.applyTabRequests()
		case <-deadline:
			t.Fatal("view did not become ready")
		}
	}
}

type gatedChildViewStore struct {
	sessions.SessionStore
	reader sessions.ViewStore
	gate   <-chan struct{}
}

func (s *gatedChildViewStore) ReadView(ctx context.Context, target sessions.ViewTarget, revision string) (*sessions.SessionView, error) {
	select {
	case <-s.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.reader.ReadView(ctx, target, revision)
}

func TestChildViewCacheHitPaintsBeforeDatabaseAndKeepsScroll(t *testing.T) {
	r, store, activity := savedChildViewFixture(t)
	child := clickChildView(t, r, activity)
	driveChildView(t, r, func() bool { return !child.viewLoading })
	if child.state.session != nil || child.state.agent != nil {
		t.Fatal("view owns runtime")
	}
	info, err := store.ReadView(context.Background(), sessions.ViewTarget{ID: child.childView.ID}, "")
	if err != nil || info.InUse {
		t.Fatalf("view leased session: %v", err)
	}
	child.model.followBottom, child.model.scrollAnchor = false, 3
	r.showTab(0)
	driveChildView(t, r, func() bool { return r.childViews.entries[info.ID] != nil })
	if len(r.tabs) != 1 {
		t.Fatal("cached child remained open")
	}
	gate := make(chan struct{})
	r.state.sessionStore = &gatedChildViewStore{SessionStore: store, reader: store, gate: gate}
	child = clickChildView(t, r, activity)
	if child.viewLoading || !strings.Contains(plainStyledText(child.model.fullTranscript()), "saved child answer") {
		t.Fatal("cache hit waited for database")
	}
	if child.model.followBottom || child.model.scrollAnchor != 3 {
		t.Fatal("scroll state lost")
	}
	if len(r.childViews.entries) != 0 {
		t.Fatal("open view is still an eviction candidate")
	}
	close(gate)
	driveChildView(t, r, func() bool { return child.childView.Unchanged })
}

func TestChildViewDraftSurvivesRefreshAndFollowupStartup(t *testing.T) {
	for _, edit := range []bool{false, true} {
		t.Run(fmt.Sprint(edit), func(t *testing.T) {
			r, _, activity := savedChildViewFixture(t)
			child := clickChildView(t, r, activity)
			driveChildView(t, r, func() bool { return !child.viewLoading })
			original := r.opener.open
			entered, release := make(chan struct{}), make(chan struct{})
			r.opener.open = func(ctx context.Context, name string, settings Settings, auto bool) (*conversationState, error) {
				close(entered)
				<-release
				return original(ctx, name, settings, auto)
			}
			child.model.ed.setText("follow up")
			child.model.mu.Lock()
			r.submitComposerLocked()
			r.submitComposerLocked()
			child.model.mu.Unlock()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("runtime did not start")
			}
			if edit {
				child.model.ed.setText("changed draft")
			}
			r.showTab(0)
			close(release)
			driveChildView(t, r, func() bool { return !child.viewOpening })
			if child.childView != nil || child.state.session == nil || !child.keepOpen {
				t.Fatal("view did not become an open runtime")
			}
			pending, ok := r.takePendingTurn()
			if edit {
				if ok || child.model.ed.text() != "changed draft" {
					t.Fatal("edited draft was sent or lost")
				}
			} else {
				if !ok || pending.model != child.model || pending.turn.userMessage.Content != "follow up" {
					t.Fatal("submitted follow-up lost after navigation")
				}
				if _, duplicate := r.takePendingTurn(); duplicate {
					t.Fatal("duplicate follow-up")
				}
			}
		})
	}
}

func TestChildViewRestoresUnansweredPromptAndReusesItOnResend(t *testing.T) {
	r, store, activity := savedChildViewFixture(t)
	ctx := context.Background()
	// The child's first run ended before it answered, as after a cancel.
	writer, err := store.Acquire(ctx, "child", sessions.AcquireOptions{ExistingOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "retry me"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	child := clickChildView(t, r, activity)
	driveChildView(t, r, func() bool { return !child.viewLoading })
	if transcript := plainStyledText(child.model.fullTranscript()); !strings.Contains(transcript, "restored to composer") || child.model.ed.text() != "retry me" || child.model.restoredDraft == nil {
		t.Fatalf("view did not restore the unanswered prompt: composer %q, transcript %q", child.model.ed.text(), transcript)
	}
	id := child.viewID()
	r.showTab(0)
	driveChildView(t, r, func() bool { return r.childViews.entries[id] != nil })
	child = clickChildView(t, r, activity)
	if child.viewLoading || child.model.ed.text() != "retry me" || child.model.restoredDraft == nil {
		t.Fatalf("cached view lost the restored prompt: %q", child.model.ed.text())
	}
	driveChildView(t, r, func() bool { return child.childView.Unchanged })
	child.model.mu.Lock()
	r.submitComposerLocked()
	child.model.mu.Unlock()
	driveChildView(t, r, func() bool { return !child.viewOpening })
	pending, ok := r.takePendingTurn()
	if !ok || child.state.session == nil || pending.turn.userMessage.Content != "retry me" || !child.model.restoreDraftNext {
		t.Fatalf("unchanged resend did not reuse the restored turn: ok %v, reuse %v", ok, child.model.restoreDraftNext)
	}
	persisted := make(chan error, 1)
	r.startManagedTurn(ctx, child, pending.turn, func(ctx context.Context, _ string, turnUI TurnUI) error {
		tui := turnUI.(*gotuiTurnUI)
		persisted <- persistUserMessageForTurn(ctx, tui.state.session, tui.turn.userMessage, tui.reuseUser, nil)
		return nil
	})
	if err := <-child.turnDone; err != nil {
		t.Fatal(err)
	}
	if err := <-persisted; err != nil {
		t.Fatal(err)
	}
	history, err := child.state.session.GetHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 || history[2].Content != "retry me" {
		t.Fatalf("resend duplicated the stored prompt: %d messages", len(history))
	}
}

// drainUITasks runs the callbacks already queued for the event loop.
func drainUITasks(r *managedREPL) {
	for {
		select {
		case task := <-r.uiTasks:
			task()
			r.applyTabRequests()
		default:
			return
		}
	}
}

func TestClearedChildViewLoadsColdOnNextInspection(t *testing.T) {
	r, _, activity := savedChildViewFixture(t)
	child := clickChildView(t, r, activity)
	driveChildView(t, r, func() bool { return !child.viewLoading })
	child.model.ed.setText("/clear")
	child.model.mu.Lock()
	r.submitComposerLocked()
	child.model.mu.Unlock()
	if strings.Contains(plainStyledText(child.model.fullTranscript()), "saved child answer") {
		t.Fatal("/clear kept the transcript")
	}
	if r.retiredChildViewTask(child) != nil {
		t.Fatal("cleared display offered for caching under the saved revision")
	}
	r.showTab(0)
	r.work.wg.Wait()
	drainUITasks(r)
	if len(r.tabs) != 1 || len(r.childViews.entries) != 0 {
		t.Fatalf("cleared view was cached: %d tabs, %d entries", len(r.tabs), len(r.childViews.entries))
	}
	child = clickChildView(t, r, activity)
	if !child.viewLoading {
		t.Fatal("cleared view was served warm")
	}
	driveChildView(t, r, func() bool {
		return strings.Contains(plainStyledText(child.model.fullTranscript()), "The saved child answer.")
	})
}

func TestClearedLeasedChildIsNotCachedOnRetirement(t *testing.T) {
	r, _, activity := savedChildViewFixture(t)
	state, err := r.opener.open(context.Background(), "child", Settings{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.addTab(state); err != nil {
		t.Fatal(err)
	}
	child := r.visibleTab()
	child.agentActivity = activity
	child.model.ed.setText("/clear")
	child.model.mu.Lock()
	r.submitComposerLocked()
	child.model.mu.Unlock()
	if r.retiredChildViewTask(child) != nil {
		t.Fatal("cleared leased child offered for caching")
	}
	r.showTab(0)
	r.work.wg.Wait()
	drainUITasks(r)
	if len(r.tabs) != 1 || len(r.childViews.entries) != 0 {
		t.Fatalf("cleared child was cached: %d tabs, %d entries", len(r.tabs), len(r.childViews.entries))
	}
	shown := clickChildView(t, r, activity)
	if !shown.viewLoading {
		t.Fatal("cleared child was served warm")
	}
	driveChildView(t, r, func() bool {
		return strings.Contains(plainStyledText(shown.model.fullTranscript()), "The saved child answer.")
	})
}

func TestChildViewRefreshCannotStealNavigation(t *testing.T) {
	r, store, activity := savedChildViewFixture(t)
	gate := make(chan struct{})
	r.state.sessionStore = &gatedChildViewStore{SessionStore: store, reader: store, gate: gate}
	child := clickChildView(t, r, activity)
	if !child.viewLoading {
		t.Fatal("expected cold view")
	}
	r.showTab(0)
	caller := r.model
	close(gate)
	driveChildView(t, r, func() bool { return len(r.childViews.entries) > 0 })
	if r.model != caller || len(r.tabs) != 1 {
		t.Fatal("late result stole navigation")
	}
}

func TestChildViewStartupShutdownReleasesUnadoptedRuntime(t *testing.T) {
	r, store, activity := savedChildViewFixture(t)
	child := clickChildView(t, r, activity)
	driveChildView(t, r, func() bool { return !child.viewLoading })
	original := r.opener.open
	entered, release := make(chan struct{}), make(chan struct{})
	r.opener.open = func(ctx context.Context, name string, settings Settings, auto bool) (*conversationState, error) {
		close(entered)
		<-release
		return original(context.WithoutCancel(ctx), name, settings, auto)
	}
	child.model.ed.setText("follow up")
	child.model.mu.Lock()
	r.submitComposerLocked()
	child.model.mu.Unlock()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not start")
	}
	r.work.cancel()
	close(release)
	if err := r.work.close(); err != nil {
		t.Fatal(err)
	}
	view, err := store.ReadView(context.Background(), sessions.ViewTarget{Name: "child"}, "")
	if err != nil || view.InUse {
		t.Fatalf("startup leaked lease: %v", err)
	}
}

func TestRetiredChildWarmsViewAfterReleasingRuntime(t *testing.T) {
	r, store, activity := savedChildViewFixture(t)
	state, err := r.opener.open(context.Background(), "child", Settings{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.addTab(state); err != nil {
		t.Fatal(err)
	}
	child := r.visibleTab()
	child.agentActivity, child.viewUsed = activity, 0
	id := child.viewID()
	r.showTab(0)
	driveChildView(t, r, func() bool { return r.childViews.entries[id] != nil })
	if state.session.Context().Err() == nil || len(r.tabs) != 1 {
		t.Fatal("cache retained runtime/tab")
	}
	if r.childViews.entries[id].used != 0 || activity.viewID != id {
		t.Fatal("completion was promoted to viewed, or lost identity")
	}
	view, err := store.ReadView(context.Background(), sessions.ViewTarget{ID: id}, "")
	if err != nil || view.InUse {
		t.Fatalf("cache retained lease: %v", err)
	}
	shown := clickChildView(t, r, activity)
	if shown.viewLoading || shown.state.session != nil {
		t.Fatal("first inspection missed warmed view")
	}
}

func TestChildViewRefreshesChangedHistoryWithoutLosingDraft(t *testing.T) {
	r, store, activity := savedChildViewFixture(t)
	child := clickChildView(t, r, activity)
	driveChildView(t, r, func() bool { return !child.viewLoading })
	id := child.viewID()
	r.showTab(0)
	driveChildView(t, r, func() bool { return r.childViews.entries[id] != nil })
	writer, err := store.Acquire(context.Background(), "child", sessions.AcquireOptions{ExpectedID: id})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.AddMessages(context.Background(), []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "another process"}, {Role: messages.MessageRoleAssistant, Content: "New external answer."}}); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	child = clickChildView(t, r, activity)
	modelBeforeRefresh := child.model
	child.model.ed.setText("my unsent draft")
	driveChildView(t, r, func() bool {
		return strings.Contains(plainStyledText(child.model.fullTranscript()), "New external answer.")
	})
	if child.model.ed.text() != "my unsent draft" {
		t.Fatal("refresh lost draft")
	}
	if child.model != modelBeforeRefresh {
		t.Fatal("refresh invalidated pending UI callback targets")
	}
	r.showTab(0)
	if r.tabIndexOfModel(child.model) < 0 || r.childViews.entries[id] != nil {
		t.Fatal("draft-bearing view was evicted or cached")
	}
}

func TestChildViewFollowupRefusesReusedName(t *testing.T) {
	r, store, activity := savedChildViewFixture(t)
	child := clickChildView(t, r, activity)
	driveChildView(t, r, func() bool { return !child.viewLoading })
	original := r.opener.open
	r.opener.open = func(ctx context.Context, name string, settings Settings, auto bool) (*conversationState, error) {
		if err := store.Delete(ctx, name); err != nil {
			return nil, err
		}
		replacement, err := store.Acquire(ctx, name, sessions.AcquireOptions{})
		if err != nil {
			return nil, err
		}
		_ = replacement.Close()
		return original(ctx, name, settings, auto)
	}
	child.model.ed.setText("must stay with original child")
	child.model.mu.Lock()
	r.submitComposerLocked()
	child.model.mu.Unlock()
	driveChildView(t, r, func() bool { return !child.viewOpening })
	if child.state.session != nil || child.model.ed.text() != "must stay with original child" {
		t.Fatal("reused session received draft")
	}
	if _, ok := r.takePendingTurn(); ok {
		t.Fatal("followup submitted to replacement")
	}
}

type viewCloseMutation struct {
	sessions.Session
	after func()
}

func (s *viewCloseMutation) ViewID() string { return s.Session.(sessions.ViewIdentity).ViewID() }
func (s *viewCloseMutation) Close() error {
	err := s.Session.Close()
	if s.after != nil {
		after := s.after
		s.after = nil
		after()
	}
	return err
}

func TestRetiredChildRevisionPrecedesLeaseRelease(t *testing.T) {
	r, store, activity := savedChildViewFixture(t)
	state, err := r.opener.open(context.Background(), "child", Settings{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.addTab(state); err != nil {
		t.Fatal(err)
	}
	child := r.visibleTab()
	child.agentActivity = activity
	id := child.viewID()
	mutation := make(chan error, 1)
	state.session = &viewCloseMutation{Session: state.session, after: func() {
		writer, err := store.Acquire(context.Background(), "child", sessions.AcquireOptions{})
		if err == nil {
			err = writer.AddMessage(context.Background(), messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "Written after lease release."})
			_ = writer.Close()
		}
		mutation <- err
	}}
	r.showTab(0)
	driveChildView(t, r, func() bool { return r.childViews.entries[id] != nil })
	if err := <-mutation; err != nil {
		t.Fatal(err)
	}
	child = clickChildView(t, r, activity)
	driveChildView(t, r, func() bool {
		return strings.Contains(plainStyledText(child.model.fullTranscript()), "Written after lease release.")
	})
}

func TestChildViewParentNavigationFollowsRename(t *testing.T) {
	r, store, activity := savedChildViewFixture(t)
	child := clickChildView(t, r, activity)
	driveChildView(t, r, func() bool { return !child.viewLoading })
	parentTab := r.removeTab(0)
	if err := parentTab.state.Close(); err != nil {
		t.Fatal(err)
	}
	parent, err := store.Acquire(context.Background(), "caller", sessions.AcquireOptions{ExistingOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Rename(context.Background(), "renamed-caller"); err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	r.runTabCommand("/parent")
	runUITask(t, r)
	select {
	case result := <-r.openDone:
		r.finishOpen(result)
	case <-time.After(5 * time.Second):
		t.Fatal("parent did not open")
	}
	if r.visibleTab().name != "renamed-caller" || len(r.tabs) != 1 {
		t.Fatalf("parent navigation: %d tabs, visible %s", len(r.tabs), r.visibleTab().name)
	}
}
