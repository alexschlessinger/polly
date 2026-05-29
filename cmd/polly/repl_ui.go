package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	// goalCol is the rune column that vertical movement (up/down) tries to
	// keep as the cursor crosses lines of differing length; -1 means "unset —
	// recompute from the cursor on the next vertical move". Every horizontal
	// move or edit resets it, so each fresh up/down run starts from the column
	// the cursor is on, while a run of consecutive up/downs holds its column.
	goalCol int
}

func (e *lineEditor) text() string { return string(e.buf) }
func (e *lineEditor) empty() bool  { return len(e.buf) == 0 }

func (e *lineEditor) setText(s string) {
	e.buf = []rune(s)
	e.cursor = len(e.buf)
	e.goalCol = -1
}

func (e *lineEditor) clear() {
	e.buf = nil
	e.cursor = 0
	e.goalCol = -1
}

func (e *lineEditor) insert(r rune) {
	e.buf = append(e.buf[:e.cursor], append([]rune{r}, e.buf[e.cursor:]...)...)
	e.cursor++
	e.goalCol = -1
}

func (e *lineEditor) backspace() {
	if e.cursor > 0 {
		e.buf = append(e.buf[:e.cursor-1], e.buf[e.cursor:]...)
		e.cursor--
	}
	e.goalCol = -1
}

func (e *lineEditor) left() {
	if e.cursor > 0 {
		e.cursor--
	}
	e.goalCol = -1
}

func (e *lineEditor) right() {
	if e.cursor < len(e.buf) {
		e.cursor++
	}
	e.goalCol = -1
}

func (e *lineEditor) home() { e.cursor = 0; e.goalCol = -1 }
func (e *lineEditor) end()  { e.cursor = len(e.buf); e.goalCol = -1 }

func (e *lineEditor) killToStart() {
	e.buf = append([]rune(nil), e.buf[e.cursor:]...)
	e.cursor = 0
	e.goalCol = -1
}

func (e *lineEditor) killToEnd() {
	e.buf = append([]rune(nil), e.buf[:e.cursor]...)
	e.goalCol = -1
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

func (e *lineEditor) wordLeft()  { e.cursor = e.prevWordStart(); e.goalCol = -1 }
func (e *lineEditor) wordRight() { e.cursor = e.nextWordEnd(); e.goalCol = -1 }

func (e *lineEditor) deleteWordBackward() {
	e.goalCol = -1
	start := e.prevWordStart()
	if start == e.cursor {
		return
	}
	e.buf = append(e.buf[:start], e.buf[e.cursor:]...)
	e.cursor = start
}

func (e *lineEditor) deleteWordForward() {
	e.goalCol = -1
	end := e.nextWordEnd()
	if end == e.cursor {
		return
	}
	e.buf = append(e.buf[:e.cursor], e.buf[end:]...)
}

// lineStartAt returns the index of the first rune on the logical line that
// contains position pos (0, or one past the preceding '\n').
func (e *lineEditor) lineStartAt(pos int) int {
	i := pos
	for i > 0 && e.buf[i-1] != '\n' {
		i--
	}
	return i
}

// lineEndAt returns the index just past the last rune on the logical line that
// contains pos (the index of the next '\n', or len(buf)).
func (e *lineEditor) lineEndAt(pos int) int {
	i := pos
	for i < len(e.buf) && e.buf[i] != '\n' {
		i++
	}
	return i
}

// up moves the cursor to the same column on the previous logical line, holding
// the goal column across shorter lines. It returns false when the cursor is
// already on the first line, so the caller can fall through to history recall.
func (e *lineEditor) up() bool {
	start := e.lineStartAt(e.cursor)
	if start == 0 {
		return false // already on the first line
	}
	if e.goalCol < 0 {
		e.goalCol = e.cursor - start
	}
	prevStart := e.lineStartAt(start - 1)
	prevLen := (start - 1) - prevStart // runes before the '\n' that ends it
	col := e.goalCol
	if col > prevLen {
		col = prevLen
	}
	e.cursor = prevStart + col
	return true
}

// down mirrors up onto the next logical line. It returns false when the cursor
// is already on the last line.
func (e *lineEditor) down() bool {
	end := e.lineEndAt(e.cursor)
	if end >= len(e.buf) {
		return false // already on the last line
	}
	if e.goalCol < 0 {
		e.goalCol = e.cursor - e.lineStartAt(e.cursor)
	}
	nextStart := end + 1
	nextLen := e.lineEndAt(nextStart) - nextStart
	col := e.goalCol
	if col > nextLen {
		col = nextLen
	}
	e.cursor = nextStart + col
	return true
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
	pasting      bool // inside a bracketed paste; runes go in verbatim
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
	m := &replModel{
		currentAssistant: -1,
		historyIdx:       -1,
		state:            turnStateIdle,
		followBottom:     true,
	}
	m.ed.goalCol = -1
	return m
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

func (m *replModel) appendToolEndLine(kind transcriptKind, label, duration string) {
	switch kind {
	case transcriptToolOK:
		body := strings.TrimSpace(duration + " " + label)
		m.appendLine("  " + styled("✓", "green", "bold") + " " + styled(body, "grey", ""))
	case transcriptToolDenied:
		m.appendLine("  " + styled("✗", "red", "bold") + " " + styled("denied "+label, "grey", ""))
	}
}

// appendToolErrorLine renders a failed tool call as a single line: a red ✗, the
// grey metadata (timing · command · exit code), and the error message itself in
// dark red so it stands out without being the bright-red wall of the full
// output. code/summary may each be empty (no exit code from an MCP error, no
// output from a silent failure). The model still receives the full output — this
// only shapes the transcript.
func (m *replModel) appendToolErrorLine(label, duration, code, summary string) {
	meta := strings.TrimSpace(duration + " " + label)
	line := "  " + styled("✗", "red", "bold") + " " + styled(meta, "grey", "")
	if code != "" {
		line += styled(" · "+code, "grey", "")
	}
	if summary != "" {
		line += styled(" · ", "grey", "") + styled(summary, "darkred", "")
	}
	m.appendLine(line)
}

// toolErrorSummaryMax bounds the one-line error summary so a failing tool can't
// reflow into a multi-row block. It's a rune budget, not a column count, so on a
// very narrow terminal the line may still wrap — but it can never be huge.
const toolErrorSummaryMax = 100

// toolErrorParts splits a failed tool call into its display segments: a compact
// "exit N" code (empty when none applies) and the most useful single line of
// output (empty when the failure produced none). The model still receives the
// full output — this only shapes the transcript.
func toolErrorParts(err error) (code, summary string) {
	if c, ok := toolExitCode(err); ok {
		code = fmt.Sprintf("exit %d", c)
	}
	if s := toolErrorSummary(err); s != "" {
		summary = truncateRunes(s, toolErrorSummaryMax)
	}
	return code, summary
}

// toolExitCode recovers a process exit code from a failed tool call. BashTool
// wraps *exec.ExitError with %w so errors.As finds it directly; ShellTool
// formats it with %v, so we also parse the conventional "exit status N" text.
// A negative code (process killed by a signal) is reported as not-found, since
// "exit -1" is more confusing than just showing the output summary.
func toolExitCode(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if code := ee.ExitCode(); code >= 0 {
			return code, true
		}
		return 0, false
	}
	if n, ok := parseExitStatus(err.Error()); ok {
		return n, true
	}
	return 0, false
}

// parseExitStatus extracts N from a "...exit status N..." message.
func parseExitStatus(s string) (int, bool) {
	const marker = "exit status "
	i := strings.Index(s, marker)
	if i < 0 {
		return 0, false
	}
	j := i + len(marker)
	k := j
	for k < len(s) && s[k] >= '0' && s[k] <= '9' {
		k++
	}
	if k == j {
		return 0, false
	}
	n, err := strconv.Atoi(s[j:k])
	if err != nil {
		return 0, false
	}
	return n, true
}

// toolErrorSummary returns the single most useful line of a tool failure: the
// last non-blank line of the command's output when the error embeds one (the
// "(output: …)" tail that bash/shell tools append — usually the real error in a
// traceback or build log), otherwise the last line of the error message itself.
func toolErrorSummary(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if out, ok := extractToolOutput(msg); ok {
		return lastLine(out)
	}
	return lastLine(msg)
}

// extractToolOutput pulls the OUTPUT out of a "...(output: OUTPUT)" wrapper that
// BashTool/ShellTool append to their errors. ok is false when there's no such
// wrapper (e.g. a timeout, "tool not found", or a structured MCP error).
func extractToolOutput(s string) (string, bool) {
	const marker = " (output: "
	i := strings.Index(s, marker)
	if i < 0 {
		return "", false
	}
	out := s[i+len(marker):]
	return strings.TrimSuffix(out, ")"), true
}

// lastLine returns the last non-blank line of s, trimmed of surrounding space.
func lastLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// truncateRunes shortens s to at most max runes, appending an ellipsis when it
// had to cut. Rune-based so multibyte output isn't sliced mid-character.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
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
		"  Ctrl-J            newline (multi-line input)",
		"  Tab               complete /command",
		"  Ctrl-C            interrupt turn (twice to quit)",
		"  Up / Down         move line; recall history at top/bottom",
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

// inputPromptWidth is the visible column count of the "> " prompt prefix (and
// of the matching indent on wrapped continuation lines).
const inputPromptWidth = 2

// maxInputRows caps how tall the editable input region can grow for multi-line
// prompts. Beyond this the region anchors to the bottom so the cursor stays
// visible while composing or after a paste.
const maxInputRows = 8

// inputRows is how many terminal rows the input region currently occupies. The
// busy/approval/search overlays are always single-line; an editable prompt
// grows with its embedded newlines, capped at maxInputRows. Caller must hold m.mu.
func (m *replModel) inputRows() int {
	if m.busy || m.approval != nil || m.searching {
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

// renderInput produces the bottom input region: its display text (possibly
// multiple lines), the row count it occupies, the cursor's row/col within that
// region, and whether the cursor should be shown. The busy/approval/search
// overlays are single-line and hide the cursor; the editable prompt may span
// several rows, anchored to the bottom when it overflows maxInputRows. Caller
// must hold m.mu.
func (m *replModel) renderInput() (text string, rows, curRow, curCol int, editable bool) {
	switch {
	case m.searching:
		return m.searchDisplay(), 1, 0, 0, false
	case m.approval != nil:
		call := m.approval.calls[m.approval.index]
		label := toolLabel(call)
		text = styled("allow ", "blue", "bold") +
			styled(label, "grey", "") +
			styled("? [y/n/a] ", "blue", "bold")
		return text, 1, 0, 0, false
	case m.busy:
		return m.busyIndicator(), 1, 0, 0, false
	}

	lines := strings.Split(m.ed.text(), "\n")
	cr, cc := m.inputCursorRowCol()

	// Window the visible lines to maxInputRows. Anchor to the bottom so the
	// newest lines (and the cursor after a paste) stay visible — but if the
	// cursor has moved up above that window (editing higher in a long paste),
	// scroll up just enough to keep the cursor's line on screen.
	start := 0
	if len(lines) > maxInputRows {
		start = len(lines) - maxInputRows
		if cr < start {
			start = cr
		}
	}
	visible := lines[start : start+min(maxInputRows, len(lines)-start)]

	parts := make([]string, len(visible))
	for i, ln := range visible {
		if start+i == 0 {
			parts[i] = styled("> ", "blue", "bold") + styleEscape(ln)
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
	return strings.Join(parts, "\n"), len(visible), curRow, cc, true
}

// inputDisplay returns just the input region's text. Retained for tests that
// assert on the busy/approval overlays.
func (m *replModel) inputDisplay() string {
	text, _, _, _, _ := m.renderInput()
	return text
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

	events := pollManagedEvents(ui.DefaultBackend.Screen)
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

// insertPasted adds one key from a bracketed paste to the editor as literal
// text, mapping newline keys to '\n'. Pasted content is dropped while busy,
// approving, or searching — there's no editable prompt to receive it. Caller
// must hold m.mu.
func (r *managedREPL) insertPasted(e ui.Event) {
	m := r.model
	if m.busy || m.approval != nil || m.searching {
		return
	}
	switch e.ID {
	case "<Enter>", "<C-j>":
		m.ed.insert('\n')
	case "<Space>":
		m.ed.insert(' ')
	case "<Tab>":
		m.ed.insert('\t')
	default:
		if e.Type == ui.KeyboardEvent {
			if runes := []rune(e.ID); len(runes) == 1 && runes[0] >= 0x20 {
				m.ed.insert(runes[0])
			}
		}
	}
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
	statusRows := 0
	if !r.model.quiet {
		statusRows = 1
	}
	height := h - r.model.inputRows() - statusRows
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
}

// layout (re)builds the root flex for the current input height. The input row
// count varies with multi-line prompts, so the flex is rebuilt each render
// rather than sized once at setup.
func (r *managedREPL) layout(w, h, inputRows int, showStatus bool) {
	flex := widgets.NewFlex()
	noBorder(&flex.Block)
	flex.Direction = widgets.FlexColumn
	flex.AddItem(r.transcriptW, 0, 1, false)
	flex.AddItem(r.inputW, inputRows, 0, false)
	if showStatus {
		flex.AddItem(r.statusW, 1, 0, false)
	}
	flex.SetRect(0, 0, w, h)
	r.rootFlex = flex
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

	showStatus := !r.model.quiet
	statusRows := 0
	if showStatus {
		statusRows = 1
	}

	r.model.mu.Lock()
	input, inputRows, curRow, curCol, editable := r.model.renderInput()
	transcriptHeight := h - inputRows - statusRows
	if transcriptHeight < 1 {
		transcriptHeight = 1
	}
	transcript := r.model.visibleTranscript(transcriptHeight)
	status := r.model.statusRow(w)
	r.model.mu.Unlock()

	r.transcriptW.Text = transcript
	r.inputW.Text = input
	r.statusW.Text = status

	r.layout(w, h, inputRows, showStatus)
	ui.Clear()
	r.placeCursor(editable, curCol, transcriptHeight+curRow, w)
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

	// Bracketed-paste markers bound a run of literal input. While pasting, keys
	// are inserted verbatim — newlines included — instead of triggering actions,
	// so a multi-line paste lands as one prompt rather than firing a submit per
	// line. (gotui drops these markers; our own event pump surfaces them.)
	if e.ID == pasteStartID || e.ID == pasteEndID {
		m.pasting = e.ID == pasteStartID
		return false
	}
	if m.pasting {
		r.insertPasted(e)
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
		// Only a single-line "/…" is a command; a multi-line prompt that happens
		// to start with "/" is real input.
		if !strings.Contains(trimmed, "\n") && strings.HasPrefix(trimmed, "/") {
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
	case "<C-j>":
		// Ctrl-J inserts a newline for composing multi-line prompts; Enter sends.
		m.ed.insert('\n')
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
		t.repl.model.appendToolEndLine(transcriptToolDenied, label, "")
	case err != nil:
		code, summary := toolErrorParts(err)
		t.repl.model.appendToolErrorLine(label, fmt.Sprintf("%.1fs", duration.Seconds()), code, summary)
	default:
		t.repl.model.appendToolEndLine(transcriptToolOK, label, fmt.Sprintf("%.1fs", duration.Seconds()))
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
