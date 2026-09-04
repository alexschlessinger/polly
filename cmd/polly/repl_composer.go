package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

// The composer: prompt history, search, approval prompts, paste, and submit.

func (m *replModel) setSlashHintLine(hint string) {
	if m.slashHints == hint {
		return
	}
	m.slashHints = hint
	m.visual.invalidate()
}

// promptHistory is the composer's recall state: the accepted inputs, the ↑/↓
// cursor with the draft it displaced, and the reverse-incremental search.
type promptHistory struct {
	entries []string
	idx     int    // entry being recalled, -1 while editing the draft
	draft   string // composer text displaced by the first ↑

	searching bool
	query     string
	match     int // entry of the current search hit, -1 when nothing matches
}

// record appends one accepted input and returns recall to the live draft.
func (h *promptHistory) record(input string) {
	h.idx = -1
	h.draft = ""
	h.entries = append(h.entries, input)
}

// rewrite replaces the newest entry equal to old, so a prompt whose display
// text changed after acceptance recalls in its final form.
func (h *promptHistory) rewrite(old, text string) {
	for i := len(h.entries) - 1; i >= 0; i-- {
		if h.entries[i] == old {
			h.entries[i] = text
			return
		}
	}
}

func (h *promptHistory) up(ed *lineEditor) {
	if len(h.entries) == 0 {
		return
	}
	if h.idx == -1 {
		h.draft = ed.text()
		h.idx = len(h.entries) - 1
	} else if h.idx > 0 {
		h.idx--
	}
	ed.setText(h.entries[h.idx])
}

func (h *promptHistory) down(ed *lineEditor) {
	if h.idx == -1 {
		return
	}
	if h.idx < len(h.entries)-1 {
		h.idx++
		ed.setText(h.entries[h.idx])
	} else {
		h.idx = -1
		ed.setText(h.draft)
	}
}

// historyUp and historyDown recall older and newer inputs into the composer.
// An approval prompt owns the keyboard, so recall is inert while one is up.
func (m *replModel) historyUp() {
	if m.approval == nil {
		m.hist.up(&m.ed)
	}
}

func (m *replModel) historyDown() {
	if m.approval == nil {
		m.hist.down(&m.ed)
	}
}

// startSearch enters reverse-incremental search with an empty query.
func (h *promptHistory) startSearch() {
	h.searching = true
	h.query = ""
	h.match = -1
}

func (h *promptHistory) endSearch() {
	h.searching = false
	h.query = ""
	h.match = -1
}

// searchFrom scans backward from index from, returning the newest entry
// containing query, or -1.
func (h *promptHistory) searchFrom(from int, query string) int {
	if query == "" || from >= len(h.entries) {
		return -1
	}
	for i := from; i >= 0; i-- {
		if strings.Contains(h.entries[i], query) {
			return i
		}
	}
	return -1
}

// searchType extends the query by one rune and re-matches from the newest entry.
func (h *promptHistory) searchType(r rune) {
	h.query += string(r)
	h.match = h.searchFrom(len(h.entries)-1, h.query)
}

// searchBackspace shortens the query by one rune and re-matches.
func (h *promptHistory) searchBackspace() {
	if h.query == "" {
		return
	}
	q := []rune(h.query)
	h.query = string(q[:len(q)-1])
	h.match = h.searchFrom(len(h.entries)-1, h.query)
}

// searchNext steps to the next older match (repeated Ctrl-R). Stays put when
// there's no earlier hit.
func (h *promptHistory) searchNext() {
	start := len(h.entries) - 1
	if h.match >= 0 {
		start = h.match - 1
	}
	if next := h.searchFrom(start, h.query); next >= 0 {
		h.match = next
	}
}

// acceptSearch places the current match into the editor and leaves search
// mode. The text is not submitted; the user can edit it and press Enter.
func (h *promptHistory) acceptSearch(ed *lineEditor) {
	if h.match >= 0 && h.match < len(h.entries) {
		ed.setText(h.entries[h.match])
	}
	h.endSearch()
}

// searchDisplay renders the reverse-i-search prompt and the current match.
func (h *promptHistory) searchDisplay() string {
	matched := ""
	if h.match >= 0 && h.match < len(h.entries) {
		matched = h.entries[h.match]
	}
	prompt := fmt.Sprintf("(reverse-i-search)`%s`: ", h.query)
	return styled(prompt, "accent", "bold") + styleEscape(matched)
}

// handleApprovalAnswer applies one answer to the pending approval batch.
// Returns true when the batch is complete and the reply was sent.
func (m *replModel) handleApprovalAnswer(answer byte) bool {
	a := m.approval
	if a == nil {
		return false
	}
	if a.out == nil {
		a.out = make([]bool, len(a.calls))
	}
	switch answer {
	case 'a':
		for i := a.index; i < len(a.out); i++ {
			a.out[i] = true
		}
		m.finishApproval()
		return true
	case 'y':
		a.out[a.index] = true
		a.index++
		a.viewed = false
	case 'n':
		a.out[a.index] = false
		a.index++
		a.viewed = false
	}
	if a.index >= len(a.out) {
		m.finishApproval()
		return true
	}
	return false
}

// showApprovalArgs expands the current approval candidate's full arguments
// into the transcript as a gutter block, so [v]iew survives scrollback and the
// prompt stays put. No-op when already expanded for this call. Caller must
// hold m.mu.
func (m *replModel) showApprovalArgs() {
	a := m.approval
	if a == nil || a.viewed {
		return
	}
	a.viewed = true
	call := a.calls[a.index]
	var b strings.Builder
	b.WriteString("  " + styled("╭─ "+call.Name, "muted", ""))
	for _, line := range strings.Split(expandToolCall(call), "\n") {
		b.WriteString("\n  " + styled("│ ", "muted", "") + styled(line, "code", ""))
	}
	m.appendLine(b.String())
	m.followBottom = true
}

func (m *replModel) finishApproval() {
	if m.approval == nil {
		return
	}
	out := append([]bool(nil), m.approval.out...)
	m.approval.reply <- out
	close(m.approval.reply)
	m.approval = nil
}

// inputPromptWidth is the visible column count of the "> " prompt prefix (and
// of the matching indent on wrapped continuation lines).
const inputPromptWidth = 2

// maxInputRows caps how tall the editable input region can grow for multi-line
// prompts. Beyond this the region anchors to the bottom so the cursor stays
// visible while composing or after a paste.
const maxInputRows = 8

// inputRows is how many terminal rows the input region currently occupies. The
// approval/search overlays are always single-line; the composer remains visible
// and editable while a turn runs, including in quiet mode.
func (m *replModel) inputRows() int {
	if m.approval != nil || m.hist.searching {
		return 1
	}
	n := 1 + strings.Count(m.ed.text(), "\n")
	if n > maxInputRows {
		n = maxInputRows
	}
	return n
}

// inputCursorRowCol is the cursor's row (0-based, within the full input text)
// and on-screen column, accounting for the prompt/indent prefix and wide runes.
// Caller must hold m.mu.
func (m *replModel) inputCursorRowCol() (row, col int) {
	before := m.ed.buf[:m.ed.cursor]
	lineStart := 0
	for i, r := range before {
		if r == '\n' {
			row++
			lineStart = i + 1
		}
	}
	col = inputPromptWidth + rw.StringWidth(string(before[lineStart:]))
	return row, col
}

// renderInputForTerminal produces the bottom input region: its display text
// (possibly multiple lines), the cursor's row/col within that region, and
// whether the cursor should be shown. The region occupies min(maxRows,
// inputRows()) rows, which frameLayoutFor has already fixed as l.inputRows.
// The busy/approval/search overlays are single-line and hide the cursor; the
// editable prompt may span several rows, anchored to the bottom when it
// overflows maxRows. Caller must hold m.mu.
func (m *replModel) renderInputForTerminal(maxRows, width int) (text string, curRow, curCol int, editable bool) {
	if maxRows < 1 {
		maxRows = 1
	}
	switch {
	case m.hist.searching:
		return m.hist.searchDisplay(), 0, 0, false
	case m.approval != nil:
		return m.approvalPrompt(width), 0, 0, false
	}

	lines := strings.Split(m.ed.text(), "\n")
	cr, cc := m.inputCursorRowCol()

	// Window the visible lines to maxRows. Anchor to the bottom so the
	// newest lines (and the cursor after a paste) stay visible — but if the
	// cursor has moved up above that window (editing higher in a long paste),
	// scroll up just enough to keep the cursor's line on screen.
	start := 0
	if len(lines) > maxRows {
		start = len(lines) - maxRows
		if cr < start {
			start = cr
		}
	}
	visible := lines[start : start+min(maxRows, len(lines)-start)]

	parts := make([]string, len(visible))
	for i, ln := range visible {
		if width > inputPromptWidth {
			contentWidth := width - inputPromptWidth
			if start+i == cr {
				cursorContentCol := cc - inputPromptWidth
				lineStartCol := cursorContentCol - contentWidth
				if lineStartCol < 0 {
					lineStartCol = 0
				}
				var skipped int
				ln, skipped = sliceDisplayWidth(ln, lineStartCol, contentWidth)
				cc = inputPromptWidth + cursorContentCol - skipped
				if cc > width-1 {
					cc = width - 1
				}
				if cc < inputPromptWidth {
					cc = inputPromptWidth
				}
			} else {
				ln, _ = sliceDisplayWidth(ln, 0, contentWidth)
			}
		}
		if start+i == 0 {
			parts[i] = styled("> ", "accent", "bold") + styleEscape(ln)
		} else {
			parts[i] = "  " + styleEscape(ln)
		}
	}

	curRow = cr - start
	if curRow < 0 {
		curRow = 0
	}
	if curRow > len(visible)-1 {
		curRow = len(visible) - 1
	}
	return strings.Join(parts, "\n"), curRow, cc, true
}

func (m *replModel) approvalPrompt(width int) string {
	call := m.approval.calls[m.approval.index]
	prefix := "allow "
	if len(m.approval.calls) > 1 {
		prefix += fmt.Sprintf("(%d/%d) ", m.approval.index+1, len(m.approval.calls))
	}
	actions := "[y]es [N]o [a]ll [v]iew"
	if width > 0 && width < 46 {
		actions = "[y/N/a/v]"
	}
	suffix := "? " + actions
	label := toolLabel(call)
	if width > 0 {
		budget := width - rw.StringWidth(prefix) - rw.StringWidth(suffix)
		if budget < 0 {
			prefix = ""
			budget = width - rw.StringWidth(suffix)
		}
		if budget < 0 {
			// Truncating bracketed action text can leave an unmatched "[";
			// wrapping that fragment in gotui style markup makes the parser expose
			// the markup itself. Render the tiny fallback literally.
			return styleEscape(rw.Truncate(actions, width, "…"))
		}
		if budget == 0 {
			label = ""
		} else {
			label = rw.Truncate(label, budget, "…")
		}
	}
	return styled(prefix, "accent", "bold") +
		styled(label, "muted", "") +
		styled(suffix, "accent", "bold")
}

func sliceDisplayWidth(s string, start, width int) (string, int) {
	if start <= 0 && (width <= 0 || rw.StringWidth(s) <= width) {
		return s, 0
	}
	if width <= 0 {
		return "", 0
	}
	var b strings.Builder
	col := 0
	skipped := 0
	outWidth := 0
	for _, r := range s {
		runeWidth := rw.RuneWidth(r)
		if runeWidth > 0 && col+runeWidth <= start {
			col += runeWidth
			skipped = col
			continue
		}
		if runeWidth > 0 && outWidth+runeWidth > width {
			break
		}
		b.WriteRune(r)
		col += runeWidth
		outWidth += runeWidth
	}
	return b.String(), skipped
}

// busyLabel maps the current turn state to the word shown on the input row.
func (m *replModel) busyLabel() string {
	if m.canceling {
		return "canceling"
	}
	switch m.state {
	case turnStateThinking:
		return "thinking"
	case turnStateStreaming:
		return "streaming"
	case turnStateTool:
		if m.toolName != "" {
			return "running " + m.toolName
		}
		return "running tool"
	default:
		return "waiting"
	}
}

// maxPersistedHistory bounds how many input lines are kept across runs. On
// startup the history file is rewritten to its trailing maxPersistedHistory
// lines, so it never grows without limit.
const maxPersistedHistory = 500

// replHistoryPath is where input history persists. POLLY_HISTORY_FILE overrides
// it (used by tests); otherwise it sits beside the session store under
// ~/.pollytool.
func replHistoryPath() (string, error) {
	if p := os.Getenv("POLLY_HISTORY_FILE"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".pollytool", "repl_history"), nil
}

// loadHistory reads the last maxPersistedHistory non-blank lines from path.
// A missing or unreadable file yields nil — history is always best-effort.
func loadHistory(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" && !attachmentTokenPattern.MatchString(l) {
			lines = append(lines, l)
		}
	}
	if len(lines) > maxPersistedHistory {
		lines = lines[len(lines)-maxPersistedHistory:]
	}
	return lines
}

// initHistory loads prior history into the model and opens an append handle,
// rewriting the file to its trimmed tail so it stays bounded. All failures are
// silent: a REPL with no persisted history is still fully functional.
func (r *managedREPL) initHistory() {
	path, err := replHistoryPath()
	if err != nil {
		return
	}
	r.model.hist.entries = loadHistory(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return
	}
	for _, l := range r.model.hist.entries {
		fmt.Fprintln(f, l)
	}
	r.histFile = f
}

func (r *managedREPL) closeHistory() {
	if r.histFile != nil {
		_ = r.histFile.Close()
		r.histFile = nil
	}
}

// appendHistory persists one submitted prompt. Single-line only — multi-line
// prompts would break the line-per-entry format and are not stored.
func (r *managedREPL) appendHistory(prompt string) {
	if r.histFile == nil || strings.ContainsRune(prompt, '\n') || attachmentTokenPattern.MatchString(prompt) {
		return
	}
	fmt.Fprintln(r.histFile, prompt)
}

// recordAcceptedInput records one accepted input for recall/search and
// best-effort persistence. It does not echo a prompt or start a turn.
func (r *managedREPL) recordAcceptedInput(input string) {
	if input == "" {
		return
	}
	r.model.hist.record(input)
	r.appendHistory(input)
}

// handleSearchKey processes one key while reverse-i-search is active. Enter (or
// any non-editing key) accepts the current match into the editor; Esc/Ctrl-C/
// Ctrl-G cancel; Ctrl-R steps to the next older match; printable runes extend
// the query. Caller must hold m.mu.
func (r *managedREPL) handleSearchKey(e ui.Event) bool {
	m := r.model
	switch e.ID {
	case "<C-c>", "<Escape>", "<C-g>":
		m.hist.endSearch()
	case "<C-r>":
		m.hist.searchNext()
	case "<Enter>":
		m.hist.acceptSearch(&m.ed)
	case "<Backspace>", "<Delete>":
		m.hist.searchBackspace()
	case "<Space>":
		m.hist.searchType(' ')
	default:
		if ch, ok := printableRune(e); ok {
			m.hist.searchType(ch)
			return false
		}
		// Any other key (cursor moves, etc.) accepts the match and exits.
		m.hist.acceptSearch(&m.ed)
	}
	return false
}

// bufferPasted adds one key from a bracketed paste to the paste buffer as
// literal text, mapping newline keys to '\n'. The composer remains available
// while a turn runs, so busy paste behaves exactly like busy typing.
func (m *replModel) bufferPasted(e ui.Event) {
	switch e.ID {
	case "<Enter>", "<C-j>":
		m.pasteBuf = append(m.pasteBuf, '\n')
	case "<Space>":
		m.pasteBuf = append(m.pasteBuf, ' ')
	case "<Tab>":
		m.pasteBuf = append(m.pasteBuf, '\t')
	default:
		if ch, ok := printableRune(e); ok {
			m.pasteBuf = append(m.pasteBuf, ch)
		}
	}
}

// flushPasteBuffer lands a completed bracketed paste. A paste that is nothing
// but existing local image paths — the terminal drag-drop signature — attaches
// those images and leaves "[image #N]" tokens in the composer; any other paste
// is inserted verbatim. Search and approval modes swallow pastes, as they
// swallowed each key before buffering existed.
func (m *replModel) flushPasteBuffer() {
	text := string(m.pasteBuf)
	m.pasteBuf = m.pasteBuf[:0]
	if text == "" || m.approval != nil || m.hist.searching {
		return
	}
	if images := m.pastedImageAttachments(text); len(images) > 0 {
		for _, img := range images {
			m.insertEditorText(m.registerAttachment(img.Path, filepath.Base(img.Path)) + " ")
		}
		return
	}
	m.insertEditorText(text)
}

func (m *replModel) insertEditorText(s string) {
	for _, r := range s {
		m.ed.insert(r)
	}
}

// captureClipboardToComposer reads an image from the system clipboard in the
// background and, on success, attaches it and leaves its "[image #N]" token at
// the cursor. The read happens off the event loop — osascript and friends can
// take a beat — and lands via a UI task. Caller must hold r.model.mu.
func (r *managedREPL) captureClipboardToComposer() {
	// The capture belongs to the tab it started on: by the time the read
	// lands, another tab may be visible, and the image (and the flag reset)
	// must still go to the composer that asked for it.
	m := r.model
	if m.clipboardCapture {
		return
	}
	m.clipboardCapture = true
	go func() {
		dir, err := attachmentCacheDir()
		var path string
		if err == nil {
			path, err = captureClipboardImage(context.Background(), dir)
		}
		r.postUITask(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.clipboardCapture = false
			if err != nil {
				m.appendNoticeLine("clipboard: " + err.Error())
				return
			}
			img, ok := resolveLocalTranscriptImage(path, "clipboard image", m.imageBaseDir)
			if !ok {
				m.appendNoticeLine("clipboard: captured image could not be used")
				return
			}
			m.insertEditorText(m.registerAttachment(img.Path, "clipboard image") + " ")
		})
	}()
}

func (m *replModel) denyApprovalLocked() {
	if m.approval == nil {
		return
	}
	denied := make([]bool, len(m.approval.calls))
	select {
	case m.approval.reply <- denied:
	default:
	}
	close(m.approval.reply)
	m.approval = nil
}

// runComposerCommandLocked submits a slash command typed into the composer and
// reports whether it asked the REPL to quit. Caller must hold m.mu.
func (r *managedREPL) runComposerCommandLocked(trimmed string) bool {
	m := r.model
	m.ed.clear()
	r.recordAcceptedInput(trimmed)
	m.followBottom = true
	handled, quit := r.runCommand(trimmed)
	if quit {
		r.requestQuit()
		return true
	}
	if !handled {
		m.appendNoticeLine(defaultReplCommands.unknownCommandNotice(trimmed))
	}
	return false
}

// submitComposerLocked handles Enter on the composer. Only a single-line "/…"
// is a command; a multi-line prompt that happens to start with "/" is real
// input. While a turn runs, busy-safe commands still execute and everything
// else queues behind it. Reports whether the REPL should quit. Caller must
// hold m.mu.
func (r *managedREPL) submitComposerLocked() bool {
	m := r.model
	trimmed := strings.TrimSpace(m.ed.text())
	if trimmed == "" {
		return false
	}
	if r.quitting {
		m.appendNoticeLine("leaving; input not sent")
		return false
	}
	if m.clipboardCapture {
		m.appendNoticeLine("clipboard: waiting for image capture")
		m.followBottom = true
		return false
	}
	if (m.busy || r.opening != "") && defaultReplCommands.busySafeCommand(trimmed) {
		return r.runComposerCommandLocked(trimmed)
	}
	if r.opening != "" {
		// The draft stays put: it can go to the new tab once it is live.
		m.appendNoticeLine("opening " + r.opening + "; input held until it opens")
		m.followBottom = true
		return false
	}
	isCommand := !strings.Contains(trimmed, "\n") && strings.HasPrefix(trimmed, "/")
	if m.busy {
		r.queueComposerInputLocked(trimmed, isCommand)
		return false
	}
	if isCommand {
		return r.runComposerCommandLocked(trimmed)
	}
	r.submitComposerTurnLocked(trimmed)
	return false
}

// queueComposerInputLocked parks input behind the running turn. Commands
// remain text-only, while prompts are fully prepared before joining the queue.
func (r *managedREPL) queueComposerInputLocked(trimmed string, isCommand bool) {
	m := r.model
	queued := queuedREPLInput{text: trimmed}
	if !isCommand {
		turn, err := r.prepareManagedTurnLocked(trimmed)
		if err != nil {
			m.appendErrorLine(err.Error())
			return
		}
		queued.turn = &turn
	}
	m.ed.clear()
	r.recordAcceptedInput(trimmed)
	m.queue = append(m.queue, queued)
	m.appendQueuedInput(&m.queue[len(m.queue)-1])
}

// submitComposerTurnLocked starts a turn from the idle composer. A restored
// draft resubmitted unchanged reuses its already-persisted user message.
func (r *managedREPL) submitComposerTurnLocked(trimmed string) {
	m := r.model
	turn, restoredPersistence, reuseRestored := m.acceptedRestoredTurn(trimmed)
	if !reuseRestored {
		var err error
		turn, err = r.prepareManagedTurnLocked(trimmed)
		if err != nil {
			m.appendErrorLine(err.Error())
			return
		}
	}
	select {
	case r.pending <- pendingTurn{model: m, turn: turn}:
		m.ed.clear()
		r.recordAcceptedInput(trimmed)
		m.currentPersistence = restoredPersistence
		m.restoreDraftNext = reuseRestored
		m.clearRestoredDraft()
		m.beginManagedTurn(turn)
	default:
		m.appendErrorLine("turn queue is unavailable")
	}
}

// printableRune returns the single printable rune a keyboard event carries.
// Any multi-character event ID — bracketed key names like "<F1>" as well as
// gotui's bare "Unknown_Mouse_Button" — and any control rune is rejected, so
// stray events never get typed into an input.
func printableRune(e ui.Event) (rune, bool) {
	if e.Type != ui.KeyboardEvent {
		return 0, false
	}
	runes := []rune(e.ID)
	if len(runes) != 1 || runes[0] < 0x20 {
		return 0, false
	}
	return runes[0], true
}
