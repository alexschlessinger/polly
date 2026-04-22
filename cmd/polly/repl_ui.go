package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"golang.org/x/term"
)

const (
	hideCursor = esc + "[?25l"
	showCursor = esc + "[?25h"
)

type spanStyle int

const (
	spanPlain spanStyle = iota
	spanPrompt
	spanAssistant
	spanToolStart
	spanToolOK
	spanToolErr
	spanToolLabel
	spanDim
)

func styleSpan(kind spanStyle, text string) string {
	switch kind {
	case spanPrompt:
		return promptStyle.Styled(text)
	case spanAssistant:
		return assistantOut.Styled(text)
	case spanToolStart:
		return toolStartStyle.Styled(text)
	case spanToolOK:
		return toolOkStyle.Styled(text)
	case spanToolErr:
		return toolErrStyle.Styled(text)
	case spanToolLabel:
		return toolLabelStyle.Styled(text)
	case spanDim:
		return dimStyle.Styled(text)
	default:
		return text
	}
}

type uiSpan struct {
	text  string
	style spanStyle
}

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

func (e transcriptEntry) logicalLines() [][]uiSpan {
	switch e.kind {
	case transcriptUser:
		return splitStyledText([]uiSpan{
			{text: "> ", style: spanPrompt},
			{text: e.text, style: spanPlain},
		})
	case transcriptAssistant:
		return splitStyledText([]uiSpan{{text: e.text, style: spanAssistant}})
	case transcriptToolStart:
		return [][]uiSpan{{
			{text: "  →", style: spanToolStart},
			{text: " " + e.text, style: spanToolLabel},
		}}
	case transcriptToolOK:
		return [][]uiSpan{{
			{text: "  ✓", style: spanToolOK},
			{text: " " + strings.TrimSpace(strings.TrimSpace(e.duration+" "+e.text)), style: spanToolLabel},
		}}
	case transcriptToolErr:
		return [][]uiSpan{{
			{text: "  ✗", style: spanToolErr},
			{text: " " + strings.TrimSpace(strings.TrimSpace(e.duration+" "+e.text)), style: spanToolLabel},
			{text: " - " + e.errText, style: spanToolErr},
		}}
	case transcriptToolDenied:
		return [][]uiSpan{{
			{text: "  ✗", style: spanToolErr},
			{text: " denied " + e.text, style: spanToolLabel},
		}}
	case transcriptNotice:
		return splitStyledText([]uiSpan{{text: e.text, style: spanDim}})
	case transcriptBlank:
		return [][]uiSpan{{}}
	default:
		return [][]uiSpan{{{text: e.text, style: spanPlain}}}
	}
}

func splitStyledText(spans []uiSpan) [][]uiSpan {
	lines := [][]uiSpan{{}}
	for _, span := range spans {
		parts := strings.Split(span.text, "\n")
		for i, part := range parts {
			if part != "" {
				lines[len(lines)-1] = append(lines[len(lines)-1], uiSpan{text: part, style: span.style})
			}
			if i < len(parts)-1 {
				lines = append(lines, []uiSpan{})
			}
		}
	}
	return lines
}

func wrapStyledLine(spans []uiSpan, width int) []string {
	if width < 1 {
		return []string{""}
	}
	if len(spans) == 0 {
		return []string{""}
	}

	var out []string
	var b strings.Builder
	cols := 0
	flush := func() {
		out = append(out, b.String())
		b.Reset()
		cols = 0
	}

	for _, span := range spans {
		for _, r := range span.text {
			if cols == width {
				flush()
			}
			b.WriteString(styleSpan(span.style, string(r)))
			cols++
		}
	}
	if b.Len() > 0 || len(out) == 0 {
		flush()
	}
	return out
}

type approvalState struct {
	calls   []messages.ChatMessageToolCall
	index   int
	results []bool
	reply   chan []bool
}

func newApprovalState(calls []messages.ChatMessageToolCall, reply chan []bool) *approvalState {
	return &approvalState{
		calls:   calls,
		results: make([]bool, len(calls)),
		reply:   reply,
	}
}

type replKeyKind int

const (
	keyRune replKeyKind = iota
	keyEnter
	keyBackspace
	keyDelete
	keyLeft
	keyRight
	keyUp
	keyDown
	keyHome
	keyEnd
	keyPageUp
	keyPageDown
	keyCtrlA
	keyCtrlE
	keyCtrlU
	keyCtrlK
	keyCtrlW
	keyCtrlC
	keyCtrlD
)

type replKey struct {
	kind replKeyKind
	r    rune
}

type replScreen struct {
	w int
	h int

	status       barState
	quiet        bool
	busy         bool
	followBottom bool
	scrollOffset int

	transcript       []transcriptEntry
	currentAssistant int

	input       []rune
	cursor      int
	inputScroll int

	history      []string
	historyIndex int
	historyDraft string

	approval *approvalState
}

func newReplScreen(w, h int, quiet bool, model, contextName string, tools, skills int) *replScreen {
	if contextName == "" {
		contextName = "-"
	}
	return &replScreen{
		w:                w,
		h:                h,
		quiet:            quiet,
		followBottom:     true,
		historyIndex:     -1,
		currentAssistant: -1,
		status: barState{
			model:       model,
			contextName: contextName,
			turnState:   "idle",
			tools:       tools,
			skills:      skills,
			utf8:        utf8Locale(os.Getenv("LANG") + "," + os.Getenv("LC_ALL")),
		},
	}
}

func (s *replScreen) setSize(w, h int) {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	s.w = w
	s.h = h
	s.ensureInputCursorVisible()
	s.clampScroll()
}

func (s *replScreen) viewportHeight() int {
	if s.h <= 2 {
		return 0
	}
	return s.h - 2
}

func (s *replScreen) appendEntry(entry transcriptEntry) {
	beforeRows := 0
	if !s.followBottom {
		beforeRows = len(s.wrappedTranscriptRows())
	}
	s.transcript = append(s.transcript, entry)
	if entry.kind != transcriptAssistant {
		s.currentAssistant = -1
	}
	if s.followBottom {
		s.scrollOffset = 0
	} else {
		s.scrollOffset += len(s.wrappedTranscriptRows()) - beforeRows
		s.clampScroll()
	}
}

func (s *replScreen) appendBlankIfNeeded() {
	if len(s.transcript) == 0 {
		return
	}
	if s.transcript[len(s.transcript)-1].kind == transcriptBlank {
		return
	}
	s.appendEntry(transcriptEntry{kind: transcriptBlank})
}

func (s *replScreen) appendUserPrompt(prompt string) {
	s.appendEntry(transcriptEntry{kind: transcriptUser, text: prompt})
}

func (s *replScreen) appendAssistantText(text string) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	beforeRows := 0
	if !s.followBottom {
		beforeRows = len(s.wrappedTranscriptRows())
	}
	if s.currentAssistant >= 0 && s.currentAssistant < len(s.transcript) {
		s.transcript[s.currentAssistant].text += text
	} else {
		s.appendEntry(transcriptEntry{kind: transcriptAssistant, text: text})
		s.currentAssistant = len(s.transcript) - 1
		if !s.followBottom {
			return
		}
	}
	if s.followBottom {
		s.scrollOffset = 0
	} else {
		s.scrollOffset += len(s.wrappedTranscriptRows()) - beforeRows
		s.clampScroll()
	}
}

func (s *replScreen) appendToolStart(call messages.ChatMessageToolCall) {
	s.appendEntry(transcriptEntry{kind: transcriptToolStart, text: toolLabel(call)})
}

func (s *replScreen) appendToolEnd(call messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
	label := toolLabel(call)
	if result == llm.ToolDeniedContent {
		s.appendEntry(transcriptEntry{kind: transcriptToolDenied, text: label})
		return
	}
	dur := fmt.Sprintf("%.1fs", duration.Seconds())
	if err != nil {
		s.appendEntry(transcriptEntry{
			kind:     transcriptToolErr,
			text:     label,
			duration: dur,
			errText:  err.Error(),
		})
		return
	}
	s.appendEntry(transcriptEntry{
		kind:     transcriptToolOK,
		text:     label,
		duration: dur,
	})
}

func (s *replScreen) appendNotice(text string) {
	s.appendEntry(transcriptEntry{kind: transcriptNotice, text: text})
}

func (s *replScreen) setWaiting() {
	s.status.turnState = "waiting"
}

func (s *replScreen) setThinking() {
	s.status.turnState = "thinking"
}

func (s *replScreen) setStreaming() {
	s.status.turnState = "streaming"
}

func (s *replScreen) setToolState(name string) {
	s.status.turnState = "tool: " + name
}

func (s *replScreen) startTurn() {
	s.busy = true
	s.status.turnStarted = time.Now()
	s.status.turnState = "waiting"
}

func (s *replScreen) stopTurn() {
	s.busy = false
	s.approval = nil
	s.currentAssistant = -1
	s.status.turnState = "idle"
	s.status.turnStarted = time.Time{}
}

func (s *replScreen) recordTurnTokens(in, out int) {
	s.status.lastIn = in
	s.status.lastOut = out
}

func (s *replScreen) inputPrefix() string {
	if s.approval != nil {
		return "allow? [Y/n/a] "
	}
	return "> "
}

func (s *replScreen) inputViewWidth() int {
	w := s.w - visibleWidth(s.inputPrefix())
	if w < 0 {
		return 0
	}
	return w
}

func (s *replScreen) ensureInputCursorVisible() {
	viewWidth := s.inputViewWidth()
	if s.approval != nil || viewWidth <= 0 {
		s.inputScroll = 0
		return
	}
	if s.cursor < s.inputScroll {
		s.inputScroll = s.cursor
	}
	if s.cursor > s.inputScroll+viewWidth {
		s.inputScroll = s.cursor - viewWidth
	}
	if s.inputScroll < 0 {
		s.inputScroll = 0
	}
	maxScroll := len(s.input)
	if s.inputScroll > maxScroll {
		s.inputScroll = maxScroll
	}
}

func (s *replScreen) resetHistoryBrowse() {
	s.historyIndex = -1
	s.historyDraft = ""
}

func (s *replScreen) submitPrompt() string {
	prompt := string(s.input)
	s.input = nil
	s.cursor = 0
	s.inputScroll = 0
	s.resetHistoryBrowse()
	if prompt != "" {
		s.history = append(s.history, prompt)
		s.appendUserPrompt(prompt)
	}
	s.busy = true
	return prompt
}

func (s *replScreen) backspaceWord() {
	if s.cursor == 0 {
		return
	}
	start := s.cursor
	for start > 0 && s.input[start-1] == ' ' {
		start--
	}
	for start > 0 && s.input[start-1] != ' ' {
		start--
	}
	s.input = append(s.input[:start], s.input[s.cursor:]...)
	s.cursor = start
	s.ensureInputCursorVisible()
}

func (s *replScreen) historyUp() {
	if len(s.history) == 0 || s.busy || s.approval != nil {
		return
	}
	if s.historyIndex == -1 {
		s.historyDraft = string(s.input)
		s.historyIndex = len(s.history) - 1
	} else if s.historyIndex > 0 {
		s.historyIndex--
	}
	s.input = []rune(s.history[s.historyIndex])
	s.cursor = len(s.input)
	s.ensureInputCursorVisible()
}

func (s *replScreen) historyDown() {
	if s.historyIndex == -1 || s.busy || s.approval != nil {
		return
	}
	if s.historyIndex < len(s.history)-1 {
		s.historyIndex++
		s.input = []rune(s.history[s.historyIndex])
	} else {
		s.historyIndex = -1
		s.input = []rune(s.historyDraft)
	}
	s.cursor = len(s.input)
	s.ensureInputCursorVisible()
}

func (s *replScreen) inputString() string {
	return string(s.input)
}

func (s *replScreen) handleApprovalKey(key replKey) bool {
	if s.approval == nil {
		return false
	}

	answer := byte(0)
	switch key.kind {
	case keyEnter:
		answer = 'y'
	case keyRune:
		switch strings.ToLower(string(key.r)) {
		case "y":
			answer = 'y'
		case "n":
			answer = 'n'
		case "a":
			answer = 'a'
		default:
			return false
		}
	case keyPageUp:
		s.pageUp()
		return false
	case keyPageDown:
		s.pageDown()
		return false
	case keyEnd:
		s.followBottom = true
		s.scrollOffset = 0
		return false
	default:
		return false
	}

	idx := s.approval.index
	switch answer {
	case 'a':
		for i := idx; i < len(s.approval.results); i++ {
			s.approval.results[i] = true
		}
		s.finishApproval()
	case 'y':
		s.approval.results[idx] = true
		s.approval.index++
		if s.approval.index >= len(s.approval.results) {
			s.finishApproval()
		}
	case 'n':
		s.approval.results[idx] = false
		s.approval.index++
		if s.approval.index >= len(s.approval.results) {
			s.finishApproval()
		}
	}
	return false
}

func (s *replScreen) finishApproval() {
	if s.approval == nil {
		return
	}
	reply := append([]bool(nil), s.approval.results...)
	s.approval.reply <- reply
	close(s.approval.reply)
	s.approval = nil
	s.setWaiting()
}

func (s *replScreen) pageUp() {
	total := len(s.wrappedTranscriptRows())
	vh := s.viewportHeight()
	if vh == 0 || total <= vh {
		return
	}
	s.followBottom = false
	s.scrollOffset += vh
	maxOffset := total - vh
	if s.scrollOffset > maxOffset {
		s.scrollOffset = maxOffset
	}
}

func (s *replScreen) pageDown() {
	if s.scrollOffset == 0 {
		s.followBottom = true
		return
	}
	vh := s.viewportHeight()
	s.scrollOffset -= vh
	if s.scrollOffset <= 0 {
		s.scrollOffset = 0
		s.followBottom = true
	}
}

func (s *replScreen) clampScroll() {
	total := len(s.wrappedTranscriptRows())
	vh := s.viewportHeight()
	if s.followBottom || total <= vh {
		s.scrollOffset = 0
		if total <= vh {
			s.followBottom = true
		}
		return
	}
	maxOffset := total - vh
	if s.scrollOffset > maxOffset {
		s.scrollOffset = maxOffset
	}
	if s.scrollOffset < 0 {
		s.scrollOffset = 0
	}
}

func (s *replScreen) handleKey(key replKey) (submit string, exit bool) {
	if key.kind == keyCtrlC {
		return "", true
	}
	if s.approval != nil {
		return "", s.handleApprovalKey(key)
	}
	if s.busy {
		switch key.kind {
		case keyPageUp:
			s.pageUp()
		case keyPageDown:
			s.pageDown()
		case keyEnd:
			s.followBottom = true
			s.scrollOffset = 0
		}
		return "", false
	}

	switch key.kind {
	case keyEnter:
		trimmed := strings.TrimSpace(string(s.input))
		if trimmed == "" {
			return "", false
		}
		if trimmed == "/exit" || trimmed == "/quit" {
			return "", true
		}
		return s.submitPrompt(), false
	case keyRune:
		s.input = append(s.input[:s.cursor], append([]rune{key.r}, s.input[s.cursor:]...)...)
		s.cursor++
		s.ensureInputCursorVisible()
	case keyBackspace:
		if s.cursor > 0 {
			s.input = append(s.input[:s.cursor-1], s.input[s.cursor:]...)
			s.cursor--
			s.ensureInputCursorVisible()
		}
	case keyDelete:
		if s.cursor < len(s.input) {
			s.input = append(s.input[:s.cursor], s.input[s.cursor+1:]...)
			s.ensureInputCursorVisible()
		}
	case keyLeft:
		if s.cursor > 0 {
			s.cursor--
			s.ensureInputCursorVisible()
		}
	case keyRight:
		if s.cursor < len(s.input) {
			s.cursor++
			s.ensureInputCursorVisible()
		}
	case keyHome, keyCtrlA:
		s.cursor = 0
		s.ensureInputCursorVisible()
	case keyEnd, keyCtrlE:
		s.cursor = len(s.input)
		s.followBottom = true
		s.scrollOffset = 0
		s.ensureInputCursorVisible()
	case keyCtrlU:
		s.input = append([]rune(nil), s.input[s.cursor:]...)
		s.cursor = 0
		s.ensureInputCursorVisible()
	case keyCtrlK:
		s.input = append([]rune(nil), s.input[:s.cursor]...)
		s.ensureInputCursorVisible()
	case keyCtrlW:
		s.backspaceWord()
	case keyUp:
		s.historyUp()
	case keyDown:
		s.historyDown()
	case keyPageUp:
		s.pageUp()
	case keyPageDown:
		s.pageDown()
	case keyCtrlD:
		if len(s.input) == 0 {
			return "", true
		}
		if s.cursor < len(s.input) {
			s.input = append(s.input[:s.cursor], s.input[s.cursor+1:]...)
			s.ensureInputCursorVisible()
		}
	}
	return "", false
}

func (s *replScreen) wrappedTranscriptRows() []string {
	width := s.w
	if width < 1 {
		width = 1
	}
	var rows []string
	for _, entry := range s.transcript {
		for _, line := range entry.logicalLines() {
			rows = append(rows, wrapStyledLine(line, width)...)
		}
	}
	return rows
}

func (s *replScreen) visibleTranscriptRows() []string {
	all := s.wrappedTranscriptRows()
	vh := s.viewportHeight()
	if vh <= 0 {
		return nil
	}
	if len(all) <= vh {
		return all
	}
	if s.followBottom {
		return all[len(all)-vh:]
	}
	start := len(all) - vh - s.scrollOffset
	if start < 0 {
		start = 0
	}
	end := start + vh
	if end > len(all) {
		end = len(all)
	}
	return all[start:end]
}

func (s *replScreen) renderInputRow() string {
	prefix := promptStyle.Styled(s.inputPrefix())
	if s.approval != nil {
		return prefix
	}
	if s.busy {
		return prefix + dimStyle.Styled("")
	}
	viewWidth := s.inputViewWidth()
	if viewWidth <= 0 {
		return prefix
	}
	start := s.inputScroll
	if start > len(s.input) {
		start = len(s.input)
	}
	end := start + viewWidth
	if end > len(s.input) {
		end = len(s.input)
	}
	return prefix + string(s.input[start:end])
}

func (s *replScreen) cursorRowCol() (row, col int, visible bool) {
	if s.approval != nil {
		return maxInt(1, s.h-1), visibleWidth(s.inputPrefix()) + 1, true
	}
	if s.busy {
		return 0, 0, false
	}
	row = maxInt(1, s.h-1)
	col = visibleWidth(s.inputPrefix()) + 1 + (s.cursor - s.inputScroll)
	if col < 1 {
		col = 1
	}
	if col > s.w {
		col = s.w
	}
	return row, col, true
}

func (s *replScreen) renderFrame() []string {
	if s.h < 1 {
		return nil
	}
	rows := make([]string, s.h)
	transcriptRows := s.visibleTranscriptRows()
	for i := 0; i < s.viewportHeight() && i < len(transcriptRows); i++ {
		rows[i] = transcriptRows[i]
	}
	if s.h >= 2 {
		rows[s.h-2] = s.renderInputRow()
		rows[s.h-1] = s.renderStatusRow()
	} else {
		rows[s.h-1] = s.renderStatusRow()
	}
	return rows
}

func (s *replScreen) renderStatusRow() string {
	if s.quiet {
		return ""
	}
	return styleStatusLine(renderBar(s.status, paintWidth(s.w)), s.status.turnState)
}

type managedEventKind int

const (
	managedEventInput managedEventKind = iota
	managedEventResize
	managedEventTick
	managedEventTurnStart
	managedEventTurnStop
	managedEventThinking
	managedEventAssistantContent
	managedEventToolStart
	managedEventToolEnd
	managedEventTokens
	managedEventNotice
	managedEventApprovalRequest
)

type managedEvent struct {
	kind     managedEventKind
	data     []byte
	w        int
	h        int
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
	out io.Writer
	in  *os.File
	sz  sizer

	model   string
	context string
	quiet   bool
	tools   int
	skills  int
	config  *Config

	events      chan managedEvent
	submissions chan string
	exitCh      chan struct{}
	done        chan struct{}
	ownerDone   chan struct{}

	rawState *term.State

	exitRequested atomic.Bool

	prevFrame []string

	activeTurnMu     sync.Mutex
	activeTurnCancel context.CancelFunc
}

func newManagedREPL(config *Config, contextName string, tools, skills int) *managedREPL {
	return &managedREPL{
		out:         os.Stderr,
		in:          os.Stdin,
		sz:          termSizer{fd: int(os.Stderr.Fd())},
		model:       stripProviderPrefix(config.Model),
		context:     contextName,
		quiet:       config.Quiet,
		tools:       tools,
		skills:      skills,
		config:      config,
		events:      make(chan managedEvent, 64),
		submissions: make(chan string, 1),
		exitCh:      make(chan struct{}, 1),
		done:        make(chan struct{}),
		ownerDone:   make(chan struct{}),
	}
}

func supportsManagedREPL() bool {
	if !isTerminal() || os.Getenv("TERM") == "dumb" {
		return false
	}
	_, _, err := term.GetSize(int(os.Stderr.Fd()))
	return err == nil
}

func (ui *managedREPL) sendEvent(ev managedEvent) {
	select {
	case <-ui.done:
		return
	case ui.events <- ev:
	}
}

func (ui *managedREPL) requestExit() {
	if ui.exitRequested.CompareAndSwap(false, true) {
		ui.cancelActiveTurn()
		select {
		case ui.exitCh <- struct{}{}:
		default:
		}
	}
}

func (ui *managedREPL) setActiveTurnCancel(cancel context.CancelFunc) {
	ui.activeTurnMu.Lock()
	ui.activeTurnCancel = cancel
	ui.activeTurnMu.Unlock()
}

func (ui *managedREPL) clearActiveTurnCancel() {
	ui.activeTurnMu.Lock()
	ui.activeTurnCancel = nil
	ui.activeTurnMu.Unlock()
}

func (ui *managedREPL) cancelActiveTurn() {
	ui.activeTurnMu.Lock()
	cancel := ui.activeTurnCancel
	ui.activeTurnMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (ui *managedREPL) Install() error {
	state, err := term.MakeRaw(int(ui.in.Fd()))
	if err != nil {
		return err
	}
	ui.rawState = state
	return nil
}

func (ui *managedREPL) Close() {
	select {
	case <-ui.done:
	default:
		close(ui.done)
	}
	ui.cancelActiveTurn()
	<-ui.ownerDone
	if ui.rawState != nil {
		_ = term.Restore(int(ui.in.Fd()), ui.rawState)
		ui.rawState = nil
	}

	w, h := 1, 1
	if sw, sh, err := ui.sz.Size(); err == nil {
		w, h = sw, sh
	}
	_ = w
	var b strings.Builder
	b.WriteString(showCursor)
	if h > 1 {
		b.WriteString(cursorPos(h-1, 1))
		b.WriteString(clearLine)
	}
	b.WriteString(cursorPos(h, 1))
	b.WriteString(clearLine)
	b.WriteString(cursorPos(h, 1))
	b.WriteString("\r\n")
	fmt.Fprint(ui.out, b.String())
}

func (ui *managedREPL) Run(ctx context.Context, runTurn func(context.Context, string, TurnUI) error) error {
	if err := ui.Install(); err != nil {
		return err
	}
	setBeforeExit(ui.Close)
	defer setBeforeExit(nil)
	defer ui.Close()

	w, h, err := ui.sz.Size()
	if err != nil {
		return err
	}

	go ui.ownerLoop(ctx, newReplScreen(w, h, ui.quiet, ui.model, ui.context, ui.tools, ui.skills))
	go ui.readInputLoop()
	go ui.watchResizes()
	go ui.tickLoop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ui.exitCh:
			return nil
		case prompt := <-ui.submissions:
			turnCtx, cancel := context.WithCancel(ctx)
			ui.setActiveTurnCancel(cancel)
			err := runTurn(turnCtx, prompt, &managedTurnUI{repl: ui, config: ui.config})
			cancel()
			ui.clearActiveTurnCancel()
			if ui.exitRequested.Load() {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
}

func (ui *managedREPL) readInputLoop() {
	buf := make([]byte, 256)
	for {
		n, err := ui.in.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			ui.sendEvent(managedEvent{kind: managedEventInput, data: data})
		}
		if err != nil {
			ui.requestExit()
			return
		}
		select {
		case <-ui.done:
			return
		default:
		}
	}
}

func (ui *managedREPL) watchResizes() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	defer signal.Stop(ch)
	for {
		select {
		case <-ui.done:
			return
		case <-ch:
			w, h, err := ui.sz.Size()
			if err != nil {
				continue
			}
			ui.sendEvent(managedEvent{kind: managedEventResize, w: w, h: h})
		}
	}
}

func (ui *managedREPL) tickLoop() {
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		select {
		case <-ui.done:
			return
		case <-t.C:
			ui.sendEvent(managedEvent{kind: managedEventTick})
		}
	}
}

func (ui *managedREPL) ownerLoop(ctx context.Context, screen *replScreen) {
	defer close(ui.ownerDone)
	var pending []byte
	ui.render(screen, true)
	for {
		select {
		case <-ui.done:
			return
		case <-ctx.Done():
			return
		case ev := <-ui.events:
			full := false
			switch ev.kind {
			case managedEventInput:
				var keys []replKey
				keys, pending = parseReplKeys(pending, ev.data)
				for _, key := range keys {
					submit, exit := screen.handleKey(key)
					if exit {
						ui.requestExit()
						break
					}
					if submit != "" {
						select {
						case ui.submissions <- submit:
						default:
						}
					}
				}
			case managedEventResize:
				screen.setSize(ev.w, ev.h)
				full = true
			case managedEventTick:
			case managedEventTurnStart:
				screen.startTurn()
			case managedEventTurnStop:
				screen.stopTurn()
			case managedEventThinking:
				screen.setThinking()
			case managedEventAssistantContent:
				screen.setStreaming()
				screen.appendAssistantText(ev.text)
			case managedEventToolStart:
				if toolDisplayEnabled(ui.config) {
					for _, call := range ev.calls {
						screen.appendToolStart(call)
					}
				}
				if len(ev.calls) > 0 {
					screen.setToolState(ev.calls[0].Name)
				}
			case managedEventToolEnd:
				if toolDisplayEnabled(ui.config) {
					screen.appendToolEnd(ev.call, ev.result, ev.duration, ev.err)
				}
				if screen.busy {
					screen.setWaiting()
				}
			case managedEventTokens:
				screen.recordTurnTokens(ev.in, ev.out)
			case managedEventNotice:
				if ev.text == "" {
					screen.appendBlankIfNeeded()
				} else {
					screen.appendNotice(ev.text)
				}
			case managedEventApprovalRequest:
				screen.approval = newApprovalState(ev.calls, ev.reply)
				screen.setWaiting()
			}
			ui.render(screen, full)
		}
	}
}

func (ui *managedREPL) render(screen *replScreen, full bool) {
	frame := screen.renderFrame()
	if full || len(ui.prevFrame) != len(frame) {
		ui.prevFrame = make([]string, len(frame))
	}

	var b strings.Builder
	b.WriteString(hideCursor)
	for i, row := range frame {
		if !full && ui.prevFrame[i] == row {
			continue
		}
		b.WriteString(cursorPos(i+1, 1))
		b.WriteString(clearLine)
		b.WriteString(row)
		ui.prevFrame[i] = row
	}
	if row, col, visible := screen.cursorRowCol(); visible {
		b.WriteString(showCursor)
		b.WriteString(cursorPos(row, col))
	}
	fmt.Fprint(ui.out, b.String())
}

type managedTurnUI struct {
	repl           *managedREPL
	config         *Config
	needsSeparator bool
	contentPrinted bool
}

func (ui *managedTurnUI) Start() {
	ui.repl.sendEvent(managedEvent{kind: managedEventTurnStart})
}

func (ui *managedTurnUI) Stop() {
	ui.repl.sendEvent(managedEvent{kind: managedEventTurnStop})
}

func (ui *managedTurnUI) ShowThinking(tokens int) {
	ui.repl.sendEvent(managedEvent{kind: managedEventThinking})
}

func (ui *managedTurnUI) AppendAssistantText(content string) {
	if ui.needsSeparator {
		ui.repl.sendEvent(managedEvent{kind: managedEventNotice, text: ""})
		ui.needsSeparator = false
	}
	ui.repl.sendEvent(managedEvent{kind: managedEventAssistantContent, text: content})
	ui.contentPrinted = true
}

func (ui *managedTurnUI) AppendToolStart(calls []messages.ChatMessageToolCall) {
	if len(calls) == 0 {
		return
	}
	if toolDisplayEnabled(ui.config) && ui.contentPrinted {
		ui.repl.sendEvent(managedEvent{kind: managedEventNotice, text: ""})
		ui.contentPrinted = false
	}
	ui.needsSeparator = toolDisplayEnabled(ui.config)
	ui.repl.sendEvent(managedEvent{kind: managedEventToolStart, calls: calls})
}

func (ui *managedTurnUI) ApproveToolCalls(calls []messages.ChatMessageToolCall) []bool {
	if !ui.config.Confirm {
		approved := make([]bool, len(calls))
		for i := range approved {
			approved[i] = true
		}
		return approved
	}
	reply := make(chan []bool, 1)
	ui.repl.sendEvent(managedEvent{kind: managedEventApprovalRequest, calls: calls, reply: reply})
	results, ok := <-reply
	if !ok {
		approved := make([]bool, len(calls))
		return approved
	}
	return results
}

func (ui *managedTurnUI) AppendToolEnd(call messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
	ui.repl.sendEvent(managedEvent{
		kind:     managedEventToolEnd,
		call:     call,
		result:   result,
		duration: duration,
		err:      err,
	})
}

func (ui *managedTurnUI) AppendWarning(text string) {
	ui.repl.sendEvent(managedEvent{kind: managedEventNotice, text: "Warning: " + text})
}

func (ui *managedTurnUI) RecordTurnTokens(in, out int) {
	ui.repl.sendEvent(managedEvent{kind: managedEventTokens, in: in, out: out})
}

func (ui *managedTurnUI) FinishTextTurn() {}

func parseReplKeys(pending, input []byte) ([]replKey, []byte) {
	data := append(append([]byte(nil), pending...), input...)
	var keys []replKey
	for len(data) > 0 {
		b := data[0]
		switch b {
		case 0x01:
			keys = append(keys, replKey{kind: keyCtrlA})
			data = data[1:]
		case 0x03:
			keys = append(keys, replKey{kind: keyCtrlC})
			data = data[1:]
		case 0x04:
			keys = append(keys, replKey{kind: keyCtrlD})
			data = data[1:]
		case 0x05:
			keys = append(keys, replKey{kind: keyCtrlE})
			data = data[1:]
		case 0x0b:
			keys = append(keys, replKey{kind: keyCtrlK})
			data = data[1:]
		case 0x15:
			keys = append(keys, replKey{kind: keyCtrlU})
			data = data[1:]
		case 0x17:
			keys = append(keys, replKey{kind: keyCtrlW})
			data = data[1:]
		case '\r', '\n':
			keys = append(keys, replKey{kind: keyEnter})
			data = data[1:]
		case 0x7f:
			keys = append(keys, replKey{kind: keyBackspace})
			data = data[1:]
		case 0x1b:
			key, n, ok, needMore := parseEscapeKey(data)
			if needMore {
				return keys, data
			}
			if ok {
				keys = append(keys, key)
				data = data[n:]
			} else {
				data = data[1:]
			}
		default:
			r, n := utf8.DecodeRune(data)
			if r == utf8.RuneError && n == 1 {
				if !utf8.FullRune(data) {
					return keys, data
				}
				data = data[1:]
				continue
			}
			if r >= 0x20 {
				keys = append(keys, replKey{kind: keyRune, r: r})
			}
			data = data[n:]
		}
	}
	return keys, nil
}

func parseEscapeKey(data []byte) (replKey, int, bool, bool) {
	if len(data) < 2 {
		return replKey{}, 0, false, true
	}
	if data[1] == '[' {
		if len(data) < 3 {
			return replKey{}, 0, false, true
		}
		switch data[2] {
		case 'A':
			return replKey{kind: keyUp}, 3, true, false
		case 'B':
			return replKey{kind: keyDown}, 3, true, false
		case 'C':
			return replKey{kind: keyRight}, 3, true, false
		case 'D':
			return replKey{kind: keyLeft}, 3, true, false
		case 'H':
			return replKey{kind: keyHome}, 3, true, false
		case 'F':
			return replKey{kind: keyEnd}, 3, true, false
		case '1', '3', '4', '5', '6', '7', '8':
			if len(data) < 4 {
				return replKey{}, 0, false, true
			}
			if data[3] != '~' {
				return replKey{}, 0, false, false
			}
			switch data[2] {
			case '1', '7':
				return replKey{kind: keyHome}, 4, true, false
			case '3':
				return replKey{kind: keyDelete}, 4, true, false
			case '4', '8':
				return replKey{kind: keyEnd}, 4, true, false
			case '5':
				return replKey{kind: keyPageUp}, 4, true, false
			case '6':
				return replKey{kind: keyPageDown}, 4, true, false
			}
		}
	}
	if data[1] == 'O' {
		if len(data) < 3 {
			return replKey{}, 0, false, true
		}
		switch data[2] {
		case 'H':
			return replKey{kind: keyHome}, 3, true, false
		case 'F':
			return replKey{kind: keyEnd}, 3, true, false
		}
	}
	return replKey{}, 0, false, false
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func runManagedREPL(ctx context.Context, config *Config, state *conversationState) error {
	ui := newManagedREPL(config, state.session.GetName(), toolCount(state.toolRegistry), skillCount(state.skillCatalog))
	return ui.Run(ctx, func(turnCtx context.Context, prompt string, turnUI TurnUI) error {
		return executeTurn(turnCtx, config, state, prompt, nil, nil, turnUI)
	})
}

func runFallbackREPL(ctx context.Context, config *Config, state *conversationState) error {
	reader := bufio.NewReader(os.Stdin)
	return runREPLLoop(reader, os.Stderr, func(prompt string) error {
		return executeTurn(ctx, config, state, prompt, nil, reader, nil)
	})
}

var errManagedREPLUnsupported = errors.New("managed repl unsupported")
