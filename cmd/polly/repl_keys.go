package main

import (
	"image"
	"time"

	ui "github.com/metaspartan/gotui/v5"
)

func (r *managedREPL) requestQuit() {
	select {
	case r.quit <- struct{}{}:
	default:
	}
}

// requestSuspend queues a Ctrl-Z suspension on the UI loop, which restores
// the terminal before stopping the foreground process group.
func (r *managedREPL) requestSuspend() {
	select {
	case r.suspend <- struct{}{}:
	default:
	}
}

// handleEvent mutates the model in response to a UI event. Returns true on
// quit.
func (r *managedREPL) handleEvent(e ui.Event) bool {
	if e.Type == ui.ResizeEvent {
		return false
	}

	r.model.mu.Lock()
	defer r.model.mu.Unlock()

	quit := r.handleEventLocked(e)
	if !quit {
		r.refreshSlashHints()
	}
	return quit
}

// refreshSlashHints recomputes the transient hint line from the composer
// state. Running once per input event (rather than inside each key handler)
// keeps the hint a pure function of the current input. Caller must hold m.mu.
func (r *managedREPL) refreshSlashHints() {
	m := r.model
	text := m.ed.text()
	if text != m.slashHintSource {
		m.slashHintSource = text
		m.slashHintsHidden = false
	}
	hint := ""
	if !m.slashHintsHidden && !m.pasting && !m.hist.searching && m.approval == nil && m.modal == nil {
		hint = defaultReplCommands.hintFor(newManagedReplCommandContext(r), text)
	}
	m.setSlashHintLine(hint)
}

func (r *managedREPL) handleEventLocked(e ui.Event) bool {
	m := r.model
	// Track motion even while a dialog, search, or paste owns input, so hover
	// highlights (grip, thumbs) follow the pointer. Keys never depend on it.
	if e.Type == ui.MouseEvent {
		if mouse, ok := e.Payload.(ui.Mouse); ok {
			next := image.Pt(mouse.X, mouse.Y)
			r.chromeHoverChanged = r.inspectorDragging || r.scrollDrag.pane != ""
			for _, rect := range []image.Rectangle{r.chrome.divider, r.inspectorScrollbar.track, r.modalScrollbar.track} {
				r.chromeHoverChanged = r.chromeHoverChanged || next.In(rect) != (r.mousePositionKnown && r.mousePosition.In(rect))
			}
			r.mousePosition = image.Pt(mouse.X, mouse.Y)
			r.mousePositionKnown = true
			// A new target under the pointer repaints, like the grip and
			// thumb highlights; motion within one target does not.
			r.chromeHoverChanged = r.chromeHoverChanged || r.hoverTargetAt(next) != r.hover
			if mouse.Drag && !r.inspectorDragging && r.scrollDrag.pane == "" {
				// Held-button motion is not a click; only an active divider
				// drag consumes it.
				return false
			}
		}
	}
	viewport := r.transcriptHeight()
	terminalWidth, terminalHeight := ui.TerminalDimensions()
	if terminalWidth < 1 {
		terminalWidth = 80
	}
	if terminalHeight < 2 {
		terminalHeight = 24
	}

	// Focus reports update notification gating and are never input, whatever
	// mode (paste, search, approval) is active.
	switch e.ID {
	case focusGainedID:
		m.focusKnown, m.focused = true, true
		return false
	case focusLostID:
		m.focusKnown, m.focused = true, false
		r.mousePositionKnown = false
		m.resetAffordances()
		return false
	}
	if e.Type == ui.KeyboardEvent {
		m.affordances.inputAt = time.Now()
	}

	// The warning that turns run in other tabs holds for the very next
	// quit key only; any other key withdraws it.
	if e.Type == ui.KeyboardEvent && e.ID != "<C-c>" {
		r.quitWarned = false
	}

	// Selection and credential modals own all remaining input. In particular,
	// key material never passes through paste handling or the composer.
	if m.modal != nil {
		if r.handleScrollbar(e, true) {
			return false
		}
		r.handleModalEvent(e)
		return false
	}

	// Tab shortcuts work in every mode: a turn or an approval on the tab
	// left behind keeps waiting there.
	if e.Type == ui.KeyboardEvent {
		if i, ok := r.workspaceShortcut(e.ID); ok {
			r.requestShowTabLocked(i)
			return false
		}
	}
	if r.handleScrollbar(e, false) || r.handleInspectorEvent(e) {
		return false
	}
	terminalWidth = r.frameLayoutFor(terminalWidth, terminalHeight).mainWidth()

	// Scroll keys work in every mode (idle, busy, approval) so the user
	// can review history without interrupting the agent.
	switch e.ID {
	case "<PageUp>":
		m.scrollByWidth(-viewport/2, viewport, terminalWidth)
		return false
	case "<PageDown>":
		m.scrollByWidth(viewport/2, viewport, terminalWidth)
		return false
	case "<MouseWheelUp>":
		m.scrollByWidth(-3, viewport, terminalWidth)
		return false
	case "<MouseWheelDown>":
		m.scrollByWidth(3, viewport, terminalWidth)
		return false
	case "<MouseLeft>":
		if mouse, ok := e.Payload.(ui.Mouse); ok {
			if image.Pt(mouse.X, mouse.Y).In(m.parentLink) {
				r.requestParentLocked()
				return false
			}
			if m.status.sessionField.hit(mouse.X, mouse.Y, terminalHeight) {
				r.openSessionsPicker()
				return false
			}
			if m.status.agentsField.hit(mouse.X, mouse.Y, terminalHeight) {
				r.openSessionsPickerSelected(r.attentionAgentName())
				return false
			}
			if r.openAgentAt(mouse.X, mouse.Y) {
				return false
			}
			if !m.toggleDisclosureAt(mouse.X, mouse.Y, terminalWidth) {
				r.openImageAt(mouse.X, mouse.Y)
			}
		}
		return false
	}

	// Drop any other mouse events (release, drag, unknown buttons). gotui
	// returns "Unknown_Mouse_Button" — a bare string — for events it
	// doesn't recognize, which would otherwise be typed into the prompt
	// by the default input case below.
	if e.Type == ui.MouseEvent {
		return false
	}

	// Bracketed-paste markers bound a run of literal input. While pasting, keys
	// are buffered verbatim — newlines included — instead of triggering
	// actions, so a multi-line paste lands as one prompt rather than firing a
	// submit per line. The buffer is inspected once at the closing marker:
	// terminal drag-drops (a paste that is purely image paths) become
	// attachment tokens; everything else enters the editor as literal text.
	// (gotui drops these markers; our own event pump surfaces them.)
	if e.ID == pasteStartID || e.ID == pasteEndID {
		m.pasting = e.ID == pasteStartID
		if m.pasting {
			m.pasteBuf = m.pasteBuf[:0]
		} else {
			m.flushPasteBuffer()
		}
		return false
	}
	if m.pasting {
		m.bufferPasted(e)
		return false
	}

	// Ctrl-O toggles the active turn's reasoning disclosure, or the newest
	// completed one while idle. It never moves focus away from the composer.
	if e.ID == "<C-o>" {
		m.toggleLatestReasoning(0)
		return false
	}

	// tcell runs the terminal in raw mode, so the terminal driver cannot turn
	// Ctrl-Z into SIGTSTP for us. Queue suspension on the UI loop, which first
	// restores the terminal and then stops the foreground process group. This
	// remains available during turns, searches, and approval prompts.
	if e.ID == "<C-z>" {
		r.requestSuspend()
		return false
	}

	// Reverse-incremental search owns the keyboard while active, so Ctrl-C
	// cancels the search rather than quitting. Scroll still works (handled above).
	if m.hist.searching {
		return r.handleSearchKey(e)
	}

	// Ctrl-C is the universal interrupt: cancel an in-flight turn (first
	// press) or quit (second press, or at an idle prompt). See handleInterrupt.
	if e.ID == "<C-c>" {
		return r.handleInterrupt()
	}

	// Approval has its own keyset.
	if m.approval != nil {
		if m.approval != m.paintedApproval || m.approval.index != m.paintedApprovalIndex {
			return false
		}
		switch e.ID {
		case "y", "Y":
			m.handleApprovalAnswer('y')
		case "<Enter>", "<Escape>", "n", "N":
			m.handleApprovalAnswer('n')
		case "a", "A":
			m.handleApprovalAnswer('a')
		}
		return false
	}

	// While a turn is in flight the prompt stays editable so the user can
	// compose the next message; Enter queues it (see submitComposerLocked)
	// rather than submitting immediately. Editing/history/search keys all
	// work as usual.

	switch e.ID {
	case "<Escape>":
		// Escape cancels an in-flight turn like Ctrl-C, but never quits: at
		// idle (or while already canceling) it hides the slash hint line until
		// the input next changes.
		m.slashHintsHidden = true
		if m.busy && !m.canceling {
			r.cancelBusyTurn()
		}
	case "<C-d>":
		m.ed.deleteForward()
	case "<Enter>":
		return r.submitComposerLocked()
	case "<C-j>":
		// Ctrl-J inserts a newline for composing multi-line prompts; Enter sends.
		m.ed.insert('\n')
	case "<Backspace>", "<C-h>":
		m.ed.backspace()
	case "<Delete>":
		m.ed.deleteForward()
	case "<C-w>":
		m.ed.deleteWordBackward()
	case "<M-d>":
		m.ed.deleteWordForward()
	case "<Left>":
		m.ed.left()
	case "<Right>":
		m.ed.right()
	case "<M-b>":
		m.ed.wordLeft()
	case "<M-f>":
		m.ed.wordRight()
	case "<Home>", "<C-a>":
		m.ed.home()
	case "<End>", "<C-e>":
		m.ed.end()
		m.scrollToBottom()
	case "<C-u>":
		m.ed.killToStart()
	case "<C-k>":
		m.ed.killToEnd()
	case "<C-v>":
		r.captureClipboardToComposer()
	case "<C-l>":
		m.clearDisplay()
	case "<C-r>":
		m.hist.startSearch()
	case "<C-g>":
		// The picker opens on the agent that needs attention, when one does.
		r.openSessionsPickerSelected(r.attentionAgentName())
	case "<Up>":
		// Move up a line within a multi-line prompt; recall older history only
		// when already on the first line (zsh up-line-or-history).
		if !m.ed.up() {
			m.historyUp()
		}
	case "<Down>":
		if !m.ed.down() {
			m.historyDown()
		}
	case "<Space>":
		m.ed.insert(' ')
	case "<Tab>":
		// An empty composer has nothing to complete: Tab hands the keys to
		// the open inspector instead (Esc or typing hands them back).
		if i := &r.workspace().inspector; m.ed.empty() && i.open && !i.searching {
			i.focused = true
			return false
		}
		// Complete with the live command context so completers can see
		// session state (e.g. loaded tool names for "/tools show").
		cur := m.ed.text()
		if completed, _, ok := defaultReplCommands.complete(cur, newManagedReplCommandContext(r)); ok {
			if completed != cur {
				m.ed.setText(completed)
			}
			return false
		}
		m.ed.insert('\t')
	default:
		if ch, ok := printableRune(e); ok {
			m.ed.insert(ch)
		}
	}
	return false
}
