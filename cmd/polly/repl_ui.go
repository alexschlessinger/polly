package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	tcell "github.com/gdamore/tcell/v3"
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

// turnOutcome is the last settled result shown in the status row while the
// composer is idle. It deliberately lives outside the transcript: completing a
// turn must not append chrome that shifts the answer vertically.
type turnOutcome int

const (
	turnOutcomeNone turnOutcome = iota
	turnOutcomeDone
	turnOutcomeFailed
	turnOutcomeCanceled
)

// init registers polly's semantic accent colors. Each name maps to an ANSI
// palette slot (XTerm 0–15) that the terminal (e.g. Ghostty) remaps to the
// active theme — unlike gotui's dark*/cyan names, which resolve to fixed RGB
// (e.g. darkred = 0x8B0000) and ignore the theme. Quiet variants are produced
// with the "dim" modifier at the call site, not a darker fixed color.
func init() {
	ui.StyleParserColorMap["ok"] = ui.ColorGreen      // success ✓
	ui.StyleParserColorMap["err"] = ui.ColorRed       // failure ✗ / errors
	ui.StyleParserColorMap["run"] = ui.ColorTeal      // running-tool arrow (ANSI cyan, XTerm6)
	ui.StyleParserColorMap["accent"] = ui.ColorBlue   // prompts & interactive markers
	ui.StyleParserColorMap["active"] = ui.ColorYellow // status-bar active turn
	ui.StyleParserColorMap["muted"] = ui.ColorGrey    // metadata (ANSI bright-black, XTerm8)
	ui.StyleParserColorMap["code"] = ui.ColorWhite    // fenced code block contents
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

// Chroma can split source punctuation into individual tokens. A token that is
// literally "[" or "]" cannot be nested safely inside gotui's own
// [text](style) syntax, so code rendering substitutes private runes until after
// ParseStyles has assigned cell styles. The transcript parser restores the
// original source characters before wrapping or drawing.
const (
	styledLiteralOpenBracket  rune = '\ue100'
	styledLiteralCloseBracket rune = '\ue101'
)

var styledLiteralBracketReplacer = strings.NewReplacer(
	"[", string(styledLiteralOpenBracket),
	"]", string(styledLiteralCloseBracket),
)

func styledCodeLiteral(text, fg, modifier string) string {
	text = styledLiteralBracketReplacer.Replace(text)
	return styled(text, fg, modifier)
}

func parseStyledCells(text string, defaultStyle ui.Style) []ui.Cell {
	cells := ui.ParseStyles(text, defaultStyle)
	for i := range cells {
		switch cells[i].Rune {
		case styledLiteralOpenBracket:
			cells[i].Rune = '['
		case styledLiteralCloseBracket:
			cells[i].Rune = ']'
		}
	}
	return cells
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

func (e *lineEditor) deleteForward() {
	if e.cursor < len(e.buf) {
		e.buf = append(e.buf[:e.cursor], e.buf[e.cursor+1:]...)
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

func (e *lineEditor) home() { e.cursor = e.lineStartAt(e.cursor); e.goalCol = -1 }
func (e *lineEditor) end()  { e.cursor = e.lineEndAt(e.cursor); e.goalCol = -1 }

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

// managedTurnInput is the immutable boundary between accepting composer input
// and running a model turn. displayText is UI-only; userMessage is the exact
// normalized payload carried through queues and restored composer drafts.
type managedTurnInput struct {
	displayText string
	userMessage messages.ChatMessage
}

// turnPersistenceAck belongs to one logical user turn. Provider goroutines can
// mark it without taking the model lock, so queue projection never observes a
// persisted session message while its acknowledgement is blocked behind the
// event loop. Keeping the pointer turn-owned makes late callbacks harmless: an
// old callback can only update the old turn (or an unchanged restored-draft
// resubmission), never a newer prompt.
type turnPersistenceAck struct {
	mu        sync.Mutex
	active    int
	persisted bool
	settled   chan struct{}
}

func newTurnPersistenceAck(persisted bool) *turnPersistenceAck {
	return &turnPersistenceAck{persisted: persisted}
}

func (a *turnPersistenceAck) beginPersistence() {
	if a == nil {
		return
	}
	for {
		a.mu.Lock()
		if a.active > 0 {
			settled := a.settled
			a.mu.Unlock()
			<-settled
			continue
		}
		a.settled = make(chan struct{})
		a.active = 1
		a.mu.Unlock()
		return
	}
}

func (a *turnPersistenceAck) finishPersistence(persisted bool) {
	if a == nil {
		return
	}
	a.mu.Lock()
	if persisted {
		a.persisted = true
	}
	if a.active > 0 {
		a.active--
		if a.active == 0 {
			close(a.settled)
		}
	}
	a.mu.Unlock()
}

// snapshotSessionHistory returns a session snapshot consistent with the turn's
// persistence state. Persistence begins under this same turn-local lock before
// AddMessage and ends after it returns. Projection either snapshots before that
// interval (and includes the not-yet-persisted user itself) or waits until the
// interval is over and observes the stored user; it can never see the session
// write without its acknowledgement.
func (a *turnPersistenceAck) snapshotSessionHistory(ctx context.Context, session sessions.Session) ([]messages.ChatMessage, bool, error) {
	if a == nil {
		history, err := session.GetHistory(ctx)
		return history, false, err
	}
	for {
		a.mu.Lock()
		if a.active > 0 {
			settled := a.settled
			a.mu.Unlock()
			<-settled
			continue
		}
		history, err := session.GetHistory(ctx)
		persisted := a.persisted
		a.mu.Unlock()
		return history, persisted, err
	}
}

type queuedREPLInput struct {
	text            string
	turn            *managedTurnInput
	transcriptIndex int
	transcriptShown bool
}

// materializeQueuedImagesForReset snapshots prepared queued images before the
// session namespace is cleared. The returned queue is self-contained: callers
// may safely remove every artifact and then externalize these exact bytes into
// the fresh namespace. Caller must hold m.mu.
func (m *replModel) materializeQueuedImagesForReset(ctx context.Context) ([]queuedREPLInput, error) {
	queue := make([]queuedREPLInput, len(m.queue))
	copy(queue, m.queue)
	for i := range queue {
		if queue[i].turn == nil {
			continue
		}
		turn := cloneManagedTurn(*queue[i].turn)
		message, err := materializeArtifactImageParts(ctx, turn.userMessage, m.artifactStore)
		if err != nil {
			return nil, err
		}
		turn.userMessage = message
		queue[i].turn = &turn
	}
	return queue, nil
}

func materializeArtifactImageParts(ctx context.Context, msg messages.ChatMessage, store artifacts.Store) (messages.ChatMessage, error) {
	msg = cloneChatMessage(msg)
	for i, part := range msg.Parts {
		if part.Artifact == nil || part.Artifact.Kind != artifacts.KindImage {
			continue
		}
		ref := part.Artifact
		if store == nil {
			return messages.ChatMessage{}, fmt.Errorf("image artifact %s has no session store", ref.ID)
		}
		if !artifacts.ValidID(ref.ID) || ref.Bytes < 0 || ref.Bytes > int64(maxLocalImageBytes) {
			return messages.ChatMessage{}, fmt.Errorf("image artifact %s has invalid metadata", ref.ID)
		}
		r, err := store.Open(ctx, ref.ID)
		if err != nil {
			return messages.ChatMessage{}, fmt.Errorf("read queued image artifact %s: %w", ref.ID, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(r, ref.Bytes+1))
		closeErr := r.Close()
		if readErr != nil {
			return messages.ChatMessage{}, fmt.Errorf("read queued image artifact %s: %w", ref.ID, readErr)
		}
		if closeErr != nil {
			return messages.ChatMessage{}, fmt.Errorf("close queued image artifact %s: %w", ref.ID, closeErr)
		}
		if int64(len(data)) != ref.Bytes {
			return messages.ChatMessage{}, fmt.Errorf("queued image artifact %s size changed", ref.ID)
		}
		reference := part.Reference
		if reference == "" {
			reference = ref.ImageToken
		}
		if reference == "" {
			reference = ref.Reference
		}
		msg.Parts[i] = messages.ContentPart{
			Type: "image_base64", ImageData: base64.StdEncoding.EncodeToString(data),
			MimeType: ref.MIMEType, FileName: ref.Name, Reference: reference,
		}
	}
	return msg, nil
}

// restoreQueuedImagesAfterReset repopulates only artifacts referenced by
// future queued turns and rebuilds their stable-token registry. On failure the
// remaining prepared bytes stay inline until those turns are marked not sent.
// Caller must hold m.mu.
func (m *replModel) restoreQueuedImagesAfterReset(ctx context.Context, queue []queuedREPLInput) error {
	m.queue = queue
	m.attachments = make(map[int]composerAttachment)
	m.ambiguousAttachments = make(map[int]bool)
	// attachmentSeq stays monotonic across the reset: input-recall history and
	// unsubmitted drafts survive it carrying old tokens, and reusing a number
	// would silently rebind such a token to a different file. A cleared token
	// now fails as unknown instead.
	for i := range m.queue {
		if m.queue[i].turn == nil {
			continue
		}
		turn := cloneManagedTurn(*m.queue[i].turn)
		externalized, err := externalizeMessageImages(ctx, turn.userMessage, m.artifactStore)
		if err != nil {
			m.queue[i].turn = &turn
			return err
		}
		turn.userMessage = externalized
		m.rememberArtifactAttachments(turn.userMessage)
		m.queue[i].turn = &turn
	}
	return nil
}

func textManagedTurn(prompt string) managedTurnInput {
	return managedTurnInput{
		displayText: prompt,
		userMessage: messages.ChatMessage{Role: messages.MessageRoleUser, Content: prompt},
	}
}

func cloneManagedTurn(turn managedTurnInput) managedTurnInput {
	turn.userMessage = cloneChatMessage(turn.userMessage)
	return turn
}

func cloneChatMessage(msg messages.ChatMessage) messages.ChatMessage {
	msg.Parts = append([]messages.ContentPart(nil), msg.Parts...)
	for i := range msg.Parts {
		if msg.Parts[i].Artifact != nil {
			ref := *msg.Parts[i].Artifact
			msg.Parts[i].Artifact = &ref
		}
	}
	msg.ToolCalls = append([]messages.ChatMessageToolCall(nil), msg.ToolCalls...)
	if msg.Metadata != nil {
		metadata := make(map[string]any, len(msg.Metadata))
		for key, value := range msg.Metadata {
			metadata[key] = value
		}
		msg.Metadata = metadata
	}
	return msg
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
	// transcriptImages is a private sidecar keyed by transcript entry. String
	// interfaces stay unchanged while the managed TUI can give explicit local
	// image references stable slots in scrollback.
	transcriptImages map[int][]transcriptImage
	imageBaseDir     string
	nativeImages     bool
	imageCellWidth   int
	imageCellHeight  int
	artifactStore    artifacts.Store
	// imagePlacements is the last rendered frame's native thumbnail geometry,
	// in absolute screen cells, kept for mouse-click hit-testing.
	imagePlacements []terminalImagePlacement

	// attachments maps "[image #N]" composer tokens to validated local images
	// or durable image artifacts for this session. Tokens resolve once when the
	// composer accepts a prompt; queues and restored drafts retain prepared references.
	attachments          map[int]composerAttachment
	ambiguousAttachments map[int]bool
	attachmentSeq        int
	// clipboardCapture serializes Ctrl+V: one platform clipboard read may be
	// in flight at a time.
	clipboardCapture bool
	// pasteBuf accumulates one bracketed paste so its complete text can be
	// inspected (drag-dropped image paths become attachments) before anything
	// reaches the editor.
	pasteBuf []rune

	// currentAssistant points at the entry that the agent is currently
	// streaming into, or -1 when no streaming entry exists.
	currentAssistant int

	// streamRaw accumulates the in-flight assistant message's raw markdown;
	// streamShown is how many of its bytes are currently rendered. The whole
	// message re-renders through goldmark per growth of the visible prefix —
	// bounded by message size, never conversation size — while unclosed inline
	// markup near the tail is held back (safeVisibleLen) so text lands on
	// screen already styled instead of visibly transforming.
	streamRaw   strings.Builder
	streamShown int

	// flatCache memoizes flattenTranscript's result; nil means "stale". Every
	// transcript mutation clears it so the next flatten recomputes. render(),
	// visibleTranscript and scrollBy all flatten, often without an intervening
	// change (idle typing, scrolling, waiting for the first token), so the cache
	// turns those into O(1) instead of re-splitting the whole backlog each time.
	flatCache []string
	// visualCache keeps the fully styled/wrapped transcript for the current
	// terminal width. Busy spinner ticks can then redraw only visible rows
	// without reparsing a long, unchanged context 20 times per second.
	visualCache             [][]ui.Cell
	visualCacheWidth        int
	visualCacheValid        bool
	visualCacheNativeImages bool
	visualCacheCellWidth    int
	visualCacheCellHeight   int
	visualBlocks            []transcriptVisualBlock

	// slashHints is a transient command-completion hint derived from the
	// composer: while a single-line input starts with "/", the matching
	// commands (or the active command's argument keywords) render as a muted
	// line near the transcript. It is not part of the transcript or persistent
	// history. Esc hides the line until the input text next changes;
	// slashHintSource tracks which text the hidden flag applies to.
	slashHints       string
	slashHintsHidden bool
	slashHintSource  string
	// activeTools tracks tool calls currently executing, each pinned to the
	// transcript entry that displays it. While a tool runs, render() rewrites
	// that entry every frame with a breathing arrow and live elapsed time; when
	// it finishes the entry is frozen into the final ✓/✗ line. Parallel calls
	// finish out of order, so each is matched back to its line by call ID.
	activeTools []activeTool
	// toolWindow pins the transcript entries of the current turn's visible
	// tool rows, oldest first. At most toolWindowSize rows stay visible;
	// older rows fold into the turn's rollup line.
	toolWindow []int
	// toolRollupIndex is the transcript entry holding this turn's "… N tool
	// calls" rollup, or -1 while every call is still visible.
	toolRollupIndex int
	// turnToolCalls counts every tool call started this turn, visible or
	// folded — the total the rollup line reports.
	turnToolCalls int

	ed           lineEditor
	busy         bool
	canceling    bool
	turnID       int64
	pasting      bool // inside a bracketed paste; runes go in verbatim
	approval     *approvalState
	history      []string
	historyIdx   int
	historyDraft string

	// queue holds inputs submitted while a turn is in flight (the prompt stays
	// editable during a turn). Commands remain text-only; prompts carry the
	// exact prepared message accepted from the composer.
	queue []queuedREPLInput

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
	totalIn     int
	totalOut    int
	lastElapsed time.Duration
	lastOutcome turnOutcome

	// thinkingChars accumulates the streamed reasoning text length this turn;
	// the status row shows a ~chars/4 token estimate while state is thinking.
	thinkingChars int

	// Live reasoning block. thinkingIndex is the transcript entry showing the
	// current segment (-1 when none is open); thinkingWrapped holds the lines
	// it has settled and thinkingPending the runes not yet wide enough to
	// settle into one. Text is committed a whole line at a time so the block
	// steps instead of reflowing on every chunk. thinkingStarted and
	// thinkingSegChars back the rollup the segment collapses into.
	thinkingIndex    int
	thinkingWrapped  []string
	thinkingPending  []rune
	thinkingStarted  time.Time
	thinkingSegChars int
	// thinkingSegRaw accumulates the segment's reasoning verbatim, separate
	// from the display window, which discards what scrolls past it.
	thinkingSegRaw strings.Builder

	// thinkingLog keeps each completed segment's full reasoning for this
	// session, so /thinking can show what the block only ever windowed. Not
	// persisted — providers stream reasoning once and it is dropped from the
	// assistant message, so this is the only copy and it dies with the process.
	thinkingLog []string

	// focusKnown/focused mirror the terminal's focus reports (tcell
	// EnableFocus). Desktop notifications fire only when the terminal has
	// explicitly said the user is elsewhere; with no reports they stay silent.
	focusKnown bool
	focused    bool

	// notices queues desktop-notification bodies. Turn goroutines push under
	// mu; the render loop drains and emits on the event-loop goroutine so no
	// turn goroutine ever writes to the terminal.
	notices []string

	// streamCursorFrame is the styled caret currently appended to the
	// streaming assistant block ("" when hidden). Tracked like the breathing
	// tool arrows so a pulse flip invalidates the visual cache.
	streamCursorFrame string


	// Failed/canceled input is restored to the composer. When submitted again
	// unchanged, restoreDraftNext reuses its already-persisted user message;
	// edited drafts are ordinary new turns.
	currentPrompt       string
	currentTurn         managedTurnInput
	currentPersistence  *turnPersistenceAck
	restoredDraft       *managedTurnInput
	restoredPersistence *turnPersistenceAck
	restoreDraftNext    bool
	turnHasOutput       bool
	unsavedLabeled      bool

	// runningTools counts tool calls currently in flight this turn. A parallel
	// batch starts several at once; the status only returns to "waiting" when
	// the last of them finishes, so the first to complete doesn't prematurely
	// flip the bar (and drop the running-tool name) while siblings still run.
	runningTools int
}

type transcriptVisualBlock struct {
	key        string
	text       string
	followed   bool
	rows       [][]ui.Cell
	images     []transcriptImage
	imageSpans []transcriptImageSpan
}

type approvalState struct {
	calls []messages.ChatMessageToolCall
	index int
	out   []bool
	reply chan []bool
	// viewed marks that [v]iew already expanded the current call's arguments,
	// so holding v can't spam the transcript. Reset as index advances.
	viewed bool
}

func newReplModel() *replModel {
	baseDir, _ := os.Getwd()
	m := &replModel{
		currentAssistant: -1,
		transcriptImages: make(map[int][]transcriptImage),
		imageBaseDir:     baseDir,
		historyIdx:       -1,
		state:            turnStateIdle,
		followBottom:     true,
		toolRollupIndex:  -1,
		thinkingIndex:    -1,
	}
	m.ed.goalCol = -1
	return m
}

const turnCancelDetachAfter = 2 * time.Second

// statusRow renders live/last-turn state on the left and stable context on the
// right. Right alignment means a completion can change "streaming" to
// "done 1.8s" without moving the model/context horizontally.
func (m *replModel) statusRow(width int) string {
	if m.quiet || width <= 0 {
		return ""
	}
	const sep = " · "

	leftRaw, leftStyled := m.statusActivity()
	type field struct {
		drop int
		text string
	}
	fields := []field{}
	if m.modelName != "" {
		fields = append(fields, field{drop: 3, text: m.modelName})
	}
	fields = append(fields, field{drop: 0, text: m.contextName})
	if m.toolCount > 0 {
		fields = append(fields, field{drop: 2, text: fmt.Sprintf("tools:%d", m.toolCount)})
	}
	if m.skillCount > 0 {
		fields = append(fields, field{drop: 4, text: fmt.Sprintf("skills:%d", m.skillCount)})
	}

	fieldWidth := func(fs []field) int {
		parts := make([]string, len(fs))
		for i, f := range fs {
			parts[i] = f.text
		}
		return rw.StringWidth(strings.Join(parts, sep))
	}
	needed := func() int {
		n := rw.StringWidth(leftRaw) + fieldWidth(fields)
		if leftRaw != "" && len(fields) > 0 {
			n++
		}
		return n
	}
	for needed() > width && len(fields) > 1 {
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

	// Context is the one non-droppable field. Truncate it rather than letting a
	// wide context name push operational state off-screen.
	if needed() > width && len(fields) == 1 {
		budget := width - rw.StringWidth(leftRaw)
		if leftRaw != "" {
			budget--
		}
		if budget < 0 {
			budget = 0
		}
		if budget == 0 {
			fields[0].text = ""
		} else {
			fields[0].text = rw.Truncate(fields[0].text, budget, "…")
		}
	}

	rightRawParts := make([]string, len(fields))
	rightStyledParts := make([]string, len(fields))
	for i, f := range fields {
		rightRawParts[i] = f.text
		rightStyledParts[i] = styled(f.text, "muted", "")
	}
	rightRaw := strings.Join(rightRawParts, sep)
	rightStyled := strings.Join(rightStyledParts, styled(sep, "muted", ""))

	rightWidth := rw.StringWidth(rightRaw)
	leftBudget := width - rightWidth
	if leftRaw != "" && rightRaw != "" {
		leftBudget--
	}
	if leftBudget < 0 {
		leftBudget = 0
	}
	if rw.StringWidth(leftRaw) > leftBudget {
		if leftBudget == 0 {
			leftRaw, leftStyled = "", ""
		} else {
			leftRaw = rw.Truncate(leftRaw, leftBudget, "…")
			leftStyled = styled(leftRaw, "muted", "")
		}
	}

	if rightRaw == "" {
		return leftStyled
	}
	gap := width - rw.StringWidth(leftRaw) - rightWidth
	if leftRaw != "" && gap < 1 {
		gap = 1
	}
	if gap < 0 {
		gap = 0
	}
	return leftStyled + strings.Repeat(" ", gap) + rightStyled
}

func (m *replModel) statusActivity() (raw, rendered string) {
	if m.busy {
		glyph, spinning := m.spinnerFrame()
		word := m.state.label(m.toolName)
		if m.canceling {
			word = "canceling"
		}
		meta := m.busyStatusMeta()
		if spinning {
			raw = string(glyph) + " " + word
			rendered = styled(string(glyph), "accent", "bold") + " " + styled(word, "active", "bold")
		} else {
			raw = word
			rendered = styled(word, "active", "bold")
		}
		if meta != "" {
			raw += " · " + meta
			rendered += " " + styled("· "+meta, "muted", "")
		}
		return raw, rendered
	}

	switch m.lastOutcome {
	case turnOutcomeDone:
		meta := formatElapsed(m.lastElapsed)
		if m.totalIn > 0 || m.totalOut > 0 {
			meta += fmt.Sprintf(" · %s/%s tok", humanizeTokens(m.totalIn), humanizeTokens(m.totalOut))
		}
		raw = "done " + meta
		rendered = styled("done", "ok", "bold") + " " + styled(meta, "muted", "")
	case turnOutcomeFailed:
		raw = "failed"
		rendered = styled(raw, "err", "bold")
	case turnOutcomeCanceled:
		raw = "canceled"
		rendered = styled(raw, "muted", "bold")
	}
	return raw, rendered
}

// thinkingCharsPerToken is the standard rough chars→tokens estimate used for
// the live thinking counter; the value is presented with a "~".
const thinkingCharsPerToken = 4

// thinkingBlockLines is how many settled reasoning lines stay on screen, and
// thinkingBlockMaxWidth caps how wide they wrap on a roomy terminal so the
// block reads as a quoted aside rather than full-bleed body text.
const (
	thinkingBlockLines    = 3
	thinkingBlockMaxWidth = 78
)

// thinkingBlockIndent is the gutter every reasoning line sits in; the first
// line replaces its last two columns with the caret.
const thinkingBlockIndent = "    "

// activityTicker is the pinned bottom-row notice shown while the user is
// scrolled up: how much transcript lies below the viewport, what the agent is
// doing right now, and the way back. Empty when following or nothing is below.
// Caller must hold m.mu.
func (m *replModel) activityTicker(totalRows, topRow, height int) string {
	if m.followBottom || height <= 0 {
		return ""
	}
	below := totalRows - (topRow + height)
	if below <= 0 {
		return ""
	}
	word := "rows"
	if below == 1 {
		word = "row"
	}
	raw := fmt.Sprintf("↓ %d %s below", below, word)
	if m.busy {
		raw += " · " + m.busyLabel()
		if !m.turnStarted.IsZero() {
			raw += " · " + formatElapsed(time.Since(m.turnStarted))
		}
	}
	return styled(raw, "accent", "bold") + styled(" · End to follow", "muted", "")
}

// busyStatusMeta is the muted trailer after the busy state word: a thinking
// size estimate while reasoning streams, then the live turn elapsed time.
// Caller must hold m.mu.
func (m *replModel) busyStatusMeta() string {
	meta := ""
	if m.state == turnStateThinking && !m.canceling {
		if est := m.thinkingChars / thinkingCharsPerToken; est > 0 {
			meta = "~" + humanizeTokens(est) + " tok"
		}
	}
	if !m.turnStarted.IsZero() {
		elapsed := formatElapsed(time.Since(m.turnStarted))
		if meta != "" {
			return meta + " · " + elapsed
		}
		return elapsed
	}
	return meta
}

func compactQueuePreview(text string) string {
	return truncate(strings.Join(strings.Fields(text), " "), 36)
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

// frameTitle is the desired terminal window title: app · context, then the
// live turn state so progress is readable from another window or the tab bar.
// Caller must hold m.mu.
func (m *replModel) frameTitle() string {
	title := "polly"
	if m.contextName != "" && m.contextName != "-" {
		title += " · " + m.contextName
	}
	switch {
	case m.approval != nil:
		return title + " — approval needed"
	case m.busy:
		s := title + " — " + m.busyLabel()
		if !m.turnStarted.IsZero() {
			s += " · " + coarseElapsed(time.Since(m.turnStarted))
		}
		return s
	}
	switch m.lastOutcome {
	case turnOutcomeDone:
		return title + " — done · " + formatElapsed(m.lastElapsed)
	case turnOutcomeFailed:
		return title + " — failed"
	case turnOutcomeCanceled:
		return title + " — canceled"
	}
	return title
}

// frameProgress is the desired taskbar progress payload (see terminalFX): an
// indeterminate bar while a turn runs, an error badge while a failure is the
// settled outcome, nothing otherwise. Caller must hold m.mu.
func (m *replModel) frameProgress() string {
	if m.busy {
		return progressBusy
	}
	if m.lastOutcome == turnOutcomeFailed {
		return progressFail
	}
	return progressNone
}

// coarseElapsed formats a duration at whole-second granularity for surfaces
// that shouldn't churn ten times a second (window title, notifications).
func coarseElapsed(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return formatElapsed(d)
}

// notifyMinTurn is the shortest turn whose completion is worth a desktop
// notification; anything quicker, the user never had time to look away.
const notifyMinTurn = 10 * time.Second

// pushNotice queues a desktop-notification body. Caller must hold m.mu.
func (m *replModel) pushNotice(body string) {
	m.notices = append(m.notices, body)
}

// takeNotices drains queued notifications, returning them only when the
// terminal has explicitly reported itself unfocused — a watching user needs no
// ping. Drained-but-dropped notices are gone for good; going unfocused later
// must not replay stale news. Caller must hold m.mu.
func (m *replModel) takeNotices() []string {
	out := m.notices
	m.notices = nil
	if !m.focusKnown || m.focused {
		return nil
	}
	return out
}

// invalidateFlat marks the flattened-transcript cache stale. Caller must hold
// m.mu (every transcript mutation already does).
func (m *replModel) invalidateFlat() {
	m.flatCache = nil
	m.visualCacheValid = false
}

func (m *replModel) invalidateVisual() { m.visualCacheValid = false }

func (m *replModel) setTranscriptImages(index int, images []transcriptImage) {
	if len(images) == 0 {
		delete(m.transcriptImages, index)
		return
	}
	m.transcriptImages[index] = append([]transcriptImage(nil), images...)
}

func (m *replModel) deleteTranscriptEntry(index int) {
	m.transcript = append(m.transcript[:index], m.transcript[index+1:]...)
	delete(m.transcriptImages, index)
	for i := index + 1; i <= len(m.transcript); i++ {
		if images, ok := m.transcriptImages[i]; ok {
			m.transcriptImages[i-1] = images
			delete(m.transcriptImages, i)
		}
	}
	for i := range m.queue {
		if !m.queue[i].transcriptShown {
			continue
		}
		switch {
		case m.queue[i].transcriptIndex == index:
			m.queue[i].transcriptShown = false
		case m.queue[i].transcriptIndex > index:
			m.queue[i].transcriptIndex--
		}
	}
}

// appendLine appends a pre-rendered transcript entry (may contain inline
// style markup). A non-assistant boundary first settles any active assistant
// block so pending fence text and provider terminal newlines cannot leak across
// notices, warnings, tools, or user turns.
func (m *replModel) appendLine(s string) {
	if m.currentAssistant >= 0 && m.currentAssistant < len(m.transcript) {
		m.finishAssistantBlock("")
	}
	m.transcript = append(m.transcript, s)
	m.currentAssistant = -1
	m.invalidateFlat()
}

func (m *replModel) resetAssistantStream() {
	m.streamRaw.Reset()
	m.streamShown = 0
}

// appendAssistant accumulates streamed model output into the current
// assistant entry, rendering the visible (holdback-trimmed) prefix of the raw
// markdown through goldmark. Unchanged visible prefixes skip the re-render, so
// a chunk that only extends a held-back token costs nothing.
func (m *replModel) appendAssistant(text string) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if text == "" {
		return
	}
	if m.currentAssistant < 0 || m.currentAssistant >= len(m.transcript) {
		// Start a fresh assistant entry (currentAssistant is reset to -1 by any
		// intervening non-assistant line), so the builder starts empty too.
		m.resetAssistantStream()
		m.transcript = append(m.transcript, "")
		m.currentAssistant = len(m.transcript) - 1
	}
	m.streamRaw.WriteString(text)
	raw := m.streamRaw.String()
	visible := raw[:safeVisibleLen(raw)]
	if len(visible) == m.streamShown && m.transcript[m.currentAssistant] != "" {
		return
	}
	m.streamShown = len(visible)
	rendered, images := renderMarkdownWithLocalImages(visible, m.imageBaseDir)
	m.transcript[m.currentAssistant] = rendered
	m.setTranscriptImages(m.currentAssistant, images)
	m.invalidateFlat()
}

// finishAssistantStream renders any text still held back by the streaming
// holdback — at settle time the message is final, so everything shows.
func (m *replModel) finishAssistantStream() {
	if m.currentAssistant < 0 || m.currentAssistant >= len(m.transcript) {
		return
	}
	raw := m.streamRaw.String()
	if raw == "" || m.streamShown >= len(raw) {
		return
	}
	m.streamShown = len(raw)
	rendered, images := renderMarkdownWithLocalImages(raw, m.imageBaseDir)
	m.transcript[m.currentAssistant] = rendered
	m.setTranscriptImages(m.currentAssistant, images)
	m.invalidateFlat()
}

// finishAssistantBlock closes the semantic assistant block that is currently
// streaming. Provider-owned terminal newlines are removed here so layout—not
// arbitrary chunk boundaries—owns the space between turns. Internal markdown
// whitespace is preserved. label, when non-empty, records that the visible
// partial block was not committed to session history.
func (m *replModel) finishAssistantBlock(label string) bool {
	if m.currentAssistant < 0 || m.currentAssistant >= len(m.transcript) {
		return false
	}
	m.finishAssistantStream()
	idx := m.currentAssistant
	content := strings.TrimRight(m.transcript[idx], "\r\n")
	if content == "" {
		m.deleteTranscriptEntry(idx)
	} else {
		m.transcript[idx] = content
	}
	m.currentAssistant = -1
	m.resetAssistantStream()
	m.invalidateFlat()
	if label != "" && content != "" {
		m.appendLine("  " + styled(label, "muted", ""))
		m.unsavedLabeled = true
	}
	return content != ""
}

func (m *replModel) labelTurnUnsaved(label string) {
	if m.unsavedLabeled || !m.turnHasOutput {
		return
	}
	m.appendLine("  " + styled(label, "muted", ""))
	m.unsavedLabeled = true
}

func formattedUserPrompt(p string) string {
	lines := strings.Split(p, "\n")
	for i, line := range lines {
		if i == 0 {
			lines[i] = styled("> ", "accent", "bold") + styleEscape(line)
		} else {
			lines[i] = "  " + styleEscape(line)
		}
	}
	return strings.Join(lines, "\n")
}

func (m *replModel) appendUserPrompt(p string) {
	m.appendLine(formattedUserPrompt(p))
}

// appendTurnSeparator inserts the single renderer-owned blank row between
// settled transcript activity and the next user turn. No completion path adds
// spacing, which keeps an answer stationary when its status changes to done.
func (m *replModel) appendTurnSeparator() {
	if len(m.transcript) == 0 || m.transcript[len(m.transcript)-1] == "" {
		return
	}
	m.appendLine("")
}

// activeTool is one still-executing tool call, pinned to the transcript entry
// that displays it.
type activeTool struct {
	id      string
	index   int
	label   string
	started time.Time
}

// appendToolStartLine adds a transcript entry for a tool that just began and
// starts tracking it so render() can animate it. The placeholder text is
// rewritten on the very next frame by refreshActiveTools.
func (m *replModel) appendToolStartLine(id, label string) {
	m.appendLine(runningToolLine(label, 0))
	m.activeTools = append(m.activeTools, activeTool{
		id:      id,
		index:   len(m.transcript) - 1,
		label:   label,
		started: time.Now(),
	})
	m.turnToolCalls++
	m.toolWindow = append(m.toolWindow, len(m.transcript)-1)
	m.foldToolWindow()
}

// toolWindowSize is how many of the current turn's tool rows stay visible.
// Older rows fold into a single rollup line stating the turn's running
// total, so a long tool run cannot push the surrounding prose off-screen.
const toolWindowSize = 3

// toolRollupLine renders the stand-in for every folded tool row. total is
// the whole turn's call count, visible rows included; the line only exists
// once that count exceeds toolWindowSize.
func toolRollupLine(total int) string {
	return "  " + styled("…", "muted", "bold") + " " +
		styled(fmt.Sprintf("%d tool calls", total), "muted", "")
}

// foldToolWindow enforces toolWindowSize over the current turn's tool rows.
// The first fold converts a row's slot into the rollup line; later folds
// delete their row outright. A folded call survives only in the rollup's
// count.
//
// Rows fold oldest-first, but a still-running row is skipped rather than
// blocking the ones behind it: a call in a parallel batch releases its row as
// soon as it settles, so an oversized batch drains steadily instead of
// standing at full height and collapsing in one step. What stays on screen is
// therefore every in-flight call plus the most recent settled rows, and the
// area shrinks as the batch completes. While folding is skipping a running
// row the rollup can briefly sit below it; that resolves when the call
// settles and its row folds in turn.
func (m *replModel) foldToolWindow() {
	for len(m.toolWindow) > toolWindowSize {
		pos, ok := m.oldestSettledToolRow()
		if !ok {
			// Every visible row is still executing. Nothing may fold yet, so
			// the window holds above the cap until calls start landing.
			break
		}
		idx := m.toolWindow[pos]
		m.toolWindow = append(m.toolWindow[:pos], m.toolWindow[pos+1:]...)
		switch {
		case m.toolRollupIndex < 0:
			m.placeToolRollup(idx)
		case idx < m.toolRollupIndex:
			// Calls fold in completion order, so a row earlier than the rollup
			// can fold after it was planted. Move the summary up into that slot
			// and drop its old row, keeping it above every row it stands for
			// rather than stranded among the survivors.
			old := m.toolRollupIndex
			m.placeToolRollup(idx)
			m.anchorForRemovedLines(m.entryFlatStart(old), m.entryLineCount(old))
			m.removeToolRow(old)
		default:
			m.anchorForRemovedLines(m.entryFlatStart(idx), m.entryLineCount(idx))
			m.removeToolRow(idx)
		}
	}
	m.refreshToolRollup()
}

// placeToolRollup turns a folded row's slot into the rollup line. A row
// carrying tool output images is multi-line; it collapses to the single
// rollup line, and those images go with it. Caller must hold m.mu.
func (m *replModel) placeToolRollup(index int) {
	m.anchorForRemovedLines(m.entryFlatStart(index)+1, m.entryLineCount(index)-1)
	m.transcript[index] = toolRollupLine(m.turnToolCalls)
	m.setTranscriptImages(index, nil)
	m.toolRollupIndex = index
	m.invalidateFlat()
}

// oldestSettledToolRow returns the window position of the earliest row whose
// call has finished — the next one eligible to fold. Reports false when every
// visible row is still executing. Caller must hold m.mu.
func (m *replModel) oldestSettledToolRow() (int, bool) {
	for pos, idx := range m.toolWindow {
		if !m.toolRowRunning(idx) {
			return pos, true
		}
	}
	return 0, false
}

// toolRowRunning reports whether a transcript entry is a tool row whose call
// is still executing. Pins are matched by transcript index, so a stale entry
// left by an interrupted turn cannot hold a row open once its index is gone.
// Caller must hold m.mu.
func (m *replModel) toolRowRunning(index int) bool {
	for _, at := range m.activeTools {
		if at.index == index {
			return true
		}
	}
	return false
}

// refreshToolRollup restates the rollup line with the turn's running total.
// The count keeps climbing as calls start, so a window held open by in-flight
// calls still reports honestly. Caller must hold m.mu.
func (m *replModel) refreshToolRollup() {
	if m.toolRollupIndex < 0 || m.toolRollupIndex >= len(m.transcript) {
		return
	}
	next := toolRollupLine(m.turnToolCalls)
	if m.transcript[m.toolRollupIndex] != next {
		m.transcript[m.toolRollupIndex] = next
		m.invalidateFlat()
	}
}

// entryLineCount reports how many flattened rows a transcript entry
// occupies, mirroring flattenTranscript's treatment of the provisional
// streaming entry. Caller must hold m.mu.
func (m *replModel) entryLineCount(index int) int {
	if index < 0 || index >= len(m.transcript) {
		return 0
	}
	e := m.transcript[index]
	if index == m.currentAssistant {
		if e = strings.TrimRight(e, "\r\n"); e == "" {
			return 0
		}
	}
	return strings.Count(e, "\n") + 1
}

// entryFlatStart is a transcript entry's first row in flattened coordinates —
// the space scrollAnchor is expressed in. Caller must hold m.mu.
func (m *replModel) entryFlatStart(index int) int {
	start := 0
	for i := 0; i < index; i++ {
		start += m.entryLineCount(i)
	}
	return start
}

// anchorForRemovedLines keeps a scrolled-away viewport visually still when
// count rows disappear at flattened row `at`. Text above the anchor pulls it
// up by as much as vanished; text below it never moves the anchor. While
// following the bottom the render trims to the tail anyway, so there is
// nothing to correct. Caller must hold m.mu.
func (m *replModel) anchorForRemovedLines(at, count int) {
	if m.followBottom || count <= 0 {
		return
	}
	switch {
	case at+count <= m.scrollAnchor:
		m.scrollAnchor -= count
	case at < m.scrollAnchor:
		m.scrollAnchor = at
	}
	if m.scrollAnchor < 0 {
		m.scrollAnchor = 0
	}
}

// resetToolWindow drops the previous turn's window state. The rollup and its
// visible rows stay in the transcript as scrollback; the next turn starts
// counting from zero and folds into a rollup line of its own. Caller must
// hold m.mu.
func (m *replModel) resetToolWindow() {
	m.toolWindow = nil
	m.toolRollupIndex = -1
	m.turnToolCalls = 0
}

// removeToolRow deletes a folded tool row and repoints every index-pinned
// reference past it: deleteTranscriptEntry already shifts images and queue
// echoes; the tool window, rollup slot, streaming block, and (defensively)
// running-tool pins are fixed up here.
func (m *replModel) removeToolRow(index int) {
	m.deleteTranscriptEntry(index)
	m.invalidateFlat()
	for i := range m.activeTools {
		if m.activeTools[i].index > index {
			m.activeTools[i].index--
		}
	}
	for i := range m.toolWindow {
		if m.toolWindow[i] > index {
			m.toolWindow[i]--
		}
	}
	if m.toolRollupIndex > index {
		m.toolRollupIndex--
	}
	if m.currentAssistant > index {
		m.currentAssistant--
	}
}

// arrowPulse breathes the running-tool arrow between two brightnesses of one
// themed hue: it alternates the modifier (bold ↔ dim) on a fixed color so the
// arrow gently pulses while a tool executes — and follows the terminal theme.
var arrowPulse = []string{"bold", "dim"}

// arrowPulsePeriod is how long each pulse shade holds; len(arrowPulse) steps
// make one full breath (~1s), slow enough to read as a pulse, not a strobe.
const arrowPulsePeriod = 500 * time.Millisecond

// runningToolLine renders a still-executing tool entry: a breathing arrow whose
// modifier is chosen from elapsed time, the label, and a live elapsed timer.
func runningToolLine(label string, elapsed time.Duration) string {
	mod := arrowPulse[int(elapsed/arrowPulsePeriod)%len(arrowPulse)]
	return "  " + styled("→", "run", mod) + " " +
		styled(label, "muted", "") + " " +
		styled("· "+formatElapsed(elapsed), "muted", "")
}

// streamCursorGlyph is the caret appended to the streaming assistant block —
// the classic "the model is typing here" marker.
const streamCursorGlyph = "▍"

// streamCursorNow returns the styled caret for the current pulse phase, or ""
// when no assistant text is actively streaming. It breathes on the same
// bold↔dim cycle as the running-tool arrows. Caller must hold m.mu.
func (m *replModel) streamCursorNow() string {
	if !m.busy || m.canceling || m.state != turnStateStreaming || m.turnStarted.IsZero() {
		return ""
	}
	mod := arrowPulse[int(time.Since(m.turnStarted)/arrowPulsePeriod)%len(arrowPulse)]
	return styled(streamCursorGlyph, "accent", mod)
}

// refreshStreamCursor recomputes the caret frame, invalidating the visual
// cache only when it changes — the caret is display-only chrome and never
// enters the transcript. Caller must hold m.mu.
func (m *replModel) refreshStreamCursor() {
	next := m.streamCursorNow()
	if next != m.streamCursorFrame {
		m.streamCursorFrame = next
		m.invalidateVisual()
	}
}

// appendThinking takes one streamed reasoning chunk. It opens a block on the
// first chunk of a segment and buffers the text; the wrapping into display
// lines happens at render, where the terminal width is known. Caller must
// hold m.mu.
func (m *replModel) appendThinking(chunk string) {
	if chunk == "" {
		return
	}
	if m.thinkingIndex < 0 {
		m.appendLine(styled(thinkingBlockIndent+"…", "muted", "italic"))
		m.thinkingIndex = len(m.transcript) - 1
		m.thinkingStarted = time.Now()
		m.thinkingWrapped = nil
		m.thinkingPending = nil
		m.thinkingSegChars = 0
		m.thinkingSegRaw.Reset()
	}
	m.thinkingSegChars += len(chunk)
	m.thinkingSegRaw.WriteString(chunk)
	m.thinkingPending = append(m.thinkingPending, []rune(chunk)...)
}

// refreshThinkingBlock settles buffered reasoning into display lines for the
// current width and repaints the block. Called from render, which is the only
// place the terminal width is known, so a resize simply re-wraps whatever has
// not settled yet. Caller must hold m.mu.
func (m *replModel) refreshThinkingBlock(width int) {
	if m.thinkingIndex < 0 || m.thinkingIndex >= len(m.transcript) {
		return
	}
	content := min(width-len(thinkingBlockIndent), thinkingBlockMaxWidth)
	if content < 16 {
		content = 16
	}
	settled := m.settleThinkingLines(content)
	next := m.thinkingBlockText()
	if settled || m.transcript[m.thinkingIndex] != next {
		m.transcript[m.thinkingIndex] = next
		m.invalidateFlat()
	}
}

// settleThinkingLines moves pending reasoning into finished lines, breaking on
// word boundaries at the given width. Only whole lines are committed, so the
// block advances a line at a time instead of reflowing on every chunk; the
// unsettled remainder stays hidden until it fills a line. Reports whether
// anything settled. Caller must hold m.mu.
func (m *replModel) settleThinkingLines(width int) bool {
	settled := false
	for {
		line, rest, ok := takeThinkingLine(m.thinkingPending, width)
		if !ok {
			break
		}
		m.thinkingWrapped = append(m.thinkingWrapped, line)
		m.thinkingPending = rest
		settled = true
		// Only the visible tail is kept for display; the full text lives in
		// the segment's log entry when it closes.
		if len(m.thinkingWrapped) > thinkingBlockLines {
			m.thinkingWrapped = append([]string(nil), m.thinkingWrapped[len(m.thinkingWrapped)-thinkingBlockLines:]...)
		}
	}
	return settled
}

// takeThinkingLine peels one full display line off the pending reasoning,
// breaking at the last word boundary that fits. Reports false when the text
// cannot yet fill a line. Newlines in the reasoning end a line early, so the
// model's own paragraphing survives.
func takeThinkingLine(pending []rune, width int) (line string, rest []rune, ok bool) {
	// A hard break settles the line early so the model's own paragraphing
	// survives — but only when what precedes it actually fits, otherwise the
	// paragraph is wrapped by width first and the break handled on a later pass.
	for i, r := range pending {
		if r != '\n' {
			continue
		}
		if head := strings.TrimSpace(string(pending[:i])); rw.StringWidth(head) <= width {
			return head, pending[i+1:], true
		}
		break
	}
	if rw.StringWidth(strings.TrimSpace(string(pending))) <= width {
		return "", pending, false
	}
	cut, lastSpace := 0, -1
	for i, r := range pending {
		if rw.StringWidth(string(pending[:i+1])) > width {
			break
		}
		cut = i + 1
		if r == ' ' || r == '\t' {
			lastSpace = i
		}
	}
	if lastSpace > 0 {
		cut = lastSpace
	}
	return strings.TrimSpace(string(pending[:cut])), pending[cut:], true
}

// thinkingBlockText renders the block: the settled lines in their gutter, the
// newest last, with the breathing caret marking the live first row. Caller
// must hold m.mu.
func (m *replModel) thinkingBlockText() string {
	mod := "dim"
	if !m.turnStarted.IsZero() {
		mod = arrowPulse[int(time.Since(m.turnStarted)/arrowPulsePeriod)%len(arrowPulse)]
	}
	head := styled(streamCursorGlyph, "accent", mod) + " "
	if len(m.thinkingWrapped) == 0 {
		return "  " + head + styled("thinking…", "muted", "italic")
	}
	var b strings.Builder
	for i, line := range m.thinkingWrapped {
		if i > 0 {
			b.WriteString("\n")
		}
		if i == 0 {
			b.WriteString("  " + head)
		} else {
			b.WriteString(thinkingBlockIndent)
		}
		b.WriteString(styled(line, "muted", "italic"))
	}
	return b.String()
}

// finishThinkingBlock collapses the open reasoning segment into its permanent
// one-line rollup and files the full text for /thinking. Safe to call when no
// segment is open. Caller must hold m.mu.
func (m *replModel) finishThinkingBlock() {
	if m.thinkingIndex < 0 {
		return
	}
	idx := m.thinkingIndex
	m.thinkingIndex = -1
	if full := strings.TrimSpace(m.thinkingSegRaw.String()); full != "" {
		m.thinkingLog = append(m.thinkingLog, full)
	}
	rollup := thinkingRollupLine(time.Since(m.thinkingStarted), m.thinkingSegChars)
	m.thinkingWrapped = nil
	m.thinkingPending = nil
	m.thinkingSegChars = 0
	m.thinkingSegRaw.Reset()
	if idx >= len(m.transcript) {
		return
	}
	// The block may have grown past one row; collapsing to a single line
	// shortens it, so a scrolled-away viewport is held still.
	m.anchorForRemovedLines(m.entryFlatStart(idx)+1, m.entryLineCount(idx)-1)
	m.transcript[idx] = rollup
	m.invalidateFlat()
}

// resetThinkingBlock drops any open segment's state without leaving a rollup.
// A new turn or a cleared display starts from nothing; the log of completed
// segments survives, since it is the session's only copy. Caller must hold
// m.mu.
func (m *replModel) resetThinkingBlock() {
	m.thinkingIndex = -1
	m.thinkingWrapped = nil
	m.thinkingPending = nil
	m.thinkingSegChars = 0
	m.thinkingSegRaw.Reset()
	m.thinkingStarted = time.Time{}
}

// thinkingRollupLine is what a finished reasoning segment leaves behind: how
// long the model thought and roughly how much it produced.
func thinkingRollupLine(elapsed time.Duration, chars int) string {
	body := "thought for " + formatElapsed(elapsed)
	if est := chars / thinkingCharsPerToken; est > 0 {
		body += " · ~" + humanizeTokens(est) + " tok"
	}
	return "  " + styled("⋯", "muted", "bold") + " " + styled(body, "muted", "")
}

// refreshActiveTools rewrites each running tool's transcript entry with the
// current breathing-arrow frame and live elapsed time. Caller must hold m.mu.
func (m *replModel) refreshActiveTools() {
	if len(m.activeTools) == 0 {
		return
	}
	changed := false
	for _, at := range m.activeTools {
		if at.index >= 0 && at.index < len(m.transcript) {
			next := runningToolLine(at.label, time.Since(at.started))
			if m.transcript[at.index] != next {
				m.transcript[at.index] = next
				changed = true
			}
		}
	}
	if changed {
		m.invalidateFlat()
	}
}

// takeActiveTool stops tracking a finished tool and returns the transcript index
// of its line so the caller can freeze it into a final ✓/✗ entry. It matches by
// call id, falling back to the oldest still-running entry (the only one in the
// common sequential case, and a safe default if an id is missing). Returns false
// when nothing is tracked. Caller must hold m.mu.
func (m *replModel) takeActiveTool(id string) (int, bool) {
	if len(m.activeTools) == 0 {
		return -1, false
	}
	pick := 0
	for i, at := range m.activeTools {
		if at.id == id {
			pick = i
			break
		}
	}
	idx := m.activeTools[pick].index
	m.activeTools = append(m.activeTools[:pick], m.activeTools[pick+1:]...)
	return idx, true
}

func (m *replModel) settleActiveTools(reason string) {
	if len(m.activeTools) == 0 {
		return
	}
	for _, at := range m.activeTools {
		if at.index < 0 || at.index >= len(m.transcript) {
			continue
		}
		m.transcript[at.index] = "  " + styled("✗", "err", "bold") + " " +
			styled(strings.TrimSpace(reason+" "+at.label), "muted", "")
	}
	m.invalidateFlat()
}

// toolOKLine / toolDeniedLine / toolErrorLine build the final transcript entry
// for a completed tool call. They return the styled string (rather than
// appending) so AppendToolEnd can freeze it over the running line in place.

func toolOKLine(label, duration, meta string) string {
	body := strings.TrimSpace(duration + " " + label)
	if meta != "" {
		body += " · " + meta
	}
	return "  " + styled("✓", "ok", "bold") + " " + styled(body, "muted", "")
}

func toolDeniedLine(label string) string {
	return "  " + styled("✗", "err", "bold") + " " + styled("denied "+label, "muted", "")
}

// toolErrorLine renders a failed tool call as a red ✗ plus the muted metadata
// (timing · command · exit code) — the same shape as a success line. The
// tool's own output/error text is deliberately not shown; the model still
// receives the full output, this is display only.
func toolErrorLine(label, duration, meta string) string {
	body := strings.TrimSpace(duration + " " + label)
	if meta != "" {
		body += " · " + meta
	}
	return "  " + styled("✗", "err", "bold") + " " + styled(body, "muted", "")
}

func (m *replModel) appendNoticeLine(text string) {
	m.appendLine(styled(text, "muted", ""))
}

func (m *replModel) clearDisplay() {
	m.transcript = nil
	m.transcriptImages = make(map[int][]transcriptImage)
	m.currentAssistant = -1
	m.activeTools = nil
	m.resetToolWindow()
	m.resetThinkingBlock()
	for i := range m.queue {
		m.queue[i].transcriptShown = false
	}
	if !m.busy {
		m.runningTools = 0
	}
	m.resetAssistantStream()
	m.scrollAnchor = 0
	m.followBottom = true
	m.invalidateFlat()
}

const resumedTurnLimit = 5

// hydrateHistory makes a resumed context honest about what the model already
// remembers. It shows only recent user turns, keeps assistant prose, and folds
// raw tool exchanges into compact activity rows.
func (m *replModel) hydrateHistory(history []messages.ChatMessage, contextName string) {
	for _, msg := range history {
		m.rememberArtifactAttachments(msg)
	}
	totalTurns := 0
	for _, msg := range history {
		if msg.Role == messages.MessageRoleUser && !agentSyntheticMessage(msg) {
			totalTurns++
		}
	}
	if totalTurns == 0 {
		return
	}

	showTurns := min(totalTurns, resumedTurnLimit)
	skipTurns := totalTurns - showTurns
	start := 0
	seen := 0
	for i, msg := range history {
		if msg.Role != messages.MessageRoleUser || agentSyntheticMessage(msg) {
			continue
		}
		if seen == skipTurns {
			start = i
			break
		}
		seen++
	}

	name := contextName
	if name == "" {
		name = "context"
	}
	turnWord := "turns"
	if totalTurns == 1 {
		turnWord = "turn"
	}
	if totalTurns > showTurns {
		m.appendNoticeLine(fmt.Sprintf("resumed %s · showing last %d of %d turns", name, showTurns, totalTurns))
	} else {
		m.appendNoticeLine(fmt.Sprintf("resumed %s · %d %s", name, totalTurns, turnWord))
	}

	var toolNames []string
	toolFailed := false
	toolResults := 0
	toolKnownSuccesses := 0
	flushTools := func() {
		if len(toolNames) == 0 {
			return
		}
		glyph := "·"
		color := "muted"
		if toolFailed {
			glyph = "✗"
			color = "err"
		} else if toolResults == len(toolNames) && toolKnownSuccesses == toolResults {
			glyph = "✓"
			color = "ok"
		}
		word := "tool"
		if len(toolNames) != 1 {
			word = "tools"
		}
		m.appendLine("  " + styled(glyph, color, "bold") + " " +
			styled(fmt.Sprintf("%d %s · %s", len(toolNames), word, compactToolNames(toolNames)), "muted", ""))
		toolNames = nil
		toolFailed = false
		toolResults = 0
		toolKnownSuccesses = 0
	}

	lastRole := ""
	var lastUserTurn *managedTurnInput
	lastUserContextOnly := false
	for _, msg := range history[start:] {
		switch msg.Role {
		case messages.MessageRoleUser:
			if agentSyntheticMessage(msg) {
				continue
			}
			flushTools()
			m.appendTurnSeparator()
			content, restorable, contextOnly := historyUserSummary(msg)
			m.appendUserPrompt(content)
			lastUserTurn = nil
			if turn, ok := restorableHistoryTurn(msg, content, restorable, m.artifactStore); ok {
				lastUserTurn = &turn
			}
			lastUserContextOnly = contextOnly
			lastRole = msg.Role
		case messages.MessageRoleAssistant:
			flushTools()
			if content := msg.GetContent(); content != "" {
				m.appendAssistant(content)
				m.finishAssistantBlock("")
			}
			for _, call := range msg.ToolCalls {
				name := call.Name
				if name == "" {
					name = "tool"
				}
				toolNames = append(toolNames, name)
			}
			lastRole = msg.Role
		case messages.MessageRoleTool:
			if len(toolNames) == 0 {
				name := msg.ToolName
				if name == "" {
					name = "tool"
				}
				toolNames = append(toolNames, name)
			}
			toolResults++
			if toolWasDenied(msg.Content) || msg.IsError() {
				toolFailed = true
			} else if succeeded, known := msg.ToolSucceeded(); known {
				if succeeded {
					toolKnownSuccesses++
				} else {
					toolFailed = true
				}
			}
			lastRole = msg.Role
		case messages.MessageRoleInternal:
			flushTools()
			if status, _ := msg.Metadata[messages.MetadataKeyTurnStatus].(string); status == messages.TurnStatusToolDenied {
				m.appendLine("  " + styled("✗", "err", "bold") + " " + styled("tool request denied", "muted", ""))
				// A durable internal completion marker settles the preceding user
				// turn without becoming model-visible assistant content.
				lastRole = messages.MessageRoleAssistant
			}
		}
	}
	flushTools()
	if lastRole == messages.MessageRoleUser && !lastUserContextOnly {
		if lastUserTurn != nil {
			turn := cloneManagedTurn(*lastUserTurn)
			m.restoreTurnDraft(turn, newTurnPersistenceAck(true))
			m.appendLine("  " + styled("incomplete · restored to composer", "muted", ""))
		} else {
			m.appendLine("  " + styled("incomplete", "muted", ""))
		}
	}
	m.followBottom = true
}

func agentSyntheticMessage(msg messages.ChatMessage) bool {
	synthetic, _ := msg.Metadata[messages.MetadataKeyAgentSynthetic].(bool)
	return synthetic
}

func restorableHistoryTurn(msg messages.ChatMessage, display string, simpleContent bool, store artifacts.Store) (managedTurnInput, bool) {
	contextOnly, _ := msg.Metadata[messages.MetadataKeyContextImport].(bool)
	if contextOnly || msg.Role != messages.MessageRoleUser {
		return managedTurnInput{}, false
	}
	if simpleContent {
		return cloneManagedTurn(managedTurnInput{displayText: display, userMessage: msg}), true
	}
	imageCount := 0
	for _, part := range msg.Parts {
		switch part.Type {
		case "text":
			if part.FileName != "" {
				return managedTurnInput{}, false
			}
		case "image_base64":
			if !portablePersistedImagePart(part) {
				upgraded, err := upgradeLegacyImagePart(part)
				if err != nil || upgraded.Type != "image_base64" || !portablePersistedImagePart(upgraded) {
					return managedTurnInput{}, false
				}
			}
			imageCount++
		case "image_artifact":
			if !availableImageArtifact(store, part.Artifact) {
				return managedTurnInput{}, false
			}
			imageCount++
		default:
			return managedTurnInput{}, false
		}
		if imageCount > maxPromptAttachments {
			return managedTurnInput{}, false
		}
	}
	if imageCount == 0 {
		return managedTurnInput{}, false
	}
	return cloneManagedTurn(managedTurnInput{displayText: display, userMessage: msg}), true
}

func portablePersistedImagePart(part messages.ContentPart) bool {
	return validatePortablePersistedImagePart(part) == nil
}

func validatePortablePersistedImagePart(part messages.ContentPart) error {
	if len(part.ImageData) > maxPortableEncodedImageBytes {
		return fmt.Errorf("encoded image uses %d bytes; per-image portable limit is 10,000,000 bytes (10 MB)", len(part.ImageData))
	}
	data, err := base64.StdEncoding.DecodeString(part.ImageData)
	if err != nil || len(data) == 0 {
		return fmt.Errorf("invalid or empty base64 data")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return fmt.Errorf("invalid raster image data")
	}
	if max(config.Width, config.Height) > uploadMaxLongEdge ||
		int64(config.Width)*int64(config.Height) > maxLocalImagePixels {
		return fmt.Errorf("image dimensions %dx%d exceed the prepared-image bounds", config.Width, config.Height)
	}
	if _, decodedFormat, err := image.Decode(bytes.NewReader(data)); err != nil || decodedFormat != format {
		return fmt.Errorf("invalid %s image data", format)
	}
	wantMIME := ""
	switch format {
	case "png":
		wantMIME = "image/png"
	case "jpeg":
		wantMIME = "image/jpeg"
	case "webp":
		wantMIME = "image/webp"
	default:
		return fmt.Errorf("unsupported image format %q", format)
	}
	if part.MimeType != wantMIME {
		return fmt.Errorf("image MIME %q does not match %q bytes", part.MimeType, format)
	}
	return nil
}

func historyUserSummary(msg messages.ChatMessage) (display string, restorable, contextOnly bool) {
	display = msg.Content
	contextOnly, _ = msg.Metadata[messages.MetadataKeyContextImport].(bool)
	if name, ok := legacyImportedTextFile(msg); ok {
		display = "[attached: " + name + "]"
		return display, false, true
	}
	if contextOnly && len(msg.Parts) == 0 {
		return "[context added]", false, true
	}
	var attachments []string
	if display == "" {
		for _, part := range msg.Parts {
			if part.Type == "text" && part.FileName == "" {
				display += part.Text
			}
		}
	}
	for _, part := range msg.Parts {
		if part.FileName != "" {
			attachments = append(attachments, part.FileName)
		} else if part.Type != "text" {
			attachments = append(attachments, "attachment")
		}
	}
	if len(attachments) > 0 {
		label := compactToolNames(attachments)
		if display == "" {
			display = "[attached: " + label + "]"
		} else {
			display += " [attached: " + label + "]"
		}
	}
	if display == "" {
		display = "[empty message]"
	}
	// This summary only identifies simple Content drafts. restorableHistoryTurn
	// separately recognizes persisted image_base64 parts while rejecting text
	// file bodies and context imports that cannot be reconstructed safely.
	restorable = len(msg.Parts) == 0 && msg.Content != ""
	return display, restorable, contextOnly
}

func legacyImportedTextFile(msg messages.ChatMessage) (string, bool) {
	if len(msg.Parts) != 0 || !strings.HasPrefix(msg.Content, "=== ") {
		return "", false
	}
	lineEnd := strings.IndexByte(msg.Content, '\n')
	if lineEnd < 8 {
		return "", false
	}
	header := msg.Content[:lineEnd]
	if !strings.HasSuffix(header, " ===") {
		return "", false
	}
	name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(header, "=== "), " ==="))
	return name, name != ""
}

func compactToolNames(names []string) string {
	const visible = 3
	shown := names
	if len(shown) > visible {
		shown = shown[:visible]
	}
	text := strings.Join(shown, ", ")
	if len(names) > visible {
		text += fmt.Sprintf(" +%d", len(names)-visible)
	}
	return truncate(text, 120)
}

func (m *replModel) setSlashHintLine(hint string) {
	if m.slashHints == hint {
		return
	}
	m.slashHints = hint
	m.invalidateVisual()
}

// beginTurn echoes a user prompt and marks a turn in flight. Shared by the idle
// submit path and the queued-prompt drain; neither records history here (callers
// do that when the text is first accepted). Caller must hold m.mu.
func (m *replModel) beginTurn(prompt string) {
	m.beginManagedTurn(textManagedTurn(prompt))
}

func (m *replModel) beginManagedTurn(turn managedTurnInput) {
	prompt := turn.displayText
	m.appendTurnSeparator()
	m.appendUserPrompt(prompt)
	m.decorateUserPrompt(len(m.transcript)-1, turn)
	m.beginManagedTurnState(turn)
}

func (m *replModel) decorateUserPrompt(index int, turn managedTurnInput) {
	if images := preparedMessageTranscriptImagesWithStore(turn.userMessage, m.artifactStore); len(images) > 0 {
		// The echoed prompt gains thumbnail slots for its attachments. Pasted
		// private-use runes are stripped first so they cannot pose as slot
		// anchors in an entry that now carries real ones.
		m.transcript[index] = stripTranscriptImageMarkers(m.transcript[index]) +
			"\n" + renderTranscriptImages(images, "  ")
		m.setTranscriptImages(index, images)
		m.invalidateFlat()
	}
}

// appendQueuedInput echoes accepted input without settling the assistant block
// that may still be streaming. The transcript, rather than the status bar, is
// the visible acknowledgement that Polly retained the input.
func (m *replModel) appendQueuedInput(item *queuedREPLInput) {
	if len(m.transcript) > 0 && m.transcript[len(m.transcript)-1] != "" {
		m.transcript = append(m.transcript, "")
	}
	entry := formattedUserPrompt(item.text) + "\n  " + styled("(queued)", "muted", "")
	m.transcript = append(m.transcript, entry)
	item.transcriptIndex = len(m.transcript) - 1
	item.transcriptShown = true
	if item.turn != nil {
		m.decorateUserPrompt(item.transcriptIndex, *item.turn)
	}
	m.followBottom = true
	m.invalidateFlat()
}

func (m *replModel) activateQueuedInput(item queuedREPLInput) {
	if item.transcriptShown && item.transcriptIndex >= 0 && item.transcriptIndex < len(m.transcript) {
		m.transcript[item.transcriptIndex] = formattedUserPrompt(item.text)
		if item.turn != nil {
			m.decorateUserPrompt(item.transcriptIndex, *item.turn)
		} else {
			m.invalidateFlat()
		}
		return
	}
	m.appendTurnSeparator()
	m.appendUserPrompt(item.text)
	if item.turn != nil {
		m.decorateUserPrompt(len(m.transcript)-1, *item.turn)
	}
}

func (m *replModel) markQueuedInputNotSent(item queuedREPLInput) {
	if !item.transcriptShown || item.transcriptIndex < 0 || item.transcriptIndex >= len(m.transcript) {
		return
	}
	m.transcript[item.transcriptIndex] = formattedUserPrompt(item.text) + "\n  " + styled("(not sent)", "muted", "")
	if item.turn != nil {
		m.decorateUserPrompt(item.transcriptIndex, *item.turn)
	} else {
		m.invalidateFlat()
	}
}

func (m *replModel) discardQueuedInputs() int {
	count := len(m.queue)
	for _, item := range m.queue {
		m.markQueuedInputNotSent(item)
	}
	m.queue = nil
	return count
}

func (m *replModel) beginManagedTurnState(turn managedTurnInput) {
	prompt := turn.displayText
	m.busy = true
	m.canceling = false
	m.state = turnStateWaiting
	m.runningTools = 0
	m.resetToolWindow()
	m.thinkingChars = 0
	m.resetThinkingBlock()
	m.turnStarted = time.Now()
	m.currentPrompt = prompt
	m.currentTurn = cloneManagedTurn(turn)
	if m.currentPersistence == nil {
		m.currentPersistence = newTurnPersistenceAck(false)
	}
	m.turnHasOutput = false
	m.unsavedLabeled = false
	m.lastOutcome = turnOutcomeNone
	// Token counts are per-turn and live only in the status row.
	m.lastIn = 0
	m.lastOut = 0
	m.followBottom = true
}

func editableTurnPrompt(turn managedTurnInput) string {
	if turn.userMessage.Content != "" {
		return turn.userMessage.Content
	}
	var prompt strings.Builder
	for _, part := range turn.userMessage.Parts {
		if part.Type == "text" && part.FileName == "" {
			prompt.WriteString(part.Text)
		}
	}
	if prompt.Len() > 0 {
		return prompt.String()
	}
	return turn.displayText
}

// restoreTurnDraft puts failed/canceled input back in the composer without
// overwriting anything the user typed while the turn was running. The original
// remains available through input history in that case.
func (m *replModel) restoreTurnDraft(turn managedTurnInput, persistence *turnPersistenceAck) bool {
	turn = cloneManagedTurn(turn)
	originalDisplay := turn.displayText
	turn.displayText = editableTurnPrompt(turn)
	m.rememberArtifactAttachments(turn.userMessage)
	for _, part := range turn.userMessage.Parts {
		if part.Type != "image_base64" && part.Type != "image_artifact" {
			continue
		}
		token := strings.TrimSpace(part.Reference)
		match := attachmentTokenPattern.FindStringSubmatch(token)
		validToken := len(match) == 2 && match[0] == token
		if validToken {
			attachments, err := m.promptAttachments(token)
			validToken = err == nil && len(attachments) == 1
		}
		if !validToken {
			token = m.bindRestoredImageAttachment(part)
		}
		if token != "" && !strings.Contains(turn.displayText, token) {
			if replaced, ok := replaceRestoredImagePath(turn.displayText, part.FileName, token); ok {
				turn.displayText = replaced
			} else {
				turn.displayText = strings.TrimSpace(turn.displayText + " " + token)
			}
		}
	}
	if turn.displayText != originalDisplay {
		for i := len(m.history) - 1; i >= 0; i-- {
			if m.history[i] == originalDisplay {
				m.history[i] = turn.displayText
				break
			}
		}
	}
	m.restoredDraft = &turn
	m.restoredPersistence = persistence
	if !m.ed.empty() {
		return false
	}
	m.ed.setText(turn.displayText)
	return true
}

func replaceRestoredImagePath(prompt, fileName, token string) (string, bool) {
	if fileName == "" {
		return prompt, false
	}
	for _, word := range splitPromptWords(prompt) {
		path := trimPromptPathPunctuation(word.text)
		if filepath.Base(path) != fileName {
			continue
		}
		replacement := strings.Replace(word.text, path, token, 1)
		return prompt[:word.pos] + replacement + prompt[word.pos+len(word.text):], true
	}
	return prompt, false
}

func (m *replModel) bindRestoredImageAttachment(part messages.ContentPart) string {
	var ref *artifacts.Ref
	if part.Artifact != nil && part.Artifact.Kind == artifacts.KindImage {
		copy := *part.Artifact
		ref = &copy
	} else if part.Type == "image_base64" && part.ImageData != "" && m.artifactStore != nil {
		data, err := base64.StdEncoding.DecodeString(part.ImageData)
		if err != nil || len(data) == 0 {
			return ""
		}
		stored, err := m.artifactStore.Put(context.Background(), artifacts.Blob{
			Kind: artifacts.KindImage, MIMEType: part.MimeType, Name: part.FileName, Data: data,
		})
		if err != nil {
			return ""
		}
		ref = &stored
	}
	if ref == nil {
		return ""
	}
	m.attachmentSeq++
	token := attachmentToken(m.attachmentSeq)
	ref.ImageToken = token
	m.attachments[m.attachmentSeq] = composerAttachment{Label: ref.Name, Reference: token, Artifact: ref}
	return token
}

func (m *replModel) acceptedRestoredTurn(prompt string) (managedTurnInput, *turnPersistenceAck, bool) {
	if m.restoredDraft == nil || prompt != m.restoredDraft.displayText {
		return managedTurnInput{}, nil, false
	}
	return cloneManagedTurn(*m.restoredDraft), m.restoredPersistence, true
}

func (m *replModel) clearRestoredDraft() {
	m.restoredDraft = nil
	m.restoredPersistence = nil
}

func (m *replModel) historyUp() {
	if len(m.history) == 0 || m.approval != nil {
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
	if m.historyIdx == -1 || m.approval != nil {
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
	if m.approval != nil || m.searching {
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
	return m.renderInputWithMaxRows(maxInputRows)
}

func (m *replModel) renderInputWithMaxRows(maxRows int) (text string, rows, curRow, curCol int, editable bool) {
	return m.renderInputForTerminal(maxRows, 0)
}

func (m *replModel) renderInputForTerminal(maxRows, width int) (text string, rows, curRow, curCol int, editable bool) {
	if maxRows < 1 {
		maxRows = 1
	}
	switch {
	case m.searching:
		return m.searchDisplay(), 1, 0, 0, false
	case m.approval != nil:
		return m.approvalPrompt(width), 1, 0, 0, false
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
	return strings.Join(parts, "\n"), len(visible), curRow, cc, true
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

// inputDisplay returns just the input region's text. Retained for tests that
// assert on the busy/approval overlays.
func (m *replModel) inputDisplay() string {
	text, _, _, _, _ := m.renderInput()
	return text
}

// spinnerFrames is the braille dot cycle used by the busy indicator.
var spinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// spinnerFrame returns the current braille frame for the busy animation,
// advanced off turnStarted at 100ms so the rate is independent of the render
// tick. ok is false at idle, where no spinner should show. Caller must hold m.mu.
func (m *replModel) spinnerFrame() (rune, bool) {
	if !m.busy || m.turnStarted.IsZero() {
		return 0, false
	}
	frame := spinnerFrames[int(time.Since(m.turnStarted)/(100*time.Millisecond))%len(spinnerFrames)]
	return frame, true
}

// busyIndicator renders the animated processing line: spinner + a friendly
// state word + elapsed time, e.g. "⠹ running bash · 3.8s". It is the inline
// busy row used only in quiet mode, where there is no status bar to carry the
// spinner. Caller must hold m.mu.
func (m *replModel) busyIndicator() string {
	var elapsed time.Duration
	if !m.turnStarted.IsZero() {
		elapsed = time.Since(m.turnStarted)
	}
	frame, ok := m.spinnerFrame()
	if !ok {
		frame = spinnerFrames[0]
	}
	return styled(string(frame), "accent", "bold") + " " +
		styled(m.busyLabel(), "muted", "") + " " +
		styled("· "+formatElapsed(elapsed), "muted", "")
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

	logoW       *transcriptParagraph
	transcriptW *transcriptParagraph
	dividerW    *widgets.Paragraph
	inputW      *widgets.Paragraph
	statusW     *widgets.Paragraph
	rootFlex    *widgets.Flex

	quit       chan struct{}
	suspend    chan struct{}
	pending    chan managedTurnInput
	turnCancel context.CancelFunc

	// suspendProcess stops polly's foreground process group after tcell has
	// restored the terminal. The shell resumes the group with SIGCONT on `fg`.
	// It is replaceable in tests so event handling never stops the test process.
	suspendProcess func() error

	// openImage launches the OS viewer for a clicked transcript thumbnail;
	// swappable in tests.
	openImage func(path string) error

	// uiTasks carries deferred UI mutations (e.g. a finished clipboard read)
	// onto the event loop, which repaints after running each one. Tasks take
	// the model lock themselves.
	uiTasks chan func()

	// fx drives window-level terminal effects (title, taskbar progress,
	// desktop notifications); nil outside a managed-screen Run (unit tests).
	fx *terminalFX
	// images owns native Kitty/Sixel placements. Nil means captions/paths only.
	images *terminalImageManager

	// startupLogoVisible reserves a small header above the transcript until the
	// first real turn starts. The composer and status remain live from frame one.
	startupLogoVisible bool

	// histFile is the append handle for persistent input history; nil when
	// history couldn't be opened (best-effort — never fatal).
	histFile *os.File
}

// transcriptParagraph is a small, REPL-specific paragraph renderer. gotui's
// stock Paragraph clips from the top after wrapping, which hides the newest
// rows of a long assistant message exactly when follow-bottom matters most.
// This renderer wraps before clipping and can pin overflow to the bottom.
type transcriptParagraph struct {
	ui.Block
	Text      string
	TextStyle ui.Style
	PinBottom bool
	TopRow    int
	Rows      [][]ui.Cell
	UseRows   bool
	// OverlayBottom, when non-empty, replaces the pane's bottom row — the
	// scrolled-up activity ticker. It covers one transcript row; the row is
	// still reachable by scrolling, so nothing is lost.
	OverlayBottom []ui.Cell
}

func newTranscriptParagraph() *transcriptParagraph {
	return &transcriptParagraph{
		Block:     *ui.NewBlock(),
		TextStyle: ui.Theme.Paragraph.Text,
		PinBottom: true,
	}
}

func (p *transcriptParagraph) Draw(buf *ui.Buffer) {
	p.Block.Draw(buf)
	if p.Inner.Dx() <= 0 || p.Inner.Dy() <= 0 {
		return
	}
	rows := p.Rows
	if !p.UseRows {
		rows = transcriptVisualRows(p.Text, p.TextStyle, p.Inner.Dx())
	}
	p.drawRows(buf, rows)
}

func (p *transcriptParagraph) drawRows(buf *ui.Buffer, rows [][]ui.Cell) {
	height := p.Inner.Dy()
	if height <= 0 || len(rows) == 0 {
		return
	}

	start := 0
	if p.PinBottom && len(rows) > height {
		start = len(rows) - height
	} else if !p.PinBottom {
		start = p.TopRow
		if start < 0 {
			start = 0
		}
		if start > len(rows)-1 {
			start = len(rows) - 1
		}
	}
	rows = rows[start:]
	if len(rows) > height {
		rows = rows[:height]
	}

	topPadding := 0
	if p.PinBottom && len(rows) < height {
		topPadding = height - len(rows)
	}
	for i, row := range rows {
		y := i + topPadding
		if y >= height {
			break
		}
		for _, cx := range ui.BuildCellWithXArray(row) {
			if rw.RuneWidth(cx.Cell.Rune) == 0 {
				continue
			}
			buf.SetCell(cx.Cell, image.Pt(cx.X, y).Add(p.Inner.Min))
		}
	}

	if len(p.OverlayBottom) > 0 {
		y := height - 1
		for x := 0; x < p.Inner.Dx(); x++ {
			buf.SetCell(ui.Cell{Rune: ' ', Style: ui.StyleClear}, image.Pt(x, y).Add(p.Inner.Min))
		}
		for _, cx := range ui.BuildCellWithXArray(p.OverlayBottom) {
			if cx.X >= p.Inner.Dx() || rw.RuneWidth(cx.Cell.Rune) == 0 {
				continue
			}
			buf.SetCell(cx.Cell, image.Pt(cx.X, y).Add(p.Inner.Min))
		}
	}
}

func wrapCellsHard(cells []ui.Cell, width int) []ui.Cell {
	if width <= 0 || len(cells) == 0 {
		return cells
	}
	out := make([]ui.Cell, 0, len(cells))
	col := 0
	for _, cell := range cells {
		if cell.Rune == '\n' {
			out = append(out, cell)
			col = 0
			continue
		}
		cellWidth := rw.RuneWidth(cell.Rune)
		if cellWidth > 0 && col > 0 && col+cellWidth > width {
			out = append(out, ui.Cell{Rune: '\n', Style: ui.StyleClear})
			col = 0
		}
		out = append(out, cell)
		col += cellWidth
	}
	return out
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
	r.model.historyIdx = -1
	r.model.historyDraft = ""
	r.model.history = append(r.model.history, input)
	r.appendHistory(input)
}

// prepareManagedTurnLocked resolves every attachment, externalizes prepared
// bytes when possible, and validates only this immutable queued turn. Earlier
// images are selected later by llm.Agent's model projection.
// Caller must hold r.model.mu.
func (r *managedREPL) prepareManagedTurnLocked(prompt string) (managedTurnInput, error) {
	attachments, err := r.model.promptAttachments(prompt)
	if err != nil {
		return managedTurnInput{}, fmt.Errorf("error processing attachments: %w", err)
	}
	userMessage, err := buildREPLUserMessage(prompt, attachments)
	if err != nil {
		return managedTurnInput{}, fmt.Errorf("error processing attachments: %w", err)
	}
	if r.state != nil {
		userMessage, err = externalizeMessageImages(r.state.session.Context(), userMessage, r.state.artifactStore)
		if err != nil {
			return managedTurnInput{}, fmt.Errorf("persist attachment: %w", err)
		}
	}
	r.model.rememberArtifactAttachments(userMessage)
	turn := cloneManagedTurn(managedTurnInput{displayText: prompt, userMessage: userMessage})
	if err := validatePreparedUserMessage(turn.userMessage); err != nil {
		return managedTurnInput{}, err
	}
	return turn, nil
}

// sandboxNoticeLine summarizes the sandbox posture for the REPL startup
// notice, so the operator can see at a glance which tools run restricted.
func sandboxNoticeLine(config *Config, state *conversationState) string {
	return currentSandboxPosture(config, state).noticeString()
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
		config:         config,
		model:          m,
		quit:           make(chan struct{}, 1),
		suspend:        make(chan struct{}, 1),
		pending:        make(chan managedTurnInput, 1),
		uiTasks:        make(chan func(), 8),
		openImage:      openImageInViewer,
		suspendProcess: suspendCurrentProcessGroup,
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
	// gotui inits the tcell screen with a white default foreground, and tcell
	// substitutes that default for any cell drawn with the zero style — which is
	// exactly our ColorClear body text (input + LLM responses). Reset the screen
	// default to all-defaults so unstyled text emits the terminal's own
	// foreground (SGR 39) and follows the theme instead of being forced white.
	ui.DefaultBackend.Screen.SetStyle(tcell.StyleDefault)
	// gotui enables full mouse-motion tracking during Init. Trim that to
	// button-level tracking: with tracking fully off, terminals translate the
	// wheel into arrow keys on the alt screen (alternate scroll mode), which
	// lands in history recall instead of scrolling the transcript. Button
	// tracking delivers real wheel events while leaving motion/drag alone;
	// clicks are captured but dropped (native selection needs shift/option).
	ui.DefaultBackend.Screen.EnableMouse(tcell.MouseButtonEvents)
	// Focus reports gate desktop notifications (notify only when the user is
	// known to be elsewhere); terminals without focus reporting simply never
	// flip the gate open.
	ui.DefaultBackend.Screen.EnableFocus()
	r.fx = newTerminalFX(ui.DefaultBackend.Screen)
	r.images = newTerminalImageManager(ui.DefaultBackend.Screen)
	r.model.mu.Lock()
	r.model.nativeImages = r.images != nil
	r.model.invalidateVisual()
	r.model.mu.Unlock()
	// Restore the terminal exactly once. Signal cancellation unwinds through
	// this defer; terminal effects clear first so the progress OSC goes out
	// while the screen is still up.
	var closeOnce sync.Once
	closeUI := func() {
		closeOnce.Do(func() {
			if r.images != nil {
				r.images.shutdown()
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
	r.startupLogoVisible = true
	if !r.model.quiet {
		r.model.appendNoticeLine(sandboxNoticeLine(r.config, r.state))
	}
	r.model.mu.Lock()
	r.appendPendingSandboxWarningsLocked()
	r.model.mu.Unlock()
	r.render()

	events := pollManagedEvents(ui.DefaultBackend.Screen)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	var turnDone chan error
	var cancelDetach <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			r.cancelTurn()
			r.releaseApproval()
			return context.Cause(ctx)
		case <-r.quit:
			r.cancelTurn()
			r.releaseApproval()
			return nil
		case <-r.suspend:
			if err := r.suspendUI(ui.DefaultBackend.Screen); err != nil {
				r.model.mu.Lock()
				r.model.appendNoticeLine("suspend failed: " + err.Error())
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
		case <-r.images.readyEvents():
			// CPU-heavy image preparation finishes off-thread. The event loop
			// remains the sole owner of terminal writes and cell locks.
			r.render()
		case <-ticker.C:
			if r.needsTick() {
				r.render()
			}
		case <-cancelDetach:
			cancelDetach = nil
			if turnDone != nil && r.cancelPending() {
				turnDone = r.detachCanceledTurn(ctx, runTurn)
				r.render()
			}
		case err := <-turnDone:
			turnDone = nil
			cancelDetach = nil
			r.endTurn(err)
			turnDone = r.startNextQueued(ctx, runTurn)
			r.render()
		case ev := <-events:
			if r.handleEvent(ev) {
				r.cancelTurn()
				r.releaseApproval()
				return nil
			}
			if turn, ok := r.takePending(); ok {
				turnDone = r.startManagedTurn(ctx, turn, runTurn)
				cancelDetach = nil
			}
			if turnDone == nil {
				turnDone = r.startNextQueued(ctx, runTurn)
			}
			if turnDone != nil && cancelDetach == nil && r.cancelPending() {
				cancelDetach = time.After(turnCancelDetachAfter)
			}
			if r.wantsRenderForEvent(ev) {
				r.render()
			}
		case turn := <-r.pending:
			turnDone = r.startManagedTurn(ctx, turn, runTurn)
			cancelDetach = nil
			r.render()
		case task := <-r.uiTasks:
			task()
			r.render()
		}
	}
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
	if r == nil || r.state == nil {
		return false
	}
	warnings := r.state.drainSandboxWarnings()
	for _, body := range warnings {
		r.model.appendNoticeLine("Warning: " + body)
	}
	return len(warnings) > 0
}

// needsTick reports whether the periodic render tick should repaint. Only a
// live turn animates (spinner, breathing tool arrow, elapsed timers); at idle
// nothing changes between events, so the tick repaint is skipped — otherwise
// the REPL would redraw the full screen ~20×/sec while just sitting at a prompt.
func (r *managedREPL) needsTick() bool {
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	return r.model.busy
}

// wantsRenderForEvent reports whether to repaint after handling ev. Bracketed
// paste arrives as one event per rune; those are coalesced into a single
// repaint when the paste's closing marker flips m.pasting back off (handleEvent
// clears it), so a large paste draws once instead of once per character.
func (r *managedREPL) wantsRenderForEvent(ev ui.Event) bool {
	if ev.ID == pasteStartID {
		return false
	}
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	return !r.model.pasting
}

func (r *managedREPL) takePending() (managedTurnInput, bool) {
	select {
	case p := <-r.pending:
		return p, true
	default:
		return managedTurnInput{}, false
	}
}

func (r *managedREPL) startTurn(ctx context.Context, prompt string, runTurn func(context.Context, string, TurnUI) error) chan error {
	return r.startManagedTurn(ctx, textManagedTurn(prompt), runTurn)
}

func (r *managedREPL) startManagedTurn(ctx context.Context, turn managedTurnInput, runTurn func(context.Context, string, TurnUI) error) chan error {
	r.startupLogoVisible = false
	r.model.mu.Lock()
	r.model.turnID++
	turnID := r.model.turnID
	reuseUser := r.model.restoreDraftNext
	r.model.restoreDraftNext = false
	persistence := r.model.currentPersistence
	if persistence == nil {
		persistence = newTurnPersistenceAck(false)
		r.model.currentPersistence = persistence
	}
	r.model.mu.Unlock()

	turnCtx, cancel := context.WithCancel(ctx)
	r.turnCancel = cancel
	done := make(chan error, 1)
	tui := &gotuiTurnUI{repl: r, config: r.config, turnID: turnID, reuseUser: reuseUser, turn: cloneManagedTurn(turn), persistence: persistence}
	go func() {
		done <- runTurn(turnCtx, turn.displayText, tui)
	}()
	return done
}

// startNextQueued drains inputs queued during the turn that just ended. It runs
// queued commands inline (honoring a quit) and starts a turn for the first
// queued prompt, returning that turn's done channel — or nil when the queue
// empties without a prompt. Called from the main loop after endTurn, the safe
// point where currentAssistant is already reset so activating a queued prompt
// or a queued /clear can't corrupt the just-finished stream.
func (r *managedREPL) startNextQueued(ctx context.Context, runTurn func(context.Context, string, TurnUI) error) chan error {
	for {
		r.model.mu.Lock()
		if len(r.model.queue) == 0 {
			r.model.mu.Unlock()
			return nil
		}
		item := r.model.queue[0]
		r.model.queue = r.model.queue[1:]
		text := item.text
		r.model.activateQueuedInput(item)

		if item.turn != nil {
			turn := cloneManagedTurn(*item.turn)
			r.model.beginManagedTurnState(turn)
			r.model.mu.Unlock()
			return r.startManagedTurn(ctx, turn, runTurn)
		}

		// runCommand and its helpers expect the model lock held (as in
		// handleEvent); requestQuit does not, so release before quitting.
		handled, quit := r.runCommand(text)
		if !handled {
			r.model.appendNoticeLine(defaultReplCommands.unknownCommandNotice(text))
		}
		if r.model.busy {
			turn, ok := r.takePending()
			r.model.mu.Unlock()
			if !ok {
				return nil
			}
			return r.startManagedTurn(ctx, turn, runTurn)
		}
		r.model.mu.Unlock()
		if quit {
			r.requestQuit()
			return nil
		}
	}
}

func (r *managedREPL) endTurn(err error) {
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	m := r.model
	wasCanceling := m.canceling
	if !m.turnStarted.IsZero() {
		m.lastElapsed = time.Since(m.turnStarted)
	}
	m.busy = false
	m.canceling = false
	m.toolName = ""
	// Any tool whose OnToolEnd never fired must settle to a terminal row; leaving
	// the animated arrow frozen would make an idle transcript look active.
	activeToolReason := "stopped"
	if errors.Is(err, context.Canceled) {
		activeToolReason = "canceled"
	} else if err != nil {
		activeToolReason = "failed"
	}
	m.finishThinkingBlock()
	m.settleActiveTools(activeToolReason)
	m.activeTools = nil
	m.runningTools = 0
	// Those rows are terminal now, so an interrupted batch contracts to the
	// cap like any other.
	m.foldToolWindow()
	switch {
	case err == nil:
		m.finishAssistantBlock("")
		if !m.turnHasOutput {
			m.appendNoticeLine("(no response)")
		}
		m.lastOutcome = turnOutcomeDone
	case errors.Is(err, context.Canceled):
		m.finishAssistantBlock("canceled · not saved")
		m.labelTurnUnsaved("canceled · not saved")
		m.lastOutcome = turnOutcomeCanceled
		m.discardQueuedInputs()
		if m.restoreTurnDraft(m.currentTurn, m.currentPersistence) {
			m.appendNoticeLine("input restored to composer")
		} else {
			m.appendNoticeLine("input available with ↑ · current draft preserved")
		}
		if !wasCanceling {
			m.appendNoticeLine("turn canceled")
		}
	default:
		m.finishAssistantBlock("failed · not saved")
		m.labelTurnUnsaved("failed · not saved")
		m.appendLine(styled("Error: "+err.Error(), "err", ""))
		m.lastOutcome = turnOutcomeFailed
		m.discardQueuedInputs()
		if m.restoreTurnDraft(m.currentTurn, m.currentPersistence) {
			m.appendNoticeLine("input restored to composer")
		} else {
			m.appendNoticeLine("input available with ↑ · current draft preserved")
		}
	}
	// A long turn settling is the "walk away and get pinged" moment; a quick
	// one never gave the user time to leave. Cancellation is user-initiated,
	// so only done/failed notify.
	if m.lastElapsed >= notifyMinTurn &&
		(m.lastOutcome == turnOutcomeDone || m.lastOutcome == turnOutcomeFailed) {
		body := "done in " + coarseElapsed(m.lastElapsed)
		if m.lastOutcome == turnOutcomeFailed {
			body = "failed after " + coarseElapsed(m.lastElapsed)
		}
		if preview := compactQueuePreview(m.currentPrompt); preview != "" {
			body += " — " + preview
		}
		m.pushNotice(body)
	}
	m.currentAssistant = -1
	m.resetAssistantStream()
	m.turnStarted = time.Time{}
	m.state = turnStateIdle
	m.currentPrompt = ""
	m.currentTurn = managedTurnInput{}
	m.currentPersistence = nil
	// A settled turn owns no callbacks. Advance the generation before exposing
	// the idle prompt so a provider goroutine that emits late cannot reopen the
	// assistant block or move the status back to streaming.
	m.turnID++
	r.turnCancel = nil
}

func (r *managedREPL) cancelTurn() {
	if r.turnCancel != nil {
		r.turnCancel()
		r.turnCancel = nil
	}
}

func (r *managedREPL) cancelPending() bool {
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	return r.model.busy && r.model.canceling
}

func (r *managedREPL) abandonCanceledTurn() {
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	m := r.model
	if !m.busy || !m.canceling {
		return
	}
	m.turnID++
	if !m.turnStarted.IsZero() {
		m.lastElapsed = time.Since(m.turnStarted)
	}
	m.finishAssistantBlock("canceled · not saved")
	m.labelTurnUnsaved("canceled · not saved")
	m.busy = false
	m.canceling = false
	m.currentAssistant = -1
	m.resetAssistantStream()
	m.turnStarted = time.Time{}
	m.toolName = ""
	m.finishThinkingBlock()
	m.settleActiveTools("canceled")
	m.activeTools = nil
	m.runningTools = 0
	m.foldToolWindow()
	m.denyApprovalLocked()
	m.state = turnStateIdle
	m.lastOutcome = turnOutcomeCanceled
	restored := cloneManagedTurn(m.currentTurn)
	restoredPersistence := m.currentPersistence
	m.currentPrompt = ""
	m.currentTurn = managedTurnInput{}
	m.currentPersistence = nil
	m.discardQueuedInputs()
	if m.restoreTurnDraft(restored, restoredPersistence) {
		m.appendNoticeLine("input restored to composer")
	} else {
		m.appendNoticeLine("input available with ↑ · current draft preserved")
	}
	m.appendNoticeLine("^C cancellation timed out; detached turn")
}

func (r *managedREPL) detachCanceledTurn(ctx context.Context, runTurn func(context.Context, string, TurnUI) error) chan error {
	r.abandonCanceledTurn()
	return r.startNextQueued(ctx, runTurn)
}

// handleInterrupt processes Ctrl-C. While a turn is in flight the first press
// cancels it — denying any pending approval so the turn goroutine isn't parked
// on the reply channel — and keeps the REPL open. A second press while the turn
// is still winding down quits. At idle, Ctrl-C first clears a draft; only an
// empty prompt exits. Returns true to quit. Caller must hold m.mu.
func (r *managedREPL) handleInterrupt() bool {
	m := r.model
	if !m.busy {
		if !m.ed.empty() {
			m.ed.clear()
			return false
		}
		r.requestQuit()
		return true
	}
	if m.canceling {
		r.requestQuit()
		return true
	}
	r.cancelBusyTurn("^C cancel requested")
	return false
}

// cancelBusyTurn cancels the in-flight turn: freezes the visible partial,
// cancels the turn context, and denies any pending approval so the turn
// goroutine isn't parked on the reply channel. Pending input is marked not sent
// when cancellation settles. Caller must hold m.mu and ensure m.busy &&
// !m.canceling.
func (r *managedREPL) cancelBusyTurn(notice string) {
	m := r.model
	m.canceling = true
	// Freeze the visible partial immediately, but do not label it unsaved until
	// the turn actually settles as canceled. Completion and cancel can race; a
	// successful result must never retain a false "not saved" label.
	m.finishAssistantBlock("")
	r.cancelTurn()
	m.denyApprovalLocked()
	m.appendNoticeLine(notice)
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
		if e.Type == ui.KeyboardEvent {
			if runes := []rune(e.ID); len(runes) == 1 && runes[0] >= 0x20 {
				m.pasteBuf = append(m.pasteBuf, runes[0])
			}
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
	if text == "" || m.approval != nil || m.searching {
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
	if r.model.clipboardCapture {
		return
	}
	r.model.clipboardCapture = true
	go func() {
		dir, err := attachmentCacheDir()
		var path string
		if err == nil {
			path, err = captureClipboardImage(context.Background(), dir)
		}
		r.postUITask(func() {
			m := r.model
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

func (r *managedREPL) releaseApproval() {
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	r.model.denyApprovalLocked()
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

// transcriptHeight returns the current usable line count of the transcript
// pane based on the live terminal dimensions. Used by scroll handlers so
// scroll deltas match what the user actually sees.
func (r *managedREPL) transcriptHeight() int {
	_, h := ui.TerminalDimensions()
	statusRows := 0
	if !r.model.quiet {
		statusRows = 1
	}
	inputRows := r.model.inputRows()
	dividerRows := dividerRowCount(h, inputRows, statusRows, r.model.quiet)
	contentHeight := h - inputRows - statusRows - dividerRows
	logoRows := startupLogoRowCount(contentHeight, r.startupLogoVisible, r.images != nil)
	height := contentHeight - logoRows
	if height < 1 {
		return 1
	}
	return height
}

// dividerRowCount is the height of the rule separating the transcript from the
// bottom chrome: one row outside quiet mode, dropped entirely when the
// terminal is too short to spare it.
func dividerRowCount(h, inputRows, statusRows int, quiet bool) int {
	if quiet || h-inputRows-statusRows < 2 {
		return 0
	}
	return 1
}

func (r *managedREPL) requestQuit() {
	select {
	case r.quit <- struct{}{}:
	default:
	}
}

func (r *managedREPL) setupWidgets() {
	// gotui paragraphs default their TextStyle to ColorWhite, which forces
	// unstyled text (primary input, LLM responses) to white and ignores the
	// terminal theme. ColorClear (= tcell.ColorDefault) inherits the terminal's
	// default foreground instead, so text follows the theme like our accents do.
	r.logoW = newTranscriptParagraph()
	noBorder(&r.logoW.Block)
	r.logoW.TextStyle = ui.NewStyle(ui.ColorClear)
	r.logoW.UseRows = true
	r.logoW.PinBottom = false

	r.transcriptW = newTranscriptParagraph()
	noBorder(&r.transcriptW.Block)
	r.transcriptW.TextStyle = ui.NewStyle(ui.ColorClear)

	r.dividerW = widgets.NewParagraph()
	noBorder(&r.dividerW.Block)
	r.dividerW.WrapText = false
	r.dividerW.TextStyle = ui.NewStyle(ui.ColorClear)

	r.inputW = widgets.NewParagraph()
	noBorder(&r.inputW.Block)
	r.inputW.WrapText = false
	r.inputW.TextStyle = ui.NewStyle(ui.ColorClear)

	r.statusW = widgets.NewParagraph()
	noBorder(&r.statusW.Block)
	r.statusW.WrapText = false
	r.statusW.TextStyle = ui.NewStyle(ui.ColorGrey)
}

// layout (re)builds the root flex for the current input height. The input row
// count varies with multi-line prompts, so the flex is rebuilt each render
// rather than sized once at setup.
func (r *managedREPL) layout(w, h, inputRows, logoRows int, showStatus, showDivider bool) {
	flex := widgets.NewFlex()
	noBorder(&flex.Block)
	flex.Direction = widgets.FlexColumn
	if logoRows > 0 {
		flex.AddItem(r.logoW, logoRows, 0, false)
	}
	flex.AddItem(r.transcriptW, 0, 1, false)
	if showDivider {
		flex.AddItem(r.dividerW, 1, 0, false)
	}
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
	imageCellWidth, imageCellHeight := 0, 0
	if r.images != nil {
		imageCellWidth, imageCellHeight = r.images.cellDimensions()
	}
	showStatus := !r.model.quiet
	statusRows := 0
	if showStatus {
		statusRows = 1
	}

	r.model.mu.Lock()
	if r.model.imageCellWidth != imageCellWidth || r.model.imageCellHeight != imageCellHeight {
		r.model.imageCellWidth = imageCellWidth
		r.model.imageCellHeight = imageCellHeight
		r.model.invalidateVisual()
	}
	r.model.refreshActiveTools()
	r.model.refreshStreamCursor()
	r.model.refreshThinkingBlock(w)
	inputMaxRows := maxInputRows
	if h-statusRows > 1 {
		inputMaxRows = min(inputMaxRows, h-statusRows-1)
	} else {
		inputMaxRows = 1
	}
	input, inputRows, curRow, curCol, editable := r.model.renderInputForTerminal(inputMaxRows, w)
	dividerRows := dividerRowCount(h, inputRows, statusRows, r.model.quiet)
	contentHeight := h - inputRows - statusRows - dividerRows
	logoRows := startupLogoRowCount(contentHeight, r.startupLogoVisible, r.images != nil)
	transcriptHeight := contentHeight - logoRows
	if transcriptHeight < 0 {
		transcriptHeight = 0
	}
	transcriptRows := r.model.transcriptRows(w)
	pinTranscriptBottom := r.model.followBottom
	topRow := r.model.scrollAnchor
	if !pinTranscriptBottom {
		maxTop := len(transcriptRows) - transcriptHeight
		if maxTop < 0 {
			maxTop = 0
		}
		if topRow >= maxTop {
			topRow = maxTop
			r.model.followBottom = true
			pinTranscriptBottom = true
		}
		if topRow < 0 {
			topRow = 0
		}
		r.model.scrollAnchor = topRow
	}
	status := r.model.statusRow(w)
	title := r.model.frameTitle()
	progress := r.model.frameProgress()
	notices := r.model.takeNotices()
	ticker := r.model.activityTicker(len(transcriptRows), topRow, transcriptHeight)
	imagePlacements := r.model.visibleImagePlacements(
		len(transcriptRows), transcriptHeight, topRow, logoRows, w,
		pinTranscriptBottom, ticker != "",
	)
	r.model.imagePlacements = imagePlacements
	r.model.mu.Unlock()

	if logoRows == imageLogoHeight && r.images != nil {
		// The image splash rides the same placement pipeline as thumbnails:
		// its band is blank in the text layer and the manager draws, diffs,
		// and releases it like any other placement.
		if logo, ok := startupLogoPlacement(w, imageCellWidth, imageCellHeight); ok {
			imagePlacements = append([]terminalImagePlacement{logo}, imagePlacements...)
		}
	}

	imagesChanged := false
	if r.images != nil {
		imagesChanged = r.images.prepare(imagePlacements)
	}

	var overlay []ui.Cell
	if ticker != "" {
		overlay = ui.ParseStyles(ticker, ui.NewStyle(ui.ColorClear))
	}

	r.transcriptW.Rows = transcriptRows
	r.transcriptW.UseRows = true
	r.transcriptW.PinBottom = pinTranscriptBottom
	r.transcriptW.TopRow = topRow
	r.transcriptW.OverlayBottom = overlay
	if logoRows == imageLogoHeight {
		r.logoW.Rows = make([][]ui.Cell, logoRows)
	} else {
		r.logoW.Rows = pollyLogoRows(w)
	}
	r.logoW.TopRow = 0
	r.logoW.OverlayBottom = nil
	r.inputW.Text = input
	r.statusW.Text = status
	if dividerRows > 0 {
		r.dividerW.Text = styled(strings.Repeat("─", w), "muted", "")
	}

	r.layout(w, h, inputRows, logoRows, showStatus, dividerRows > 0)
	ui.Clear()
	r.placeCursor(editable, curCol, logoRows+transcriptHeight+dividerRows+curRow, w)
	ui.Render(r.rootFlex)
	if r.images != nil {
		r.images.commit(imagesChanged)
	}

	// Window-level effects go out after the frame, on this same goroutine, so
	// their escapes serialize with tcell's own writes.
	if r.fx != nil {
		r.fx.setTitle(title)
		r.fx.setProgress(progress)
		for _, body := range notices {
			r.fx.notify("polly", body)
		}
	}
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
	_, height := screen.Size()
	if rowY < 0 {
		rowY = 0
	}
	if height > 0 && rowY > height-1 {
		rowY = height - 1
	}
	screen.ShowCursor(x, rowY)
}

// visibleTranscript returns the slice of transcript lines that should fill
// the pane right now, honoring scroll state. Caller must hold m.mu.
//
// Scrolling is logical-line based; transcriptParagraph handles wrapped overflow
// inside the selected slice so follow-bottom still shows the newest rows.
func (m *replModel) visibleTranscript(maxLines int) string {
	lines := m.flattenTranscript()
	if m.slashHints != "" {
		withHints := make([]string, 0, len(lines)+1)
		withHints = append(withHints, lines...)
		withHints = append(withHints, styled(m.slashHints, "muted", ""))
		lines = withHints
	}
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

// fullTranscript returns every semantic block plus transient slash hints.
// Visual clipping happens after style parsing and wrapping in
// transcriptParagraph, so wrapped rows remain reachable through scrollback.
func (m *replModel) fullTranscript() string {
	lines := m.flattenTranscript()
	if m.slashHints != "" {
		lines = append(append([]string(nil), lines...), styled(m.slashHints, "muted", ""))
	}
	return strings.Join(lines, "\n")
}

func (m *replModel) transcriptRows(width int) [][]ui.Cell {
	if width < 1 {
		width = 1
	}
	if m.nativeImages && m.refreshTranscriptImageSources() {
		m.invalidateVisual()
	}
	if m.visualCacheValid && m.visualCacheWidth == width &&
		m.visualCacheCellWidth == m.imageCellWidth && m.visualCacheCellHeight == m.imageCellHeight {
		return m.visualCache
	}

	sources := m.transcriptDisplayEntries()
	oldBlocks := m.visualBlocks
	canPatch := m.visualCacheWidth == width && m.visualCacheNativeImages == m.nativeImages &&
		m.visualCacheCellWidth == m.imageCellWidth && m.visualCacheCellHeight == m.imageCellHeight &&
		len(oldBlocks) == len(sources)
	if len(oldBlocks) != len(sources) {
		m.visualBlocks = make([]transcriptVisualBlock, len(sources))
	}

	offset := 0
	for i, source := range sources {
		followed := i < len(sources)-1
		var old transcriptVisualBlock
		if i < len(oldBlocks) {
			old = oldBlocks[i]
		}
		rows := old.rows
		imageSpans := old.imageSpans
		changed := m.visualCacheWidth != width || m.visualCacheNativeImages != m.nativeImages ||
			m.visualCacheCellWidth != m.imageCellWidth || m.visualCacheCellHeight != m.imageCellHeight ||
			old.key != source.key || old.text != source.text || old.followed != followed || !transcriptImagesEqual(old.images, source.images)
		if changed {
			nativeSlots := m.nativeImages && width >= minimumImageThumbnailCols
			rows, imageSpans = transcriptBlockRowsWithImages(
				source.text, followed, width, source.images, nativeSlots,
				m.imageCellWidth, m.imageCellHeight,
			)
		}
		if canPatch && len(rows) != len(old.rows) {
			canPatch = false
		}
		if canPatch && changed {
			copy(m.visualCache[offset:offset+len(rows)], rows)
		}
		m.visualBlocks[i] = transcriptVisualBlock{
			key:        source.key,
			text:       source.text,
			followed:   followed,
			rows:       rows,
			images:     append([]transcriptImage(nil), source.images...),
			imageSpans: imageSpans,
		}
		offset += len(old.rows)
	}

	if !canPatch {
		total := 0
		for _, block := range m.visualBlocks {
			total += len(block.rows)
		}
		rows := make([][]ui.Cell, 0, total)
		for _, block := range m.visualBlocks {
			rows = append(rows, block.rows...)
		}
		m.visualCache = rows
	}
	m.visualCacheWidth = width
	m.visualCacheNativeImages = m.nativeImages
	m.visualCacheCellWidth = m.imageCellWidth
	m.visualCacheCellHeight = m.imageCellHeight
	m.visualCacheValid = true
	return m.visualCache
}

func (m *replModel) transcriptDisplayBlocks() []string {
	entries := m.transcriptDisplayEntries()
	blocks := make([]string, len(entries))
	for i, entry := range entries {
		blocks[i] = entry.text
	}
	return blocks
}

func (m *replModel) transcriptDisplayEntries() []transcriptDisplayBlock {
	blocks := make([]transcriptDisplayBlock, 0, len(m.transcript)+1)
	for i, entry := range m.transcript {
		if i == m.currentAssistant {
			entry = strings.TrimRight(entry, "\r\n")
			if entry == "" {
				continue
			}
			images := m.transcriptImages[i]
			if len(images) > 0 && strings.HasSuffix(entry, string(transcriptImageMarker(len(images)-1))) {
				// Keep the pulsing stream caret out of the final reserved image
				// row. The newline is stable even on the caret's hidden frame, so
				// follow-bottom does not oscillate by one row.
				entry += "\n" + m.streamCursorFrame
			} else {
				entry += m.streamCursorFrame
			}
		}
		blocks = append(blocks, transcriptDisplayBlock{
			key:    fmt.Sprintf("transcript:%d", i),
			text:   entry,
			images: m.transcriptImages[i],
		})
	}
	if m.slashHints != "" {
		blocks = append(blocks, transcriptDisplayBlock{key: "slash", text: styled(m.slashHints, "muted", "")})
	}
	return blocks
}

func transcriptBlockRows(text string, followed bool, width int) [][]ui.Cell {
	rows, _ := transcriptBlockRowsWithImages(text, followed, width, nil, false, 0, 0)
	return rows
}

func transcriptBlockRowsWithImages(text string, followed bool, width int, images []transcriptImage, native bool, cellWidth, cellHeight int) ([][]ui.Cell, []transcriptImageSpan) {
	cells := parseStyledCells(text, ui.NewStyle(ui.ColorClear))
	if followed {
		// The joined renderer places one newline between transcript blocks. Add
		// that separator before splitting: a normal separator is absorbed by the
		// following block, while a block that already ends in a (possibly styled)
		// newline correctly produces an interior blank row.
		cells = append(cells, ui.Cell{Rune: '\n', Style: ui.StyleClear})
	}
	rows := ui.SplitCells(wrapTranscriptCells(cells, width), '\n')
	return locateTranscriptImages(rows, images, native, width, cellWidth, cellHeight)
}

// flattenTranscript expands embedded "\n" within entries into separate lines
// so scroll math is uniform.
func (m *replModel) flattenTranscript() []string {
	if m.flatCache != nil {
		return m.flatCache
	}
	out := make([]string, 0, len(m.transcript))
	for i, e := range m.transcript {
		if i == m.currentAssistant {
			// A provider often streams its final newline as its own chunk. Keep it
			// provisional until more text arrives so completion does not create and
			// then remove a visible blank row.
			e = strings.TrimRight(e, "\r\n")
			if e == "" {
				continue
			}
		}
		if strings.Contains(e, "\n") {
			out = append(out, strings.Split(e, "\n")...)
		} else {
			out = append(out, e)
		}
	}
	m.flatCache = out
	return out
}

// scrollBy moves the scroll anchor by delta lines (negative = up). Caller
// must hold m.mu. Disengages followBottom on first upward scroll; re-engages
// when the user scrolls back to the bottom.
func (m *replModel) scrollBy(delta, viewportHeight int) {
	width, _ := ui.TerminalDimensions()
	if width < 1 {
		width = 80
	}
	m.scrollByWidth(delta, viewportHeight, width)
}

func (m *replModel) scrollByWidth(delta, viewportHeight, width int) {
	total := len(m.transcriptRows(width))
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

// openImageAt opens the transcript thumbnail under the given screen cell, if
// any, in the OS image viewer. Placements come from the last rendered frame;
// the embedded splash logo (no backing file) is skipped. Caller must hold m.mu.
func (r *managedREPL) openImageAt(x, y int) {
	if r.openImage == nil {
		return
	}
	for _, p := range r.model.imagePlacements {
		if p.Path == "" || x < p.X || x >= p.X+p.Cols || y < p.Y || y >= p.Y+p.Rows {
			continue
		}
		if err := r.openImage(p.Path); err != nil {
			r.model.appendNoticeLine("open failed: " + err.Error())
		}
		return
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
	if !m.slashHintsHidden && !m.pasting && !m.searching && m.approval == nil {
		hint = defaultReplCommands.hintFor(newManagedReplCommandContext(r), text)
	}
	m.setSlashHintLine(hint)
}

func (r *managedREPL) handleEventLocked(e ui.Event) bool {
	m := r.model
	viewport := r.transcriptHeight()
	terminalWidth, _ := ui.TerminalDimensions()
	if terminalWidth < 1 {
		terminalWidth = 80
	}

	// Focus reports update notification gating and are never input, whatever
	// mode (paste, search, approval) is active.
	switch e.ID {
	case focusGainedID:
		m.focusKnown, m.focused = true, true
		return false
	case focusLostID:
		m.focusKnown, m.focused = true, false
		return false
	}

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
			r.openImageAt(mouse.X, mouse.Y)
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

	// tcell runs the terminal in raw mode, so the terminal driver cannot turn
	// Ctrl-Z into SIGTSTP for us. Queue suspension on the UI loop, which first
	// restores the terminal and then stops the foreground process group. This
	// remains available during turns, searches, and approval prompts.
	if e.ID == "<C-z>" {
		select {
		case r.suspend <- struct{}{}:
		default:
		}
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
		case "y", "Y":
			m.handleApprovalAnswer('y')
		case "<Enter>", "<Escape>", "n", "N":
			m.handleApprovalAnswer('n')
		case "a", "A":
			m.handleApprovalAnswer('a')
		case "v", "V":
			m.showApprovalArgs()
		}
		return false
	}

	// While a turn is in flight the prompt stays editable so the user can
	// compose the next message; Enter queues it (see below) rather than
	// submitting immediately. Editing/history/search keys all work as usual.

	switch e.ID {
	case "<Escape>":
		// Escape cancels an in-flight turn like Ctrl-C, but never quits: at
		// idle (or while already canceling) it hides the slash hint line until
		// the input next changes.
		m.slashHintsHidden = true
		if m.busy && !m.canceling {
			r.cancelBusyTurn("esc cancel requested")
		}
	case "<C-d>":
		if m.ed.empty() && !m.busy {
			r.requestQuit()
			return true
		}
		m.ed.deleteForward()
	case "<Enter>":
		trimmed := strings.TrimSpace(m.ed.text())
		if trimmed == "" {
			return false
		}
		if m.clipboardCapture {
			m.appendNoticeLine("clipboard: waiting for image capture")
			m.followBottom = true
			return false
		}
		if m.busy && defaultReplCommands.busySafeCommand(trimmed) {
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
		isCommand := !strings.Contains(trimmed, "\n") && strings.HasPrefix(trimmed, "/")
		if m.busy {
			// A turn is running — commands remain text-only, while prompts are
			// fully prepared before joining the queue.
			queued := queuedREPLInput{text: trimmed}
			if !isCommand {
				turn, err := r.prepareManagedTurnLocked(trimmed)
				if err != nil {
					m.appendLine(styled("Error: "+err.Error(), "err", ""))
					m.followBottom = true
					return false
				}
				queued.turn = &turn
			}
			m.ed.clear()
			r.recordAcceptedInput(trimmed)
			m.queue = append(m.queue, queued)
			m.appendQueuedInput(&m.queue[len(m.queue)-1])
			return false
		}
		// Only a single-line "/…" is a command; a multi-line prompt that happens
		// to start with "/" is real input.
		if isCommand {
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
		turn, restoredPersistence, reuseRestored := m.acceptedRestoredTurn(trimmed)
		if !reuseRestored {
			var err error
			turn, err = r.prepareManagedTurnLocked(trimmed)
			if err != nil {
				m.appendLine(styled("Error: "+err.Error(), "err", ""))
				m.followBottom = true
				return false
			}
		}
		select {
		case r.pending <- turn:
			m.ed.clear()
			r.recordAcceptedInput(trimmed)
			m.currentPersistence = restoredPersistence
			m.restoreDraftNext = reuseRestored
			m.clearRestoredDraft()
			m.beginManagedTurn(turn)
		default:
			m.appendLine(styled("Error: turn queue is unavailable", "err", ""))
			m.followBottom = true
		}
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
	repl        *managedREPL
	config      *Config
	turnID      int64
	reuseUser   bool
	turn        managedTurnInput
	persistence *turnPersistenceAck
}

func (t *gotuiTurnUI) Start() {}
func (t *gotuiTurnUI) Stop()  {}

func (t *gotuiTurnUI) UserMessagePersistenceStarted() {
	t.persistence.beginPersistence()
}

func (t *gotuiTurnUI) UserMessagePersistenceFinished(persisted bool) {
	t.persistence.finishPersistence(persisted)
}

func (t *gotuiTurnUI) activeLocked() bool {
	return t.turnID == 0 || t.repl.model.turnID == t.turnID
}

func (t *gotuiTurnUI) acceptingLocked() bool {
	return t.activeLocked() && !t.repl.model.canceling
}

func denyToolCalls(calls []messages.ChatMessageToolCall) []bool {
	return make([]bool, len(calls))
}

func (t *gotuiTurnUI) ShowThinking(chunk string) {
	t.repl.model.mu.Lock()
	if !t.acceptingLocked() {
		t.repl.model.mu.Unlock()
		return
	}
	t.repl.model.state = turnStateThinking
	t.repl.model.thinkingChars += len(chunk)
	t.repl.model.appendThinking(chunk)
	t.repl.model.mu.Unlock()
}

func (t *gotuiTurnUI) AppendAssistantText(content string) {
	t.repl.model.mu.Lock()
	if !t.acceptingLocked() {
		t.repl.model.mu.Unlock()
		return
	}
	t.repl.model.state = turnStateStreaming
	if content != "" {
		t.repl.model.turnHasOutput = true
		// The answer supersedes the reasoning that produced it: close the
		// block before the first token lands so the rollup sits above it.
		t.repl.model.finishThinkingBlock()
	}
	t.repl.model.appendAssistant(content)
	t.repl.model.mu.Unlock()
}

func (t *gotuiTurnUI) AppendToolStart(calls []messages.ChatMessageToolCall) {
	t.repl.model.mu.Lock()
	defer t.repl.model.mu.Unlock()
	if !t.acceptingLocked() {
		return
	}
	if len(calls) > 0 {
		t.repl.model.turnHasOutput = true
		t.repl.model.finishThinkingBlock()
		t.repl.model.finishAssistantBlock("")
		t.repl.model.runningTools += len(calls)
		t.repl.model.state = turnStateTool
		t.repl.model.toolName = calls[0].Name
	}
	if !toolDisplayEnabled(t.config) {
		return
	}
	for _, c := range calls {
		t.repl.model.appendToolStartLine(c.ID, toolLabel(c))
	}
}

func (t *gotuiTurnUI) ApproveToolCalls(calls []messages.ChatMessageToolCall) []bool {
	if len(calls) == 0 {
		return nil
	}
	t.repl.model.mu.Lock()
	if !t.activeLocked() || t.repl.model.canceling {
		t.repl.model.mu.Unlock()
		return denyToolCalls(calls)
	}
	t.repl.model.mu.Unlock()

	if !t.config.Confirm {
		approved := make([]bool, len(calls))
		for i := range approved {
			approved[i] = true
		}
		return approved
	}
	reply := make(chan []bool, 1)
	t.repl.model.mu.Lock()
	if !t.activeLocked() || t.repl.model.canceling {
		t.repl.model.mu.Unlock()
		return denyToolCalls(calls)
	}
	t.repl.model.approval = &approvalState{calls: calls, reply: reply}
	label := toolLabel(calls[0])
	if len(calls) > 1 {
		label += fmt.Sprintf(" +%d more", len(calls)-1)
	}
	t.repl.model.pushNotice("approval needed: " + truncate(label, 80))
	t.repl.model.mu.Unlock()
	results, ok := <-reply
	if !ok {
		return make([]bool, len(calls))
	}
	return results
}

func (t *gotuiTurnUI) AppendToolEnd(call messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
	label := toolLabel(call)
	denied := toolWasDenied(result)
	var discoveredImages []transcriptImage
	if toolDisplayEnabled(t.config) && !denied {
		// Tool output can be large. Discovery touches only Markdown/path syntax
		// and the filesystem, so keep it outside the model lock and let the TUI
		// continue painting while it runs.
		discoveredImages = discoverToolOutputImages(result, t.repl.model.imageBaseDir)
	}
	t.repl.model.mu.Lock()
	defer t.repl.model.mu.Unlock()
	m := t.repl.model
	if !t.acceptingLocked() {
		return
	}
	if m.runningTools > 0 {
		m.runningTools--
	}
	m.turnHasOutput = true
	// Return to "waiting" only once every tool in the batch has finished;
	// otherwise the first of several parallel tools to complete would flip the
	// status back to waiting (and drop the running-tool name) mid-batch.
	if m.busy && m.runningTools == 0 {
		m.state = turnStateWaiting
		m.toolName = ""
	}
	if !toolDisplayEnabled(t.config) {
		return
	}
	var final string
	switch {
	case denied:
		final = toolDeniedLine(label)
	case err != nil:
		final = toolErrorLine(label, formatElapsed(duration), toolFailureMeta(err))
	default:
		final = toolOKLine(label, formatElapsed(duration), resultLineMeta(result))
	}
	images := discoveredImages
	if len(images) > 0 {
		// Tool-produced media stays visibly subordinate to the compact tool row;
		// assistant-authored images remain flush with the transcript body.
		final += "\n" + renderTranscriptImages(images, "    ")
	}
	// Freeze the final line over the running (breathing) one in place, so a tool
	// is a single transcript line from start to finish. Falls back to appending
	// if the start line wasn't tracked (shouldn't happen when display is on).
	if idx, ok := m.takeActiveTool(call.ID); ok && idx >= 0 {
		m.transcript[idx] = final
		m.setTranscriptImages(idx, images)
		m.invalidateFlat()
	} else {
		m.appendLine(final)
		m.toolWindow = append(m.toolWindow, len(m.transcript)-1)
		m.setTranscriptImages(len(m.transcript)-1, images)
		m.invalidateFlat()
	}
	// This call settling may have released the last pin holding the window
	// open, letting a bulged batch contract to the cap.
	m.foldToolWindow()
}

func (t *gotuiTurnUI) AppendWarning(text string) {
	t.repl.model.mu.Lock()
	if !t.acceptingLocked() {
		t.repl.model.mu.Unlock()
		return
	}
	t.repl.model.appendNoticeLine("Warning: " + text)
	t.repl.model.turnHasOutput = true
	t.repl.model.mu.Unlock()
}

func (t *gotuiTurnUI) RecordTurnTokens(in, out int) {
	t.repl.model.mu.Lock()
	if !t.acceptingLocked() {
		t.repl.model.mu.Unlock()
		return
	}
	// Replace this turn's contribution so the callback remains safe if a
	// provider reports updated usage more than once.
	t.repl.model.totalIn += in - t.repl.model.lastIn
	t.repl.model.totalOut += out - t.repl.model.lastOut
	t.repl.model.lastIn = in
	t.repl.model.lastOut = out
	t.repl.model.mu.Unlock()
}

func (t *gotuiTurnUI) FinishTextTurn() {
	t.repl.model.mu.Lock()
	if t.acceptingLocked() {
		t.repl.model.finishAssistantBlock("")
	}
	t.repl.model.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Entry points used by runREPL
// ---------------------------------------------------------------------------

func runManagedREPL(ctx context.Context, config *Config, state *conversationState) error {
	name, err := state.session.GetName(ctx)
	if err != nil {
		return fmt.Errorf("read session name: %w", err)
	}
	history, err := state.session.GetHistory(ctx)
	if err != nil {
		return fmt.Errorf("read session history: %w", err)
	}
	repl := newManagedREPL(config, name, toolCount(state.toolRegistry), skillCount(state.skillCatalog))
	repl.state = state
	repl.model.artifactStore = state.artifactStore
	repl.model.hydrateHistory(history, name)
	if state.autoNamedContext {
		repl.model.appendNoticeLine("session '" + name + "' · /rename to keep a name · resume later with polly -L")
	}
	return repl.Run(ctx, func(turnCtx context.Context, prompt string, turnUI TurnUI) error {
		reuseUser := false
		userMsg := messages.ChatMessage{Role: messages.MessageRoleUser, Content: prompt}
		if tui, ok := turnUI.(*gotuiTurnUI); ok {
			reuseUser = tui.reuseUser
			userMsg = cloneChatMessage(tui.turn.userMessage)
		}
		// The exit code is a one-shot concern; the REPL already rendered
		// any warning.
		_, err := executeTurnWithUserMessage(turnCtx, config, state, userMsg, nil, nil, turnUI, reuseUser)
		return err
	})
}

func runFallbackREPL(ctx context.Context, config *Config, state *conversationState) error {
	reader := bufio.NewReader(os.Stdin)
	drainSandboxWarningsToWriter(os.Stderr, state)
	writeFallbackSandboxNotice(os.Stderr, config, state)
	if state.autoNamedContext && !config.Quiet {
		name, err := state.session.GetName(ctx)
		if err != nil {
			return fmt.Errorf("read session name: %w", err)
		}
		fmt.Fprintf(os.Stderr, "session '%s' · /rename to keep a name · resume later with polly -L\n", name)
	}
	commandCtx := newWriterReplCommandContext(config, state, os.Stderr)
	commandCtx.ctx = ctx
	return runREPLLoopWithCommands(ctx, reader, os.Stderr, commandCtx, func(prompt string) error {
		turnCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		// The exit code is a one-shot concern; the REPL already rendered
		// any warning.
		_, err := executeTurn(turnCtx, config, state, prompt, nil, reader, nil)
		drainSandboxWarningsToWriter(os.Stderr, state)
		// If the turn was cancelled but the parent context is still alive
		// (not a shutdown signal), treat it as a recoverable per-turn
		// cancellation and let the loop continue.
		if errors.Is(err, context.Canceled) && ctx.Err() == nil {
			return fmt.Errorf("cancelled")
		}
		return err
	})
}

func drainSandboxWarningsToWriter(w io.Writer, state *conversationState) {
	if w == nil || state == nil {
		return
	}
	for _, body := range state.drainSandboxWarnings() {
		fmt.Fprintln(w, "Warning: "+body)
	}
}

func writeFallbackSandboxNotice(w io.Writer, config *Config, state *conversationState) {
	if config == nil || config.Quiet {
		return
	}
	fmt.Fprintln(w, sandboxNoticeLine(config, state))
}

func runREPLLoop(ctx context.Context, reader *bufio.Reader, promptWriter io.Writer, runTurn func(string) error) error {
	return runREPLLoopWithCommands(ctx, reader, promptWriter, newWriterReplCommandContext(nil, nil, promptWriter), runTurn)
}

type fallbackLineResult struct {
	line string
	err  error
}

// readFallbackLine starts at most one blocked read while the fallback REPL is
// idle. Cancellation can therefore release the session/store promptly without
// racing a background stdin consumer against confirmation reads during turns.
func readFallbackLine(ctx context.Context, reader *bufio.Reader) (string, error) {
	result := make(chan fallbackLineResult, 1)
	go func() {
		line, err := readLine(reader)
		result <- fallbackLineResult{line: line, err: err}
	}()
	select {
	case <-ctx.Done():
		return "", context.Cause(ctx)
	case read := <-result:
		if err := context.Cause(ctx); err != nil {
			return "", err
		}
		return read.line, read.err
	}
}

func terminalSessionError(err error) bool {
	return errors.Is(err, sessions.ErrSessionLeaseLost) || errors.Is(err, sessions.ErrStoreClosed)
}

func runREPLLoopWithCommands(ctx context.Context, reader *bufio.Reader, promptWriter io.Writer, commandCtx *replCommandContext, runTurn func(string) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		if _, err := fmt.Fprint(promptWriter, "> "); err != nil {
			return err
		}
		line, err := readFallbackLine(ctx, reader)
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
		if strings.HasPrefix(trimmed, "/") {
			handled, quit, err := defaultReplCommands.dispatch(trimmed, commandCtx)
			if err != nil {
				return err
			}
			if quit {
				return nil
			}
			if handled {
				continue
			}
			if _, err := fmt.Fprintln(promptWriter, defaultReplCommands.unknownCommandNotice(trimmed)); err != nil {
				return err
			}
			continue
		}
		if err := runTurn(line); err != nil {
			// Lease loss and store shutdown end the whole session. Other per-turn
			// failures remain recoverable and are rendered inline.
			if terminalSessionError(err) {
				return err
			}
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			if errors.Is(err, context.Canceled) {
				return nil
			}
			if _, werr := fmt.Fprintf(promptWriter, "Error: %v\n", err); werr != nil {
				return werr
			}
		}
	}
}
