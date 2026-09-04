package main

import (
	"context"
	"fmt"
	"time"
)

// Turns across tabs. Every tab may have a turn in flight. The event loop
// multiplexes them through one wake channel (tabEvents) and a scan of the
// tabs (settleTabs), so a hidden tab keeps working while another holds the
// screen. Leaving cancels every turn and waits a short grace for them to
// settle, so the work they completed is persisted before the loop returns.

// turnRunner executes one turn for the managed REPL: the prompt's turn on
// the session behind turnUI, reporting through it.
type turnRunner func(ctx context.Context, prompt string, turnUI TurnUI) error

// wakeTabs asks the event loop to scan the tabs. Callable from any
// goroutine; a wake already pending covers this one, since the scan looks at
// every tab.
func (r *managedREPL) wakeTabs() {
	select {
	case r.tabEvents <- struct{}{}:
	default:
	}
}

// settleTabs handles what changed on any tab: a turn that returned settles
// onto its own tab and that tab's queue runs on; a canceled turn past its
// grace is abandoned; a tab whose lease ended is dropped. Returns the typed
// cause when a lost lease ends the run. Runs on the event loop with no model
// lock held.
func (r *managedREPL) settleTabs(ctx context.Context, runTurn turnRunner) error {
	now := time.Now()
	for _, tab := range append([]*replTab(nil), r.tabs...) {
		if tab.turnDone == nil {
			continue
		}
		select {
		case err := <-tab.turnDone:
			r.settleTurn(tab, err)
			r.startQueued(ctx, tab, runTurn)
		default:
			if !tab.cancelDetachAt.IsZero() && !now.Before(tab.cancelDetachAt) && r.cancelPending(tab) {
				r.abandonCanceledTurn(tab)
				r.startQueued(ctx, tab, runTurn)
			}
		}
	}
	return r.dropLostSessions()
}

// armCancelDetach starts the grace after which a canceled turn that has not
// settled is abandoned by settleTabs. Runs on the event loop.
func (r *managedREPL) armCancelDetach(tab *replTab) {
	if tab.turnDone == nil || !tab.cancelDetachAt.IsZero() {
		return
	}
	tab.cancelDetachAt = time.Now().Add(turnCancelDetachAfter)
	time.AfterFunc(turnCancelDetachAfter, r.wakeTabs)
}

// startPendingTurn starts the turn the composer just accepted, on the tab it
// was typed into.
func (r *managedREPL) startPendingTurn(ctx context.Context, runTurn turnRunner) {
	if p, ok := r.takePendingTurn(); ok {
		r.startManagedTurn(ctx, r.tabForModel(p.model), p.turn, runTurn)
	}
}

// runningTurns counts the tabs with a turn goroutine still running.
func (r *managedREPL) runningTurns() int {
	n := 0
	for _, tab := range r.tabs {
		if tab.turnDone != nil {
			n++
		}
	}
	return n
}

// hiddenTurns counts the running turns on tabs other than the visible one.
func (r *managedREPL) hiddenTurns() int {
	n := 0
	for _, tab := range r.tabs {
		if tab.turnDone != nil && tab.model != r.model {
			n++
		}
	}
	return n
}

// requestIdleQuitLocked handles a quit asked for at an idle prompt (Ctrl-C on
// an empty composer, Ctrl-D). With turns running in other tabs the first
// request only says so; the next one quits, canceling them. Returns true to
// quit. Caller must hold r.model.mu.
func (r *managedREPL) requestIdleQuitLocked() bool {
	if n := r.hiddenTurns(); n > 0 && !r.quitWarned && !r.quitting {
		r.quitWarned = true
		r.model.appendNoticeLine(hiddenTurnsWarning(n))
		return false
	}
	r.requestQuit()
	return true
}

func hiddenTurnsWarning(n int) string {
	if n == 1 {
		return "1 turn running in another tab · ^C again to cancel it and quit"
	}
	return fmt.Sprintf("%d turns running in other tabs · ^C again to cancel them and quit", n)
}

// beginQuit starts leaving: every turn is canceled and every pending
// approval denied. It reports whether the loop can return now, which it can
// when no turn goroutine is left to wait for, or when quitting was already
// under way and this second request cuts the grace short. Otherwise the loop
// runs on until the turns settle or quitDeadline passes. Runs on the event
// loop with no model lock held.
func (r *managedREPL) beginQuit() bool {
	select {
	case <-r.quit:
	default:
	}
	if r.quitting {
		return true
	}
	r.quitting = true
	for _, tab := range r.tabs {
		r.cancelTabTurn(tab)
	}
	if r.runningTurns() == 0 {
		return true
	}
	r.quitDeadline = time.After(turnCancelDetachAfter)
	return false
}

// quitSettled reports whether a quit under way has nothing left to wait for.
func (r *managedREPL) quitSettled() bool {
	return r.quitting && r.runningTurns() == 0
}

// cancelTabTurn cancels the turn on tab the way Ctrl-C does on the visible
// one: the partial output freezes, the context is canceled, and a pending
// approval is denied. Runs on the event loop with no model lock held.
func (r *managedREPL) cancelTabTurn(tab *replTab) {
	m := tab.model
	m.mu.Lock()
	if m.busy && !m.canceling {
		m.canceling = true
		m.finishAssistantBlock("")
	}
	m.denyApprovalLocked()
	m.mu.Unlock()
	r.cancelTurn(tab)
}

// cancelTurns cancels every tab's turn context and denies every pending
// approval, for the exit paths that return without waiting.
func (r *managedREPL) cancelTurns() {
	for _, tab := range r.tabs {
		r.cancelTurn(tab)
	}
	r.releaseApprovals()
}

// releaseApprovals denies every tab's pending approval so no turn goroutine
// stays parked on a reply channel.
func (r *managedREPL) releaseApprovals() {
	for _, tab := range r.tabs {
		tab.model.mu.Lock()
		tab.model.denyApprovalLocked()
		tab.model.mu.Unlock()
	}
}

// takeHiddenNotices drains the desktop-notification bodies queued on hidden
// tabs, under the visible tab's focus gate (see takeNotices): a hidden turn
// that settles pings the user the way a visible one does. Runs on the event
// loop with no model lock held.
func (r *managedREPL) takeHiddenNotices(focusKnown, focused bool) []string {
	var out []string
	for _, tab := range r.tabs {
		m := tab.model
		if m == r.model {
			continue
		}
		m.mu.Lock()
		notices := m.notices
		m.notices = nil
		m.mu.Unlock()
		if focusKnown && !focused {
			out = append(out, notices...)
		}
	}
	return out
}
