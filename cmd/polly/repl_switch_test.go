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
// which is all the switch machinery needs. It ignores the open context so a
// drained switch still produces a runtime to close.
func testSessionOpener(store sessions.SessionStore) *sessionOpener {
	return &sessionOpener{
		prepare: func(_ context.Context, name string, _ func(string)) (string, Settings, error) {
			return name, Settings{}, nil
		},
		open: func(_ context.Context, name string, settings Settings) (*conversationState, error) {
			session, err := store.Acquire(context.Background(), name, sessions.AcquireOptions{})
			if err != nil {
				return nil, err
			}
			return &conversationState{sessionStore: store, session: session, artifactStore: session.ArtifactStore(), settings: settings}, nil
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

func newSwitchTestREPL(t *testing.T, store sessions.SessionStore, current string) (*managedREPL, sessions.Session) {
	t.Helper()
	session := testAcquireSession(t, store, current)
	r := newManagedREPL(&Config{}, current, 0, 0)
	r.state = &conversationState{sessionStore: store, session: session}
	r.opener = testSessionOpener(store)
	return r, session
}

func TestResumePickerMarksAndRefusesSessionsInUseElsewhere(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	// Held for the whole test, standing in for another polly process.
	elsewhere := testAcquireSession(t, store, "held-elsewhere")
	defer elsewhere.Close()
	r, current := newSwitchTestREPL(t, store, "current-work")

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
	if r.switchTarget != "" || r.switchInFlight {
		t.Fatalf("picker tried to open a session leased elsewhere: target=%q", r.switchTarget)
	}
	if r.state.session != current {
		t.Fatal("refused selection replaced the live session")
	}
	if transcript := r.model.fullTranscript(); !strings.Contains(transcript, "held-elsewhere is open in another polly") {
		t.Fatalf("refusal was not explained: %q", transcript)
	}
}

func TestSwitchOpenFailureKeepsSessionAndItsSettings(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r, current := newSwitchTestREPL(t, store, "current-work")
	r.state.settings.Model = "anthropic/claude-sonnet-4-6"
	r.opener = &sessionOpener{
		prepare: func(_ context.Context, name string, notify func(string)) (string, Settings, error) {
			notify("System prompt changed, resetting conversation...")
			return name, Settings{Model: "openai/gpt-5.4"}, nil
		},
		open: func(context.Context, string, Settings) (*conversationState, error) {
			return nil, sessions.ErrSessionInUse
		},
	}

	r.model.mu.Lock()
	r.requestSwitchLocked("other-work")
	r.model.mu.Unlock()
	if !r.switchInFlight {
		t.Fatal("idle switch request did not start opening")
	}
	r.finishSwitch(<-r.switchDone)

	if r.switchTarget != "" || r.switchInFlight {
		t.Fatalf("failed switch left state behind: target=%q inFlight=%v", r.switchTarget, r.switchInFlight)
	}
	if r.state.settings.Model != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("settings after failed switch = %q, want the live session's", r.state.settings.Model)
	}
	if r.state.session != current {
		t.Fatal("failed switch replaced the live session")
	}
	transcript := r.model.fullTranscript()
	if !strings.Contains(transcript, "System prompt changed") {
		t.Fatalf("prepare notice did not reach the transcript: %q", transcript)
	}
	if !strings.Contains(transcript, "could not open other-work: it is open in another polly") {
		t.Fatalf("open failure was not explained: %q", transcript)
	}
}

func TestSwitchWhileBusyCancelsTurnThenOpensAfterItSettles(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	target := testAcquireSession(t, store, "older-work")
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	r, current := newSwitchTestREPL(t, store, "current-work")
	ctx := context.Background()

	r.model.mu.Lock()
	r.model.beginTurn("keep going")
	r.model.mu.Unlock()
	done := r.startTurn(ctx, "keep going", func(turnCtx context.Context, _ string, _ TurnUI) error {
		<-turnCtx.Done()
		return context.Cause(turnCtx)
	})

	r.model.mu.Lock()
	r.requestSwitchLocked("older-work")
	canceling := r.model.canceling
	r.model.mu.Unlock()
	if !canceling {
		t.Fatal("switch request during a turn did not cancel it")
	}
	if r.switchInFlight {
		t.Fatal("switch opened the target before the running turn settled")
	}
	if r.state.session != current {
		t.Fatal("switch replaced the session while a turn was running on it")
	}

	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("turn ended with %v, want cancellation", err)
	}
	r.endTurn(err)
	if next := r.continueAfterTurn(ctx, nil); next != nil {
		t.Fatal("a pending switch started queued input instead of opening the target")
	}
	if !r.switchInFlight {
		t.Fatal("settled turn did not start the pending switch")
	}
	r.finishSwitch(<-r.switchDone)
	if name, err := r.state.session.GetName(ctx); err != nil || name != "older-work" {
		t.Fatalf("live session = %q, %v; want older-work", name, err)
	}
	if r.model.busy || r.model.canceling {
		t.Fatal("switched model inherited the canceled turn's busy state")
	}
	if current.Context().Err() == nil {
		t.Fatal("previous session was left open after the switch")
	}
}

func TestDrainSwitchClosesAnOpenedSessionNobodyOwns(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	target := testAcquireSession(t, store, "older-work")
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	r, current := newSwitchTestREPL(t, store, "current-work")

	r.model.mu.Lock()
	r.requestSwitchLocked("older-work")
	r.model.mu.Unlock()
	r.drainSwitch()

	if r.switchInFlight {
		t.Fatal("drain left the switch in flight")
	}
	if sessionInUse(t, store, "older-work") {
		t.Fatal("drained switch leaked the opened session's lease")
	}
	if r.state.session != current {
		t.Fatal("drain replaced the live session")
	}
	// The loop's exit path closes the live session through its owner, not here.
	if current.Context().Err() != nil {
		t.Fatal("drain closed the live session")
	}
}

func TestAttachStateCarriesScreenFactsAcrossSessions(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r, _ := newSwitchTestREPL(t, store, "current-work")
	r.model.turnID = 7
	r.model.hist.entries = []string{"first prompt", "second prompt"}
	r.model.focusKnown, r.model.focused = true, false
	r.model.nativeImages = true
	r.model.imageCellWidth, r.model.imageCellHeight = 9, 18
	var announced *conversationState
	r.onStateChange = func(state *conversationState) { announced = state }

	next := testAcquireSession(t, store, "older-work")
	state := &conversationState{sessionStore: store, session: next}
	if err := r.attachState(state); err != nil {
		t.Fatal(err)
	}
	m := r.model
	if m.turnID != 7 {
		t.Fatalf("turn generation reset to %d on attach", m.turnID)
	}
	if strings.Join(m.hist.entries, ",") != "first prompt,second prompt" {
		t.Fatalf("prompt history lost on attach: %v", m.hist.entries)
	}
	if !m.focusKnown || m.focused {
		t.Fatalf("focus state lost on attach: known=%v focused=%v", m.focusKnown, m.focused)
	}
	if !m.nativeImages || m.imageCellWidth != 9 || m.imageCellHeight != 18 {
		t.Fatal("image geometry lost on attach")
	}
	if m.status.contextName != "older-work" {
		t.Fatalf("status context = %q, want older-work", m.status.contextName)
	}
	if r.state != state || announced != state {
		t.Fatal("attach did not make the new state live and announce it")
	}
}

func TestComposerHoldsInputWhileSwitching(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.switchTarget = "older-work"
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	r.model.insertEditorText("draft for the next session")
	if r.submitComposerLocked() {
		t.Fatal("submit during a switch requested quit")
	}
	if got := r.model.ed.text(); got != "draft for the next session" {
		t.Fatalf("held draft was cleared: %q", got)
	}
	if transcript := r.model.fullTranscript(); !strings.Contains(transcript, "switching to older-work") {
		t.Fatalf("hold was not explained: %q", transcript)
	}
	select {
	case <-r.pending:
		t.Fatal("a turn was started during the switch")
	default:
	}
	r.model.ed.clear()
	r.model.insertEditorText("/help")
	r.submitComposerLocked()
	if transcript := r.model.fullTranscript(); !strings.Contains(transcript, "/help") || !strings.Contains(transcript, "/resume") {
		t.Fatalf("busy-safe command did not run during the switch: %q", transcript)
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
