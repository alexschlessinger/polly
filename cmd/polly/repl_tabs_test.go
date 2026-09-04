package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
	ui "github.com/metaspartan/gotui/v5"
)

// testSessionOpener opens sessions from store with no tools, skills, or agent,
// which is all the tab machinery needs. It ignores the open context so a
// drained open still produces a runtime to close.
func testSessionOpener(store sessions.SessionStore) *sessionOpener {
	return &sessionOpener{
		prepare: func(_ context.Context, name string, _ func(string)) (string, Settings, error) {
			return name, Settings{}, nil
		},
		open: func(_ context.Context, name string, settings Settings, auto bool) (*conversationState, error) {
			session, err := store.Acquire(context.Background(), name, sessions.AcquireOptions{Auto: auto})
			if err != nil {
				return nil, err
			}
			return &conversationState{sessionStore: store, session: session, artifactStore: session.ArtifactStore(), settings: settings}, nil
		},
		newName: func(ctx context.Context) (string, error) { return generateSessionName(ctx, store) },
	}
}

func sessionInUse(t *testing.T, store sessions.SessionStore, name string) bool {
	t.Helper()
	summaries, err := store.ListSummaries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, summary := range summaries {
		if summary.Metadata.Name == name {
			return summary.InUse
		}
	}
	t.Fatalf("session %q not listed", name)
	return false
}

// newTabTestREPL builds a REPL holding one tab per named session, opened
// through the test opener in order, with the last one visible.
func newTabTestREPL(t *testing.T, store sessions.SessionStore, names ...string) *managedREPL {
	t.Helper()
	r := newManagedREPL(&Config{}, "-", 0, 0)
	r.opener = testSessionOpener(store)
	for _, name := range names {
		resolved, settings, err := r.opener.prepare(context.Background(), name, func(string) {})
		if err != nil {
			t.Fatal(err)
		}
		state, err := r.opener.open(context.Background(), resolved, settings, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.addTab(state); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// runTabCommand runs a slash command and applies the tab change it recorded,
// as the event loop does after a handler returns.
func (r *managedREPL) runTabCommand(line string) {
	r.runCommand(line)
	r.applyTabRequests()
}

func startBlockedTurn(t *testing.T, r *managedREPL) chan error {
	t.Helper()
	r.model.mu.Lock()
	r.model.beginTurn("keep going")
	r.model.mu.Unlock()
	return r.startTurn(context.Background(), "keep going", func(turnCtx context.Context, _ string, _ TurnUI) error {
		<-turnCtx.Done()
		return context.Cause(turnCtx)
	})
}

func settleBlockedTurn(t *testing.T, r *managedREPL, done chan error) {
	t.Helper()
	r.model.mu.Lock()
	r.cancelBusyTurn()
	r.model.mu.Unlock()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("turn ended with %v, want cancellation", err)
	}
	r.endTurn(err)
}

func TestResumePickerMarksAndRefusesSessionsInUseElsewhere(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	// Held for the whole test, standing in for another polly process.
	elsewhere := testAcquireSession(t, store, "held-elsewhere")
	defer elsewhere.Close()
	r := newTabTestREPL(t, store, "current-work")
	current := r.state.session

	r.openResumePicker()
	var held *replModalItem
	for i := range r.model.modal.items {
		if r.model.modal.items[i].value == "held-elsewhere" {
			held = &r.model.modal.items[i]
			r.model.modal.selected = i
		}
	}
	if held == nil || !strings.HasSuffix(held.label, "in use") {
		t.Fatalf("held session not marked in use: %+v", held)
	}
	r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	if r.opening != "" {
		t.Fatalf("picker tried to open a session leased elsewhere: opening=%q", r.opening)
	}
	if r.state.session != current || len(r.tabs) != 1 {
		t.Fatal("refused selection changed the tabs")
	}
	if transcript := r.model.fullTranscript(); !strings.Contains(transcript, "held-elsewhere is open in another polly") {
		t.Fatalf("refusal was not explained: %q", transcript)
	}
}

func TestOpenFailureKeepsVisibleTabAndItsSettings(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "current-work")
	current := r.state.session
	r.state.settings.Model = "anthropic/claude-sonnet-4-6"
	r.opener = &sessionOpener{
		prepare: func(_ context.Context, name string, notify func(string)) (string, Settings, error) {
			notify("System prompt changed, resetting conversation...")
			return name, Settings{Model: "openai/gpt-5.4"}, nil
		},
		open: func(context.Context, string, Settings, bool) (*conversationState, error) {
			return nil, sessions.ErrSessionInUse
		},
	}

	r.requestOpenLocked("other-work")
	if r.opening != "other-work" {
		t.Fatalf("idle open request did not start opening: opening=%q", r.opening)
	}
	r.finishOpen(<-r.openDone)

	if r.opening != "" || len(r.tabs) != 1 {
		t.Fatalf("failed open left state behind: opening=%q tabs=%d", r.opening, len(r.tabs))
	}
	if r.state.session != current || r.state.settings.Model != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("failed open changed the visible tab: model=%q", r.state.settings.Model)
	}
	transcript := r.model.fullTranscript()
	if !strings.Contains(transcript, "System prompt changed") {
		t.Fatalf("prepare notice did not reach the transcript: %q", transcript)
	}
	if !strings.Contains(transcript, "could not open other-work: it is open in another polly") {
		t.Fatalf("open failure was not explained: %q", transcript)
	}
}

func TestLeavingABusyTabIsRefusedUntilTheTurnSettles(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	target := testAcquireSession(t, store, "older-work")
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	r := newTabTestREPL(t, store, "first-work", "current-work")
	r.showTab(1)
	current := r.state.session
	done := startBlockedTurn(t, r)

	r.requestOpenLocked("older-work")
	r.runTabCommand("/new")
	r.runTabCommand("/tab 1")
	r.runTabCommand("/close")
	if r.opening != "" || r.showTabRequest >= 0 || r.closeTabRequest {
		t.Fatalf("a running turn did not block tab changes: opening=%q show=%d close=%v", r.opening, r.showTabRequest, r.closeTabRequest)
	}
	if r.state.session != current || len(r.tabs) != 2 {
		t.Fatal("tab changes were applied under a running turn")
	}
	transcript := r.model.fullTranscript()
	for _, want := range []string{
		"finish or cancel the current turn before opening another session",
		"finish or cancel the current turn before switching tabs",
		"cancel the running turn before closing this tab",
	} {
		if !strings.Contains(transcript, want) {
			t.Fatalf("refusal %q missing from %q", want, transcript)
		}
	}
	if r.model.canceling {
		t.Fatal("a refused tab change canceled the running turn")
	}

	settleBlockedTurn(t, r, done)
	r.requestOpenLocked("older-work")
	if r.opening != "older-work" {
		t.Fatalf("settled turn still blocks opening: opening=%q", r.opening)
	}
	r.finishOpen(<-r.openDone)
	if name, err := r.state.session.GetName(context.Background()); err != nil || name != "older-work" {
		t.Fatalf("live session = %q, %v; want older-work", name, err)
	}
	if len(r.tabs) != 3 || current.Context().Err() != nil {
		t.Fatalf("the tab left behind did not stay open: tabs=%d", len(r.tabs))
	}
	if r.model.busy || r.model.canceling {
		t.Fatal("new tab inherited the settled turn's busy state")
	}
}

func TestDrainOpenClosesAnOpenedSessionNobodyOwns(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	target := testAcquireSession(t, store, "older-work")
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	r := newTabTestREPL(t, store, "current-work")
	current := r.state.session

	r.requestOpenLocked("older-work")
	r.drainOpen()

	if r.opening != "" {
		t.Fatal("drain left the open in flight")
	}
	if sessionInUse(t, store, "older-work") {
		t.Fatal("drained open leaked the opened session's lease")
	}
	if r.state.session != current || len(r.tabs) != 1 {
		t.Fatal("drain changed the tabs")
	}
	// The loop's exit path closes the tabs through closeTabs, not here.
	if current.Context().Err() != nil {
		t.Fatal("drain closed the visible session")
	}
}

func TestShowTabMovesScreenFactsAndKeepsEachTranscript(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "first-work", "second-work")
	r.showTab(0)
	r.model.hist.entries = []string{"first prompt", "second prompt"}
	r.model.focusKnown, r.model.focused = true, false
	r.model.nativeImages = true
	r.model.imageCellWidth, r.model.imageCellHeight = 9, 18
	r.model.appendNoticeLine("only in the first tab")

	r.runTabCommand("/tab 2")
	m := r.model
	if r.visibleTabIndex() != 1 || r.state != r.tabs[1].state {
		t.Fatal("/tab 2 did not show the second tab")
	}
	if strings.Join(m.hist.entries, ",") != "first prompt,second prompt" {
		t.Fatalf("prompt history did not follow the screen: %v", m.hist.entries)
	}
	if !m.focusKnown || m.focused {
		t.Fatalf("focus state did not follow the screen: known=%v focused=%v", m.focusKnown, m.focused)
	}
	if !m.nativeImages || m.imageCellWidth != 9 || m.imageCellHeight != 18 {
		t.Fatal("image geometry did not follow the screen")
	}
	if m.status.contextName != "second-work" {
		t.Fatalf("status context = %q, want second-work", m.status.contextName)
	}
	if strings.Contains(m.fullTranscript(), "only in the first tab") {
		t.Fatal("transcripts leaked between tabs")
	}

	r.runTabCommand("/tab first-work")
	if r.visibleTabIndex() != 0 || !strings.Contains(r.model.fullTranscript(), "only in the first tab") {
		t.Fatal("/tab by name did not show the first tab with its transcript")
	}

	r.runTabCommand("/tab 3")
	r.runTabCommand("/tab nope")
	r.runTabCommand("/tab")
	transcript := r.model.fullTranscript()
	for _, want := range []string{"no tab 3 (2 open)", "no tab named nope", "tabs (2):", "1  first-work  current", "2  second-work"} {
		if !strings.Contains(transcript, want) {
			t.Fatalf("%q missing from %q", want, transcript)
		}
	}
}

func TestSetChangesOnlyTheVisibleTab(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "first-work", "second-work")
	for _, tab := range r.tabs {
		tab.state.settings.Model = "anthropic/claude-sonnet-4-6"
	}

	r.runTabCommand("/set model openai/gpt-5.4")
	if r.tabs[1].state.settings.Model != "openai/gpt-5.4" {
		t.Fatalf("visible tab model = %q", r.tabs[1].state.settings.Model)
	}
	if r.tabs[0].state.settings.Model != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("hidden tab model changed to %q", r.tabs[0].state.settings.Model)
	}
	hidden, err := r.tabs[0].state.session.GetMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if hidden.Model == "openai/gpt-5.4" {
		t.Fatal("/set persisted onto the hidden tab's session")
	}
	visible, err := r.tabs[1].state.session.GetMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if visible.Model != "openai/gpt-5.4" {
		t.Fatalf("visible session stored model = %q", visible.Model)
	}
	if r.tabs[1].model.status.modelName != "openai/gpt-5.4" || r.tabs[0].model.status.modelName == "openai/gpt-5.4" {
		t.Fatal("status rows do not reflect per-tab settings")
	}
}

func TestNewTabOpensGeneratedSessionAndCloseDiscardsIt(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "current-work")

	r.runTabCommand("/new")
	if r.opening == "" {
		t.Fatalf("/new did not start opening a session: %q", r.model.fullTranscript())
	}
	name := r.opening
	r.finishOpen(<-r.openDone)
	if len(r.tabs) != 2 || r.visibleTabIndex() != 1 || r.tabs[1].name != name {
		t.Fatalf("tabs after /new = %d, visible %d, names %q/%q", len(r.tabs), r.visibleTabIndex(), r.tabs[0].name, r.tabs[len(r.tabs)-1].name)
	}
	if !testStoreExists(t, store, name) {
		t.Fatalf("generated session %q was not created", name)
	}
	if transcript := r.model.fullTranscript(); !strings.Contains(transcript, "opened "+name+" in tab 2") {
		t.Fatalf("new tab was not announced: %q", transcript)
	}

	r.runTabCommand("/close")
	if len(r.tabs) != 1 || r.visibleTabIndex() != 0 || r.state.session != r.tabs[0].state.session {
		t.Fatalf("tabs after /close = %d, visible %d", len(r.tabs), r.visibleTabIndex())
	}
	if testStoreExists(t, store, name) {
		t.Fatal("a generated session that never ran a turn survived closing its tab")
	}
	if transcript := r.model.fullTranscript(); !strings.Contains(transcript, "closed "+name) {
		t.Fatalf("closing the tab was not announced: %q", transcript)
	}
	select {
	case <-r.quit:
		t.Fatal("closing one of two tabs quit the REPL")
	default:
	}
}

func TestClosingTheLastTabLeavesTheREPL(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "current-work")

	r.runTabCommand("/close")
	select {
	case <-r.quit:
	default:
		t.Fatal("closing the last tab did not quit")
	}
	// The exit path closes the sessions, so the tab is still whole here.
	if len(r.tabs) != 1 || r.tabs[0].state.session.Context().Err() != nil {
		t.Fatal("the last tab was closed before the exit path ran")
	}
	if err := r.closeTabs(); err != nil {
		t.Fatal(err)
	}
	if len(r.tabs) != 0 || sessionInUse(t, store, "current-work") {
		t.Fatal("closeTabs did not release the session")
	}
}

func TestLostLeaseDropsTabWhenAnotherRemains(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "first-work", "second-work")
	if err := r.tabs[1].state.session.Close(); err != nil {
		t.Fatal(err)
	}
	if !r.dropLostSession() {
		t.Fatal("a lost lease with another tab open ended the run")
	}
	if len(r.tabs) != 1 || r.visibleTabIndex() != 0 || r.tabs[0].name != "first-work" {
		t.Fatalf("tabs after lease loss = %d, visible %d", len(r.tabs), r.visibleTabIndex())
	}
	if transcript := r.model.fullTranscript(); !strings.Contains(transcript, "closed second-work") {
		t.Fatalf("lease loss was not explained: %q", transcript)
	}

	// The last tab losing its lease ends the run, as a lone session did.
	if err := r.tabs[0].state.session.Close(); err != nil {
		t.Fatal(err)
	}
	if r.dropLostSession() {
		t.Fatal("the last tab's lost lease was dropped instead of ending the run")
	}
}

func TestRenameFromPickerRenamesSessionOpenInAnotherTab(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "first-work", "second-work")

	r.renameSession("first-work", "renamed-work")
	if r.tabs[0].name != "renamed-work" || r.tabs[0].model.status.contextName != "renamed-work" {
		t.Fatalf("hidden tab not renamed: name=%q status=%q", r.tabs[0].name, r.tabs[0].model.status.contextName)
	}
	if !testStoreExists(t, store, "renamed-work") || testStoreExists(t, store, "first-work") {
		t.Fatal("rename did not reach the store")
	}
	if r.tabs[0].state.session.Context().Err() != nil {
		t.Fatal("renaming through the hidden tab's lease closed it")
	}
	if r.model.modal == nil {
		t.Fatal("rename did not reopen the picker")
	}
	for _, item := range r.model.modal.items {
		if item.value == "renamed-work" && !strings.HasSuffix(item.label, "tab 1") {
			t.Fatalf("renamed session not marked with its tab: %q", item.label)
		}
	}
}

func TestComposerHoldsInputWhileOpening(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.opening = "older-work"
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	r.model.insertEditorText("draft for the next session")
	if r.submitComposerLocked() {
		t.Fatal("submit during an open requested quit")
	}
	if got := r.model.ed.text(); got != "draft for the next session" {
		t.Fatalf("held draft was cleared: %q", got)
	}
	if transcript := r.model.fullTranscript(); !strings.Contains(transcript, "opening older-work") {
		t.Fatalf("hold was not explained: %q", transcript)
	}
	select {
	case <-r.pending:
		t.Fatal("a turn was started during the open")
	default:
	}
	r.model.ed.clear()
	r.model.insertEditorText("/help")
	r.submitComposerLocked()
	if transcript := r.model.fullTranscript(); !strings.Contains(transcript, "/help") || !strings.Contains(transcript, "/resume") {
		t.Fatalf("busy-safe command did not run during the open: %q", transcript)
	}
}

func TestTurnContextFollowsSessionLeaseAndRunContext(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "leased")
	r := newManagedREPL(&Config{}, "leased", 0, 0)
	r.state = &conversationState{sessionStore: store, session: session}

	runCtx, cancelRun := context.WithCancelCause(context.Background())
	turnCtx, cancelTurn := r.turnContext(runCtx)
	cancelTurn()
	if !errors.Is(context.Cause(turnCtx), context.Canceled) {
		t.Fatalf("user cancel cause = %v, want context.Canceled", context.Cause(turnCtx))
	}

	turnCtx, cancelTurn = r.turnContext(runCtx)
	defer cancelTurn()
	signal := errors.New("shutdown signal")
	cancelRun(signal)
	<-turnCtx.Done()
	if !errors.Is(context.Cause(turnCtx), signal) {
		t.Fatalf("run cancellation cause = %v, want the signal", context.Cause(turnCtx))
	}

	runCtx2 := context.Background()
	turnCtx, cancelTurn = r.turnContext(runCtx2)
	defer cancelTurn()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	<-turnCtx.Done()
	if context.Cause(turnCtx) == nil {
		t.Fatal("closing the session did not cancel its turn")
	}
	if r.sessionDone() == nil {
		t.Fatal("sessionDone returned nil with a live state")
	}
	r.state = nil
	if r.sessionDone() != nil {
		t.Fatal("sessionDone without a state must never fire")
	}
}
