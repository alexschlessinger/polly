package main

import (
	"context"
	"errors"
	"fmt"
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
	workspaceRoot     bool
	detachedWorkspace bool
	workspace         *sessionWorkspace
	// name is the session's name as the tab shows it; /rename keeps it
	// current. Read without a lock so a handler can find a tab by name.
	name  string
	state *conversationState
	model *replModel
	// A child view has display state but no session lease or execution runtime.
	childView                *sessions.SessionView
	viewLoading, viewOpening bool
	viewSubmit               *string
	viewUsed                 uint64
	viewTarget               sessions.ViewTarget

	// The tab's turn, owned by the event loop. turnDone carries the running
	// turn goroutine's result and is nil while none runs; turnCancel cancels
	// its context. cancelDetachAt is when a canceled turn that has not
	// settled gets abandoned, zero until a cancel. stopWatch ends the lease
	// watch that wakes the loop when the session's context ends.
	turnDone       chan error
	turnCancel     context.CancelFunc
	cancelDetachAt time.Time
	stopWatch      func() bool

	// Saved child views retain their parent link and close when unused. The
	// swarm runtime owns delegated execution; tabs own only interactive turns.
	agentActivity  *agentActivity // Display identity only; never execution state.
	parent         *replTab
	parentName     string
	delivered      bool
	keepOpen       bool
	reportsLoading bool
	reportsRepull  bool
	swarmLoading   bool
	swarmActive    bool
	swarmRefreshAt time.Time
}

// openResult is the outcome of opening a session for a new tab.
type openResult struct {
	display        *replModel
	notices        []string
	workspaceEntry *workspaceEntry
	name           string
	state          *conversationState
	err            error
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
	r.bindMemberUI(tab)
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
	return r.addPreparedTab(state, name, m)
}

func (r *managedREPL) addPreparedTab(state *conversationState, name string, m *replModel) error {
	tab := &replTab{name: name, state: state, model: m, parentName: m.status.parentName, delivered: m.status.parentName != ""}
	r.bindMemberUI(tab)
	tab.detachedWorkspace = state.workspaceEntry != nil && state.workspaceEntry.orphan
	tab.workspaceRoot = tab.parentName == "" || tab.detachedWorkspace
	if i := r.tabIndexOf(tab.parentName); i >= 0 && !tab.detachedWorkspace {
		tab.parent = r.tabs[i]
	}
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
	tab.viewUsed = r.childViews.visit()
	previous := r.model
	if previous != tab.model {
		if oldIndex := r.tabIndexOfModel(previous); oldIndex >= 0 && r.tabs[oldIndex].workspace != nil {
			old := r.tabs[oldIndex]
			r.retireInspector(old.workspace)
			old.workspace.inspector.generation++
			old.workspace.inspector.focused = false
		}
	}
	if old := r.model; old != nil && old != tab.model {
		old.mu.Lock()
		old.hidden = true
		if oldIndex := r.tabIndexOfModel(old); oldIndex >= 0 {
			r.retireMainProjection(r.tabs[oldIndex])
		}
		old.resetAffordances()
		affordancesEnabled := old.affordances.enabled
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
		next.affordances.enabled = affordancesEnabled
		next.resetAffordances()
		r.restoreMainProjection(tab)
		next.notificationMu.Lock()
		next.signals = nil
		next.notificationMu.Unlock()
		next.unseenOutcome = turnOutcomeNone
		next.mu.Unlock()
	}
	r.model = tab.model
	r.state = tab.state
	r.model.mu.Lock()
	if tab.parent != nil {
		tab.parentName = tab.parent.name
	}
	r.model.status.parentName = tab.parentName
	r.appendPendingSandboxWarningsLocked()
	r.model.mu.Unlock()
	if previous != nil && previous != tab.model {
		if i := r.tabIndexOfModel(previous); i >= 0 {
			r.closeSpentChild(r.tabs[i])
		}
	}
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
		m.appendNoticeLine("No workspace to close")
		return
	}
	if m.busy {
		m.appendNoticeLine("Cancel this workspace's turn (Esc) before closing it")
		return
	}
	// Interactive turns in saved child conversations must stop before the
	// workspace can close. Swarm executions are checked separately below.
	if n := r.runningDescendants(r.visibleTab()); n > 0 {
		m.appendNoticeLine("Stop this workspace's running agents in the inspector before closing it")
		return
	}
	if r.state != nil && r.state.swarm != nil && r.state.swarm.HasActive() {
		m.appendNoticeLine("stop this swarm's active members and workflows before closing it")
		return
	}
	r.closeTabRequest = true
}

// Count every running descendant: idle intermediate agents can still own
// active children that depend on their ancestors' runtime resources.
func (r *managedREPL) runningDescendants(parent *replTab) int {
	n := 0
	for _, tab := range r.tabs {
		if tab == parent || tab.turnDone == nil {
			continue
		}
		for p, depth := tab.parent, 0; p != nil && depth < len(r.tabs); p, depth = p.parent, depth+1 {
			if p == parent {
				n++
				break
			}
		}
	}
	return n
}

// applyTabRequests performs the tab changes handlers recorded. Runs on the
// event loop with no model lock held.
func (r *managedREPL) applyTabRequests() {
	actions := r.workspaceActions
	r.workspaceActions = nil
	for _, action := range actions {
		action()
	}
	if req := r.childViewRequest; req != nil {
		r.childViewRequest = nil
		r.showChildView(req)
	}
	r.applySpawnRequests()
	if r.closeTabRequest {
		r.closeTabRequest = false
		r.closeVisibleTab()
	}
	if i := r.showTabRequest; i >= 0 {
		r.showTabRequest = -1
		if i < len(r.tabs) {
			tab := r.tabs[i]
			if root := r.rootTab(tab); root != nil && root != tab {
				r.showTab(r.tabIndexOfModel(root.model))
				r.inspect(tabViewTarget(tab))
			} else {
				r.showTab(i)
			}
		}
	}
}

// closeVisibleTab closes the visible tab's session and shows its left
// neighbor. Closing the last workspace replaces it with a fresh session
// instead of leaving; only when no session can be made does it quit, which
// closes every session on the way out. Runs on the event loop with no model
// lock held.
func (r *managedREPL) closeVisibleTab() {
	i := r.visibleTabIndex()
	if i < 0 {
		return
	}
	r.syncWorkspaces()
	if len(r.workspaces) <= 1 {
		if !r.replaceLastWorkspace(r.tabs[i]) {
			r.requestQuit()
		}
		return
	}
	tab := r.removeTab(i)
	notice := "Closed " + tab.name
	r.closeTabState(tab)
	r.model.mu.Lock()
	r.model.appendNoticeLine(notice)
	r.model.mu.Unlock()
}

// replaceLastWorkspace starts a fresh generated session to stand in for the
// last workspace; finishOpen closes the old tab once the new one holds the
// screen, so its lease is never dropped early. It reports false when no
// session can be made here. Runs on the event loop with no model lock held.
func (r *managedREPL) replaceLastWorkspace(old *replTab) bool {
	if r.opener == nil || r.opener.newName == nil {
		return false
	}
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	if !r.canOpenLocked() {
		return true
	}
	name, err := r.opener.newName(r.runCtx)
	if err != nil {
		r.model.appendErrorLine("could not name a new session: " + err.Error())
		return true
	}
	r.replacingTab = old
	r.beginOpenLocked(name, true)
	return true
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
	// Saved child views retain their stored parent identity after its tab closes.
	for _, child := range r.tabs {
		if child.parent == tab {
			child.parent = nil
		}
	}
	visible := tab.model == r.model
	r.tabs = append(r.tabs[:i], r.tabs[i+1:]...)
	if visible {
		r.syncWorkspaces()
		if len(r.workspaces) > 0 {
			r.showTab(r.tabIndexOfModel(r.workspaces[len(r.workspaces)-1].model))
		} else if len(r.tabs) > 0 {
			r.showTab(max(0, min(i-1, len(r.tabs)-1)))
		}
	}
	return tab
}

// closeTabs closes every tab's session at exit, the visible one included. A
// generated session that never ran a turn is discarded by its close.
func (r *managedREPL) closeTabs() error {
	r.replacingTab = nil
	r.cancelTurns()
	var errs []error
	if err := r.work.close(); err != nil {
		errs = append(errs, err)
	}
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
		r.closeTabState(tab)
		r.model.mu.Lock()
		r.model.appendErrorLine("Closed " + tab.name + " · " + cause.Error())
		r.model.mu.Unlock()
	}
	return nil
}

// requestNewTabLocked opens a fresh generated session in a new tab. Caller
// must hold r.model.mu.
func (r *managedREPL) requestNewTabLocked() {
	m := r.model
	if r.opener == nil || r.opener.newName == nil {
		m.appendNoticeLine("New sessions are unavailable")
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
		r.model.appendNoticeLine("Opening sessions is unavailable")
		return
	}
	if !r.canOpenLocked() {
		return
	}
	if r.beginWorkspaceOpen(name) {
		return
	}
	r.beginOpenLocked(name, false)
}

// canOpenLocked reports whether a tab may open now, explaining a refusal in
// the transcript: only one open runs at a time. Caller must hold r.model.mu.
func (r *managedREPL) canOpenLocked() bool {
	if r.opening != "" {
		r.model.appendNoticeLine("Already opening " + r.opening)
		return false
	}
	return true
}

// beginOpenLocked resolves the session's settings on the UI goroutine, then
// builds its runtime off it; the result lands through openDone. Caller must
// hold r.model.mu and have checked canOpenLocked.
func (r *managedREPL) beginOpenLocked(name string, auto bool) {
	r.beginOpenContextLocked(r.runCtx, name, auto)
}

func (r *managedREPL) beginOpenContextLocked(openCtx context.Context, name string, auto bool) {
	m := r.model
	resolved, settings, err := r.opener.prepare(openCtx, name, m.appendNoticeLine)
	if err != nil {
		r.failOpenLocked(name, err)
		return
	}
	r.opening = resolved
	ctx, cancel := context.WithCancel(openCtx)
	r.openCancel = cancel
	open := r.opener.open
	go func() {
		state, err := open(ctx, resolved, settings, auto)
		res := openResult{name: resolved, state: state, err: err}
		if err == nil {
			res.name, res.display, res.err = r.newTabModelContext(ctx, state)
			if res.err != nil {
				_ = state.Close()
				res.state = nil
			}
		}
		r.openDone <- res
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
	if res.err == nil && res.workspaceEntry != nil {
		res.err = r.finishWorkspaceOpen(res)
	} else if res.err == nil {
		if res.display != nil {
			res.err = r.addPreparedTab(res.state, res.name, res.display)
		} else {
			res.err = r.addTab(res.state)
		}
		if res.err != nil {
			_ = res.state.Close()
		}
	}
	r.model.mu.Lock()
	for _, notice := range res.notices {
		r.model.appendNoticeLine(notice)
	}
	if res.err != nil {
		r.replacingTab = nil
		r.failOpenLocked(res.name, res.err)
		r.model.mu.Unlock()
		return
	}
	r.startupLogoVisible = false
	r.syncWorkspaces()
	r.model.mu.Unlock()
	// The workspace this session replaces leaves now that the new one holds
	// the screen.
	if old := r.replacingTab; old != nil {
		r.replacingTab = nil
		if i := r.tabIndexOfModel(old.model); i >= 0 && old.model != r.model {
			r.removeTab(i)
			r.closeTabState(old)
			r.model.mu.Lock()
			r.model.appendNoticeLine("Closed " + old.name)
			r.model.mu.Unlock()
		}
	}
	// Reports its agents posted while it was closed are its first input.
	if tab := r.visibleTab(); r.runTurn != nil && r.pullReports(r.runCtx, tab) {
		r.startQueued(r.runCtx, tab, r.runTurn)
	}
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

func modelTabActivity(m *replModel) string {
	switch {
	case m.approval != nil:
		return "approval needed"
	case !m.busy:
		switch m.unseenOutcome {
		case turnOutcomeDone:
			return "done"
		case turnOutcomeFailed:
			return "failed"
		case turnOutcomeIncomplete:
			return "incomplete"
		}
		return ""
	case m.turnStarted.IsZero():
		return m.busyLabel()
	}
	return m.busyLabel() + " · " + coarseElapsed(time.Since(m.turnStarted))
}
