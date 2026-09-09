package main

import (
	"context"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
	tcell "github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

func (r *managedREPL) Run(ctx context.Context, runTurn turnRunner) error {
	if err := ui.Init(); err != nil {
		return err
	}
	// gotui inits the tcell screen with a white default foreground, and tcell
	// substitutes that default for any cell drawn with the zero style — which is
	// exactly our ColorClear body text (input + LLM responses). Reset the screen
	// default to all-defaults so unstyled text emits the terminal's own
	// foreground (SGR 39) and follows the theme instead of being forced white.
	ui.DefaultBackend.Screen.SetStyle(tcell.StyleDefault)
	// Motion reports route inspector navigation by pointer position and allow
	// divider dragging. Button reports keep wheel scrolling separate from keys.
	// Native text selection uses the terminal's shift/option override.
	ui.DefaultBackend.Screen.EnableMouse(tcell.MouseButtonEvents | tcell.MouseMotionEvents)
	// Focus reports gate desktop notifications (notify only when the user is
	// known to be elsewhere); terminals without focus reporting simply never
	// flip the gate open.
	ui.DefaultBackend.Screen.EnableFocus()
	r.fx = newTerminalFX(ui.DefaultBackend.Screen)
	r.affordanceW = &affordanceLayer{}
	r.images = termimg.NewManager(ui.DefaultBackend.Screen)
	r.model.mu.Lock()
	r.model.affordances.enabled = ui.DefaultBackend.Screen.Colors() > 0
	r.model.affordances.inputAt = time.Now()
	r.model.nativeImages = r.images != nil
	r.model.visual.invalidate()
	r.model.mu.Unlock()
	// Restore the terminal exactly once. Signal cancellation unwinds through
	// this defer; terminal effects clear first so the progress OSC goes out
	// while the screen is still up.
	var closeOnce sync.Once
	closeUI := func() {
		closeOnce.Do(func() {
			if r.images != nil {
				r.images.Shutdown()
				r.images = nil
			}
			r.fx.shutdown()
			ui.Close()
		})
	}
	setBeforeExit(closeUI)
	defer func() {
		setBeforeExit(nil)
		closeUI()
	}()

	r.runCtx = ctx
	r.runTurn = runTurn
	defer r.drainOpen()

	r.initHistory()
	defer r.closeHistory()

	// Clipboard captures and materialized prepared-payload previews share this
	// cache; once their transcripts are gone, stale files serve no one.
	go func() {
		if dir, err := attachmentCacheDir(); err == nil {
			sweepAttachmentCache(dir, time.Now())
		}
	}()

	r.setupWidgets()
	r.startupLogoVisible = r.showStartupLogo && !r.workspace().inspector.open
	r.model.mu.Lock()
	// The sandbox posture reads from the masthead, exceptional or not; the
	// line frontends keep their startup notice.
	r.model.masthead = mastheadState{enabled: true, sandbox: currentSandboxPosture(r.config, r.state).summaryLine(false)}
	r.model.visual.invalidate()
	r.appendPendingSandboxWarningsLocked()
	r.model.mu.Unlock()
	r.render()

	events := pollManagedEvents(ui.DefaultBackend.Screen)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	// Reports posted for the open sessions while no polly held them are
	// their first input; the poll catches ones posted by children running
	// elsewhere from now on.
	reportPoll := time.NewTicker(reportPollInterval)
	defer reportPoll.Stop()
	if r.pullAllReports(ctx, runTurn) {
		r.render()
	}

	for {
		select {
		case <-ctx.Done():
			r.cancelTurns()
			return context.Cause(ctx)
		case <-r.tabEvents:
			// A turn settled, a lease ended, or a cancel grace ran out on
			// some tab. A lost lease that leaves nothing to show ends the
			// run with its typed cause, as it did when the run context was
			// parented on the session.
			if err := r.settleTabs(ctx, runTurn); err != nil {
				r.cancelTurns()
				return err
			}
			if r.quitSettled() {
				return nil
			}
			r.render()
		case <-r.quitDeadline:
			// The grace for running turns is up; they are cut off.
			return nil
		case res := <-r.openDone:
			r.finishOpen(res)
			r.render()
		case <-r.quit:
			if r.beginQuit() {
				return nil
			}
			r.render()
		case <-r.suspend:
			if err := r.suspendUI(ui.DefaultBackend.Screen); err != nil {
				r.model.mu.Lock()
				r.model.appendNoticeLine("Suspend failed · " + err.Error())
				r.model.mu.Unlock()
			}
			r.render()
		case <-r.state.sandboxWarningNotify():
			r.model.mu.Lock()
			appended := r.appendPendingSandboxWarningsLocked()
			r.model.mu.Unlock()
			if appended {
				r.render()
			}
		case <-r.images.ReadyEvents():
			// CPU-heavy image preparation finishes off-thread. The event loop
			// remains the sole owner of terminal writes and cell locks.
			r.render()
		case <-ticker.C:
			if r.needsTick() {
				r.render()
			} else {
				r.tickAffordances(time.Now())
			}
		case <-reportPoll.C:
			if r.pullAllReports(ctx, runTurn) {
				r.render()
			}
		case ev := <-events:
			if r.handleEvent(ev) {
				if r.beginQuit() {
					return nil
				}
				r.render()
				continue
			}
			r.applyTabRequests()
			r.startPendingTurn(ctx, runTurn)
			if tab := r.visibleTab(); tab.turnDone == nil {
				r.startQueued(ctx, tab, runTurn)
			}
			if r.wantsRenderForEvent(ev) {
				r.render()
			}
		case p := <-r.pending:
			r.startManagedTurn(ctx, r.tabForModel(p.model), p.turn, runTurn)
			r.render()
		case task := <-r.uiTasks:
			task()
			r.render()
		}
		// Every arm may leave a recorded tab request behind (a queued /close
		// drained by settleTabs, a picker choice landing with an open
		// session), so the loop applies them once per iteration rather than
		// trusting each arm to remember.
		if r.applyTabRequests() {
			r.render()
		}
	}
}

// postUITask hands a completed background result to the event loop. Dropping
// on a full buffer is deliberate: a task that cannot land immediately after
// shutdown has nobody to land for.
func (r *managedREPL) postUITask(task func()) {
	select {
	case r.uiTasks <- task:
	default:
	}
}

// appendPendingSandboxWarningsLocked moves every queued sandbox warning into
// the transcript. The managed event loop owns this drain so warnings found by
// background tool activation cannot mutate the model or terminal directly.
// Caller must hold r.model.mu.
func (r *managedREPL) appendPendingSandboxWarningsLocked() bool {
	if r == nil || r.state == nil {
		return false
	}
	warnings := r.state.drainSandboxWarnings()
	for _, body := range warnings {
		r.model.appendNoticeLine("Warning: " + body)
	}
	return len(warnings) > 0
}

// needsTick reports whether the periodic render tick should repaint. Only a
// live turn needs whole-frame refreshes (spinners and elapsed timers). Idle
// affordance feedback has a separate cell-only tick, avoiding a full repaint
// ~20 times a second while sitting at the prompt.
func (r *managedREPL) needsTick() bool {
	if r.workspace().inspector.open {
		i := &r.workspace().inspector
		v := i.current
		if v != nil && !v.loading && !v.unavailable && v.failures > 0 && !time.Now().Before(v.retryAt) {
			return true
		}
		if tab := r.inspectionTab(i.target); tab != nil {
			tab.model.mu.Lock()
			busy := tab.model.busy
			tab.model.mu.Unlock()
			if busy {
				return true
			}
		} else if (v == nil || !v.loading && !v.unavailable && v.failures == 0) && time.Since(r.inspectorRefreshAt) >= time.Second {
			return true
		}
	}
	for _, tab := range r.tabs {
		if tab.state != nil && tab.state.swarm != nil && (tab.swarmActive || tab.state.swarm.HasActive()) {
			return true
		}
	}
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	// A live listing (the sessions picker) repaints to follow the tabs.
	return r.model.busy || r.model.modal != nil && r.model.modal.refresh != nil
}

// wantsRenderForEvent reports whether to repaint after handling ev. Bracketed
// paste arrives as one event per rune; those are coalesced into a single
// repaint when the paste's closing marker flips m.pasting back off (handleEvent
// clears it), so a large paste draws once instead of once per character.
func (r *managedREPL) wantsRenderForEvent(ev ui.Event) bool {
	// Button-free motion and release only update pointer/drag state; a
	// repaint is due only when the target under the pointer changed (the
	// grip, a thumb, or a hover-marked link, label, row, or button).
	if ev.Type == ui.MouseEvent && ev.ID == "<MouseRelease>" {
		return r.chromeHoverChanged
	}
	if mouse, ok := ev.Payload.(ui.Mouse); ok && mouse.Drag && !r.inspectorDragging && r.scrollDrag.pane == "" {
		return false
	}
	if ev.ID == pasteStartID {
		return false
	}
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	return !r.model.pasting
}
