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

	keys := keyContext{event: e, viewport: viewport, width: terminalWidth, height: terminalHeight}
	// Scroll keys work in every mode (idle, busy, approval) so the user
	// can review history without interrupting the agent.
	if handled, quit := r.runKey(scrollPhase, keys); handled {
		return quit
	}
	switch e.ID {
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

	// Global keys stay available during turns, searches, and approval prompts.
	if handled, quit := r.runKey(globalPhase, keys); handled {
		return quit
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
	if handled, quit := r.runKey(composerPhase, keys); handled {
		return quit
	}
	if ch, ok := printableRune(e); ok {
		m.ed.insert(ch)
	}
	return false
}

// keyPhase is where in handleEventLocked a binding is consulted: scroll keys
// before any mode owns input, global keys after paste but before search and
// approval, composer keys last, once the composer has the keyboard.
type keyPhase int

const (
	scrollPhase keyPhase = iota
	globalPhase
	composerPhase
)

type keyContext struct {
	event                   ui.Event
	viewport, width, height int
}

// keyBinding is one action and the event IDs that trigger it. label names it
// in /keys; an empty label keeps an alias out of the help. run reports quit.
type keyBinding struct {
	keys  []string
	label string
	desc  string
	phase keyPhase
	run   func(r *managedREPL, k keyContext) bool
}

// keyGroup is a /keys section: its bindings, then notes for keys and mouse
// actions that other handlers own (dialogs, the inspector, approval).
type keyGroup struct {
	title    string
	bindings []keyBinding
	notes    []keyHelpRow
}

type keyHelpRow struct{ key, desc string }

type keyHelpGroup struct {
	title string
	rows  []keyHelpRow
}

func editorKey(label, desc string, edit func(*lineEditor), keys ...string) keyBinding {
	return keyBinding{keys: keys, label: label, desc: desc, phase: composerPhase, run: func(r *managedREPL, _ keyContext) bool {
		edit(&r.model.ed)
		return false
	}}
}

func replKey(label, desc string, phase keyPhase, run func(r *managedREPL, k keyContext) bool, keys ...string) keyBinding {
	return keyBinding{keys: keys, label: label, desc: desc, phase: phase, run: run}
}

func scrollKey(label, desc string, lines func(viewport int) int, keys ...string) keyBinding {
	return replKey(label, desc, scrollPhase, func(r *managedREPL, k keyContext) bool {
		r.model.scrollByWidth(lines(k.viewport), k.viewport, k.width)
		return false
	}, keys...)
}

// keyTable is the single source for key dispatch and the /keys help. It is
// built in init: the bindings reach the command registry, whose help reads
// the table back, and a package-level initializer would close that cycle.
var (
	keyTable []keyGroup
	keyIndex map[keyPhase]map[string]keyBinding
)

func init() {
	keyTable = keyBindingGroups()
	keyIndex = indexKeyBindings(keyTable)
}

func keyBindingGroups() []keyGroup {
	return []keyGroup{
		{title: "Send and edit", bindings: []keyBinding{
			replKey("Enter", "Send the message", composerPhase, func(r *managedREPL, _ keyContext) bool { return r.submitComposerLocked() }, "<Enter>"),
			editorKey("Ctrl-J", "Insert a newline", func(ed *lineEditor) { ed.insert('\n') }, "<C-j>"),
			replKey("Tab", "Complete a command · focus an open inspector", composerPhase, completeOrFocusInspector, "<Tab>"),
			replKey("Ctrl-R", "Search history", composerPhase, func(r *managedREPL, _ keyContext) bool { r.model.hist.startSearch(); return false }, "<C-r>"),
			replKey("Ctrl-V", "Attach the clipboard image", composerPhase, func(r *managedREPL, _ keyContext) bool { r.captureClipboardToComposer(); return false }, "<C-v>"),
			replKey("Ctrl-L", "Clear the display", composerPhase, func(r *managedREPL, _ keyContext) bool { r.model.clearDisplay(); return false }, "<C-l>"),
			// tcell runs the terminal in raw mode, so the terminal driver cannot
			// turn Ctrl-Z into SIGTSTP. Suspension is queued on the UI loop, which
			// restores the terminal before stopping the foreground process group.
			replKey("Ctrl-Z", "Suspend to the shell (fg resumes)", globalPhase, func(r *managedREPL, _ keyContext) bool { r.requestSuspend(); return false }, "<C-z>"),
			// Escape cancels an in-flight turn like Ctrl-C, but never quits: at
			// idle (or while already canceling) it hides the slash hint line until
			// the input next changes.
			replKey("Esc", "Dismiss a dialog, search, or the inspector · interrupt", composerPhase, func(r *managedREPL, _ keyContext) bool {
				r.model.slashHintsHidden = true
				if r.model.busy && !r.model.canceling {
					r.cancelBusyTurn()
				}
				return false
			}, "<Escape>"),
			editorKey("Home Ctrl-A", "Line start", (*lineEditor).home, "<Home>", "<C-a>"),
			replKey("End Ctrl-E", "Line end · scroll to the bottom", composerPhase, func(r *managedREPL, _ keyContext) bool {
				r.model.ed.end()
				r.model.scrollToBottom()
				return false
			}, "<End>", "<C-e>"),
			editorKey("Ctrl-U", "Clear to the line start", (*lineEditor).killToStart, "<C-u>"),
			editorKey("Ctrl-K", "Clear to the line end", (*lineEditor).killToEnd, "<C-k>"),
			editorKey("Ctrl-W", "Delete the previous word", (*lineEditor).deleteWordBackward, "<C-w>"),
			editorKey("Alt-D", "Delete the next word", (*lineEditor).deleteWordForward, "<M-d>"),
			editorKey("Alt-B Alt-F", "Move by word", (*lineEditor).wordLeft, "<M-b>"),
			editorKey("", "", (*lineEditor).wordRight, "<M-f>"),
			editorKey("Ctrl-D Delete", "Delete the next character", (*lineEditor).deleteForward, "<C-d>", "<Delete>"),
			editorKey("", "", (*lineEditor).backspace, "<Backspace>", "<C-h>"),
			editorKey("", "", (*lineEditor).left, "<Left>"),
			editorKey("", "", (*lineEditor).right, "<Right>"),
			editorKey("", "", func(ed *lineEditor) { ed.insert(' ') }, "<Space>"),
		}, notes: []keyHelpRow{
			{"Ctrl-C", "Interrupt the turn · twice to quit"},
		}},
		{title: "Navigate", bindings: []keyBinding{
			// Move within a multi-line prompt; recall history only from its first
			// or last line (zsh up-line-or-history).
			replKey("Up", "Move up a line or recall older history", composerPhase, func(r *managedREPL, _ keyContext) bool {
				if !r.model.ed.up() {
					r.model.historyUp()
				}
				return false
			}, "<Up>"),
			replKey("Down", "Move down a line or recall newer history", composerPhase, func(r *managedREPL, _ keyContext) bool {
				if !r.model.ed.down() {
					r.model.historyDown()
				}
				return false
			}, "<Down>"),
			scrollKey("PgUp", "Page the transcript up · the inspector when focused", func(v int) int { return -v / 2 }, "<PageUp>"),
			scrollKey("PgDn", "Page the transcript down · the inspector when focused", func(v int) int { return v / 2 }, "<PageDown>"),
			scrollKey("", "", func(int) int { return -3 }, "<MouseWheelUp>"),
			scrollKey("", "", func(int) int { return 3 }, "<MouseWheelDown>"),
			// The picker opens on the agent that needs attention, when one does.
			replKey("Ctrl-G", "Open the sessions picker", composerPhase, func(r *managedREPL, _ keyContext) bool {
				r.openSessionsPickerSelected(r.attentionAgentName())
				return false
			}, "<C-g>"),
		}, notes: []keyHelpRow{
			{"Alt-1..9 Alt-] Alt-[", "Switch workspace"},
			{"Shift-drag", "Select terminal text"},
		}},
		{title: "Inspect", bindings: []keyBinding{
			// Toggles the active turn's reasoning disclosure, or the newest
			// completed one while idle, without moving focus from the composer.
			replKey("Ctrl-O", "Toggle thinking for the latest turn", globalPhase, func(r *managedREPL, _ keyContext) bool {
				r.model.toggleLatestReasoning(0)
				return false
			}, "<C-o>"),
		}, notes: []keyHelpRow{
			{"Left / Right", "Previous or next tool or thought in a focused inspector"},
			{"Click detail", "Inspect an agent, tool result, or thought"},
			{"Click disclosure", "Expand thinking or tool calls"},
			{"Click thumbnail", "Open the image"},
		}},
		{title: "Approve", notes: []keyHelpRow{
			{"y", "Allow"},
			{"n Enter Esc", "Deny"},
			{"a", "Allow the rest of the batch"},
		}},
	}
}

// completeOrFocusInspector is Tab: an empty composer has nothing to complete,
// so Tab hands the keys to the open inspector instead (Esc or typing hands
// them back). Otherwise it completes with the live command context so
// completers can see session state (loaded tool names for "/tools show").
func completeOrFocusInspector(r *managedREPL, _ keyContext) bool {
	m := r.model
	if i := &r.workspace().inspector; m.ed.empty() && i.open && !i.searching {
		i.focused = true
		return false
	}
	cur := m.ed.text()
	if completed, _, ok := defaultReplCommands.complete(cur, newManagedReplCommandContext(r)); ok {
		if completed != cur {
			m.ed.setText(completed)
		}
		return false
	}
	m.ed.insert('\t')
	return false
}

func indexKeyBindings(groups []keyGroup) map[keyPhase]map[string]keyBinding {
	index := make(map[keyPhase]map[string]keyBinding)
	for _, g := range groups {
		for _, b := range g.bindings {
			if index[b.phase] == nil {
				index[b.phase] = make(map[string]keyBinding)
			}
			for _, id := range b.keys {
				index[b.phase][id] = b
			}
		}
	}
	return index
}

// runKey dispatches the event to the binding registered for phase, if any.
// Caller must hold r.model.mu.
func (r *managedREPL) runKey(phase keyPhase, k keyContext) (handled, quit bool) {
	b, ok := keyIndex[phase][k.event.ID]
	if !ok {
		return false, false
	}
	return true, b.run(r, k)
}

// keyHelpGroups projects the binding table into /keys sections.
func keyHelpGroups() []keyHelpGroup {
	var groups []keyHelpGroup
	for _, g := range keyTable {
		rows := make([]keyHelpRow, 0, len(g.bindings)+len(g.notes))
		for _, b := range g.bindings {
			if b.label != "" {
				rows = append(rows, keyHelpRow{b.label, b.desc})
			}
		}
		rows = append(rows, g.notes...)
		groups = append(groups, keyHelpGroup{g.title, rows})
	}
	return groups
}
