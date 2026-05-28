package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
	"github.com/metaspartan/gotui/v5/widgets"
	"golang.org/x/term"
)

// transcriptKind classifies a transcript entry so it can be rendered with the
// right glyph and color when converted to a styled row for the List widget.
type transcriptKind int

const (
	transcriptUser transcriptKind = iota
	transcriptAssistant
	transcriptToolStart
	transcriptToolOK
	transcriptToolErr
	transcriptToolDenied
	transcriptNotice
	transcriptBlank
)

type transcriptEntry struct {
	kind     transcriptKind
	text     string
	duration string
	errText  string
}

// turnState describes what the agent is currently doing for the status bar.
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
	default:
		return ""
	}
}

// styleEscape escapes [ and ] so user-supplied text isn't mistaken for the
// inline style markup gotui's ParseStyles consumes.
func styleEscape(s string) string {
	s = strings.ReplaceAll(s, "[", `\[`)
	s = strings.ReplaceAll(s, "]", `\]`)
	return s
}

// styled wraps text in gotui's inline style markup. Color names come from
// gotui's StyleParserColorMap; "" means no styling.
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

// renderEntryRows renders a transcript entry as one or more List rows. The
// List widget wraps long rows itself when WrapText is true; we only need to
// split on explicit newlines.
func renderEntryRows(e transcriptEntry) []string {
	switch e.kind {
	case transcriptUser:
		return splitLines(styled("> ", "skyblue", "bold") + styleEscape(e.text))
	case transcriptAssistant:
		return splitLines(styleEscape(e.text))
	case transcriptToolStart:
		return []string{"  " + styled("→", "teal", "") + " " + styled(e.text, "grey", "")}
	case transcriptToolOK:
		label := strings.TrimSpace(e.duration + " " + e.text)
		return []string{"  " + styled("✓", "green", "bold") + " " + styled(label, "grey", "")}
	case transcriptToolErr:
		label := strings.TrimSpace(e.duration + " " + e.text)
		row := "  " + styled("✗", "salmon", "bold") + " " + styled(label, "grey", "")
		if e.errText != "" {
			row += " " + styled("- "+e.errText, "salmon", "")
		}
		return []string{row}
	case transcriptToolDenied:
		return []string{"  " + styled("✗", "salmon", "bold") + " " + styled("denied "+e.text, "grey", "")}
	case transcriptNotice:
		return splitLines(styled(e.text, "grey", ""))
	case transcriptBlank:
		return []string{""}
	default:
		return []string{styleEscape(e.text)}
	}
}

func splitLines(text string) []string {
	if text == "" {
		return []string{""}
	}
	return strings.Split(text, "\n")
}

// replModel holds the REPL state. Mutated only from the event loop goroutine;
// the gotui widgets are populated from this model on each render.
type replModel struct {
	transcript       []transcriptEntry
	currentAssistant int
	input            []rune
	cursor           int
	history          []string
	historyIndex     int
	historyDraft     string

	busy     bool
	approval *approvalState

	state    turnState
	toolName string

	model       string
	contextName string
	tools       int
	skills      int
	quiet       bool

	turnStarted time.Time
	lastIn      int
	lastOut     int
}

type approvalState struct {
	calls []messages.ChatMessageToolCall
	index int
	reply chan []bool
	out   []bool
}

func newReplModel(config *Config, contextName string, tools, skills int) *replModel {
	if contextName == "" {
		contextName = "-"
	}
	return &replModel{
		currentAssistant: -1,
		historyIndex:     -1,
		model:            stripProviderPrefix(config.Model),
		contextName:      contextName,
		tools:            tools,
		skills:           skills,
		quiet:            config.Quiet,
		state:            turnStateIdle,
	}
}

func (m *replModel) appendEntry(e transcriptEntry) {
	m.transcript = append(m.transcript, e)
	if e.kind != transcriptAssistant {
		m.currentAssistant = -1
	}
}

func (m *replModel) appendAssistantText(text string) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if m.currentAssistant >= 0 && m.currentAssistant < len(m.transcript) {
		m.transcript[m.currentAssistant].text += text
		return
	}
	m.appendEntry(transcriptEntry{kind: transcriptAssistant, text: text})
	m.currentAssistant = len(m.transcript) - 1
}

func (m *replModel) appendToolStart(call messages.ChatMessageToolCall) {
	m.appendEntry(transcriptEntry{kind: transcriptToolStart, text: toolLabel(call)})
}

func (m *replModel) appendToolEnd(call messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
	label := toolLabel(call)
	if result == llm.ToolDeniedContent {
		m.appendEntry(transcriptEntry{kind: transcriptToolDenied, text: label})
		return
	}
	dur := fmt.Sprintf("%.1fs", duration.Seconds())
	if err != nil {
		m.appendEntry(transcriptEntry{
			kind:     transcriptToolErr,
			text:     label,
			duration: dur,
			errText:  err.Error(),
		})
		return
	}
	m.appendEntry(transcriptEntry{
		kind:     transcriptToolOK,
		text:     label,
		duration: dur,
	})
}

func (m *replModel) appendBlankIfNeeded() {
	if len(m.transcript) == 0 {
		return
	}
	if m.transcript[len(m.transcript)-1].kind == transcriptBlank {
		return
	}
	m.appendEntry(transcriptEntry{kind: transcriptBlank})
}

func (m *replModel) appendNotice(text string) {
	if text == "" {
		m.appendBlankIfNeeded()
		return
	}
	m.appendEntry(transcriptEntry{kind: transcriptNotice, text: text})
}

func (m *replModel) appendUserPrompt(p string) {
	m.appendEntry(transcriptEntry{kind: transcriptUser, text: p})
}

func (m *replModel) startTurn() {
	m.busy = true
	m.state = turnStateWaiting
	m.turnStarted = time.Now()
}

func (m *replModel) stopTurn() {
	m.busy = false
	m.approval = nil
	m.currentAssistant = -1
	m.state = turnStateIdle
	m.turnStarted = time.Time{}
	m.toolName = ""
}

func (m *replModel) inputPrefix() string {
	if m.approval != nil {
		return "allow? [Y/n/a] "
	}
	return "> "
}

func (m *replModel) submitPrompt() string {
	prompt := string(m.input)
	m.input = nil
	m.cursor = 0
	m.historyIndex = -1
	m.historyDraft = ""
	if prompt != "" {
		m.history = append(m.history, prompt)
		m.appendUserPrompt(prompt)
	}
	m.busy = true
	return prompt
}

func (m *replModel) historyUp() {
	if len(m.history) == 0 || m.busy || m.approval != nil {
		return
	}
	if m.historyIndex == -1 {
		m.historyDraft = string(m.input)
		m.historyIndex = len(m.history) - 1
	} else if m.historyIndex > 0 {
		m.historyIndex--
	}
	m.input = []rune(m.history[m.historyIndex])
	m.cursor = len(m.input)
}

func (m *replModel) historyDown() {
	if m.historyIndex == -1 || m.busy || m.approval != nil {
		return
	}
	if m.historyIndex < len(m.history)-1 {
		m.historyIndex++
		m.input = []rune(m.history[m.historyIndex])
	} else {
		m.historyIndex = -1
		m.input = []rune(m.historyDraft)
	}
	m.cursor = len(m.input)
}

func (m *replModel) backspaceWord() {
	if m.cursor == 0 {
		return
	}
	start := m.cursor
	for start > 0 && m.input[start-1] == ' ' {
		start--
	}
	for start > 0 && m.input[start-1] != ' ' {
		start--
	}
	m.input = append(m.input[:start], m.input[m.cursor:]...)
	m.cursor = start
}

// handleApprovalAnswer applies a single y/n/a answer to the pending approval
// batch and finishes the batch when every tool has been answered.
func (m *replModel) handleApprovalAnswer(answer byte) {
	if m.approval == nil {
		return
	}
	a := m.approval
	if a.out == nil {
		a.out = make([]bool, len(a.calls))
	}
	switch answer {
	case 'a':
		for i := a.index; i < len(a.out); i++ {
			a.out[i] = true
		}
		m.finishApproval()
	case 'y':
		a.out[a.index] = true
		a.index++
		if a.index >= len(a.out) {
			m.finishApproval()
		}
	case 'n':
		a.out[a.index] = false
		a.index++
		if a.index >= len(a.out) {
			m.finishApproval()
		}
	}
}

func (m *replModel) finishApproval() {
	if m.approval == nil {
		return
	}
	out := append([]bool(nil), m.approval.out...)
	m.approval.reply <- out
	close(m.approval.reply)
	m.approval = nil
	m.state = turnStateWaiting
}

// statusRow renders the bar contents based on the current model state.
// Returns a string with gotui inline style markup applied.
func (m *replModel) statusRow(width int) string {
	if m.quiet {
		return ""
	}
	utf8Bar := utf8Locale(os.Getenv("LANG") + "," + os.Getenv("LC_ALL"))
	sep := " · "
	if !utf8Bar {
		sep = " | "
	}

	type field struct {
		drop int
		text string
	}

	stateText := m.state.label(m.toolName)
	tokens := fmt.Sprintf("%s→%s", humanizeTokens(m.lastIn), humanizeTokens(m.lastOut))

	fields := []field{}
	if m.model != "" {
		fields = append(fields, field{drop: 4, text: m.model})
	}
	fields = append(fields, field{drop: 0, text: m.contextName})
	fields = append(fields, field{drop: 0, text: stateText})
	if !m.turnStarted.IsZero() {
		fields = append(fields, field{drop: 3, text: formatElapsed(time.Since(m.turnStarted))})
	}
	fields = append(fields, field{drop: 0, text: tokens})
	if m.tools > 0 {
		fields = append(fields, field{drop: 2, text: fmt.Sprintf("tools:%d", m.tools)})
	}
	if m.skills > 0 {
		fields = append(fields, field{drop: 1, text: fmt.Sprintf("skills:%d", m.skills)})
	}

	visibleLen := func(fs []field) int {
		// 1 space leading + (len-1) separators + 1 space trailing + fields
		n := 2
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
			color := "gold"
			if m.state == turnStateError {
				color = "salmon"
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

func utf8Locale(env string) bool {
	return strings.Contains(strings.ToLower(env), "utf")
}

// inputRow renders the prompt prefix + current input text. Used by the input
// Paragraph widget.
func (m *replModel) inputRow() string {
	prefix := styled(m.inputPrefix(), "skyblue", "bold")
	if m.approval != nil {
		return prefix
	}
	if m.busy {
		return prefix + styled("…", "grey", "")
	}
	return prefix + styleEscape(string(m.input))
}

// ---------------------------------------------------------------------------
// gotui-driven REPL orchestrator
// ---------------------------------------------------------------------------

// replEventKind is the discriminator for replEvent, which multiplexes UI
// events (key presses, resize) and agent callbacks (assistant text, tool
// start, tool end) into a single channel so the model is only mutated from
// one goroutine.
type replEventKind int

const (
	replEventUI replEventKind = iota
	replEventTick
	replEventTurnStart
	replEventTurnStop
	replEventThinking
	replEventAssistant
	replEventToolStart
	replEventToolEnd
	replEventTokens
	replEventNotice
	replEventApprovalRequest
	replEventQuit
)

type replEvent struct {
	kind     replEventKind
	ev       ui.Event
	text     string
	calls    []messages.ChatMessageToolCall
	call     messages.ChatMessageToolCall
	result   string
	duration time.Duration
	err      error
	in       int
	out      int
	reply    chan []bool
}

type managedREPL struct {
	config  *Config
	model   *replModel
	events  chan replEvent
	submit  chan string
	quit    chan struct{}
	done    chan struct{}
	closeMu sync.Mutex
	closed  bool

	transcriptList *widgets.List
	inputBox       *widgets.Paragraph
	statusBar      *widgets.Paragraph
	rootFlex       *widgets.Flex

	followBottom bool

	turnMu     sync.Mutex
	turnCancel context.CancelFunc
	turnDone   chan error
}

func newManagedREPL(config *Config, contextName string, tools, skills int) *managedREPL {
	return &managedREPL{
		config:       config,
		model:        newReplModel(config, contextName, tools, skills),
		events:       make(chan replEvent, 64),
		submit:       make(chan string, 1),
		quit:         make(chan struct{}, 1),
		done:         make(chan struct{}),
		followBottom: true,
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

func (r *managedREPL) sendEvent(e replEvent) {
	select {
	case <-r.done:
		return
	case r.events <- e:
	}
}

func (r *managedREPL) close() {
	r.closeMu.Lock()
	defer r.closeMu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	close(r.done)
}

func (r *managedREPL) Run(ctx context.Context, runTurn func(context.Context, string, TurnUI) error) error {
	if err := ui.Init(); err != nil {
		return err
	}
	defer ui.Close()

	r.setupWidgets()
	r.render()

	uiEvents := ui.PollEvents()

	// Bridge UI events into our multiplexed channel so the main loop can
	// also process turn callbacks without an inner select per event.
	go r.bridgeUIEvents(uiEvents)
	go r.tickLoop()

	for {
		select {
		case <-ctx.Done():
			r.shutdown()
			return ctx.Err()
		case <-r.quit:
			r.shutdown()
			return nil
		case prompt := <-r.submit:
			r.startTurn(ctx, prompt, runTurn)
		case err := <-r.currentTurnDone():
			r.clearActiveTurn()
			if err != nil {
				r.shutdown()
				return err
			}
		case ev := <-r.events:
			if r.dispatchEvent(ev) {
				r.shutdown()
				return nil
			}
			r.render()
		}
	}
}

// shutdown cancels any in-flight turn and releases any approval waiter so the
// agent goroutine can return before we tear down the TUI. Order matters:
// close(r.done) first unblocks any sendEvent that was waiting on a full
// r.events buffer, then we release approval/cancel context, then we wait for
// the agent goroutine to finish so it doesn't race with ui.Close().
func (r *managedREPL) shutdown() {
	r.close()
	r.cancelPendingApproval()
	r.cancelActiveTurn()
	if done := r.currentTurnDone(); done != nil {
		<-done
		r.clearActiveTurn()
	}
}

func (r *managedREPL) startTurn(ctx context.Context, prompt string, runTurn func(context.Context, string, TurnUI) error) {
	turnCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	r.turnMu.Lock()
	r.turnCancel = cancel
	r.turnDone = done
	r.turnMu.Unlock()
	tui := &gotuiTurnUI{repl: r, config: r.config}
	go func() {
		done <- runTurn(turnCtx, prompt, tui)
	}()
}

// currentTurnDone returns the active turn's done channel, or nil when no turn
// is running so the select case is silently disabled.
func (r *managedREPL) currentTurnDone() chan error {
	r.turnMu.Lock()
	defer r.turnMu.Unlock()
	return r.turnDone
}

func (r *managedREPL) clearActiveTurn() {
	r.turnMu.Lock()
	r.turnCancel = nil
	r.turnDone = nil
	r.turnMu.Unlock()
}

func (r *managedREPL) cancelActiveTurn() {
	r.turnMu.Lock()
	cancel := r.turnCancel
	r.turnMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (r *managedREPL) bridgeUIEvents(uiEvents <-chan ui.Event) {
	for {
		select {
		case <-r.done:
			return
		case e, ok := <-uiEvents:
			if !ok {
				return
			}
			r.sendEvent(replEvent{kind: replEventUI, ev: e})
		}
	}
}

func (r *managedREPL) tickLoop() {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-t.C:
			r.sendEvent(replEvent{kind: replEventTick})
		}
	}
}

func (r *managedREPL) requestQuit() {
	select {
	case r.quit <- struct{}{}:
	default:
	}
}

func (r *managedREPL) cancelPendingApproval() {
	if r.model.approval == nil {
		return
	}
	denied := make([]bool, len(r.model.approval.calls))
	r.model.approval.reply <- denied
	close(r.model.approval.reply)
	r.model.approval = nil
}

func (r *managedREPL) setupWidgets() {
	r.transcriptList = widgets.NewList()
	r.transcriptList.Border = false
	r.transcriptList.WrapText = true
	r.transcriptList.TextStyle = ui.NewStyle(ui.ColorWhite)
	r.transcriptList.SelectedStyle = ui.NewStyle(ui.ColorWhite)

	r.inputBox = widgets.NewParagraph()
	r.inputBox.Border = false
	r.inputBox.WrapText = false
	r.inputBox.TextStyle = ui.NewStyle(ui.ColorWhite)

	r.statusBar = widgets.NewParagraph()
	r.statusBar.Border = false
	r.statusBar.WrapText = false
	r.statusBar.TextStyle = ui.NewStyle(ui.ColorGrey)

	r.rootFlex = widgets.NewFlex()
	r.rootFlex.Border = false
	r.rootFlex.Direction = widgets.FlexColumn
	r.rootFlex.AddItem(r.transcriptList, 0, 1, false)
	r.rootFlex.AddItem(r.inputBox, 1, 0, false)
	r.rootFlex.AddItem(r.statusBar, 1, 0, false)
}

func (r *managedREPL) render() {
	w, h := ui.TerminalDimensions()
	if w < 1 || h < 1 {
		return
	}

	rows := []string{}
	for _, e := range r.model.transcript {
		rows = append(rows, renderEntryRows(e)...)
	}
	if len(rows) == 0 {
		rows = []string{""}
	}
	r.transcriptList.Rows = rows

	if r.followBottom {
		r.transcriptList.ScrollBottom()
	}

	r.inputBox.Text = r.model.inputRow()
	r.statusBar.Text = r.model.statusRow(w)

	r.rootFlex.SetRect(0, 0, w, h)
	ui.Clear()
	ui.Render(r.rootFlex)
}

// dispatchEvent mutates the model in response to an incoming event. Returns
// true when the event was a quit request.
func (r *managedREPL) dispatchEvent(ev replEvent) bool {
	switch ev.kind {
	case replEventUI:
		return r.handleUIEvent(ev.ev)
	case replEventTick:
		// Re-render to refresh elapsed time in the status bar.
	case replEventTurnStart:
		r.model.startTurn()
	case replEventTurnStop:
		r.model.stopTurn()
	case replEventThinking:
		r.model.state = turnStateThinking
	case replEventAssistant:
		r.model.state = turnStateStreaming
		r.model.appendAssistantText(ev.text)
		if r.followBottom {
			r.transcriptList.ScrollBottom()
		}
	case replEventToolStart:
		if toolDisplayEnabled(r.config) {
			for _, c := range ev.calls {
				r.model.appendToolStart(c)
			}
		}
		if len(ev.calls) > 0 {
			r.model.state = turnStateTool
			r.model.toolName = ev.calls[0].Name
		}
	case replEventToolEnd:
		if toolDisplayEnabled(r.config) {
			r.model.appendToolEnd(ev.call, ev.result, ev.duration, ev.err)
		}
		if r.model.busy {
			r.model.state = turnStateWaiting
			r.model.toolName = ""
		}
	case replEventTokens:
		r.model.lastIn = ev.in
		r.model.lastOut = ev.out
	case replEventNotice:
		r.model.appendNotice(ev.text)
	case replEventApprovalRequest:
		r.model.approval = &approvalState{
			calls: ev.calls,
			reply: ev.reply,
		}
		r.model.state = turnStateWaiting
	case replEventQuit:
		r.requestQuit()
		return true
	}
	return false
}

// handleUIEvent maps gotui events to model mutations. Returns true on quit.
func (r *managedREPL) handleUIEvent(e ui.Event) bool {
	if e.Type == ui.ResizeEvent {
		// render() reads terminal dimensions fresh each tick.
		return false
	}

	m := r.model

	// Approval shortcut: any other key is ignored unless it's y/n/a or enter.
	if m.approval != nil {
		switch e.ID {
		case "<C-c>":
			r.requestQuit()
			return true
		case "<Enter>":
			m.handleApprovalAnswer('y')
		case "y", "Y":
			m.handleApprovalAnswer('y')
		case "n", "N":
			m.handleApprovalAnswer('n')
		case "a", "A":
			m.handleApprovalAnswer('a')
		case "<PageUp>":
			r.scrollPageUp()
		case "<PageDown>":
			r.scrollPageDown()
		case "<End>":
			r.followBottom = true
			r.transcriptList.ScrollBottom()
		}
		return false
	}

	if m.busy {
		switch e.ID {
		case "<C-c>":
			r.requestQuit()
			return true
		case "<PageUp>":
			r.scrollPageUp()
		case "<PageDown>":
			r.scrollPageDown()
		case "<End>":
			r.followBottom = true
			r.transcriptList.ScrollBottom()
		case "<MouseWheelUp>":
			r.scrollUp()
		case "<MouseWheelDown>":
			r.scrollDown()
		}
		return false
	}

	switch e.ID {
	case "<C-c>":
		r.requestQuit()
		return true
	case "<C-d>":
		if len(m.input) == 0 {
			r.requestQuit()
			return true
		}
		if m.cursor < len(m.input) {
			m.input = append(m.input[:m.cursor], m.input[m.cursor+1:]...)
		}
	case "<Enter>":
		trimmed := strings.TrimSpace(string(m.input))
		if trimmed == "" {
			return false
		}
		if trimmed == "/exit" || trimmed == "/quit" {
			r.requestQuit()
			return true
		}
		prompt := m.submitPrompt()
		r.followBottom = true
		r.transcriptList.ScrollBottom()
		select {
		case r.submit <- prompt:
		default:
		}
	case "<Backspace>", "<Delete>":
		if m.cursor > 0 {
			m.input = append(m.input[:m.cursor-1], m.input[m.cursor:]...)
			m.cursor--
		}
	case "<Left>":
		if m.cursor > 0 {
			m.cursor--
		}
	case "<Right>":
		if m.cursor < len(m.input) {
			m.cursor++
		}
	case "<Home>", "<C-a>":
		m.cursor = 0
	case "<End>", "<C-e>":
		m.cursor = len(m.input)
	case "<C-u>":
		m.input = append([]rune(nil), m.input[m.cursor:]...)
		m.cursor = 0
	case "<C-k>":
		m.input = append([]rune(nil), m.input[:m.cursor]...)
	case "<C-w>":
		m.backspaceWord()
	case "<Up>":
		m.historyUp()
	case "<Down>":
		m.historyDown()
	case "<PageUp>":
		r.scrollPageUp()
	case "<PageDown>":
		r.scrollPageDown()
	case "<Space>":
		m.input = append(m.input[:m.cursor], append([]rune{' '}, m.input[m.cursor:]...)...)
		m.cursor++
	case "<Tab>":
		m.input = append(m.input[:m.cursor], append([]rune{'\t'}, m.input[m.cursor:]...)...)
		m.cursor++
	case "<MouseWheelUp>":
		r.scrollUp()
	case "<MouseWheelDown>":
		r.scrollDown()
	default:
		if len(e.ID) == 1 {
			r := []rune(e.ID)[0]
			if r >= 0x20 {
				m.input = append(m.input[:m.cursor], append([]rune{r}, m.input[m.cursor:]...)...)
				m.cursor++
			}
		}
	}
	return false
}

func (r *managedREPL) scrollUp() {
	r.followBottom = false
	r.transcriptList.ScrollUp()
}

func (r *managedREPL) scrollDown() {
	r.transcriptList.ScrollDown()
	if r.transcriptList.SelectedRow >= len(r.transcriptList.Rows)-1 {
		r.followBottom = true
	}
}

func (r *managedREPL) scrollPageUp() {
	r.followBottom = false
	r.transcriptList.ScrollPageUp()
}

func (r *managedREPL) scrollPageDown() {
	r.transcriptList.ScrollPageDown()
	if r.transcriptList.SelectedRow >= len(r.transcriptList.Rows)-1 {
		r.followBottom = true
	}
}

// ---------------------------------------------------------------------------
// gotuiTurnUI is the TurnUI impl that posts events into the managedREPL.
// ---------------------------------------------------------------------------

type gotuiTurnUI struct {
	repl           *managedREPL
	config         *Config
	needsSeparator bool
	contentPrinted bool
}

func (t *gotuiTurnUI) Start() { t.repl.sendEvent(replEvent{kind: replEventTurnStart}) }
func (t *gotuiTurnUI) Stop()  { t.repl.sendEvent(replEvent{kind: replEventTurnStop}) }

func (t *gotuiTurnUI) ShowThinking(tokens int) {
	t.repl.sendEvent(replEvent{kind: replEventThinking})
}

func (t *gotuiTurnUI) AppendAssistantText(content string) {
	if t.needsSeparator {
		t.repl.sendEvent(replEvent{kind: replEventNotice, text: ""})
		t.needsSeparator = false
	}
	t.repl.sendEvent(replEvent{kind: replEventAssistant, text: content})
	t.contentPrinted = true
}

func (t *gotuiTurnUI) AppendToolStart(calls []messages.ChatMessageToolCall) {
	if len(calls) == 0 {
		return
	}
	if toolDisplayEnabled(t.config) && t.contentPrinted {
		t.repl.sendEvent(replEvent{kind: replEventNotice, text: ""})
		t.contentPrinted = false
	}
	t.needsSeparator = toolDisplayEnabled(t.config)
	t.repl.sendEvent(replEvent{kind: replEventToolStart, calls: calls})
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
	t.repl.sendEvent(replEvent{kind: replEventApprovalRequest, calls: calls, reply: reply})
	results, ok := <-reply
	if !ok {
		return make([]bool, len(calls))
	}
	return results
}

func (t *gotuiTurnUI) AppendToolEnd(call messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
	t.repl.sendEvent(replEvent{
		kind:     replEventToolEnd,
		call:     call,
		result:   result,
		duration: duration,
		err:      err,
	})
}

func (t *gotuiTurnUI) AppendWarning(text string) {
	t.repl.sendEvent(replEvent{kind: replEventNotice, text: "Warning: " + text})
}

func (t *gotuiTurnUI) RecordTurnTokens(in, out int) {
	t.repl.sendEvent(replEvent{kind: replEventTokens, in: in, out: out})
}

func (t *gotuiTurnUI) FinishTextTurn() {}

// ---------------------------------------------------------------------------
// Entry points used by runREPL
// ---------------------------------------------------------------------------

func runManagedREPL(ctx context.Context, config *Config, state *conversationState) error {
	repl := newManagedREPL(config, state.session.GetName(), toolCount(state.toolRegistry), skillCount(state.skillCatalog))
	defer repl.close()
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
		if err := runTurn(line); err != nil {
			return err
		}
	}
}
