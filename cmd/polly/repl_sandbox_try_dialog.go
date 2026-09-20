package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	tcell "github.com/gdamore/tcell/v3"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

// The /sandbox try dialog shows a trial running, then its review, in the
// modal frame the model form uses. Only the user answers it, and a key
// typed ahead allows nothing: keys count once the review they answer is on
// screen, pasted text never counts, and the button Enter presses starts on
// Cancel.

var sandboxTryButtons = []string{"Try again", "Save to profile", "This session", "Cancel"}

const (
	sandboxTryAgainButton = iota
	sandboxTrySaveButton
	sandboxTrySessionButton
	sandboxTryCancelButton
)

// sandboxTryDetailRows bounds the explanation of the selected row.
const sandboxTryDetailRows = 4

type sandboxTryDialog struct {
	modal *replModal
	try   *sandboxTry
	// running is set while a trial runs off the event loop, which reads
	// nothing of try then; cancel stops the trial. runningTrial and
	// runningWith describe it.
	running                   bool
	cancel                    context.CancelFunc
	runningTrial, runningWith int
	// status is the line under the buttons: what the last action did, or why
	// it did nothing.
	status, statusRole string
	selected, top      int
	focus              int
	// output shows the last trial's output instead of the rows, wrapped
	// once per trial and width into outputRows.
	output                   bool
	outputTop                int
	outputRows               []string
	outputTrial, outputWidth int
	pasting                  bool
	// revision counts the reviews the dialog has shown, and painted is the
	// one the last frame drew.
	revision, painted int
	// Where the last frame drew the rows and the buttons, in the dialog's
	// inner area, for the mouse.
	rowsY, rowsShown, buttonsY int
	buttonsX                   [][2]int
}

// openSandboxTry opens the /sandbox try dialog on try and runs its first
// trial.
func (r *managedREPL) openSandboxTry(try *sandboxTry) {
	m := r.model
	d := &sandboxTryDialog{try: try, focus: sandboxTryCancelButton}
	d.modal = &replModal{
		title:      "Sandbox try",
		width:      100,
		sandboxTry: d,
		onCancel:   func() { m.appendNoticeLine("sandbox try: nothing allowed") },
	}
	r.openModal(d.modal)
	r.runSandboxTrial(d)
}

// openSandboxTryPicker lets the user choose which of the session's failed
// commands to try.
func (r *managedREPL) openSandboxTryPicker(commands []string) {
	items := make([]replModalItem, len(commands))
	for i, command := range commands {
		items[i] = replModalItem{label: commandLine(command), value: fmt.Sprint(i)}
	}
	state := r.state
	r.openModal(&replModal{
		title: "Try a failed command",
		width: 100,
		items: items,
		onSubmit: func(value string) {
			var i int
			if _, err := fmt.Sscan(value, &i); err != nil || i < 0 || i >= len(commands) {
				return
			}
			try, err := newSandboxTry(state, commands[i])
			if err != nil {
				r.model.appendNoticeLine("sandbox try: " + err.Error())
				return
			}
			r.openSandboxTry(try)
		},
	})
}

// runSandboxTrial runs the dialog's next trial off the event loop, which
// commands and keys run on, and shows its review when it ends. Closing the
// dialog stops it.
func (r *managedREPL) runSandboxTrial(d *sandboxTryDialog) {
	m := r.model
	parent := context.Background()
	if r.state != nil && r.state.session != nil {
		parent = r.state.session.Context()
	}
	ctx, cancel := context.WithCancel(parent)
	d.running, d.cancel, d.output = true, cancel, false
	d.runningTrial, d.runningWith = len(d.try.trials)+1, d.try.ticked()
	d.setStatus("", "")
	started := r.background(func() {
		stop := context.AfterFunc(r.work.ctx, cancel)
		defer stop()
		defer cancel()
		dropped, err := d.try.trial(ctx)
		r.postUI(r.work.ctx, func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.modal == nil || m.modal.sandboxTry != d {
				return
			}
			d.finish(dropped, err)
		})
	})
	if !started {
		cancel()
		d.finish(nil, errors.New("the workspace is closing"))
	}
}

// finish shows the review of the trial that just ended, or why it did not
// run.
func (d *sandboxTryDialog) finish(dropped []*sandboxProposal, err error) {
	d.running, d.cancel = false, nil
	d.revision++
	d.selected = max(0, min(d.selected, len(d.try.rows)-1))
	switch {
	case err != nil:
		d.setStatus("the trial did not run: "+err.Error(), "err")
	case len(dropped) > 0:
		names := make([]string, len(dropped))
		for i, p := range dropped {
			names[i] = p.label()
		}
		d.setStatus("unticked, since the rules now refuse them: "+strings.Join(names, ", "), "err")
	}
}

// stop ends a running trial; closing the dialog calls it.
func (d *sandboxTryDialog) stop() {
	if d.cancel != nil {
		d.cancel()
	}
}

func (d *sandboxTryDialog) setStatus(text, role string) {
	d.status, d.statusRole = text, role
}

// handleSandboxTryEvent is the dialog's input.
func (r *managedREPL) handleSandboxTryEvent(d *sandboxTryDialog, e ui.Event) bool {
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
		r.handleSandboxTryMouse(d, e)
		return true
	}
	if e.ID == "<Escape>" {
		if d.output {
			d.output = false
		} else {
			r.dismissModal()
		}
		return true
	}
	if d.running || d.painted != d.revision {
		return true
	}
	// gotui does not name legacy Backtab events; advanced terminals report Shift-Tab.
	if key, ok := e.Payload.(*tcell.EventKey); ok && (key.Key() == tcell.KeyBacktab || key.Key() == tcell.KeyTab && key.Modifiers()&tcell.ModShift != 0) {
		e.ID = "<S-Tab>"
	}
	if d.output {
		switch e.ID {
		case "<Up>":
			d.outputTop = max(0, d.outputTop-1)
		case "<Down>":
			d.outputTop++
		case "<PageUp>":
			d.outputTop = max(0, d.outputTop-10)
		case "<PageDown>":
			d.outputTop += 10
		case "<Home>":
			d.outputTop = 0
		case "<End>":
			d.outputTop = 1 << 30
		case "o":
			d.output = false
		}
		return true
	}
	last := max(0, len(d.try.rows)-1)
	switch e.ID {
	case "<Up>":
		d.selected = max(0, d.selected-1)
	case "<Down>":
		d.selected = min(last, d.selected+1)
	case "<PageUp>":
		d.selected = max(0, d.selected-max(1, d.rowsShown-1))
	case "<PageDown>":
		d.selected = min(last, d.selected+max(1, d.rowsShown-1))
	case "<Home>":
		d.selected = 0
	case "<End>":
		d.selected = last
	case " ", "<Space>":
		d.toggle(d.selected)
	case "<Tab>", "<Right>":
		d.focus = (d.focus + 1) % len(sandboxTryButtons)
	case "<S-Tab>", "<Left>":
		d.focus = (d.focus + len(sandboxTryButtons) - 1) % len(sandboxTryButtons)
	case "<Enter>":
		r.pressSandboxTryButton(d, d.focus)
	case "a":
		n := d.try.tickReads()
		d.setStatus(fmt.Sprintf("ticked %d %s", n, pluralWord(n, "read", "reads")), "muted")
	case "o":
		d.output, d.outputTop = true, 1<<30
	}
	return true
}

func (d *sandboxTryDialog) toggle(i int) {
	if err := d.try.toggle(i); err != nil {
		d.setStatus(err.Error(), "err")
		return
	}
	d.setStatus("", "")
}

func (r *managedREPL) handleSandboxTryMouse(d *sandboxTryDialog, e ui.Event) {
	mouse, ok := e.Payload.(ui.Mouse)
	if !ok || d.running {
		return
	}
	switch e.ID {
	case "<MouseWheelUp>":
		if d.output {
			d.outputTop = max(0, d.outputTop-3)
		} else {
			d.selected = max(0, d.selected-3)
		}
	case "<MouseWheelDown>":
		if d.output {
			d.outputTop += 3
		} else {
			d.selected = min(max(0, len(d.try.rows)-1), d.selected+3)
		}
	case "<MouseLeft>":
		if d.painted != d.revision {
			return
		}
		local := image.Pt(mouse.X, mouse.Y).Sub(d.modal.bounds.Min.Add(image.Pt(1, 1)))
		if local.Y == d.buttonsY {
			for i, span := range d.buttonsX {
				if local.X >= span[0] && local.X < span[1] {
					r.pressSandboxTryButton(d, i)
					return
				}
			}
		}
		if !d.output && local.Y >= d.rowsY && local.Y < d.rowsY+d.rowsShown {
			d.selected = d.top + local.Y - d.rowsY
			d.toggle(d.selected)
		}
	}
}

// pressSandboxTryButton runs one of the dialog's buttons. Allowing waits
// for an idle session, since a turn reads the profile while it runs.
func (r *managedREPL) pressSandboxTryButton(d *sandboxTryDialog, button int) {
	d.focus = button
	switch button {
	case sandboxTryAgainButton:
		r.runSandboxTrial(d)
	case sandboxTrySaveButton, sandboxTrySessionButton:
		if r.model.busy {
			d.setStatus("a turn is running; allow once it ends", "err")
			return
		}
		lines, err := d.try.allow(button == sandboxTrySaveButton)
		if err != nil {
			d.setStatus(err.Error(), "err")
			return
		}
		r.closeModal()
		for _, line := range lines {
			r.model.appendNoticeLine(line)
		}
		r.refreshSandboxPosture()
	case sandboxTryCancelButton:
		r.dismissModal()
	}
}

// text renders the dialog for a frame of maxRows rows at modalWidth.
func (d *sandboxTryDialog) text(maxRows, modalWidth int) string {
	inner := max(20, modalWidth-2)
	lines := []string{style.Styled("$ ", "muted", "") + style.Styled(rw.Truncate(commandLine(d.try.command), inner-2, "…"), "", "bold")}
	if d.running {
		status := fmt.Sprintf("trial %d running", d.runningTrial)
		if d.runningWith > 0 {
			status += fmt.Sprintf(" with %d ticked %s", d.runningWith, pluralWord(d.runningWith, "item", "items"))
		}
		lines = append(lines, style.Styled(status+"…", "muted", ""), "", centeredModalHelper("Esc stop and close", modalWidth))
		d.rowsShown, d.buttonsX = 0, nil
		return strings.Join(lines, "\n")
	}
	d.painted = d.revision
	if n := len(d.try.trials); n > 0 {
		lines = append(lines, style.Styled(d.try.trials[n-1].summary(n), "muted", ""))
	}
	for _, note := range d.try.notes() {
		for _, row := range wrapModalText(note, inner) {
			lines = append(lines, style.Styled(row, "muted", ""))
		}
	}
	lines = append(lines, "")
	footer := d.footer(modalWidth)
	// The rows get what the header, the explanation and the footer leave.
	room := max(1, maxRows-len(lines)-sandboxTryDetailRows-1-len(footer))
	d.rowsY = len(lines)
	if d.output {
		lines = append(lines, d.outputText(room, inner)...)
	} else {
		lines = append(lines, d.rowsText(room, inner)...)
		lines = append(lines, "")
		lines = append(lines, d.detailText(inner)...)
	}
	lines = append(lines, "")
	d.buttonsY = len(lines)
	return strings.Join(append(lines, footer...), "\n")
}

// rowsText renders the window of rows around the selection and records
// where for the mouse.
func (d *sandboxTryDialog) rowsText(room, inner int) []string {
	rows := d.try.rows
	if len(rows) == 0 {
		d.rowsShown = 0
		if len(d.try.trials) == 0 {
			return nil
		}
		return []string{style.Styled("The sandbox denied the command nothing polly could see.", "muted", "")}
	}
	d.selected = max(0, min(d.selected, len(rows)-1))
	d.top = max(0, min(d.top, len(rows)-room))
	if d.selected < d.top {
		d.top = d.selected
	}
	if d.selected >= d.top+room {
		d.top = d.selected - room + 1
	}
	end := min(len(rows), d.top+room)
	d.rowsShown = end - d.top
	out := make([]string, 0, d.rowsShown)
	for i := d.top; i < end; i++ {
		out = append(out, sandboxTryRowText(rows[i], i == d.selected, inner))
	}
	return out
}

func sandboxTryRowText(p *sandboxProposal, selected bool, inner int) string {
	cursor := "  "
	if selected {
		cursor = style.Styled("›", "accent", "bold") + " "
	}
	box := style.Styled("[ ]", "muted", "")
	switch {
	case p.refused != "":
		box = style.Styled(" – ", "muted", "")
	case p.ticked:
		box = style.Styled("[x]", "accent", "bold")
	}
	badge := p.badge()
	width := max(8, inner-2-4-9-rw.StringWidth(badge)-2)
	path := homeRelativePath(p.path)
	if rw.StringWidth(path) > width {
		path = rw.TruncatePrefix(path, width, "…")
	}
	role, mod := "", ""
	switch {
	case p.refused != "":
		role = "muted"
	case selected:
		role, mod = "accent", "bold"
	}
	line := cursor + box + " " + style.Styled(fmt.Sprintf("%-8s", p.kind), "muted", "") + " " + style.Styled(rw.FillRight(path, width), role, mod)
	if badge != "" {
		line += "  " + style.Styled(badge, sandboxTryBadgeRole(p), "")
	}
	return line
}

func sandboxTryBadgeRole(p *sandboxProposal) string {
	switch {
	case p.refused != "":
		return "muted"
	case p.tried > 0 && p.tried == p.lastSeen:
		return "err"
	case p.cleared:
		return "ok"
	case p.credential:
		return "active"
	}
	return "muted"
}

// detailText explains the selected row, padded to the height the longest
// explanation takes, so the buttons stay put while the selection moves.
func (d *sandboxTryDialog) detailText(inner int) []string {
	if d.selected >= len(d.try.rows) {
		return nil
	}
	height := 0
	for _, p := range d.try.rows {
		height = max(height, len(sandboxTryDetailRowsOf(p, inner)))
	}
	out := sandboxTryDetailRowsOf(d.try.rows[d.selected], inner)
	for len(out) < height {
		out = append(out, "")
	}
	return out
}

// sandboxTryDetailRowsOf is a row's explanation wrapped to inner, at most
// sandboxTryDetailRows rows, the warnings in the active role.
func sandboxTryDetailRowsOf(p *sandboxProposal, inner int) []string {
	var out []string
	for _, line := range p.details() {
		role := "muted"
		if strings.HasPrefix(line, "A credential") || strings.HasPrefix(line, "Sandboxed commands could") {
			role = "active"
		}
		for _, row := range wrapModalText(line, inner) {
			if len(out) == sandboxTryDetailRows {
				return out
			}
			out = append(out, style.Styled(row, role, ""))
		}
	}
	return out
}

// outputText shows the window of the last trial's output that outputTop
// starts, clamped so the tail fills the room.
func (d *sandboxTryDialog) outputText(room, inner int) []string {
	d.rowsShown = 0
	n := len(d.try.trials)
	if n == 0 {
		return nil
	}
	output := d.try.trials[n-1].result.Output
	if output == "" {
		return []string{style.Styled("The command printed nothing.", "muted", "")}
	}
	if d.outputTrial != n || d.outputWidth != inner {
		d.outputRows = d.outputRows[:0]
		for _, line := range strings.Split(output, "\n") {
			d.outputRows = append(d.outputRows, wrapModalText(line, inner)...)
		}
		d.outputTrial, d.outputWidth = n, inner
	}
	rows := d.outputRows
	d.outputTop = max(0, min(d.outputTop, len(rows)-room))
	end := min(len(rows), d.outputTop+room)
	out := make([]string, 0, end-d.outputTop)
	for _, row := range rows[d.outputTop:end] {
		out = append(out, style.Styled(row, "", ""))
	}
	return out
}

// footer renders the buttons, the key help and the status, recording where
// each button is for the mouse. The focused button is marked like the
// model form's Apply.
func (d *sandboxTryDialog) footer(modalWidth int) []string {
	var buttons strings.Builder
	d.buttonsX = d.buttonsX[:0]
	x := 0
	for i, label := range sandboxTryButtons {
		marker, role, mod := "  ", "", ""
		if i == d.focus {
			marker, role, mod = "› ", "accent", "bold"
		}
		button := "[ " + label + " ]"
		buttons.WriteString(style.Styled(marker, "accent", "bold") + style.Styled(button, role, mod))
		x += 2
		d.buttonsX = append(d.buttonsX, [2]int{x, x + rw.StringWidth(button)})
		x += rw.StringWidth(button)
	}
	help := "Space tick · a tick reads · ↑↓ select · Tab button · Enter press · o output · Esc cancel"
	if d.output {
		help = "↑↓ scroll · o or Esc back to the rows"
	}
	lines := []string{buttons.String(), centeredModalHelper(help, modalWidth)}
	if d.status != "" {
		for _, row := range wrapModalText(d.status, max(20, modalWidth-2)) {
			lines = append(lines, style.Styled(row, d.statusRole, ""))
		}
	}
	return lines
}

// wrapModalText wraps plain text to width the way modal details wrap.
func wrapModalText(text string, width int) []string {
	var rows []string
	for _, row := range style.VisualRows(style.Escape(text), ui.StyleClear, max(1, width)) {
		rows = append(rows, ui.CellsToString(row))
	}
	if len(rows) == 0 {
		rows = []string{""}
	}
	return rows
}

// commandLine shows a command on one line.
func commandLine(command string) string {
	return strings.ReplaceAll(strings.TrimSpace(command), "\n", " ⏎ ")
}
