package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
)

// Tabs. The managed REPL holds several open sessions at once, each with its
// own runtime, settings, screen model, and turn. One tab is visible: r.model
// and r.state always mirror it. Hidden tabs keep their session leased, their
// transcript intact, and their turn running, ready to show again.
//
// Handlers run under the visible model's lock and only record tab changes;
// the event loop applies them (applyTabRequests) once the lock is released,
// so the visible model is never swapped under a lock taken on another one.
// A tab's turn fields belong to the event loop (see repl_turns.go).

type replTab struct {
	// name is the session's name as the tab shows it; /rename keeps it
	// current. Read without a lock so a handler can find a tab by name.
	name  string
	state *conversationState
	model *replModel

	// The tab's turn, owned by the event loop. turnDone carries the running
	// turn goroutine's result and is nil while none runs; turnCancel cancels
	// its context. cancelDetachAt is when a canceled turn that has not
	// settled gets abandoned, zero until a cancel. stopWatch ends the lease
	// watch that wakes the loop when the session's context ends.
	turnDone       chan error
	turnCancel     context.CancelFunc
	cancelDetachAt time.Time
	stopWatch      func() bool
}

// openResult is the outcome of opening a session for a new tab.
type openResult struct {
	name  string
	state *conversationState
	err   error
}

// visibleTabIndex is the index of the tab on screen, or -1 when none holds
// the screen model.
func (r *managedREPL) visibleTabIndex() int {
	return r.tabIndexOfModel(r.model)
}

// visibleTab is the tab on screen. The REPL always has one: it starts on a
// tab with no session behind its model (unit tests of the screen alone stay
// there), which the first session to land replaces.
func (r *managedREPL) visibleTab() *replTab {
	if i := r.visibleTabIndex(); i >= 0 {
		return r.tabs[i]
	}
	tab := &replTab{name: "-", state: r.state, model: r.model}
	r.tabs = append(r.tabs, tab)
	return tab
}

// tabForModel finds the tab whose screen model is m, falling back to the
// visible tab.
func (r *managedREPL) tabForModel(m *replModel) *replTab {
	for _, tab := range r.tabs {
		if tab.model == m {
			return tab
		}
	}
	return r.visibleTab()
}

// tabIndexOf finds the tab holding the named session, or -1.
func (r *managedREPL) tabIndexOf(name string) int {
	for i, tab := range r.tabs {
		if tab.name == name {
			return i
		}
	}
	return -1
}

// addTab opens a tab for state and shows it. The session's lease is watched
// from here on, so its end wakes the event loop. Runs on the event loop with
// no model lock held.
func (r *managedREPL) addTab(state *conversationState) error {
	name, m, err := r.newTabModel(state)
	if err != nil {
		return err
	}
	tab := &replTab{name: name, state: state, model: m}
	if state.session != nil {
		tab.stopWatch = context.AfterFunc(state.session.Context(), r.wakeTabs)
	}
	// The screen-only tab the REPL starts on stands in until the first
	// session lands.
	if len(r.tabs) == 1 && r.tabs[0].state == nil {
		r.tabs[0] = tab
	} else {
		r.tabs = append(r.tabs, tab)
	}
	r.showTab(r.tabIndexOfModel(m))
	return nil
}

// tabIndexOfModel finds the tab whose screen model is m, or -1.
func (r *managedREPL) tabIndexOfModel(m *replModel) int {
	for i, tab := range r.tabs {
		if tab.model == m {
			return i
		}
	}
	return -1
}

// showTab makes tab i visible. Image support and geometry, focus, and the
// prompt history belong to the screen rather than to a session, so they move
// from the model leaving the screen to the one taking it. The model leaving
// goes hidden, keeping any streamed text raw; the one arriving renders what
// it streamed while hidden and drops the news it had for the visible tab,
// now that it is seen. Runs on the event loop with no model lock held.
func (r *managedREPL) showTab(i int) {
	tab := r.tabs[i]
	if old := r.model; old != nil && old != tab.model {
		old.mu.Lock()
		old.hidden = true
		nativeImages := old.nativeImages
		cellWidth, cellHeight := old.imageCellWidth, old.imageCellHeight
		focusKnown, focused := old.focusKnown, old.focused
		hist := old.hist.entries
		old.mu.Unlock()

		next := tab.model
		next.mu.Lock()
		if next.nativeImages != nativeImages || next.imageCellWidth != cellWidth || next.imageCellHeight != cellHeight {
			next.visual.invalidate()
		}
		next.nativeImages = nativeImages
		next.imageCellWidth, next.imageCellHeight = cellWidth, cellHeight
		next.focusKnown, next.focused = focusKnown, focused
		next.hist.entries = hist
		next.hidden = false
		next.signals = nil
		next.unseenOutcome = turnOutcomeNone
		next.renderAssistantStream()
		next.mu.Unlock()
	}
	r.model = tab.model
	r.state = tab.state
	r.model.mu.Lock()
	r.appendPendingSandboxWarningsLocked()
	r.model.mu.Unlock()
}

// requestShowTabLocked asks the event loop to show tab i once the current
// handler releases the model lock. A turn running on the tab left behind
// keeps running out of sight. Caller must hold r.model.mu.
func (r *managedREPL) requestShowTabLocked(i int) {
	if i < 0 || i >= len(r.tabs) || r.tabs[i].model == r.model {
		return
	}
	r.showTabRequest = i
}

// requestCloseTabLocked asks the event loop to close the visible tab. A
// running turn must be canceled first: closing would cut it off mid-work.
// Caller must hold r.model.mu.
func (r *managedREPL) requestCloseTabLocked() {
	m := r.model
	if r.visibleTabIndex() < 0 {
		m.appendNoticeLine("no tab to close")
		return
	}
	if m.busy {
		m.appendNoticeLine("cancel this tab's turn (Esc) before closing it")
		return
	}
	r.closeTabRequest = true
}

// applyTabRequests performs the tab changes handlers recorded. Runs on the
// event loop with no model lock held.
func (r *managedREPL) applyTabRequests() {
	if r.closeTabRequest {
		r.closeTabRequest = false
		r.closeVisibleTab()
	}
	if i := r.showTabRequest; i >= 0 {
		r.showTabRequest = -1
		if i < len(r.tabs) {
			r.showTab(i)
		}
	}
}

// closeVisibleTab closes the visible tab's session and shows its left
// neighbor. Closing the last tab leaves the REPL, which closes every session
// on the way out. Runs on the event loop with no model lock held.
func (r *managedREPL) closeVisibleTab() {
	i := r.visibleTabIndex()
	if i < 0 {
		return
	}
	if len(r.tabs) == 1 {
		r.requestQuit()
		return
	}
	tab := r.removeTab(i)
	notice := "closed " + tab.name
	if err := tab.state.Close(); err != nil {
		notice += " (" + err.Error() + ")"
	}
	r.model.mu.Lock()
	r.model.appendNoticeLine(notice)
	r.model.mu.Unlock()
}

// removeTab takes tab i out of the list, ending its lease watch. When it was
// on screen its left neighbor takes over. The caller closes the tab's
// session. Runs on the event loop with no model lock held.
func (r *managedREPL) removeTab(i int) *replTab {
	tab := r.tabs[i]
	if tab.stopWatch != nil {
		tab.stopWatch()
		tab.stopWatch = nil
	}
	visible := tab.model == r.model
	r.tabs = append(r.tabs[:i], r.tabs[i+1:]...)
	if visible {
		r.showTab(max(i-1, 0))
	}
	return tab
}

// closeTabs closes every tab's session at exit, the visible one included. A
// generated session that never ran a turn is discarded by its close.
func (r *managedREPL) closeTabs() error {
	var errs []error
	for _, tab := range r.tabs {
		if tab.stopWatch != nil {
			tab.stopWatch()
		}
		if tab.state == nil {
			continue
		}
		if err := tab.state.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close %s: %w", tab.name, err))
		}
	}
	r.tabs = nil
	return errors.Join(errs...)
}

// dropLostSessions closes the tabs whose session lease ended. With another
// tab open, the dead tab leaves the list, its neighbor taking the screen if
// it was visible; a dead tab whose turn is still unwinding waits for that
// turn to settle first, since its context is already canceled. The last tab
// losing its lease, or the store closing, ends the run: the typed cause is
// returned for Run to report, as it was when the run context was parented on
// the session. Runs on the event loop with no model lock held.
func (r *managedREPL) dropLostSessions() error {
	for i := 0; i < len(r.tabs); {
		tab := r.tabs[i]
		if tab.state == nil || tab.state.session == nil || tab.state.session.Context().Err() == nil {
			i++
			continue
		}
		cause := context.Cause(tab.state.session.Context())
		if len(r.tabs) == 1 || errors.Is(cause, sessions.ErrStoreClosed) {
			return cause
		}
		if tab.turnDone != nil {
			i++
			continue
		}
		r.removeTab(i)
		_ = tab.state.Close()
		r.model.mu.Lock()
		r.model.appendErrorLine("closed " + tab.name + ": " + cause.Error())
		r.model.mu.Unlock()
	}
	return nil
}

// requestNewTabLocked opens a fresh generated session in a new tab. Caller
// must hold r.model.mu.
func (r *managedREPL) requestNewTabLocked() {
	m := r.model
	if r.opener == nil || r.opener.newName == nil {
		m.appendNoticeLine("new tabs are unavailable")
		return
	}
	if !r.canOpenLocked() {
		return
	}
	name, err := r.opener.newName(r.runCtx)
	if err != nil {
		m.appendErrorLine("could not name a new session: " + err.Error())
		return
	}
	r.beginOpenLocked(name, true)
}

// requestOpenLocked opens the named session in a new tab, or shows the tab
// that already holds it. Caller must hold r.model.mu.
func (r *managedREPL) requestOpenLocked(name string) {
	if i := r.tabIndexOf(name); i >= 0 {
		r.requestShowTabLocked(i)
		return
	}
	if r.opener == nil {
		r.model.appendNoticeLine("opening sessions is unavailable")
		return
	}
	if !r.canOpenLocked() {
		return
	}
	r.beginOpenLocked(name, false)
}

// canOpenLocked reports whether a tab may open now, explaining a refusal in
// the transcript: only one open runs at a time. Caller must hold r.model.mu.
func (r *managedREPL) canOpenLocked() bool {
	if r.opening != "" {
		r.model.appendNoticeLine("already opening " + r.opening)
		return false
	}
	return true
}

// beginOpenLocked resolves the session's settings on the UI goroutine, then
// builds its runtime off it; the result lands through openDone. Caller must
// hold r.model.mu and have checked canOpenLocked.
func (r *managedREPL) beginOpenLocked(name string, auto bool) {
	m := r.model
	resolved, settings, err := r.opener.prepare(r.runCtx, name, m.appendNoticeLine)
	if err != nil {
		r.failOpenLocked(name, err)
		return
	}
	r.opening = resolved
	m.appendNoticeLine("opening " + resolved + "…")
	ctx, cancel := context.WithCancel(r.runCtx)
	r.openCancel = cancel
	open := r.opener.open
	go func() {
		state, err := open(ctx, resolved, settings, auto)
		r.openDone <- openResult{name: resolved, state: state, err: err}
	}()
}

// finishOpen lands an opened session as a new visible tab. Runs on the event
// loop with no model lock held.
func (r *managedREPL) finishOpen(res openResult) {
	r.opening = ""
	if r.openCancel != nil {
		r.openCancel()
		r.openCancel = nil
	}
	if res.err == nil {
		if res.err = r.addTab(res.state); res.err != nil {
			_ = res.state.Close()
		}
	}
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	if res.err != nil {
		r.failOpenLocked(res.name, res.err)
		return
	}
	r.startupLogoVisible = false
	r.model.appendNoticeLine(fmt.Sprintf("opened %s in tab %d", res.name, len(r.tabs)))
}

// failOpenLocked reports a failed open. Caller must hold r.model.mu.
func (r *managedREPL) failOpenLocked(name string, err error) {
	reason := err.Error()
	if errors.Is(err, sessions.ErrSessionInUse) {
		reason = "it is open in another polly"
	}
	r.model.appendErrorLine("could not open " + name + ": " + reason)
}

// drainOpen runs when the event loop exits with an open still in flight: the
// runtime it produces has no owner, so wait for it and close it rather than
// leak its lease and tool processes.
func (r *managedREPL) drainOpen() {
	if r.opening == "" {
		return
	}
	if r.openCancel != nil {
		r.openCancel()
		r.openCancel = nil
	}
	res := <-r.openDone
	r.opening = ""
	if res.state != nil {
		_ = res.state.Close()
	}
}

// tabLines lists the open tabs for /tab, with what each one's turn is doing.
// Caller must hold r.model.mu; other tabs' models are locked here.
func (r *managedREPL) tabLines() []string {
	if len(r.tabs) == 0 {
		return []string{"no tabs"}
	}
	visible := r.visibleTabIndex()
	lines := []string{fmt.Sprintf("tabs (%d):", len(r.tabs))}
	for i, tab := range r.tabs {
		line := fmt.Sprintf("  %d  %s", i+1, tab.name)
		if activity := r.tabActivity(tab); activity != "" {
			line += "  " + activity
		}
		if i == visible {
			line += "  current"
		}
		lines = append(lines, line)
	}
	return lines
}

// tabActivity describes the turn running on tab, how its last turn ended
// while the tab was hidden, or "" at idle. Caller must hold r.model.mu;
// another tab's model is locked here, never the reverse.
func (r *managedREPL) tabActivity(tab *replTab) string {
	m := tab.model
	if m != r.model {
		m.mu.Lock()
		defer m.mu.Unlock()
	}
	switch {
	case m.approval != nil:
		return "approval needed"
	case !m.busy:
		switch m.unseenOutcome {
		case turnOutcomeDone:
			return "done"
		case turnOutcomeFailed:
			return "failed"
		}
		return ""
	case m.turnStarted.IsZero():
		return m.busyLabel()
	}
	return m.busyLabel() + " · " + coarseElapsed(time.Since(m.turnStarted))
}

// resolveTab finds a tab by 1-based position or session name.
func (r *managedREPL) resolveTab(arg string) (int, error) {
	if n, err := strconv.Atoi(arg); err == nil {
		if n < 1 || n > len(r.tabs) {
			return -1, fmt.Errorf("no tab %d (%d open)", n, len(r.tabs))
		}
		return n - 1, nil
	}
	if i := r.tabIndexOf(arg); i >= 0 {
		return i, nil
	}
	return -1, fmt.Errorf("no tab named %s", arg)
}
