package main

import (
	"context"
	"fmt"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
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
			r.afterSettle(ctx, tab, err, runTurn)
		default:
			if !tab.cancelDetachAt.IsZero() && !now.Before(tab.cancelDetachAt) && r.cancelPending(tab) {
				r.abandonCanceledTurn(tab)
				r.afterSettle(ctx, tab, context.Canceled, runTurn)
			}
		}
	}
	r.closeSpentTabs()
	return r.dropLostSessions()
}

// afterSettle retires unused saved views, reads historical reports and drains
// queued input. Swarm results are persisted by the runtime.
func (r *managedREPL) afterSettle(ctx context.Context, tab *replTab, err error, runTurn turnRunner) {
	if r.closeSpentChild(tab) {
		return
	}
	r.pullReports(ctx, tab)
	r.startQueued(ctx, tab, runTurn)
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
// an empty composer). With turns running in other tabs the first
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
		tab.model.approvalsClosed = true
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
		m.notificationMu.Lock()
		notices := m.notices
		m.notices = nil
		m.notificationMu.Unlock()
		if focusKnown && !focused {
			out = append(out, notices...)
		}
	}
	return out
}

// pendingTurn is a turn the composer accepted, waiting for the event loop
// to start it on the tab whose model took it.
type pendingTurn struct {
	model *replModel
	turn  managedTurnInput
}

func (r *managedREPL) takePendingTurn() (pendingTurn, bool) {
	select {
	case p := <-r.pending:
		return p, true
	default:
		return pendingTurn{}, false
	}
}

// startManagedTurn starts turn on tab: its goroutine reports into the tab's
// own model and session, whichever tab is visible meanwhile, and its result
// reaches the loop through tab.turnDone and a tab wake. Runs on the event
// loop with no model lock held.
func (r *managedREPL) startManagedTurn(ctx context.Context, tab *replTab, turn managedTurnInput, runTurn turnRunner) {
	r.startupLogoVisible = false
	if tab.parentName != "" {
		tab.keepOpen = true
	}
	m := tab.model
	m.mu.Lock()
	m.turnID++
	turnID := m.turnID
	reuseUser := m.restoreDraftNext
	m.restoreDraftNext = false
	persistence := m.currentPersistence
	if persistence == nil {
		persistence = newTurnPersistenceAck(false)
		m.currentPersistence = persistence
	}
	m.mu.Unlock()

	turnCtx, cancel := turnContext(ctx, tab.state)
	tab.turnCancel = cancel
	tab.cancelDetachAt = time.Time{}
	done := make(chan error, 1)
	tab.turnDone = done
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, state: tab.state, turnID: turnID, reuseUser: reuseUser, turn: cloneManagedTurn(turn), persistence: persistence}
	go func() {
		err := runTurn(turnCtx, turn.displayText, tui)
		done <- err
		r.wakeTabs()
	}()
}

// startQueued drains the inputs queued on tab while its turn ran, now that it
// has settled. Queued commands run inline against that tab, hidden or not
// (honoring a quit), and the first queued prompt starts the tab's next turn.
// Called from the event loop after settleTurn, the safe point where
// currentAssistant is already reset so activating a queued prompt or a
// queued /clear can't corrupt the just-finished stream. Nothing starts while
// the REPL is leaving.
func (r *managedREPL) startQueued(ctx context.Context, tab *replTab, runTurn turnRunner) {
	if r.quitting || tab.turnDone != nil || tab.reportsLoading || runTurn == nil {
		return
	}
	m := tab.model
	for {
		m.mu.Lock()
		if len(m.queue) == 0 {
			m.mu.Unlock()
			return
		}
		item := m.queue[0]
		m.queue = m.queue[1:]
		text := item.text
		m.activateQueuedInput(item)

		if item.turn != nil {
			turn := cloneManagedTurn(*item.turn)
			m.beginManagedTurnState(turn)
			m.mu.Unlock()
			r.startManagedTurn(ctx, tab, turn, runTurn)
			return
		}

		// runCommand and its helpers expect the model lock held (as in
		// handleEvent); requestQuit does not, so release before quitting.
		handled, quit := r.runCommandOn(tab, text)
		if !handled {
			m.appendNoticeLine(defaultReplCommands.unknownCommandNotice(text))
		}
		if m.busy {
			// The command submitted a turn; give it its goroutine.
			m.mu.Unlock()
			r.startPendingTurn(ctx, runTurn)
			return
		}
		m.mu.Unlock()
		if quit {
			r.requestQuit()
			return
		}
	}
}

// runCommandOn runs a slash command against tab. Command handlers address
// the visible tab, so for a hidden tab r.model and r.state name it for the
// duration; nothing else reads them meanwhile, since this runs on the event
// loop. Caller must hold tab.model.mu.
func (r *managedREPL) runCommandOn(tab *replTab, line string) (handled, quit bool) {
	if tab.model == r.model {
		return r.runCommand(line)
	}
	model, state := r.model, r.state
	r.model, r.state = tab.model, tab.state
	defer func() { r.model, r.state = model, state }()
	return r.runCommand(line)
}

// settleTurn lands a returned turn on its tab: the outcome is labeled, the
// tab's screen state returns to idle, and its turn fields clear. Runs on the
// event loop with no model lock held.
func (r *managedREPL) settleTurn(tab *replTab, err error) {
	m := tab.model
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.turnStarted.IsZero() {
		m.lastElapsed = time.Since(m.turnStarted)
	}
	completion := turnCompletion{Err: err, Elapsed: m.lastElapsed, ProgressSaved: err == nil || turnProgressSaved(err)}
	if m.completion != nil {
		completion = *m.completion
		if err != nil {
			completion.Err = err
		}
	}
	m.lastElapsed = completion.Elapsed
	m.lastOutcome = completion.outcome()
	err = completion.Err
	m.busy = false
	m.canceling = false
	m.toolName = ""
	// Any tool whose OnToolEnd never fired must settle to a terminal row; leaving
	// the animated arrow frozen would make an idle transcript look active.
	activeToolReason := "stopped"
	if m.lastOutcome == turnOutcomeCanceled {
		activeToolReason = "canceled"
	} else if err != nil && m.lastOutcome != turnOutcomeIncomplete {
		activeToolReason = "failed"
	}
	m.turnDock.reasoningIDs = append([]int64(nil), m.turnReasoningIDs...)
	m.turnDock.toolIDs = append([]int64(nil), m.turnToolDisclosureIDs...)
	// A partially persisted turn keeps its completed iterations — reasoning
	// included — so only a turn that saved nothing marks its thinking unsaved.
	progressSaved := completion.ProgressSaved
	m.completeThinkingTurn(err != nil && !progressSaved)
	m.settleActiveTools(activeToolReason)
	m.activeTools = nil
	m.activeToolsPhase = -1
	m.runningTools = 0
	// Like reasoning, tool activity defaults closed and auto-collapses when the
	// turn settles. Canceled/failed row details remain one click away.
	m.completeToolDisclosure()
	m.collapseTurnToolDisclosures()
	// Only call out data loss. Persisted progress needs no extra success label.
	unsavedSuffix := ""
	if !progressSaved {
		unsavedSuffix = " · not saved"
	}
	switch {
	case m.lastOutcome == turnOutcomeDone:
		m.finishAssistantBlock("")
		if !m.turnHasOutput {
			m.appendNoticeLine("No response")
		}
	case m.lastOutcome == turnOutcomeIncomplete:
		// A cap is news like a failure but settles like success: completed
		// work is persisted, queued input still flows, and the prompt is not
		// clawed back — matching how hydration treats an interrupted turn.
		label := turnOutcomeLabel(m.lastOutcome) + unsavedSuffix
		m.finishAssistantBlock(label)
		m.labelTurnOutcome(label)
		if !m.turnHasOutput {
			m.appendNoticeLine("No response")
		}
	case m.lastOutcome == turnOutcomeCanceled:
		m.finishAssistantBlock("canceled" + unsavedSuffix)
		m.labelTurnOutcome("canceled" + unsavedSuffix)
		m.discardQueuedInputs()
		if !m.restoreTurnDraft(m.currentTurn, m.currentPersistence) {
			m.appendNoticeLine("Input available with ↑ · current draft preserved")
		}
	default:
		m.finishAssistantBlock("failed" + unsavedSuffix)
		m.labelTurnOutcome("failed" + unsavedSuffix)
		m.appendLine(style.Styled("Error: "+err.Error(), "err", ""))
		m.discardQueuedInputs()
		if !m.restoreTurnDraft(m.currentTurn, m.currentPersistence) {
			m.appendNoticeLine("Input available with ↑ · current draft preserved")
		}
	}
	m.settleTurnDock()
	m.attachTurnDockTrailer()
	// Settling out of sight is news for the visible tab: the badge until this
	// tab is shown, and a notice line. Cancellation is user-initiated.
	if m.hidden {
		switch m.lastOutcome {
		case turnOutcomeDone:
			m.unseenOutcome = m.lastOutcome
			m.signalHiddenLocked(signalTurnDone, formatElapsed(m.lastElapsed))
		case turnOutcomeFailed:
			m.unseenOutcome = m.lastOutcome
			m.signalHiddenLocked(signalTurnFailed, style.Truncate(err.Error(), 120))
		case turnOutcomeIncomplete:
			// err is nil for a token cap, so the detail is elapsed, not err.
			m.unseenOutcome = m.lastOutcome
			m.signalHiddenLocked(signalTurnIncomplete, formatElapsed(m.lastElapsed))
		}
	}
	// A long turn settling is the "walk away and get pinged" moment; a quick
	// one never gave the user time to leave. Cancellation is user-initiated,
	// so only done/failed/incomplete notify.
	if m.lastElapsed >= notifyMinTurn && m.lastOutcome != turnOutcomeCanceled {
		body := "done in " + coarseElapsed(m.lastElapsed)
		switch m.lastOutcome {
		case turnOutcomeFailed:
			body = "failed after " + coarseElapsed(m.lastElapsed)
		case turnOutcomeIncomplete:
			body = "incomplete after " + coarseElapsed(m.lastElapsed)
		}
		if preview := compactQueuePreview(m.currentPrompt); preview != "" {
			body += " — " + preview
		}
		m.pushNotice(body)
	}
	m.currentAssistant = -1
	m.resetAssistantStream()
	m.turnStarted = time.Time{}
	m.state = turnStateIdle
	m.currentPrompt = ""
	m.currentTurn = managedTurnInput{}
	m.currentPersistence = nil
	// A settled turn owns no callbacks. Advance the generation before exposing
	// the idle prompt so a provider goroutine that emits late cannot reopen the
	// assistant block or move the status back to streaming.
	m.turnID++
	tab.turnDone = nil
	tab.cancelDetachAt = time.Time{}
	r.cancelTurn(tab)
}

// cancelTurn cancels tab's turn context, if a turn is running.
func (r *managedREPL) cancelTurn(tab *replTab) {
	if tab.turnCancel != nil {
		tab.turnCancel()
		tab.turnCancel = nil
	}
}

// cancelPending reports whether tab's turn was canceled and has yet to settle.
func (r *managedREPL) cancelPending(tab *replTab) bool {
	tab.model.mu.Lock()
	defer tab.model.mu.Unlock()
	return tab.model.busy && tab.model.canceling
}

// abandonCanceledTurn gives up on tab's canceled turn once the grace is
// over: the tab returns to idle and the turn's late result is dropped when
// its goroutine finally returns. Runs on the event loop with no model lock
// held.
func (r *managedREPL) abandonCanceledTurn(tab *replTab) {
	m := tab.model
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.busy || !m.canceling {
		return
	}
	tab.turnDone = nil
	tab.cancelDetachAt = time.Time{}
	r.cancelTurn(tab)
	m.turnID++
	if !m.turnStarted.IsZero() {
		m.lastElapsed = time.Since(m.turnStarted)
	}
	// Detaching advances the turn generation, which also revokes the stuck
	// turn's session write via TurnPersistenceAllowed — newer turns may start
	// appending, so its late results must be dropped. "not saved" stays true.
	m.finishAssistantBlock("canceled · not saved")
	m.labelTurnOutcome("canceled · not saved")
	m.busy = false
	m.canceling = false
	m.currentAssistant = -1
	m.resetAssistantStream()
	m.turnStarted = time.Time{}
	m.toolName = ""
	m.turnDock.reasoningIDs = append([]int64(nil), m.turnReasoningIDs...)
	m.turnDock.toolIDs = append([]int64(nil), m.turnToolDisclosureIDs...)
	m.completeThinkingTurn(true)
	m.settleActiveTools("canceled")
	m.activeTools = nil
	m.activeToolsPhase = -1
	m.runningTools = 0
	m.completeToolDisclosure()
	m.collapseTurnToolDisclosures()
	m.denyApprovalLocked()
	m.state = turnStateIdle
	m.lastOutcome = turnOutcomeCanceled
	m.settleTurnDock()
	m.attachTurnDockTrailer()
	restored := cloneManagedTurn(m.currentTurn)
	restoredPersistence := m.currentPersistence
	m.currentPrompt = ""
	m.currentTurn = managedTurnInput{}
	m.currentPersistence = nil
	m.discardQueuedInputs()
	if !m.restoreTurnDraft(restored, restoredPersistence) {
		m.appendNoticeLine("Input available with ↑ · current draft preserved")
	}
	m.appendNoticeLine("Cancellation timed out · turn detached")
}

// turnContext parents a turn on its session's lease context, so losing the
// lease cancels the turn with its typed cause, and bridges the run context
// in for signals. Cancel reports context.Canceled, the user-cancel cause the
// turn UI labels as canceled.
func turnContext(ctx context.Context, state *conversationState) (context.Context, context.CancelFunc) {
	parent := ctx
	if state != nil && state.session != nil {
		parent = state.session.Context()
	}
	turnCtx, cancel := context.WithCancelCause(parent)
	stop := context.AfterFunc(ctx, func() { cancel(context.Cause(ctx)) })
	return turnCtx, func() {
		stop()
		cancel(context.Canceled)
	}
}

// handleInterrupt processes Ctrl-C. While a turn is in flight the first press
// cancels it — denying any pending approval so the turn goroutine isn't parked
// on the reply channel — and keeps the REPL open. A second press while the turn
// is still winding down quits. At idle, Ctrl-C first clears a draft; an empty
// prompt exits, after one warning when turns run in other tabs
// (requestIdleQuitLocked). Returns true to quit. Caller must hold m.mu.
func (r *managedREPL) handleInterrupt() bool {
	m := r.model
	if !m.busy {
		if !m.ed.empty() {
			m.ed.clear()
			return false
		}
		return r.requestIdleQuitLocked()
	}
	if m.canceling {
		r.requestQuit()
		return true
	}
	r.cancelBusyTurn()
	return false
}

// cancelBusyTurn cancels the in-flight turn: freezes the visible partial,
// cancels the turn context, and denies any pending approval so the turn
// goroutine isn't parked on the reply channel. Pending input is marked not sent
// when cancellation settles. Caller must hold m.mu and ensure m.busy &&
// !m.canceling.
func (r *managedREPL) cancelBusyTurn() {
	m := r.model
	m.canceling = true
	// Freeze the visible partial immediately, but do not label it unsaved until
	// the turn actually settles as canceled. Completion and cancel can race; a
	// successful result must never retain a false "not saved" label.
	m.finishAssistantBlock("")
	tab := r.visibleTab()
	r.cancelTurn(tab)
	r.armCancelDetach(tab)
	m.denyApprovalLocked()
}
