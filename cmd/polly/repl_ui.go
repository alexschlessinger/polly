package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
	"github.com/metaspartan/gotui/v5/widgets"
	"golang.org/x/term"
)

// turnState describes what the agent is currently doing, for the status bar.
type turnState int

const (
	turnStateIdle turnState = iota
	turnStateWaiting
	turnStateThinking
	turnStateStreaming
	turnStateTool
	turnStateError
)

func (s turnState) label(toolName string) string {
	switch s {
	case turnStateIdle:
		return "idle"
	case turnStateWaiting:
		return "waiting"
	case turnStateThinking:
		return "thinking"
	case turnStateStreaming:
		return "streaming"
	case turnStateTool:
		if toolName == "" {
			return "tool"
		}
		return "tool: " + toolName
	case turnStateError:
		return "error"
	}
	return ""
}

// styleEscape neutralizes the only sequence gotui's ParseStyles treats as
// style markup: a "[...](...)" run. gotui has no backslash escape — balanced
// brackets already render literally via its nesting counter — so we just break
// a "](" adjacency (e.g. a markdown link in model output) with a zero-width
// space, leaving every other character untouched. Adding backslashes would be
// wrong: gotui renders them verbatim.
func styleEscape(s string) string {
	return strings.ReplaceAll(s, "](", "]\u200b(")
}

// styled wraps text in gotui's inline style markup. Color names come from
// gotui's StyleParserColorMap; empty fg/modifier means no styling. The text is
// run through styleEscape — callers don't need to pre-sanitize.
func styled(text, fg, modifier string) string {
	if text == "" {
		return ""
	}
	text = styleEscape(text)
	parts := []string{}
	if fg != "" {
		parts = append(parts, "fg:"+fg)
	}
	if modifier != "" {
		parts = append(parts, "mod:"+modifier)
	}
	if len(parts) == 0 {
		return text
	}
	return "[" + text + "](" + strings.Join(parts, ",") + ")"
}

// lineEditor is a single-line rune buffer with a cursor and readline-style
// editing operations. It owns no terminal state and performs no rendering —
// the REPL feeds it discrete key events and reads back text/cursor for display.
type lineEditor struct {
	buf    []rune
	cursor int
}

func (e *lineEditor) text() string { return string(e.buf) }
func (e *lineEditor) empty() bool  { return len(e.buf) == 0 }

func (e *lineEditor) setText(s string) {
	e.buf = []rune(s)
	e.cursor = len(e.buf)
}

func (e *lineEditor) clear() {
	e.buf = nil
	e.cursor = 0
}

func (e *lineEditor) insert(r rune) {
	e.buf = append(e.buf[:e.cursor], append([]rune{r}, e.buf[e.cursor:]...)...)
	e.cursor++
}

func (e *lineEditor) backspace() {
	if e.cursor > 0 {
		e.buf = append(e.buf[:e.cursor-1], e.buf[e.cursor:]...)
		e.cursor--
	}
}

func (e *lineEditor) left() {
	if e.cursor > 0 {
		e.cursor--
	}
}

func (e *lineEditor) right() {
	if e.cursor < len(e.buf) {
		e.cursor++
	}
}

func (e *lineEditor) home() { e.cursor = 0 }
func (e *lineEditor) end()  { e.cursor = len(e.buf) }

func (e *lineEditor) killToStart() {
	e.buf = append([]rune(nil), e.buf[e.cursor:]...)
	e.cursor = 0
}

func (e *lineEditor) killToEnd() {
	e.buf = append([]rune(nil), e.buf[:e.cursor]...)
}

// prevWordStart is the index where the word before the cursor begins: skip any
// whitespace to the left, then skip the run of non-whitespace.
func (e *lineEditor) prevWordStart() int {
	i := e.cursor
	for i > 0 && unicode.IsSpace(e.buf[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(e.buf[i-1]) {
		i--
	}
	return i
}

// nextWordEnd is the index just past the word after the cursor: skip any
// whitespace to the right, then skip the run of non-whitespace.
func (e *lineEditor) nextWordEnd() int {
	i, n := e.cursor, len(e.buf)
	for i < n && unicode.IsSpace(e.buf[i]) {
		i++
	}
	for i < n && !unicode.IsSpace(e.buf[i]) {
		i++
	}
	return i
}

func (e *lineEditor) wordLeft()  { e.cursor = e.prevWordStart() }
func (e *lineEditor) wordRight() { e.cursor = e.nextWordEnd() }

func (e *lineEditor) deleteWordBackward() {
	start := e.prevWordStart()
	if start == e.cursor {
		return
	}
	e.buf = append(e.buf[:start], e.buf[e.cursor:]...)
	e.cursor = start
}

func (e *lineEditor) deleteWordForward() {
	end := e.nextWordEnd()
	if end == e.cursor {
		return
	}
	e.buf = append(e.buf[:e.cursor], e.buf[end:]...)
}

// displayWidthToCursor is the terminal column count of the text left of the
// cursor, accounting for wide runes.
func (e *lineEditor) displayWidthToCursor() int {
	return rw.StringWidth(string(e.buf[:e.cursor]))
}

// replModel is the mutex-protected state for the MVP TUI. Mutated from both
// the main event loop and any in-flight turn goroutine, so every read/write
// holds mu.
type replModel struct {
	mu sync.Mutex

	// transcript is the accumulated text rendered into the upper pane.
	// Each entry is a logical "block" (user prompt, assistant turn, notice,
	// tool line) and may contain inline style markup. They get joined with
	// "\n" at render time.
	transcript []string

	// currentAssistant points at the entry that the agent is currently
	// streaming into, or -1 when no streaming entry exists.
	currentAssistant int

	ed           lineEditor
	busy         bool
	canceling    bool
	approval     *approvalState
	history      []string
	historyIdx   int
	historyDraft string

	// Reverse-incremental history search (Ctrl-R). searchMatch is the index
	// into history of the current hit, or -1 when the query matches nothing.
	searching   bool
	searchQuery string
	searchMatch int

	// Scrollback. When followBottom is true, the render trims to the most
	// recent lines that fit. When false, scrollAnchor names the absolute
	// transcript-line index of the top visible row.
	followBottom bool
	scrollAnchor int

	// Status bar fields. modelName/contextName/toolCount/skillCount are
	// set once at startup; the rest are mutated as a turn progresses.
	modelName   string
	contextName string
	toolCount   int
	skillCount  int
	quiet       bool

	state       turnState
	toolName    string
	turnStarted time.Time
	lastIn      int
	lastOut     int
}

type approvalState struct {
	calls []messages.ChatMessageToolCall
	index int
	out   []bool
	reply chan []bool
}

func newReplModel() *replModel {
	return &replModel{
		currentAssistant: -1,
		historyIdx:       -1,
		state:            turnStateIdle,
		followBottom:     true,
	}
}

// statusRow renders the bar contents at the given terminal width. Drops
// low-priority fields when they don't fit. Returns "" when the user asked
// for quiet mode.
func (m *replModel) statusRow(width int) string {
	if m.quiet {
		return ""
	}
	const sep = " · "

	type field struct {
		drop int // higher = dropped sooner
		text string
	}

	stateText := m.state.label(m.toolName)
	tokens := fmt.Sprintf("%s→%s", humanizeTokens(m.lastIn), humanizeTokens(m.lastOut))

	fields := []field{}
	if m.modelName != "" {
		fields = append(fields, field{drop: 4, text: m.modelName})
	}
	fields = append(fields, field{drop: 0, text: m.contextName})
	fields = append(fields, field{drop: 0, text: stateText})
	if !m.turnStarted.IsZero() {
		fields = append(fields, field{drop: 3, text: formatElapsed(time.Since(m.turnStarted))})
	}
	fields = append(fields, field{drop: 0, text: tokens})
	if m.toolCount > 0 {
		fields = append(fields, field{drop: 2, text: fmt.Sprintf("tools:%d", m.toolCount)})
	}
	if m.skillCount > 0 {
		fields = append(fields, field{drop: 1, text: fmt.Sprintf("skills:%d", m.skillCount)})
	}

	visibleLen := func(fs []field) int {
		n := 2 // leading + trailing space
		for i, f := range fs {
			n += len([]rune(f.text))
			if i < len(fs)-1 {
				n += len([]rune(sep))
			}
		}
		return n
	}

	for visibleLen(fields) > width && len(fields) > 0 {
		idx := -1
		best := 0
		for i, f := range fields {
			if f.drop > best {
				best = f.drop
				idx = i
			}
		}
		if idx < 0 {
			break
		}
		fields = append(fields[:idx], fields[idx+1:]...)
	}

	parts := make([]string, len(fields))
	for i, f := range fields {
		if f.text == stateText && stateText != "" && stateText != "idle" {
			color := "yellow"
			if m.state == turnStateError {
				color = "red"
			}
			parts[i] = styled(f.text, color, "bold")
			continue
		}
		parts[i] = styled(f.text, "grey", "")
	}
	return " " + strings.Join(parts, styled(sep, "grey", "")) + " "
}

func humanizeTokens(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 100_000:
		whole := n / 1000
		frac := (n % 1000) / 100
		return fmt.Sprintf("%d.%dk", whole, frac)
	case n < 1_000_000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		whole := n / 1_000_000
		frac := (n % 1_000_000) / 100_000
		return fmt.Sprintf("%d.%dM", whole, frac)
	}
}

func formatElapsed(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	m := int(d / time.Minute)
	s := int((d % time.Minute) / time.Second)
	return fmt.Sprintf("%dm%02ds", m, s)
}

// appendLine appends a pre-rendered transcript entry (may contain inline
// style markup). Resets the streaming assistant cursor.
func (m *replModel) appendLine(s string) {
	m.transcript = append(m.transcript, s)
	m.currentAssistant = -1
}

// appendAssistant accumulates streamed model output into the current
// assistant entry. Text is bracket-escaped so any '[' or ']' in the model's
// response don't trip the style parser.
func (m *replModel) appendAssistant(text string) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	escaped := styleEscape(text)
	if m.currentAssistant >= 0 && m.currentAssistant < len(m.transcript) {
		m.transcript[m.currentAssistant] += escaped
		return
	}
	m.transcript = append(m.transcript, escaped)
	m.currentAssistant = len(m.transcript) - 1
}

func (m *replModel) appendUserPrompt(p string) {
	m.appendLine(styled("> ", "blue", "bold") + styleEscape(p))
}

func (m *replModel) appendToolStartLine(label string) {
	m.appendLine("  " + styled("→", "cyan", "") + " " + styled(label, "grey", ""))
}

func (m *replModel) appendToolEndLine(kind transcriptKind, label, duration, errText string) {
	switch kind {
	case transcriptToolOK:
		body := strings.TrimSpace(duration + " " + label)
		m.appendLine("  " + styled("✓", "green", "bold") + " " + styled(body, "grey", ""))
	case transcriptToolErr:
		body := strings.TrimSpace(duration + " " + label)
		line := "  " + styled("✗", "red", "bold") + " " + styled(body, "grey", "")
		if errText != "" {
			line += " " + styled("- "+errText, "red", "")
		}
		m.appendLine(line)
	case transcriptToolDenied:
		m.appendLine("  " + styled("✗", "red", "bold") + " " + styled("denied "+label, "grey", ""))
	}
}

func (m *replModel) appendNoticeLine(text string) {
	m.appendLine(styled(text, "grey", ""))
}

// slashCommands are the REPL meta-commands recognized at the prompt. Used by
// Tab completion; /help documents them and the Enter handler dispatches them.
var slashCommands = []string{"/clear", "/context", "/exit", "/help", "/quit", "/skills", "/stats", "/tools"}

// completeSlash attempts Tab completion of a slash command. Given the current
// input line it returns the text the input should become — extended to the
// longest common prefix of all matches, or the sole match — and the matching
// commands. ok is false when completion doesn't apply (the line isn't a bare
// "/command" token) or nothing matches, in which case the caller falls back to
// inserting a literal tab.
func completeSlash(input string) (completed string, matches []string, ok bool) {
	if !strings.HasPrefix(input, "/") || strings.ContainsAny(input, " \t") {
		return "", nil, false
	}
	for _, c := range slashCommands {
		if strings.HasPrefix(c, input) {
			matches = append(matches, c)
		}
	}
	if len(matches) == 0 {
		return "", nil, false
	}
	return longestCommonPrefix(matches), matches, true
}

// longestCommonPrefix returns the longest leading string shared by every
// element. Slash commands are ASCII, so byte-slicing the prefix is safe.
func longestCommonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	prefix := ss[0]
	for _, s := range ss[1:] {
		for !strings.HasPrefix(s, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	return prefix
}

// helpLines is the content shown by the /help command, one entry per line.
func helpLines() []string {
	return []string{
		"commands:",
		"  /help             show this help",
		"  /context, /stats  session tokens, capacity, counts",
		"  /tools            list loaded tools",
		"  /skills           list loaded skills",
		"  /clear            clear conversation history",
		"  /exit, /quit      leave the REPL",
		"keys:",
		"  Enter             send message",
		"  Tab               complete /command",
		"  Ctrl-C            interrupt turn (twice to quit)",
		"  Up / Down         previous / next input",
		"  Ctrl-R            reverse-search history",
		"  PgUp / PgDn       scroll transcript",
		"  Ctrl-A / Ctrl-E   line start / end",
		"  Ctrl-U / Ctrl-K   clear before / after cursor",
		"  Ctrl-W            delete previous word",
		"  Alt-B / Alt-F     word left / right",
		"  Alt-D             delete next word",
		"  y / n / a         answer approval prompts",
	}
}

// appendHelp writes the /help output into the transcript. Caller must hold m.mu.
func (m *replModel) appendHelp() {
	for _, line := range helpLines() {
		m.appendNoticeLine(line)
	}
}

// runCommand dispatches a slash command entered at the prompt. It returns
// handled=false when the line isn't a recognized command, and quit=true when
// the command should end the REPL. Caller must hold m.mu.
func (r *managedREPL) runCommand(line string) (handled, quit bool) {
	switch line {
	case "/exit", "/quit":
		return true, true
	case "/help":
		r.model.appendHelp()
		return true, false
	case "/clear":
		r.cmdClear()
		return true, false
	case "/context", "/stats":
		r.cmdContext()
		return true, false
	case "/tools":
		r.cmdTools()
		return true, false
	case "/skills":
		r.cmdSkills()
		return true, false
	}
	return false, false
}

// cmdClear resets both the conversation history and the visible transcript.
// The next turn rebuilds the request from the now-empty session.
func (r *managedREPL) cmdClear() {
	if r.state != nil && r.state.session != nil {
		r.state.session.Clear()
	}
	r.model.transcript = nil
	r.model.currentAssistant = -1
	r.model.appendNoticeLine("context cleared")
}

// cmdContext prints session capacity and message statistics.
func (r *managedREPL) cmdContext() {
	m := r.model
	if r.state == nil || r.state.session == nil {
		m.appendNoticeLine("no active session")
		return
	}
	s := r.state.session
	m.appendNoticeLine("context: " + s.GetName())
	if m.modelName != "" {
		m.appendNoticeLine("model: " + m.modelName)
	}
	if pct := s.GetCapacityPercentage(); pct > 0 {
		m.appendNoticeLine(fmt.Sprintf("tokens: %s (%.0f%% of capacity)", humanizeTokens(s.GetTotalTokens()), pct))
	} else {
		m.appendNoticeLine("tokens: " + humanizeTokens(s.GetTotalTokens()))
	}
	c := s.GetMessageCounts()
	m.appendNoticeLine(fmt.Sprintf("messages: user %d · assistant %d · tool %d · system %d",
		c["user"], c["assistant"], c["tool"], c["system"]))
	m.appendNoticeLine(fmt.Sprintf("tool calls: %d", s.GetToolCallCount()))
}

// cmdTools lists the loaded tools by namespaced name.
func (r *managedREPL) cmdTools() {
	m := r.model
	var all []tools.Tool
	if r.state != nil && r.state.toolRegistry != nil {
		all = r.state.toolRegistry.All()
	}
	if len(all) == 0 {
		m.appendNoticeLine("no tools loaded")
		return
	}
	m.appendNoticeLine(fmt.Sprintf("tools (%d):", len(all)))
	for _, t := range all {
		m.appendNoticeLine("  " + t.GetName())
	}
}

// cmdSkills lists the loaded skills with their descriptions.
func (r *managedREPL) cmdSkills() {
	m := r.model
	var list []*skills.Skill
	if r.state != nil && r.state.skillCatalog != nil {
		list = r.state.skillCatalog.List()
	}
	if len(list) == 0 {
		m.appendNoticeLine("no skills loaded")
		return
	}
	m.appendNoticeLine(fmt.Sprintf("skills (%d):", len(list)))
	for _, s := range list {
		line := "  " + s.Name
		if s.Description != "" {
			line += " — " + s.Description
		}
		m.appendNoticeLine(line)
	}
}

// transcriptKind classifies a tool-end line for appendToolEndLine.
type transcriptKind int

const (
	transcriptToolOK transcriptKind = iota
	transcriptToolErr
	transcriptToolDenied
)

// submitPrompt finalizes the current input as a user turn. Returns the prompt
// string (possibly empty if input was blank).
func (m *replModel) submitPrompt() string {
	prompt := strings.TrimSpace(m.ed.text())
	m.ed.clear()
	m.historyIdx = -1
	m.historyDraft = ""
	if prompt == "" {
		return ""
	}
	m.history = append(m.history, prompt)
	m.appendUserPrompt(prompt)
	m.busy = true
	m.canceling = false
	m.state = turnStateWaiting
	m.turnStarted = time.Now()
	m.followBottom = true
	return prompt
}

func (m *replModel) historyUp() {
	if len(m.history) == 0 || m.busy || m.approval != nil {
		return
	}
	if m.historyIdx == -1 {
		m.historyDraft = m.ed.text()
		m.historyIdx = len(m.history) - 1
	} else if m.historyIdx > 0 {
		m.historyIdx--
	}
	m.ed.setText(m.history[m.historyIdx])
}

func (m *replModel) historyDown() {
	if m.historyIdx == -1 || m.busy || m.approval != nil {
		return
	}
	if m.historyIdx < len(m.history)-1 {
		m.historyIdx++
		m.ed.setText(m.history[m.historyIdx])
	} else {
		m.historyIdx = -1
		m.ed.setText(m.historyDraft)
	}
}

// startSearch enters reverse-incremental history search with an empty query.
func (m *replModel) startSearch() {
	m.searching = true
	m.searchQuery = ""
	m.searchMatch = -1
}

func (m *replModel) endSearch() {
	m.searching = false
	m.searchQuery = ""
	m.searchMatch = -1
}

// searchFrom scans history backward from index `from`, returning the index of
// the most recent entry containing query, or -1.
func (m *replModel) searchFrom(from int, query string) int {
	if query == "" || from >= len(m.history) {
		return -1
	}
	for i := from; i >= 0; i-- {
		if strings.Contains(m.history[i], query) {
			return i
		}
	}
	return -1
}

// searchType extends the query by one rune and re-matches from the newest entry.
func (m *replModel) searchType(r rune) {
	m.searchQuery += string(r)
	m.searchMatch = m.searchFrom(len(m.history)-1, m.searchQuery)
}

// searchBackspace shortens the query by one rune and re-matches.
func (m *replModel) searchBackspace() {
	if m.searchQuery == "" {
		return
	}
	q := []rune(m.searchQuery)
	m.searchQuery = string(q[:len(q)-1])
	m.searchMatch = m.searchFrom(len(m.history)-1, m.searchQuery)
}

// searchNext steps to the next older match (repeated Ctrl-R). Stays put when
// there's no earlier hit.
func (m *replModel) searchNext() {
	start := len(m.history) - 1
	if m.searchMatch >= 0 {
		start = m.searchMatch - 1
	}
	if next := m.searchFrom(start, m.searchQuery); next >= 0 {
		m.searchMatch = next
	}
}

// acceptSearch places the current match into the editor and leaves search mode.
// The text is not submitted — the user can edit it and press Enter.
func (m *replModel) acceptSearch() {
	if m.searchMatch >= 0 && m.searchMatch < len(m.history) {
		m.ed.setText(m.history[m.searchMatch])
	}
	m.endSearch()
}

// searchDisplay renders the reverse-i-search prompt and the current match.
func (m *replModel) searchDisplay() string {
	matched := ""
	if m.searchMatch >= 0 && m.searchMatch < len(m.history) {
		matched = m.history[m.searchMatch]
	}
	prompt := fmt.Sprintf("(reverse-i-search)`%s`: ", m.searchQuery)
	return styled(prompt, "blue", "bold") + styleEscape(matched)
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
	case 'n':
		a.out[a.index] = false
		a.index++
	}
	if a.index >= len(a.out) {
		m.finishApproval()
		return true
	}
	return false
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

// inputPromptWidth is the visible column count of the "> " prompt prefix.
const inputPromptWidth = 2

// inputCursorColumn returns the on-screen column of the edit cursor: the
// prompt prefix width plus the display width of the input up to the cursor.
// Caller must hold m.mu.
func (m *replModel) inputCursorColumn() int {
	return inputPromptWidth + m.ed.displayWidthToCursor()
}

// inputDisplay renders the bottom input row text (prefix + content) with
// inline style markup for the prompt prefix.
func (m *replModel) inputDisplay() string {
	switch {
	case m.searching:
		return m.searchDisplay()
	case m.approval != nil:
		call := m.approval.calls[m.approval.index]
		label := toolLabel(call)
		return styled("allow ", "blue", "bold") +
			styled(label, "grey", "") +
			styled("? [y/n/a] ", "blue", "bold")
	case m.busy:
		return m.busyIndicator()
	default:
		return styled("> ", "blue", "bold") + styleEscape(m.ed.text())
	}
}

// spinnerFrames is the braille dot cycle used by the busy indicator.
var spinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// busyIndicator renders the animated processing line: spinner + a friendly
// state word + elapsed time, e.g. "⠹ running bash · 3.8s". The frame advances
// every 100ms off turnStarted so the animation rate is independent of the
// render tick. Caller must hold m.mu.
func (m *replModel) busyIndicator() string {
	var elapsed time.Duration
	if !m.turnStarted.IsZero() {
		elapsed = time.Since(m.turnStarted)
	}
	frame := spinnerFrames[int(elapsed/(100*time.Millisecond))%len(spinnerFrames)]
	return styled(string(frame), "blue", "bold") + " " +
		styled(m.busyLabel(), "grey", "") + " " +
		styled("· "+formatElapsed(elapsed), "grey", "")
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

// ---------------------------------------------------------------------------
// managedREPL wires the model to gotui widgets and the agent.
// ---------------------------------------------------------------------------

type managedREPL struct {
	config *Config

	// state backs the session slash commands (/clear, /context, /tools,
	// /skills). Nil in unit tests that exercise only the editor/event layer;
	// command handlers guard against that.
	state *conversationState

	model *replModel

	transcriptW *widgets.Paragraph
	inputW      *widgets.Paragraph
	statusW     *widgets.Paragraph
	rootFlex    *widgets.Flex

	quit       chan struct{}
	pending    chan string
	turnCancel context.CancelFunc

	// histFile is the append handle for persistent input history; nil when
	// history couldn't be opened (best-effort — never fatal).
	histFile *os.File
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
		if strings.TrimSpace(l) != "" {
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
	r.model.history = loadHistory(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return
	}
	for _, l := range r.model.history {
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
	if r.histFile == nil || strings.ContainsRune(prompt, '\n') {
		return
	}
	fmt.Fprintln(r.histFile, prompt)
}

func newManagedREPL(config *Config, contextName string, toolCount, skillCount int) *managedREPL {
	m := newReplModel()
	m.modelName = stripProviderPrefix(config.Model)
	if contextName == "" {
		contextName = "-"
	}
	m.contextName = contextName
	m.toolCount = toolCount
	m.skillCount = skillCount
	m.quiet = config.Quiet
	return &managedREPL{
		config:  config,
		model:   m,
		quit:    make(chan struct{}, 1),
		pending: make(chan string, 1),
	}
}

// supportsManagedREPL returns true when stdin/stdout are TTYs and gotui's
// tcell backend is likely to initialize cleanly.
func supportsManagedREPL() bool {
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return false
	}
	return true
}

func (r *managedREPL) Run(ctx context.Context, runTurn func(context.Context, string, TurnUI) error) error {
	if err := ui.Init(); err != nil {
		return err
	}
	// Restore the terminal exactly once, whether we return normally or a
	// signal short-circuits to os.Exit (which skips deferred calls).
	var closeOnce sync.Once
	closeUI := func() { closeOnce.Do(ui.Close) }
	setBeforeExit(closeUI)
	defer func() {
		setBeforeExit(nil)
		closeUI()
	}()

	r.initHistory()
	defer r.closeHistory()

	r.setupWidgets()
	r.render()

	events := ui.PollEvents()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	var turnDone chan error

	for {
		select {
		case <-ctx.Done():
			r.cancelTurn()
			r.releaseApproval()
			return ctx.Err()
		case <-r.quit:
			r.cancelTurn()
			r.releaseApproval()
			if turnDone != nil {
				<-turnDone
			}
			return nil
		case <-ticker.C:
			r.render()
		case err := <-turnDone:
			turnDone = nil
			r.endTurn(err)
			r.render()
		case ev := <-events:
			if r.handleEvent(ev) {
				r.cancelTurn()
				r.releaseApproval()
				if turnDone != nil {
					<-turnDone
				}
				return nil
			}
			if prompt := r.takePending(); prompt != "" {
				turnDone = r.startTurn(ctx, prompt, runTurn)
			}
			r.render()
		case prompt := <-r.pending:
			turnDone = r.startTurn(ctx, prompt, runTurn)
			r.render()
		}
	}
}

func (r *managedREPL) takePending() string {
	select {
	case p := <-r.pending:
		return p
	default:
		return ""
	}
}

func (r *managedREPL) startTurn(ctx context.Context, prompt string, runTurn func(context.Context, string, TurnUI) error) chan error {
	turnCtx, cancel := context.WithCancel(ctx)
	r.turnCancel = cancel
	done := make(chan error, 1)
	tui := &gotuiTurnUI{repl: r, config: r.config}
	go func() {
		done <- runTurn(turnCtx, prompt, tui)
	}()
	return done
}

func (r *managedREPL) endTurn(err error) {
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	r.model.busy = false
	r.model.canceling = false
	r.model.currentAssistant = -1
	r.model.turnStarted = time.Time{}
	r.model.toolName = ""
	if err != nil && !errors.Is(err, context.Canceled) {
		r.model.appendLine(styled("Error: "+err.Error(), "red", ""))
		r.model.state = turnStateError
	} else {
		r.model.state = turnStateIdle
	}
	r.turnCancel = nil
}

func (r *managedREPL) cancelTurn() {
	if r.turnCancel != nil {
		r.turnCancel()
		r.turnCancel = nil
	}
}

// handleInterrupt processes Ctrl-C. While a turn is in flight the first press
// cancels it — denying any pending approval so the turn goroutine isn't parked
// on the reply channel — and keeps the REPL open. A second press while the turn
// is still winding down, or Ctrl-C at an idle prompt, quits. Returns true to
// quit. Caller must hold m.mu.
func (r *managedREPL) handleInterrupt() bool {
	m := r.model
	if !m.busy || m.canceling {
		r.requestQuit()
		return true
	}
	m.canceling = true
	r.cancelTurn()
	if m.approval != nil {
		denied := make([]bool, len(m.approval.calls))
		m.approval.reply <- denied
		close(m.approval.reply)
		m.approval = nil
	}
	m.appendNoticeLine("^C interrupted")
	return false
}

// handleSearchKey processes one key while reverse-i-search is active. Enter (or
// any non-editing key) accepts the current match into the editor; Esc/Ctrl-C/
// Ctrl-G cancel; Ctrl-R steps to the next older match; printable runes extend
// the query. Caller must hold m.mu.
func (r *managedREPL) handleSearchKey(e ui.Event) bool {
	m := r.model
	switch e.ID {
	case "<C-c>", "<Escape>", "<C-g>":
		m.endSearch()
	case "<C-r>":
		m.searchNext()
	case "<Enter>":
		m.acceptSearch()
	case "<Backspace>", "<Delete>":
		m.searchBackspace()
	case "<Space>":
		m.searchType(' ')
	default:
		if e.Type == ui.KeyboardEvent {
			if runes := []rune(e.ID); len(runes) == 1 && runes[0] >= 0x20 {
				m.searchType(runes[0])
				return false
			}
		}
		// Any other key (cursor moves, etc.) accepts the match and exits.
		m.acceptSearch()
	}
	return false
}

func (r *managedREPL) releaseApproval() {
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	if r.model.approval == nil {
		return
	}
	denied := make([]bool, len(r.model.approval.calls))
	r.model.approval.reply <- denied
	close(r.model.approval.reply)
	r.model.approval = nil
}

// transcriptHeight returns the current usable line count of the transcript
// pane based on the live terminal dimensions. Used by scroll handlers so
// scroll deltas match what the user actually sees.
func (r *managedREPL) transcriptHeight() int {
	_, h := ui.TerminalDimensions()
	chrome := 1
	if !r.model.quiet {
		chrome = 2
	}
	height := h - chrome
	if height < 1 {
		return 1
	}
	return height
}

func (r *managedREPL) requestQuit() {
	select {
	case r.quit <- struct{}{}:
	default:
	}
}

func (r *managedREPL) setupWidgets() {
	r.transcriptW = widgets.NewParagraph()
	noBorder(&r.transcriptW.Block)
	r.transcriptW.WrapText = true
	r.transcriptW.VerticalAlignment = ui.AlignBottom

	r.inputW = widgets.NewParagraph()
	noBorder(&r.inputW.Block)
	r.inputW.WrapText = false

	r.statusW = widgets.NewParagraph()
	noBorder(&r.statusW.Block)
	r.statusW.WrapText = false
	r.statusW.TextStyle = ui.NewStyle(ui.ColorGrey)

	r.rootFlex = widgets.NewFlex()
	noBorder(&r.rootFlex.Block)
	r.rootFlex.Direction = widgets.FlexColumn
	r.rootFlex.AddItem(r.transcriptW, 0, 1, false)
	r.rootFlex.AddItem(r.inputW, 1, 0, false)
	if !r.model.quiet {
		r.rootFlex.AddItem(r.statusW, 1, 0, false)
	}
}

// noBorder disables the visible border AND cancels the unconditional 1-cell
// Inner inset that Block.SetRect adds. Without negative padding, a 1-row
// borderless Paragraph collapses to zero inner rows and refuses to paint.
func noBorder(b *ui.Block) {
	b.Border = false
	b.PaddingLeft = -1
	b.PaddingRight = -1
	b.PaddingTop = -1
	b.PaddingBottom = -1
}

func (r *managedREPL) render() {
	w, h := ui.TerminalDimensions()
	if w < 1 || h < 2 {
		return
	}

	chrome := 1
	if !r.model.quiet {
		chrome = 2
	}
	transcriptHeight := h - chrome
	if transcriptHeight < 1 {
		transcriptHeight = 1
	}

	r.model.mu.Lock()
	transcript := r.model.visibleTranscript(transcriptHeight)
	input := r.model.inputDisplay()
	status := r.model.statusRow(w)
	editable := !r.model.busy && r.model.approval == nil && !r.model.searching
	cursorCol := r.model.inputCursorColumn()
	r.model.mu.Unlock()

	r.transcriptW.Text = transcript
	r.inputW.Text = input
	r.statusW.Text = status

	r.rootFlex.SetRect(0, 0, w, h)
	ui.Clear()
	r.placeCursor(editable, cursorCol, h-chrome, w)
	ui.Render(r.rootFlex)
}

// placeCursor positions (or hides) the hardware terminal cursor on the input
// row. gotui's render flushes the screen with Show(), which also emits the
// cursor state set here, so this must run before ui.Render.
func (r *managedREPL) placeCursor(editable bool, cursorCol, rowY, width int) {
	screen := ui.DefaultBackend.Screen
	if screen == nil {
		return
	}
	if !editable {
		screen.HideCursor()
		return
	}
	x := cursorCol
	if x > width-1 {
		x = width - 1
	}
	screen.ShowCursor(x, rowY)
}

// visibleTranscript returns the slice of transcript lines that should fill
// the pane right now, honoring scroll state. Caller must hold m.mu.
//
// Scrolling is line-based (not wrap-aware). That's good enough for MVP — long
// wrapped messages can scroll past partially without confusing the user.
func (m *replModel) visibleTranscript(maxLines int) string {
	lines := m.flattenTranscript()
	total := len(lines)
	if total == 0 {
		return ""
	}

	if m.followBottom {
		if total <= maxLines {
			return strings.Join(lines, "\n")
		}
		return strings.Join(lines[total-maxLines:], "\n")
	}

	top := m.scrollAnchor
	if top < 0 {
		top = 0
	}
	if top+maxLines >= total {
		// User scrolled all the way down; re-engage follow.
		m.followBottom = true
		top = total - maxLines
		if top < 0 {
			top = 0
		}
	}
	end := top + maxLines
	if end > total {
		end = total
	}
	return strings.Join(lines[top:end], "\n")
}

// flattenTranscript expands embedded "\n" within entries into separate lines
// so scroll math is uniform.
func (m *replModel) flattenTranscript() []string {
	var out []string
	for _, e := range m.transcript {
		if strings.Contains(e, "\n") {
			out = append(out, strings.Split(e, "\n")...)
		} else {
			out = append(out, e)
		}
	}
	return out
}

// scrollBy moves the scroll anchor by delta lines (negative = up). Caller
// must hold m.mu. Disengages followBottom on first upward scroll; re-engages
// when the user scrolls back to the bottom.
func (m *replModel) scrollBy(delta, viewportHeight int) {
	lines := m.flattenTranscript()
	total := len(lines)
	if total <= viewportHeight {
		m.followBottom = true
		m.scrollAnchor = 0
		return
	}

	if m.followBottom {
		m.scrollAnchor = total - viewportHeight
	}
	m.followBottom = false
	m.scrollAnchor += delta
	if m.scrollAnchor < 0 {
		m.scrollAnchor = 0
	}
	if m.scrollAnchor >= total-viewportHeight {
		m.scrollAnchor = total - viewportHeight
		m.followBottom = true
	}
}

func (m *replModel) scrollToBottom() {
	m.followBottom = true
}

// handleEvent mutates the model in response to a UI event. Returns true on
// quit.
func (r *managedREPL) handleEvent(e ui.Event) bool {
	if e.Type == ui.ResizeEvent {
		return false
	}

	r.model.mu.Lock()
	defer r.model.mu.Unlock()

	m := r.model
	viewport := r.transcriptHeight()

	// Scroll keys work in every mode (idle, busy, approval) so the user
	// can review history without interrupting the agent.
	switch e.ID {
	case "<PageUp>":
		m.scrollBy(-viewport/2, viewport)
		return false
	case "<PageDown>":
		m.scrollBy(viewport/2, viewport)
		return false
	case "<MouseWheelUp>":
		m.scrollBy(-3, viewport)
		return false
	case "<MouseWheelDown>":
		m.scrollBy(3, viewport)
		return false
	}

	// Drop any other mouse events (release, drag, unknown buttons). gotui
	// returns "Unknown_Mouse_Button" — a bare string — for events it
	// doesn't recognize, which would otherwise be typed into the prompt
	// by the default input case below.
	if e.Type == ui.MouseEvent {
		return false
	}

	// Reverse-incremental search owns the keyboard while active, so Ctrl-C
	// cancels the search rather than quitting. Scroll still works (handled above).
	if m.searching {
		return r.handleSearchKey(e)
	}

	// Ctrl-C is the universal interrupt: cancel an in-flight turn (first
	// press) or quit (second press, or at an idle prompt). See handleInterrupt.
	if e.ID == "<C-c>" {
		return r.handleInterrupt()
	}

	// Approval has its own keyset.
	if m.approval != nil {
		switch e.ID {
		case "<Escape>":
			r.requestQuit()
			return true
		case "<Enter>", "y", "Y":
			m.handleApprovalAnswer('y')
		case "n", "N":
			m.handleApprovalAnswer('n')
		case "a", "A":
			m.handleApprovalAnswer('a')
		}
		return false
	}

	// Busy: keys other than scroll/interrupt (both handled above) are ignored.
	if m.busy {
		return false
	}

	switch e.ID {
	case "<C-d>":
		if m.ed.empty() {
			r.requestQuit()
			return true
		}
	case "<Enter>":
		trimmed := strings.TrimSpace(m.ed.text())
		if trimmed == "" {
			return false
		}
		if strings.HasPrefix(trimmed, "/") {
			m.ed.clear()
			m.historyIdx = -1
			m.historyDraft = ""
			m.followBottom = true
			handled, quit := r.runCommand(trimmed)
			if quit {
				r.requestQuit()
				return true
			}
			if !handled {
				m.appendNoticeLine("unknown command: " + trimmed + " (try /help)")
			}
			return false
		}
		prompt := m.submitPrompt()
		r.appendHistory(prompt)
		select {
		case r.pending <- prompt:
		default:
		}
	case "<Backspace>", "<Delete>":
		m.ed.backspace()
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
	case "<C-r>":
		m.startSearch()
	case "<Up>":
		m.historyUp()
	case "<Down>":
		m.historyDown()
	case "<Space>":
		m.ed.insert(' ')
	case "<Tab>":
		cur := m.ed.text()
		if completed, matches, ok := completeSlash(cur); ok {
			if completed != cur {
				m.ed.setText(completed)
			} else if len(matches) > 1 {
				m.appendNoticeLine(strings.Join(matches, "  "))
				m.followBottom = true
			}
			return false
		}
		m.ed.insert('\t')
	default:
		// Only printable single-rune keyboard events become input. This
		// rejects any multi-character event ID — bracketed key names like
		// "<F1>" as well as gotui's bare "Unknown_Mouse_Button" — so stray
		// events never get typed into the prompt.
		if e.Type != ui.KeyboardEvent {
			return false
		}
		runes := []rune(e.ID)
		if len(runes) == 1 && runes[0] >= 0x20 {
			m.ed.insert(runes[0])
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// gotuiTurnUI: TurnUI impl that pokes the managedREPL model under lock.
// ---------------------------------------------------------------------------

type gotuiTurnUI struct {
	repl   *managedREPL
	config *Config
}

func (t *gotuiTurnUI) Start() {}
func (t *gotuiTurnUI) Stop()  {}

func (t *gotuiTurnUI) ShowThinking(tokens int) {
	t.repl.model.mu.Lock()
	t.repl.model.state = turnStateThinking
	t.repl.model.mu.Unlock()
}

func (t *gotuiTurnUI) AppendAssistantText(content string) {
	t.repl.model.mu.Lock()
	t.repl.model.state = turnStateStreaming
	t.repl.model.appendAssistant(content)
	t.repl.model.mu.Unlock()
}

func (t *gotuiTurnUI) AppendToolStart(calls []messages.ChatMessageToolCall) {
	t.repl.model.mu.Lock()
	defer t.repl.model.mu.Unlock()
	if len(calls) > 0 {
		t.repl.model.state = turnStateTool
		t.repl.model.toolName = calls[0].Name
	}
	if !toolDisplayEnabled(t.config) {
		return
	}
	for _, c := range calls {
		t.repl.model.appendToolStartLine(toolLabel(c))
	}
}

func (t *gotuiTurnUI) ApproveToolCalls(calls []messages.ChatMessageToolCall) []bool {
	if !t.config.Confirm {
		approved := make([]bool, len(calls))
		for i := range approved {
			approved[i] = true
		}
		return approved
	}
	reply := make(chan []bool, 1)
	t.repl.model.mu.Lock()
	t.repl.model.approval = &approvalState{calls: calls, reply: reply}
	t.repl.model.mu.Unlock()
	results, ok := <-reply
	if !ok {
		return make([]bool, len(calls))
	}
	return results
}

func (t *gotuiTurnUI) AppendToolEnd(call messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
	label := toolLabel(call)
	t.repl.model.mu.Lock()
	defer t.repl.model.mu.Unlock()
	if t.repl.model.busy {
		t.repl.model.state = turnStateWaiting
		t.repl.model.toolName = ""
	}
	if !toolDisplayEnabled(t.config) {
		return
	}
	switch {
	case result == llm.ToolDeniedContent:
		t.repl.model.appendToolEndLine(transcriptToolDenied, label, "", "")
	case err != nil:
		t.repl.model.appendToolEndLine(transcriptToolErr, label, fmt.Sprintf("%.1fs", duration.Seconds()), err.Error())
	default:
		t.repl.model.appendToolEndLine(transcriptToolOK, label, fmt.Sprintf("%.1fs", duration.Seconds()), "")
	}
}

func (t *gotuiTurnUI) AppendWarning(text string) {
	t.repl.model.mu.Lock()
	t.repl.model.appendNoticeLine("Warning: " + text)
	t.repl.model.mu.Unlock()
}

func (t *gotuiTurnUI) RecordTurnTokens(in, out int) {
	t.repl.model.mu.Lock()
	t.repl.model.lastIn = in
	t.repl.model.lastOut = out
	t.repl.model.mu.Unlock()
}

func (t *gotuiTurnUI) FinishTextTurn() {}

// ---------------------------------------------------------------------------
// Entry points used by runREPL
// ---------------------------------------------------------------------------

func runManagedREPL(ctx context.Context, config *Config, state *conversationState) error {
	repl := newManagedREPL(config, state.session.GetName(), toolCount(state.toolRegistry), skillCount(state.skillCatalog))
	repl.state = state
	return repl.Run(ctx, func(turnCtx context.Context, prompt string, turnUI TurnUI) error {
		return executeTurn(turnCtx, config, state, prompt, nil, nil, turnUI)
	})
}

func runFallbackREPL(ctx context.Context, config *Config, state *conversationState) error {
	reader := bufio.NewReader(os.Stdin)
	return runREPLLoop(reader, os.Stderr, func(prompt string) error {
		return executeTurn(ctx, config, state, prompt, nil, reader, nil)
	})
}

func runREPLLoop(reader *bufio.Reader, promptWriter io.Writer, runTurn func(string) error) error {
	for {
		if _, err := fmt.Fprint(promptWriter, "> "); err != nil {
			return err
		}
		line, err := readLine(reader)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if trimmed == "/exit" || trimmed == "/quit" {
			return nil
		}
		if trimmed == "/help" {
			for _, l := range helpLines() {
				if _, err := fmt.Fprintln(promptWriter, l); err != nil {
					return err
				}
			}
			continue
		}
		if err := runTurn(line); err != nil {
			return err
		}
	}
}
