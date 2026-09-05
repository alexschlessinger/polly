package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
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
		spawn: func(ctx context.Context, parent *conversationState, req subagent.Request) (*conversationState, error) {
			name, err := generateSessionName(ctx, store)
			if err != nil {
				return nil, err
			}
			parentName, err := parent.session.GetName(ctx)
			if err != nil {
				return nil, err
			}
			session, err := store.Acquire(ctx, name, sessions.AcquireOptions{Auto: true, Parent: parentName})
			if err != nil {
				return nil, err
			}
			md, err := session.GetMetadata(ctx)
			if err != nil {
				return nil, err
			}
			md.Description = req.Label
			if err := session.SetMetadata(ctx, md); err != nil {
				return nil, err
			}
			return &conversationState{sessionStore: store, session: session, artifactStore: session.ArtifactStore(), settings: parent.settings.clone()}, nil
		},
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
	t.Cleanup(func() { _ = r.closeTabs() })
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

func TestLeavingABusyTabKeepsItsTurnRunning(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	target := testAcquireSession(t, store, "older-work")
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	r := newTabTestREPL(t, store, "first-work", "current-work")
	busy := r.tabs[1]
	done := startBlockedTurn(t, r)

	// Closing a busy tab would cut its turn off; switching away does not.
	r.runTabCommand("/close")
	if r.closeTabRequest || len(r.tabs) != 2 {
		t.Fatal("a busy tab was closed")
	}
	if got := r.model.fullTranscript(); !strings.Contains(got, "cancel this tab's turn (Esc) before closing it") {
		t.Fatalf("close refusal was not explained: %q", got)
	}
	r.runTabCommand("/tab 1")
	if r.visibleTabIndex() != 0 || r.state != r.tabs[0].state {
		t.Fatal("/tab 1 did not leave the busy tab")
	}
	if r.model.busy {
		t.Fatal("the shown tab inherited the busy state")
	}
	busy.model.mu.Lock()
	running := busy.model.busy && !busy.model.canceling && busy.model.hidden
	turnID := busy.model.turnID
	busy.model.mu.Unlock()
	if !running || busy.turnDone == nil {
		t.Fatal("leaving the tab disturbed its turn")
	}

	// The hidden turn streams into its own tab, unrendered until shown.
	tui := &gotuiTurnUI{repl: r, model: busy.model, config: r.config, state: busy.state, turnID: turnID}
	tui.AppendAssistantText("**bold** while hidden\n")
	busy.model.mu.Lock()
	rendered := busy.model.transcript[busy.model.currentAssistant].text
	raw := busy.model.streamRaw.String()
	busy.model.mu.Unlock()
	if rendered != "" || raw != "**bold** while hidden\n" {
		t.Fatalf("hidden tab rendered %q from raw %q", rendered, raw)
	}
	if strings.Contains(r.model.fullTranscript(), "while hidden") {
		t.Fatal("hidden output reached the visible tab")
	}
	if got := strings.Join(r.tabLines(), "\n"); !strings.Contains(got, "2  current-work  streaming") || !strings.Contains(got, "1  first-work  current") {
		t.Fatalf("tab list does not show the hidden turn: %q", got)
	}

	// Opening another session while a turn runs is fine too: the new tab
	// takes the screen, the turn keeps going behind it.
	r.runTabCommand("/tab 2")
	r.requestOpenLocked("older-work")
	if r.opening != "older-work" {
		t.Fatalf("a running turn blocked opening a session: %q", r.model.fullTranscript())
	}
	r.finishOpen(<-r.openDone)
	if len(r.tabs) != 3 || r.visibleTabIndex() != 2 || r.model.busy {
		t.Fatalf("tabs after opening under a turn = %d, visible %d, busy %v", len(r.tabs), r.visibleTabIndex(), r.model.busy)
	}

	r.runTabCommand("/tab 2")
	if got := plainStyledText(r.model.fullTranscript()); !strings.Contains(got, "bold while hidden") {
		t.Fatalf("showing the tab did not render its streamed text: %q", got)
	}
	settleBlockedTurn(t, r, done)
	if r.model.busy || r.model.canceling || busy.turnDone != nil {
		t.Fatal("the settled turn left the tab busy")
	}
}

func TestHiddenTurnSettlesOnItsOwnTabAndRunsItsQueue(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "first-work", "current-work")
	busy := r.tabs[1]
	release := make(chan struct{})
	runTurn := func(context.Context, string, TurnUI) error {
		<-release
		return nil
	}
	r.runTurn = runTurn
	r.model.mu.Lock()
	r.model.beginTurn("keep going")
	r.model.mu.Unlock()
	r.startTurn(context.Background(), "keep going", runTurn)
	busy.model.mu.Lock()
	busy.model.queue = queuedTextInputs("/set model openai/gpt-5.4")
	busy.model.mu.Unlock()
	r.runTabCommand("/tab 1")

	close(release)
	select {
	case <-r.tabEvents:
	case <-time.After(5 * time.Second):
		t.Fatal("the settled turn did not wake the loop")
	}
	if err := r.settleTabs(context.Background(), runTurn); err != nil {
		t.Fatal(err)
	}
	runUITask(t, r) // The inbox read completes before the queued command runs.
	if busy.turnDone != nil || busy.model.busy || busy.model.lastOutcome != turnOutcomeDone {
		t.Fatalf("hidden turn did not settle on its tab: running=%v busy=%v outcome=%v", busy.turnDone != nil, busy.model.busy, busy.model.lastOutcome)
	}
	if r.visibleTabIndex() != 0 || r.model.busy || r.model.lastOutcome != turnOutcomeNone {
		t.Fatal("settling a hidden turn touched the visible tab")
	}
	if busy.state.settings.Model != "openai/gpt-5.4" || r.state.settings.Model == "openai/gpt-5.4" {
		t.Fatalf("queued command ran on the wrong tab: hidden=%q visible=%q", busy.state.settings.Model, r.state.settings.Model)
	}
	if r.model != r.tabs[0].model || r.state != r.tabs[0].state {
		t.Fatal("running a hidden tab's queue left the screen on the wrong tab")
	}
	if got := plainStyledText(busy.model.fullTranscript()); !strings.Contains(got, "openai/gpt-5.4") {
		t.Fatalf("queued command output went elsewhere: %q", got)
	}
	if strings.Contains(plainStyledText(r.model.fullTranscript()), "openai/gpt-5.4") {
		t.Fatal("queued command output reached the visible tab")
	}
}

func TestPendingTurnStartsOnTheTabThatTookIt(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "first-work", "second-work")
	started := make(chan *replModel, 1)
	runTurn := func(_ context.Context, _ string, turnUI TurnUI) error {
		started <- turnUI.(*gotuiTurnUI).model
		return nil
	}
	r.pending <- pendingTurn{model: r.tabs[0].model, turn: textManagedTurn("hello")}
	r.startPendingTurn(context.Background(), runTurn)
	if r.tabs[0].turnDone == nil || r.tabs[1].turnDone != nil {
		t.Fatal("the pending turn did not start on the tab that took it")
	}
	select {
	case m := <-started:
		if m != r.tabs[0].model {
			t.Fatal("the turn reports into the wrong tab")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the turn did not start")
	}
	select {
	case <-r.tabEvents:
	case <-time.After(5 * time.Second):
		t.Fatal("the settled turn did not wake the loop")
	}
	if err := r.settleTabs(context.Background(), runTurn); err != nil {
		t.Fatal(err)
	}
	if r.tabs[0].turnDone != nil {
		t.Fatal("the turn did not settle on its tab")
	}
}

func TestIdleInterruptWarnsAboutHiddenTurnsThenQuitsWithGrace(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "first-work", "current-work")
	busy := r.tabs[1]
	startBlockedTurn(t, r)
	r.runTabCommand("/tab 1")

	if r.handleInterrupt() {
		t.Fatal("the first idle interrupt quit with a turn running in another tab")
	}
	if got := r.model.fullTranscript(); !strings.Contains(got, "1 turn running in another tab · ^C again to cancel it and quit") {
		t.Fatalf("no warning about the hidden turn: %q", got)
	}
	if busy.model.canceling || busy.turnDone == nil {
		t.Fatal("the warning canceled the hidden turn")
	}
	// Any other key withdraws the warning; the next Ctrl-C warns again.
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "x"})
	r.model.ed.clear()
	if r.handleInterrupt() {
		t.Fatal("an interrupt after another key quit without warning again")
	}
	if !r.handleInterrupt() {
		t.Fatal("the second idle interrupt did not quit")
	}
	if r.beginQuit() {
		t.Fatal("quit returned before the running turn settled")
	}
	select {
	case <-r.quit:
		t.Fatal("beginQuit left the quit request pending")
	default:
	}
	if !busy.model.canceling || r.quitDeadline == nil {
		t.Fatal("quitting did not cancel the hidden turn under a grace")
	}
	deadline := time.After(5 * time.Second)
	for busy.turnDone != nil {
		select {
		case <-r.tabEvents:
		case <-deadline:
			t.Fatal("the canceled turn did not settle")
		}
		if err := r.settleTabs(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	}
	if !r.quitSettled() {
		t.Fatal("quit did not settle once the turn returned")
	}
	if busy.model.lastOutcome != turnOutcomeCanceled {
		t.Fatalf("hidden turn outcome = %v, want canceled", busy.model.lastOutcome)
	}
	if !r.beginQuit() {
		t.Fatal("a repeated quit request did not return at once")
	}
}

func TestIdleEOFWarnsAboutHiddenTurnsThenQuits(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "first-work", "current-work")
	startBlockedTurn(t, r)
	r.runTabCommand("/tab 1")
	eof := ui.Event{Type: ui.KeyboardEvent, ID: "<C-d>"}
	if r.handleEvent(eof) {
		t.Fatal("the first Ctrl-D quit with a turn running in another tab")
	}
	if got := r.model.fullTranscript(); !strings.Contains(got, "1 turn running in another tab") {
		t.Fatalf("no warning about the hidden turn: %q", got)
	}
	if !r.handleEvent(eof) {
		t.Fatal("the second Ctrl-D did not quit")
	}
}

func TestIdleInterruptQuitsAtOnceWithoutHiddenTurns(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "first-work", "current-work")
	if !r.handleInterrupt() {
		t.Fatal("interrupt at an idle prompt with idle tabs did not quit")
	}
	if !r.beginQuit() {
		t.Fatal("quit waited with no turn running")
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
	r.work.wg.Wait()
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
	select {
	case <-r.tabEvents:
	case <-time.After(5 * time.Second):
		t.Fatal("the ended lease did not wake the loop")
	}
	if err := r.dropLostSessions(); err != nil {
		t.Fatalf("a lost lease with another tab open ended the run: %v", err)
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
	if err := r.dropLostSessions(); err == nil {
		t.Fatal("the last tab's lost lease was dropped instead of ending the run")
	}
}

func TestLostLeaseUnderAHiddenTurnDropsTheTabOnceItSettles(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "first-work", "second-work")
	lost := r.tabs[1]
	startBlockedTurn(t, r)
	r.runTabCommand("/tab 1")
	if err := lost.state.session.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for len(r.tabs) == 2 {
		select {
		case <-r.tabEvents:
		case <-deadline:
			t.Fatalf("the dead tab was not dropped: turn running=%v", lost.turnDone != nil)
		}
		if err := r.settleTabs(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	}
	if lost.turnDone != nil || lost.model.busy {
		t.Fatal("the tab was dropped before its turn settled")
	}
	if r.visibleTabIndex() != 0 || r.tabs[0].name != "first-work" {
		t.Fatal("the surviving tab is not on screen")
	}
	if got := plainStyledText(r.model.fullTranscript()); !strings.Contains(got, "closed second-work") {
		t.Fatalf("lease loss was not explained on the visible tab: %q", got)
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
	turnCtx, cancelTurn := turnContext(runCtx, r.state)
	cancelTurn()
	if !errors.Is(context.Cause(turnCtx), context.Canceled) {
		t.Fatalf("user cancel cause = %v, want context.Canceled", context.Cause(turnCtx))
	}

	turnCtx, cancelTurn = turnContext(runCtx, r.state)
	defer cancelTurn()
	signal := errors.New("shutdown signal")
	cancelRun(signal)
	<-turnCtx.Done()
	if !errors.Is(context.Cause(turnCtx), signal) {
		t.Fatalf("run cancellation cause = %v, want the signal", context.Cause(turnCtx))
	}

	runCtx2 := context.Background()
	turnCtx, cancelTurn = turnContext(runCtx2, r.state)
	defer cancelTurn()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	<-turnCtx.Done()
	if context.Cause(turnCtx) == nil {
		t.Fatal("closing the session did not cancel its turn")
	}
	turnCtx, cancelTurn = turnContext(runCtx2, nil)
	defer cancelTurn()
	if turnCtx.Err() != nil {
		t.Fatal("a turn without a session must follow the run context alone")
	}
}
