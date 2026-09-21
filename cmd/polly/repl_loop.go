package main

import (
	"context"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/screenimg"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
	tcell "github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

func (r *managedREPL) Run(ctx context.Context, runTurn turnRunner) error {
	// The screen this run paints on: the terminal's own, or an off-screen
	// simulation screen when a shot script supplies the input.
	if r.headless == nil {
		if err := ui.Init(); err != nil {
			return err
		}
	} else if err := r.headless.installScreen(); err != nil {
		return err
	}
	// gotui inits the tcell screen with a white default foreground, and tcell
	// substitutes that default for any cell drawn with the zero style — which is
	// exactly our ColorClear body text (input + LLM responses). Reset the screen
	// default to all-defaults so unstyled text emits the terminal's own
	// foreground (SGR 39) and follows the theme instead of being forced white.
	// themedScreen then substitutes the theme's surface roles for those
	// defaults; with both at inherit it is the same all-defaults style.
	ui.DefaultBackend.Screen = themedScreen{r.painter.track(ui.DefaultBackend.Screen)}
	syncThemeSurface()
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
	if r.headless == nil {
		r.images = termimg.NewManager(ui.DefaultBackend.Screen)
	} else {
		// Nothing off-screen speaks kitty or sixel, so the images a frame would
		// have drawn are tracked as pixels instead (see termimg.Placements) and
		// painted into each capture by repl_screenshot.go. The cell pixel size
		// is the capture's own, so placements land on the same pixels the PNG
		// draws.
		width, height := ui.DefaultBackend.Screen.Size()
		cellWidth, cellHeight := screenimg.CellSize()
		r.images = termimg.NewRenderManager(ui.DefaultBackend.Screen, width, height, cellWidth, cellHeight)
	}
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
	r.model.mu.Lock()
	// The sandbox posture reads from the masthead, exceptional or not; the
	// line frontends keep their startup notice.
	r.model.masthead = mastheadState{enabled: true, sandbox: currentSandboxPosture(r.config, r.state).summaryLine(false)}
	r.model.visual.invalidate()
	r.appendPendingSandboxWarningsLocked()
	if r.config.Setup {
		r.openSetupForm()
	}
	r.model.mu.Unlock()
	r.render()

	// Input comes from the terminal's event queue, or from a shot script that
	// plays on its own goroutine: its keys arrive here as the events a terminal
	// would deliver, and its other steps ride uiTasks (see repl_headless.go).
	var events <-chan ui.Event
	if r.headless == nil {
		events = pollManagedEvents(ui.DefaultBackend.Screen)
	} else {
		events = r.headless.keys
		go r.headless.play(ctx, r)
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	defer r.cancelWheelPaint()

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
			for _, tab := range r.tabs {
				if tab.state.finishWorkspaceChanges(ctx) {
					tab.model.mu.Lock()
					tab.model.setWorkspaceChanges(workspaceChangesPresentation(tab.state.workspaceChanges.currentReport()))
					tab.model.mu.Unlock()
					r.startQueued(ctx, tab, runTurn)
					r.render()
				}
			}
			// A theme edit is applied here and repaints on its own: needsTick()
			// is false for an idle REPL, so the watcher cannot ride that branch
			// or the new colors would wait for the next keystroke.
			now := time.Now()
			themed := r.pollTheme(now)
			if themed || r.needsTick() {
				r.render()
			} else {
				r.tickAffordances(now)
			}
		case <-r.wheelPaintC:
			r.render()
		case ev := <-events:
			if r.handlePaintEvent(ev) {
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
			r.paintAfterEvent(ev)
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

// acceptsInput reports whether the composer would submit rather than queue,
// which is what a shot script waits for before its first typed line: the
// startup workspace baseline runs behind the first frame.
func (r *managedREPL) acceptsInput() bool {
	if r.opening != "" || r.state.workspaceChangesPending() {
		return false
	}
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	return !r.model.busy
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
	if r == nil {
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
	if mouse, ok := ev.Payload.(ui.Mouse); ok && r.passiveDrag(mouse) {
		return false
	}
	if ev.ID == pasteStartID {
		return false
	}
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	return !r.model.pasting
}
