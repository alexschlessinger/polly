package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/alexschlessinger/pollytool/sessions"
)

// Tabs. The managed REPL holds several open sessions at once, each with its
// own runtime, settings, and screen model. One tab is visible: r.model and
// r.state always mirror it. Hidden tabs keep their session leased and their
// transcript intact, ready to show again.
//
// Handlers run under the visible model's lock and only record tab changes;
// the event loop applies them (applyTabRequests) once the lock is released,
// so the visible model is never swapped under a lock taken on another one.

type replTab struct {
	// name is the session's name as the tab shows it; /rename keeps it
	// current. Read without a lock so a handler can find a tab by name.
	name  string
	state *conversationState
	model *replModel
}

// openResult is the outcome of opening a session for a new tab.
type openResult struct {
	name  string
	state *conversationState
	err   error
}

// visibleTabIndex is the index of the tab on screen, or -1 when the REPL
// holds no tabs (unit tests of the screen alone).
func (r *managedREPL) visibleTabIndex() int {
	for i, tab := range r.tabs {
		if tab.model == r.model {
			return i
		}
	}
	return -1
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

// busy reports whether the visible tab has a turn in flight.
func (r *managedREPL) busy() bool {
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	return r.model.busy
}

// addTab opens a tab for state and shows it. Runs on the event loop with no
// model lock held.
func (r *managedREPL) addTab(state *conversationState) error {
	name, m, err := r.newTabModel(state)
	if err != nil {
		return err
	}
	r.tabs = append(r.tabs, &replTab{name: name, state: state, model: m})
	r.showTab(len(r.tabs) - 1)
	return nil
}

// showTab makes tab i visible. Image support and geometry, focus, and the
// prompt history belong to the screen rather than to a session, so they move
// from the model leaving the screen to the one taking it. Runs on the event
// loop with no model lock held.
func (r *managedREPL) showTab(i int) {
	tab := r.tabs[i]
	if old := r.model; old != nil && old != tab.model {
		old.mu.Lock()
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
		next.mu.Unlock()
	}
	r.model = tab.model
	r.state = tab.state
	r.model.mu.Lock()
	r.appendPendingSandboxWarningsLocked()
	r.model.mu.Unlock()
}

// requestShowTabLocked asks the event loop to show tab i once the current
// handler releases the model lock. Leaving a running turn is refused: a
// hidden tab cannot run one yet. Caller must hold r.model.mu.
func (r *managedREPL) requestShowTabLocked(i int) {
	m := r.model
	if i < 0 || i >= len(r.tabs) || r.tabs[i].model == m {
		return
	}
	if m.busy {
		m.appendNoticeLine("finish or cancel the current turn before switching tabs")
		return
	}
	r.showTabRequest = i
}

// requestCloseTabLocked asks the event loop to close the visible tab.
// Caller must hold r.model.mu.
func (r *managedREPL) requestCloseTabLocked() {
	m := r.model
	if r.visibleTabIndex() < 0 {
		m.appendNoticeLine("no tab to close")
		return
	}
	if m.busy {
		m.appendNoticeLine("cancel the running turn before closing this tab")
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
	tab := r.tabs[i]
	r.tabs = append(r.tabs[:i], r.tabs[i+1:]...)
	r.showTab(max(i-1, 0))
	notice := "closed " + tab.name
	if err := tab.state.Close(); err != nil {
		notice += " (" + err.Error() + ")"
	}
	r.model.mu.Lock()
	r.model.appendNoticeLine(notice)
	r.model.mu.Unlock()
}

// closeTabs closes every tab's session at exit, the visible one included. A
// generated session that never ran a turn is discarded by its close.
func (r *managedREPL) closeTabs() error {
	var errs []error
	for _, tab := range r.tabs {
		if err := tab.state.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close %s: %w", tab.name, err))
		}
	}
	r.tabs = nil
	return errors.Join(errs...)
}

// dropLostSession handles the visible session's lease context ending. With
// other tabs open and no turn running, the dead tab closes and its neighbor
// takes the screen. Otherwise the run must end with the typed cause, as it
// does when the store itself closed; that is reported by returning false.
// Runs on the event loop with no model lock held.
func (r *managedREPL) dropLostSession() bool {
	i := r.visibleTabIndex()
	cause := context.Cause(r.state.session.Context())
	if i < 0 || len(r.tabs) <= 1 || errors.Is(cause, sessions.ErrStoreClosed) || r.busy() {
		return false
	}
	tab := r.tabs[i]
	r.tabs = append(r.tabs[:i], r.tabs[i+1:]...)
	r.showTab(max(i-1, 0))
	_ = tab.state.Close()
	r.model.mu.Lock()
	r.model.appendErrorLine("closed " + tab.name + ": " + cause.Error())
	r.model.mu.Unlock()
	return true
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
// the transcript. A new tab takes the screen from the visible one, so a
// running turn must settle first, and only one open runs at a time. Caller
// must hold r.model.mu.
func (r *managedREPL) canOpenLocked() bool {
	m := r.model
	if r.opening != "" {
		m.appendNoticeLine("already opening " + r.opening)
		return false
	}
	if m.busy {
		m.appendNoticeLine("finish or cancel the current turn before opening another session")
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

// tabLines lists the open tabs for /tab.
func (r *managedREPL) tabLines() []string {
	if len(r.tabs) == 0 {
		return []string{"no tabs"}
	}
	visible := r.visibleTabIndex()
	lines := []string{fmt.Sprintf("tabs (%d):", len(r.tabs))}
	for i, tab := range r.tabs {
		line := fmt.Sprintf("  %d  %s", i+1, tab.name)
		if i == visible {
			line += "  current"
		}
		lines = append(lines, line)
	}
	return lines
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
