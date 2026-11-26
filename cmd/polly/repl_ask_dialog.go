package main

// askDialog is the question a turn waits on: the model's Markdown, a list of
// choices with the selected choice's description, and an optional free-text
// answer. It is a replModal payload the way sandboxTryDialog is, so it owns
// the dialog's rendering and keys while replModal owns the frame — and the
// frame docks it in the composer's place, like the setup form, so it rises
// from the status bar rather than floating over the transcript.
//
// Three rules come from the dialog being something a turn blocks on:
//
//   - It is awaited. replModal.awaited must report it, so opening /help over
//     it parks it in pendingModal and restores it, and replModal.wipe must
//     answer it, so no close path can orphan the tool goroutine behind it.
//   - Keys count only once the question is on screen. A user typing in the
//     composer when a member asks must not have their next Enter answer it.
//   - Pasted text never answers. A paste arrives as a run of key events inside
//     the modal, so the dialog tracks the bracketed-paste markers the way the
//     sandbox review does.

import (
	"context"
	"fmt"
	"image"
	"strconv"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

const (
	// askDetailRows bounds the selected choice's explanation, which sits in a
	// pane padded to the tallest description so the rows never jump.
	askDetailRows = 3
	// askBodyMaxRows bounds the Markdown window; the modal's height comes from
	// its text, so an uncapped body would fill the screen.
	askBodyMaxRows = 12
)

type askDialog struct {
	req   askRequest
	modal *replModal
	// reply receives the answer exactly once. It is buffered by AskUser, so a
	// late answer never blocks the event loop.
	reply    chan<- askAnswer
	answered bool
	// painted is false until the first frame drew the question.
	painted bool
	// shown is when the question went up, and hosted whether the terminal's
	// host has been asked it too (see repl_pane_approve.go).
	shown  time.Time
	hosted bool

	// selected and top window the choice rows; rowsY and rowsShown are where
	// the last frame drew them, for the mouse.
	selected, top    int
	rowsY, rowsShown int

	// body is the rendered Markdown, cached per inner width; bodyTop and
	// bodyShown are its scroll window.
	body      []string
	bodyWidth int
	bodyTop   int
	bodyShown int

	// other is the free-text row's editor, which uses the modal's line editor.
	other   bool
	pasting bool
}

// newAskDialog builds the dialog for req, answering on reply.
func newAskDialog(req askRequest, reply chan<- askAnswer) *askDialog {
	d := &askDialog{req: req, reply: reply}
	d.modal = &replModal{ask: d}
	return d
}

// askTitleDetail is what the panel's title row says after "Question": who is
// asking, and what about. Empty for an anonymous, headerless question.
func askTitleDetail(req askRequest) string {
	parts := make([]string, 0, 2)
	if req.Requester != "" {
		parts = append(parts, "agent "+req.Requester+" asks")
	}
	if req.Header != "" {
		parts = append(parts, req.Header)
	}
	return strings.Join(parts, " · ")
}

// answer replies once. A second answer, including one a close path produces
// after the user chose, is dropped.
func (d *askDialog) answer(a askAnswer) {
	if d.answered {
		return
	}
	d.answered = true
	if d.reply != nil {
		d.reply <- a
	}
}

// stop is replModal.wipe's hook: a dialog torn down without an answer tells the
// turn the user dismissed it, so nothing waits on a screen that is gone.
func (d *askDialog) stop() {
	d.answer(askAnswer{Outcome: askOutcomeDismissed, Why: "the dialog closed"})
}

// withdraw marks the question answered without replying: the caller stopped
// waiting (its context ended) and only needs the dialog taken down. It reports
// whether the dialog still needed closing.
func (d *askDialog) withdraw() bool {
	if d.answered {
		return false
	}
	d.answered = true
	return true
}

// rowCount is the options plus the free-text row, when it is offered.
func (d *askDialog) rowCount() int {
	n := len(d.req.Options)
	if d.req.Other {
		n++
	}
	return n
}

// rowLabel is the choice at i, or the free-text row's label.
func (d *askDialog) rowLabel(i int) string {
	if i >= len(d.req.Options) {
		return askOtherLabel
	}
	return d.req.Options[i].Label
}

// text renders the question as the panel that rises from the status bar: a
// title row carrying the key hints, the Markdown body, the choices, and the
// selected choice's explanation. maxRows is what the frame gives the panel;
// width is the full terminal width, since the panel docks in the composer's
// place like the setup form.
func (d *askDialog) text(maxRows, modalWidth int) string {
	width := max(20, modalWidth)
	inner := max(8, width-2*formPad)
	d.painted = true
	body := d.bodyRows(inner)
	detail := d.detailRows(inner)
	listRows := d.rowCount()

	// The title row is the last thing to go; the detail pane yields next, then
	// the list windows and the body takes what is left. The free-text editor
	// replaces the list and its explanation with one input row, so the
	// question stays on screen while the user types.
	budget := maxRows - 1
	var detailBlock []string
	listShown := 1
	if d.other {
		d.selected = len(d.req.Options)
	} else {
		if len(detail) > 0 && budget >= len(detail)+2 {
			detailBlock = detail
			budget -= len(detail) + 1
		}
		listShown = min(listRows, max(1, budget))
	}
	budget -= listShown
	bodyShown := 0
	if budget > 1 {
		bodyShown = min(len(body), budget-1, askBodyMaxRows)
	}

	lines := []string{d.titleRow(width)}
	d.bodyShown = 0
	if bodyShown > 0 {
		d.bodyTop = max(0, min(d.bodyTop, len(body)-bodyShown))
		for _, row := range body[d.bodyTop : d.bodyTop+bodyShown] {
			lines = append(lines, askPanelRow(row))
		}
		lines = append(lines, "")
		d.bodyShown = bodyShown
	}
	if d.other {
		d.rowsY, d.rowsShown = len(lines), 1
		lines = append(lines, askPanelRow(d.inputLine()))
	} else {
		d.selected = max(0, min(d.selected, listRows-1))
		d.top = max(0, min(d.top, listRows-listShown))
		if d.selected < d.top {
			d.top = d.selected
		}
		if d.selected >= d.top+listShown {
			d.top = d.selected - listShown + 1
		}
		d.rowsY = len(lines)
		d.rowsShown = min(listShown, listRows-d.top)
		for i := d.top; i < d.top+d.rowsShown; i++ {
			lines = append(lines, askPanelRow(askRowText(i+1, d.rowLabel(i), i == d.selected, inner)))
		}
	}
	if detailBlock != nil {
		lines = append(lines, "")
		for _, row := range detailBlock {
			lines = append(lines, askPanelRow(row))
		}
	}
	if len(lines) > maxRows {
		lines = lines[:maxRows]
	}
	return strings.Join(lines, "\n")
}

// askPanelRow indents one row to the panel's padding, like the setup form's.
// A blank row stays empty.
func askPanelRow(row string) string {
	if row == "" {
		return ""
	}
	return strings.Repeat(" ", formPad) + row
}

// titleRow names the panel and what it is about, with the key hints on the
// right, the way the setup form's title row does.
func (d *askDialog) titleRow(width int) string {
	left := []formSpan{{"Question", "", "bold"}}
	if detail := askTitleDetail(d.req); detail != "" {
		left = append(left, formSpan{"  " + detail, "muted", ""})
	}
	room := max(1, width-2*formPad) - spansWidth(left) - formNoteGap
	for _, variant := range hintVariants(d.hintPairs()) {
		spans, _, _ := hintSpans(variant, false)
		if spansWidth(spans) <= room {
			row, _ := formLine(width, left, spans)
			return row
		}
	}
	row, _ := formLine(width, left, nil)
	return row
}

// hintPairs are the panel's key hints, longest list first for titleRow's
// fit search.
func (d *askDialog) hintPairs() [][2]string {
	if d.other {
		return [][2]string{{"Enter", "answer"}, {"Esc", "back"}}
	}
	pairs := [][2]string{{"↑↓", "select"}}
	if n := d.rowCount(); n <= 9 {
		pairs = append(pairs, [2]string{fmt.Sprintf("1-%d", n), "pick"})
	}
	pairs = append(pairs, [2]string{"Enter", "choose"})
	if len(d.body) > 0 {
		pairs = append(pairs, [2]string{"PgUp/PgDn", "body"})
	}
	return append(pairs, [2]string{"Esc", "dismiss"})
}

// inputLine is the free-text editor's row: the composer's gutter and the
// answer being typed.
func (d *askDialog) inputLine() string {
	if d.modal != nil {
		if value := d.modal.input.text(); value != "" {
			return userGutter() + style.Escape(value)
		}
	}
	return userGutter() + style.Styled("type an answer", "muted", "")
}

// bodyRows renders the question once per inner width. Images stay links: no
// placement composites under a modal.
func (d *askDialog) bodyRows(inner int) []string {
	if d.body == nil || d.bodyWidth != inner {
		d.bodyWidth, d.body = inner, markdown.RenderMutedRows(d.req.Question, inner, nil)
	}
	return d.body
}

// detailRows is the selected choice's explanation, padded to the height the
// longest explanation takes so the rows stay put while the selection moves.
func (d *askDialog) detailRows(inner int) []string {
	height := 0
	for _, option := range d.req.Options {
		if strings.TrimSpace(option.Description) == "" {
			continue
		}
		height = max(height, len(wrapModalText(option.Description, inner)))
	}
	height = min(height, askDetailRows)
	if height == 0 {
		return nil
	}
	out := make([]string, height)
	if d.selected < len(d.req.Options) {
		rows := wrapModalText(d.req.Options[d.selected].Description, inner)
		for i := 0; i < height; i++ {
			row := ""
			if i < len(rows) {
				row = rows[i]
			}
			// A description longer than the pane ends visibly, so a cut
			// sentence does not read as the whole one.
			if i == height-1 && len(rows) > height {
				row = rw.Truncate(row, max(1, inner-1), "") + "…"
			}
			out[i] = style.Styled(row, "muted", "")
		}
	}
	return out
}

// askRowText is one choice row: its number, then the label, the selection in
// the accent role. A label too wide for the dialog is truncated rather than
// clipped by the frame.
func askRowText(number int, label string, selected bool, width int) string {
	cursor, role, mod := "  ", "", ""
	if selected {
		cursor, role, mod = style.Styled("›", "accent", "bold")+" ", "accent", "bold"
	}
	if room := max(4, width-4); rw.StringWidth(label) > room {
		label = rw.Truncate(label, room, "…")
	}
	return cursor + style.Styled(strconv.Itoa(number), "muted", "") + " " + style.Styled(style.Escape(label), role, mod)
}

// handleAskEvent is the dialog's input.
func (r *managedREPL) handleAskEvent(d *askDialog, e ui.Event) bool {
	m := r.model.modal
	if m == nil || m.ask != d {
		return true
	}
	switch e.ID {
	case pasteStartID:
		d.pasting = true
		return true
	case pasteEndID:
		d.pasting = false
		return true
	}
	if d.pasting {
		return true
	}
	if e.Type == ui.MouseEvent {
		r.handleAskMouse(d, e)
		return true
	}
	if !d.painted {
		return true
	}
	if d.other {
		switch e.ID {
		case "<Escape>":
			d.other = false
			m.input.clear()
		case "<Enter>":
			text := strings.TrimSpace(m.input.text())
			if text == "" {
				return true
			}
			d.answer(askAnswer{Outcome: askOutcomeAnswered, Index: len(d.req.Options), Choice: askOtherLabel, Text: text})
			r.closeModal()
		case "<Backspace>", "<C-h>":
			m.input.backspace()
		default:
			if handleModalInputKey(&m.input, e.ID) {
				return true
			}
			if ch, ok := printableRune(e); ok {
				m.input.insert(ch)
			}
		}
		return true
	}
	switch e.ID {
	case "<Up>":
		d.selected = max(0, d.selected-1)
	case "<Down>":
		d.selected = min(d.rowCount()-1, d.selected+1)
	case "<Home>":
		d.selected = 0
	case "<End>":
		d.selected = d.rowCount() - 1
	case "<PageUp>":
		d.bodyTop = max(0, d.bodyTop-max(1, d.bodyShown-1))
	case "<PageDown>":
		d.bodyTop += max(1, d.bodyShown-1)
	case "<Enter>":
		r.chooseAsk(d, d.selected)
	case "<Escape>":
		r.dismissModal()
	default:
		if len(e.ID) == 1 && e.ID[0] >= '1' && e.ID[0] <= '9' {
			if i := int(e.ID[0] - '1'); i < d.rowCount() {
				r.chooseAsk(d, i)
			}
		}
	}
	return true
}

// chooseAsk answers with row i, opening the free-text editor for its row.
func (r *managedREPL) chooseAsk(d *askDialog, i int) {
	if i < 0 || i >= d.rowCount() {
		return
	}
	if i >= len(d.req.Options) {
		d.other = true
		if d.modal != nil {
			d.modal.input.clear()
		}
		return
	}
	d.answer(askAnswer{Outcome: askOutcomeAnswered, Index: i, Choice: d.req.Options[i].Label})
	r.closeModal()
}

// handleAskMouse is the dialog's pointer input: a click chooses, the wheel
// scrolls the body over it and moves the selection below it.
func (r *managedREPL) handleAskMouse(d *askDialog, e ui.Event) {
	mouse, ok := e.Payload.(ui.Mouse)
	if !ok {
		return
	}
	m := r.model.modal
	point := image.Pt(mouse.X, mouse.Y)
	if e.ID == "<MouseLeft>" && !m.bounds.Empty() && !point.In(m.bounds) {
		r.dismissModal()
		return
	}
	// The docked panel has no border: its cells start at its bounds.
	local := point.Sub(m.bounds.Min)
	switch e.ID {
	case "<MouseWheelUp>":
		if d.bodyShown > 0 && local.Y < d.rowsY {
			d.bodyTop = max(0, d.bodyTop-3)
		} else {
			d.selected = max(0, d.selected-3)
		}
	case "<MouseWheelDown>":
		if d.bodyShown > 0 && local.Y < d.rowsY {
			d.bodyTop += 3
		} else {
			d.selected = min(d.rowCount()-1, d.selected+3)
		}
	case "<MouseLeft>":
		if d.other || !d.painted {
			return
		}
		if local.Y >= d.rowsY && local.Y < d.rowsY+d.rowsShown {
			if i := d.top + local.Y - d.rowsY; i >= 0 && i < d.rowCount() {
				d.selected = i
				r.chooseAsk(d, i)
			}
		}
	}
}

// AskUser opens the question on the tab that asked and waits for its answer.
// It runs on the tool goroutine: the event loop owns the model and the screen,
// so the dialog is installed by a posted task and the answer arrives on a
// buffered channel. ctx ending withdraws the dialog and reports a dismissal.
func (t *gotuiTurnUI) AskUser(ctx context.Context, req askRequest) askAnswer {
	if t == nil || t.model == nil || t.repl == nil {
		return askAnswer{Outcome: askOutcomeUnreachable, Why: "this run has no question dialog"}
	}
	if ctx.Err() != nil {
		return askAnswer{Outcome: askOutcomeDismissed, Why: "the turn ended"}
	}
	reply := make(chan askAnswer, 1)
	d := newAskDialog(req, reply)
	opened := t.repl.postUI(ctx, func() {
		m := t.model
		m.mu.Lock()
		defer m.mu.Unlock()
		// A window with the keys gives them to the question.
		if m == t.repl.model {
			t.repl.yieldPanelKeys()
		}
		switch {
		case d.answered:
			// The turn ended before the task ran; the caller has already
			// stopped waiting.
			return
		case !t.acceptingLocked() || m.approvalsClosed:
			reply <- askAnswer{Outcome: askOutcomeUnreachable, Why: "the turn is no longer accepting questions"}
			return
		case m.modal != nil || m.pendingModal != nil:
			reply <- askAnswer{Outcome: askOutcomeBusy, Why: "a dialog is already open on this tab"}
			return
		}
		m.modal, d.shown = d.modal, time.Now()
		detail := "question"
		if req.Requester != "" {
			detail = "question from " + req.Requester
		}
		m.signalHiddenLocked(signalApprovalNeeded, detail)
	})
	if !opened {
		return askAnswer{Outcome: askOutcomeUnreachable, Why: "the workspace is closing"}
	}
	select {
	case answer := <-reply:
		return answer
	case <-ctx.Done():
		// The task queue is still the only safe place to touch the model, and
		// the work context is what decides whether it is still running.
		t.repl.postUI(context.Background(), func() { t.withdrawAsk(d) })
		return askAnswer{Outcome: askOutcomeDismissed, Why: "the turn ended"}
	}
}

// withdrawAsk takes a question off the model once nothing waits for it.
func (t *gotuiTurnUI) withdrawAsk(d *askDialog) {
	m, r := t.model, t.repl
	m.mu.Lock()
	defer m.mu.Unlock()
	if d.withdraw() {
		r.removeAskLocked(m, d)
	}
}

// removeAskLocked takes an answered question's dialog off m, wherever it is:
// on screen, behind another tab, or parked under another dialog. Caller holds
// m.mu.
func (r *managedREPL) removeAskLocked(m *replModel, d *askDialog) {
	switch {
	case m.pendingModal == d.modal:
		m.pendingModal = nil
	case m.modal != d.modal:
		return
	case m == r.model:
		r.closeModal()
	default:
		d.modal.wipe()
		m.modal, m.pendingModal = m.pendingModal, nil
	}
}
