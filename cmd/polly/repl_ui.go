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
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	tcell "github.com/gdamore/tcell/v3"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
	"github.com/metaspartan/gotui/v5/widgets"
	"golang.org/x/term"
)

// turnState describes what the agent is currently doing for the terminal title,
// quiet-mode fallback, and scrolled-up activity ticker.
type turnState int

const (
	turnStateIdle turnState = iota
	turnStateWaiting
	turnStateThinking
	turnStateStreaming
	turnStateTool
	turnStateError
)

// turnOutcome is the last settled result shown in the fixed turn dock and
// terminal title. Completing a turn must not append chrome that shifts the
// answer vertically.
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
	// transcriptImages runs in lockstep with transcript: entry i's explicit
	// local image references live at transcriptImages[i] (nil when the entry
	// has none). Grown only by appendTranscriptEntry, shrunk only by
	// deleteTranscriptEntry, reset only by clearDisplay, rewritten in place
	// only by setTranscriptText/setTranscriptEntry/setTranscriptImages — so
	// the lanes cannot drift and every mutation invalidates the visual cache.
	// A direct write to either lane outside those owners is a bug.
	transcriptImages [][]transcriptImage
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
	// streamDeferredTable records that the last streaming render drew a table
	// in its unaligned in-flight form; settle must re-render for the aligned
	// layout even when no bytes were held back.
	streamDeferredTable bool

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
	// activeTools tracks tool calls currently executing, each pinned to its row
	// inside the current disclosure. While expanded, render() rewrites those
	// rows with a breathing arrow and live elapsed time. Parallel calls finish
	// out of order, so each is matched back to its row by call ID.
	activeTools []activeTool
	// activeToolsPhase is the arrow-pulse phase of the last refreshActiveTools
	// repaint; -1 forces a repaint on the first tick of a new batch.
	activeToolsPhase int
	// Tool activity is a semantic disclosure from its first call. It defaults
	// collapsed; deliberate expansion reveals every live or completed row.
	toolDisclosures           map[int64]*toolDisclosureRecord
	toolDisclosureAt          map[int]int64 // transcript index -> disclosure ID
	toolDisclosureSeq         int64
	turnToolDisclosureID      int64
	turnToolDisclosureIDs     []int64 // every disclosure opened this turn
	toolDisclosurePlacements  []disclosurePlacement
	imageDisclosurePlacements []disclosurePlacement
	turnDock                  turnDockState
	turnTrailers              map[int64]*turnTrailerRecord
	turnTrailerAt             map[int]int64
	turnTrailerSeq            int64
	turnTrailerPlacements     []turnTrailerPlacement
	openTurnTrailerID         int64
	modal                     *replModal

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

	// Status bar fields. Context usage describes the provider-visible request,
	// not the complete durable transcript or cumulative billed tokens.
	modelName        string
	contextName      string
	toolCount        int
	skillCount       int
	quiet            bool
	contextUsed      int
	contextLimit     int
	contextEstimated bool
	recentModels     []string
	statusSession    statusSessionPlacement

	state       turnState
	toolName    string
	turnStarted time.Time
	lastIn      int
	lastOut     int
	lastElapsed time.Duration
	lastOutcome turnOutcome

	// Reasoning disclosures are semantic per-turn records rather than free-form
	// transcript strings. The UI retains only a bounded tail; successful turns
	// already persist their complete provider reasoning on ChatMessage and
	// hydrate it back into a fresh bounded record after restart.
	reasoningRecords     map[int64]*reasoningRecord
	reasoningAt          map[int]int64 // transcript index -> record ID
	reasoningOrder       []int64       // creation order, for Ctrl-O
	reasoningSeq         int64
	turnReasoningID      int64
	turnReasoningIDs     []int64 // every reasoning record opened this turn
	turnReasoningOpen    bool    // pending Ctrl-O pre-arm before the first chunk
	thinkingSegmentOpen  bool
	thinkingSegmentStart time.Time
	reasoningPlacements  []disclosurePlacement
	reasoningWidth       int // last renderer width; avoids terminal access from provider callbacks

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
	outcomeLabeled      bool

	// runningTools counts tool calls currently in flight this turn. A parallel
	// batch starts several at once; the turn state returns to "waiting" only
	// when the last finishes, so quiet-mode/title activity stays accurate.
	runningTools int
}

type transcriptVisualBlock struct {
	key               string
	text              string
	followed          bool
	rows              [][]ui.Cell
	images            []transcriptImage
	imageSpans        []transcriptImageSpan
	reasoningIDs      []int64
	toolDisclosureIDs []int64
	turnTrailerID     int64
	activityFields    []turnDockPlacement
}

// reasoningRecord is the display projection of one user turn's provider
// reasoning. tail is intentionally bounded; the durable ChatMessage remains
// the authoritative complete copy for successful turns.
type reasoningRecord struct {
	id              int64
	transcriptIndex int
	tail            []rune
	tailVersion     uint64
	previewVersion  uint64
	previewWidth    int
	previewLines    []string
	dirty           bool
	expanded        bool
	active          bool
	complete        bool
	unsaved         bool
	elapsed         time.Duration
}

// disclosurePlacement is the last rendered header-row geometry for one
// disclosure. Like native image placements, it is viewport-aware and exists
// only for mouse hit-testing.
type disclosurePlacement struct {
	recordID  int64
	recordIDs []int64
	X, Y      int
	Cols      int
}

type statusSessionPlacement struct {
	X, Cols int
}

func (p statusSessionPlacement) hit(x, y, terminalHeight int) bool {
	return p.Cols > 0 && terminalHeight > 0 && y == terminalHeight-1 && x >= p.X && x < p.X+p.Cols
}

type toolDisclosureRow struct {
	callID           string
	label            string
	line             string
	images           []transcriptImage
	inspectionImages []transcriptImage
	settled          bool
}

type toolDisclosureRecord struct {
	id              int64
	transcriptIndex int
	rows            []toolDisclosureRow
	expanded        bool
	imagesExpanded  bool
	complete        bool
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
		toolDisclosures:  make(map[int64]*toolDisclosureRecord),
		toolDisclosureAt: make(map[int]int64),
		turnTrailers:     make(map[int64]*turnTrailerRecord),
		turnTrailerAt:    make(map[int]int64),
		reasoningRecords: make(map[int64]*reasoningRecord),
		reasoningAt:      make(map[int]int64),
		reasoningWidth:   80,
		imageBaseDir:     baseDir,
		historyIdx:       -1,
		state:            turnStateIdle,
		followBottom:     true,
		activeToolsPhase: -1,
	}
	m.ed.goalCol = -1
	return m
}

const turnCancelDetachAfter = 2 * time.Second

// statusRow renders stable session context. Per-turn activity and completion
// metrics live in the fixed turn dock immediately above the composer.
func (m *replModel) statusRow(width int) string {
	m.statusSession = statusSessionPlacement{}
	if m.quiet || width <= 0 {
		return ""
	}
	const sep = " · "

	leftRaw, leftStyled := "", ""
	if m.busy && !m.turnStarted.IsZero() {
		leftRaw = formatElapsed(time.Since(m.turnStarted))
		leftStyled = styled(leftRaw, "accent", "")
	}
	type field struct {
		drop    int
		text    string
		session bool
	}
	fields := []field{}
	if m.modelName != "" {
		fields = append(fields, field{drop: 3, text: m.modelName})
	}
	fields = append(fields, field{drop: 0, text: m.contextName, session: true})
	if context := m.contextUsageText(); context != "" {
		fields = append(fields, field{drop: 1, text: context})
	}
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
		color := "muted"
		if f.session {
			color = "accent"
		}
		rightStyledParts[i] = styled(f.text, color, "")
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
	x := width - rightWidth
	sepWidth := rw.StringWidth(sep)
	for i, f := range fields {
		fieldCols := rw.StringWidth(f.text)
		if f.session && fieldCols > 0 {
			m.statusSession = statusSessionPlacement{X: x, Cols: fieldCols}
		}
		x += fieldCols
		if i < len(fields)-1 {
			x += sepWidth
		}
	}
	return leftStyled + strings.Repeat(" ", gap) + rightStyled
}

func (m *replModel) contextUsageText() string {
	if m.contextUsed <= 0 && m.contextLimit <= 0 {
		return ""
	}
	prefix := "ctx "
	if m.contextEstimated {
		prefix += "~"
	}
	used := humanizeTokens(m.contextUsed)
	if m.contextLimit <= 0 {
		return prefix + used
	}
	if m.contextUsed > m.contextLimit && m.contextEstimated {
		used = ">" + humanizeTokens(m.contextLimit)
	}
	return prefix + used + "/" + humanizeTokens(m.contextLimit)
}

func (m *replModel) clearContextUsage(limit int) {
	m.contextUsed = 0
	m.contextLimit = limit
	m.contextEstimated = false
}

func (m *replModel) recordContextUsage(used, limit int, estimated bool) {
	if used < 0 {
		used = 0
	}
	m.contextUsed = used
	m.contextLimit = limit
	m.contextEstimated = estimated
}

// reasoningPreviewLines is the bounded expanded tail. The preview uses the
// full transcript width after its block indent. The retained/limit pair bounds
// the duplicate display copy while amortizing compaction of streamed chunks.
const (
	reasoningPreviewLines    = 5
	reasoningTailRetainRunes = 8192
	reasoningTailLimitRunes  = 2 * reasoningTailRetainRunes
)

// toolPreviewRows bounds an expanded tool disclosure the same way
// reasoningPreviewLines bounds thinking: the newest rows, everywhere the
// disclosure renders. Earlier rows collapse into one elision line; the header
// keeps the full count.
const toolPreviewRows = 5

const reasoningBlockIndent = "    "

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
	}
	return styled(raw, "accent", "bold") + styled(" · End to follow", "muted", "")
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

// invalidateVisual marks the styled/wrapped transcript cache stale so the next
// transcriptRows recomputes it. Caller must hold m.mu (every transcript
// mutation already does).
func (m *replModel) invalidateVisual() { m.visualCacheValid = false }

// setTranscriptText replaces entry index's text. Every in-place transcript
// write goes through here or setTranscriptEntry, so the visual cache is
// always marked stale by the write itself rather than by each caller.
func (m *replModel) setTranscriptText(index int, text string) {
	m.transcript[index] = text
	m.invalidateVisual()
}

// setTranscriptEntry replaces entry index's text and image list together.
func (m *replModel) setTranscriptEntry(index int, text string, images []transcriptImage) {
	m.setTranscriptText(index, text)
	m.setTranscriptImages(index, images)
}

// setTranscriptImages replaces entry index's image list. It indexes the lane
// unguarded on purpose: an out-of-range index means the lanes drifted, and
// that must fail at the write that exposed it rather than at some later
// render.
func (m *replModel) setTranscriptImages(index int, images []transcriptImage) {
	if len(images) == 0 {
		m.transcriptImages[index] = nil
	} else {
		m.transcriptImages[index] = append([]transcriptImage(nil), images...)
	}
	m.invalidateVisual()
}

func (m *replModel) deleteTranscriptEntry(index int) {
	if id, ok := m.turnTrailerAt[index]; ok {
		delete(m.turnTrailerAt, index)
		delete(m.turnTrailers, id)
		if m.openTurnTrailerID == id {
			m.openTurnTrailerID = 0
		}
	}
	if id, ok := m.reasoningAt[index]; ok {
		delete(m.reasoningAt, index)
		delete(m.reasoningRecords, id)
		for i, orderedID := range m.reasoningOrder {
			if orderedID == id {
				m.reasoningOrder = append(m.reasoningOrder[:i], m.reasoningOrder[i+1:]...)
				break
			}
		}
		if m.turnReasoningID == id {
			m.resetCurrentThinking()
		}
	}
	if id, ok := m.toolDisclosureAt[index]; ok {
		delete(m.toolDisclosureAt, index)
		delete(m.toolDisclosures, id)
		if m.turnToolDisclosureID == id {
			m.turnToolDisclosureID = 0
		}
	}
	m.transcript = slices.Delete(m.transcript, index, index+1)
	m.transcriptImages = slices.Delete(m.transcriptImages, index, index+1)
	for i := index + 1; i <= len(m.transcript); i++ {
		if id, ok := m.reasoningAt[i]; ok {
			m.reasoningAt[i-1] = id
			delete(m.reasoningAt, i)
			if record := m.reasoningRecords[id]; record != nil {
				record.transcriptIndex = i - 1
			}
		}
		if id, ok := m.toolDisclosureAt[i]; ok {
			m.toolDisclosureAt[i-1] = id
			delete(m.toolDisclosureAt, i)
			if record := m.toolDisclosures[id]; record != nil {
				record.transcriptIndex = i - 1
			}
		}
		if id, ok := m.turnTrailerAt[i]; ok {
			m.turnTrailerAt[i-1] = id
			delete(m.turnTrailerAt, i)
			if record := m.turnTrailers[id]; record != nil {
				record.transcriptIndex = i - 1
			}
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
	m.invalidateVisual()
}

// appendLine appends a pre-rendered transcript entry (may contain inline
// style markup). A non-assistant boundary first settles any active assistant
// block so pending fence text and provider terminal newlines cannot leak across
// notices, warnings, tools, or user turns.
func (m *replModel) appendLine(s string) {
	if m.currentAssistant >= 0 && m.currentAssistant < len(m.transcript) {
		m.finishAssistantBlock("")
	}
	m.appendTranscriptEntry(s)
	m.currentAssistant = -1
}

// appendTranscriptEntry grows the transcript and its image lane together and
// returns the new entry's index; every transcript append goes through here.
func (m *replModel) appendTranscriptEntry(text string) int {
	m.transcript = append(m.transcript, text)
	m.transcriptImages = append(m.transcriptImages, nil)
	m.invalidateVisual()
	return len(m.transcript) - 1
}

func (m *replModel) resetAssistantStream() {
	m.streamRaw.Reset()
	m.streamShown = 0
	m.streamDeferredTable = false
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
		m.currentAssistant = m.appendTranscriptEntry("")
	}
	m.streamRaw.WriteString(text)
	raw := m.streamRaw.String()
	visible := raw[:safeVisibleLen(raw)]
	if len(visible) == m.streamShown && m.transcript[m.currentAssistant] != "" {
		return
	}
	m.streamShown = len(visible)
	rendered, images, deferred := renderMarkdownWithLocalImages(visible, m.imageBaseDir, true)
	m.streamDeferredTable = deferred
	m.setTranscriptEntry(m.currentAssistant, rendered, images)
}

// finishAssistantStream renders any text still held back by the streaming
// holdback — at settle time the message is final, so everything shows. A
// table that streamed in its unaligned form also forces the settle render:
// holdback never withholds pipe rows, so streamShown alone would miss it.
func (m *replModel) finishAssistantStream() {
	if m.currentAssistant < 0 || m.currentAssistant >= len(m.transcript) {
		return
	}
	raw := m.streamRaw.String()
	if raw == "" || (m.streamShown >= len(raw) && !m.streamDeferredTable) {
		return
	}
	m.streamShown = len(raw)
	m.streamDeferredTable = false
	rendered, images, _ := renderMarkdownWithLocalImages(raw, m.imageBaseDir, false)
	m.setTranscriptEntry(m.currentAssistant, rendered, images)
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
		m.setTranscriptText(idx, content)
	}
	m.currentAssistant = -1
	m.resetAssistantStream()
	if label != "" && content != "" {
		m.appendLine("  " + styled(label, "muted", ""))
		m.outcomeLabeled = true
	}
	return content != ""
}

// labelTurnOutcome appends the turn's outcome label ("canceled/failed · …")
// unless the closing assistant block already carried it. Exactly one outcome
// label lands per settled turn with visible output.
func (m *replModel) labelTurnOutcome(label string) {
	if m.outcomeLabeled || !m.turnHasOutput {
		return
	}
	m.appendLine("  " + styled(label, "muted", ""))
	m.outcomeLabeled = true
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

// activeTool is one still-executing tool call, pinned to its disclosure row.
type activeTool struct {
	id      string
	row     int
	label   string
	started time.Time
}

// currentToolDisclosure returns the disclosure currently receiving rows, or —
// once a batch has settled mid-turn — the turn's most recent disclosure, so
// callers inspecting the just-completed batch still find it. Caller must hold
// m.mu.
func (m *replModel) currentToolDisclosure() *toolDisclosureRecord {
	if m.turnToolDisclosureID != 0 {
		return m.toolDisclosures[m.turnToolDisclosureID]
	}
	if n := len(m.turnToolDisclosureIDs); n > 0 {
		return m.toolDisclosures[m.turnToolDisclosureIDs[n-1]]
	}
	return nil
}

func (m *replModel) ensureToolDisclosure() *toolDisclosureRecord {
	// Only the live pointer receives new rows: once a batch settles, the next
	// batch opens a fresh disclosure at its own transcript position.
	if record := m.toolDisclosures[m.turnToolDisclosureID]; record != nil {
		return record
	}
	m.toolDisclosureSeq++
	record := &toolDisclosureRecord{id: m.toolDisclosureSeq, transcriptIndex: len(m.transcript)}
	m.toolDisclosures[record.id] = record
	m.turnToolDisclosureID = record.id
	m.turnToolDisclosureIDs = append(m.turnToolDisclosureIDs, record.id)
	m.appendLine("")
	record.transcriptIndex = len(m.transcript) - 1
	m.toolDisclosureAt[record.transcriptIndex] = record.id
	return record
}

func (m *replModel) appendToolStartRow(id, label string) *toolDisclosureRecord {
	label = stripTranscriptImageMarkers(label)
	record := m.ensureToolDisclosure()
	row := len(record.rows)
	record.rows = append(record.rows, toolDisclosureRow{
		callID: id,
		label:  label,
		line:   runningToolLine(label, 0),
	})
	if len(m.activeTools) == 0 {
		// A fresh batch restarts the pulse clock; drop the drained batch's
		// phase so a bucket collision cannot skip the first repaint.
		m.activeToolsPhase = -1
	}
	m.activeTools = append(m.activeTools, activeTool{
		id:      id,
		row:     row,
		label:   label,
		started: time.Now(),
	})
	return record
}

func toolDisclosureHeader(total int, expanded, complete bool) string {
	glyph := "▸"
	if expanded {
		glyph = "▾"
	}
	return "  " + inlineActivityControl(glyph, turnToolLabel(total), complete)
}

func toolDisclosureText(record *toolDisclosureRecord) (string, []transcriptImage) {
	if record == nil {
		return "", nil
	}
	header := toolDisclosureHeader(len(record.rows), record.expanded, record.complete)
	if !record.expanded {
		return header, nil
	}
	var b strings.Builder
	b.WriteString(header)
	var images []transcriptImage
	seen := make(map[string]struct{})
	rows := record.rows
	if len(rows) > toolPreviewRows {
		b.WriteString("\n  ")
		b.WriteString(styled(fmt.Sprintf("… %d earlier", len(rows)-toolPreviewRows), "muted", ""))
		rows = rows[len(rows)-toolPreviewRows:]
	}
	for _, row := range rows {
		if row.line == "" {
			continue
		}
		b.WriteByte('\n')
		b.WriteString(row.line)
		appendToolDisclosureImages(&b, &images, row.images, "    ", seen)
	}
	return b.String(), images
}

func transcriptImageIdentity(img transcriptImage) string {
	if img.Path != "" {
		return img.Path
	}
	return img.DisplayPath + "\x00" + img.Alt
}

func appendToolDisclosureImages(b *strings.Builder, rendered *[]transcriptImage, candidates []transcriptImage, prefix string, seen map[string]struct{}) {
	remaining := maxTranscriptImagesPerBlock - len(*rendered)
	if remaining <= 0 || len(candidates) == 0 {
		return
	}
	selected := make([]transcriptImage, 0, min(len(candidates), remaining))
	for _, img := range candidates {
		identity := transcriptImageIdentity(img)
		if _, duplicate := seen[identity]; duplicate {
			continue
		}
		seen[identity] = struct{}{}
		selected = append(selected, img)
		if len(selected) == remaining {
			break
		}
	}
	if len(selected) == 0 {
		return
	}
	block := offsetTranscriptImageMarkers(renderTranscriptImages(selected, prefix), len(*rendered))
	if block == "" {
		return
	}
	b.WriteByte('\n')
	b.WriteString(block)
	*rendered = append(*rendered, selected...)
}

func (m *replModel) toolInspectionImages(ids []int64) []transcriptImage {
	images := make([]transcriptImage, 0)
	for _, id := range ids {
		record := m.toolDisclosures[id]
		if record == nil {
			continue
		}
		for _, row := range record.rows {
			for _, img := range row.inspectionImages {
				images = append(images, img)
				if len(images) == maxTranscriptImagesPerBlock {
					return images
				}
			}
		}
	}
	return images
}

func (m *replModel) toolInspectionExpanded(ids []int64) bool {
	for _, id := range ids {
		if record := m.toolDisclosures[id]; record != nil && record.imagesExpanded {
			return true
		}
	}
	return false
}

func (m *replModel) refreshToolDisclosure(record *toolDisclosureRecord) {
	m.refreshToolDisclosureWithAnchor(record, true)
}

func (m *replModel) refreshToolDisclosureWithAnchor(record *toolDisclosureRecord, reanchor bool) {
	if record == nil || record.transcriptIndex < 0 || record.transcriptIndex >= len(m.transcript) {
		return
	}
	index := record.transcriptIndex
	text, images := toolDisclosureText(record)
	if m.transcript[index] == text && transcriptImagesEqual(m.transcriptImages[index], images) {
		return
	}
	// Tool activity renders inline in the transcript, so updates re-anchor a
	// held viewport like any other transcript mutation.
	if !reanchor {
		m.setTranscriptEntry(index, text, images)
		return
	}
	m.mutateAnchored(m.disclosureLayoutWidth(0), matchToolDisclosureBlock(record.id), func(bool) {
		m.setTranscriptEntry(index, text, images)
	})
}

// displayRecordSpan locates the flattened display block satisfying match and
// returns its first display row and row count. The viewport anchor indexes
// display rows, and adjacent activity entries merge into one display block —
// so raw per-entry offsets drift from anchor space and cannot re-anchor a
// held viewport. Forces the layout for width. Caller must hold m.mu.
func (m *replModel) displayRecordSpan(width int, match func(*transcriptVisualBlock) bool) (int, int, bool) {
	m.transcriptRows(width)
	start := 0
	for i := range m.visualBlocks {
		block := &m.visualBlocks[i]
		if match(block) {
			return start, len(block.rows), true
		}
		start += len(block.rows)
	}
	return 0, 0, false
}

// mutateAnchored applies mutate to the transcript while keeping a held
// viewport steady: the display block satisfying match is measured before and
// after, and the scroll anchor shifts by the height change. mutate receives
// whether the block was found under a held viewport, so nested refreshes can
// skip their own re-anchoring. When the viewport follows the bottom nothing
// is measured. Caller must hold m.mu.
func (m *replModel) mutateAnchored(width int, match func(*transcriptVisualBlock) bool, mutate func(held bool)) {
	if m.followBottom {
		mutate(false)
		return
	}
	oldStart, oldCount, held := m.displayRecordSpan(width, match)
	mutate(held)
	if held {
		if _, newCount, ok := m.displayRecordSpan(width, match); ok {
			m.anchorForResizedEntry(oldStart, oldCount, newCount)
		}
	}
}

// matchActivityGroup matches the merged activity block that owns every
// record in ids. Raw transcript entries over-count their independent headers
// and reasoning previews after layout combines them into one block.
func matchActivityGroup(ids []int64, reasoning bool) func(*transcriptVisualBlock) bool {
	return func(block *transcriptVisualBlock) bool {
		blockIDs := block.toolDisclosureIDs
		if reasoning {
			blockIDs = block.reasoningIDs
		}
		return activityGroupContains(blockIDs, ids)
	}
}

func matchTurnTrailerBlock(recordID int64) func(*transcriptVisualBlock) bool {
	return func(block *transcriptVisualBlock) bool {
		return block.turnTrailerID == recordID
	}
}

func matchToolDisclosureBlock(recordID int64) func(*transcriptVisualBlock) bool {
	return func(block *transcriptVisualBlock) bool {
		return slices.Contains(block.toolDisclosureIDs, recordID)
	}
}

func matchReasoningBlock(recordID int64) func(*transcriptVisualBlock) bool {
	return func(block *transcriptVisualBlock) bool {
		return slices.Contains(block.reasoningIDs, recordID)
	}
}

// completeToolDisclosure settles the live tool disclosure. A deliberately
// expanded disclosure stays open until the turn settles; the next batch opens
// a fresh disclosure at its own transcript position. Caller must hold m.mu.
func (m *replModel) completeToolDisclosure() {
	if record := m.toolDisclosures[m.turnToolDisclosureID]; record != nil {
		record.complete = true
		m.refreshToolDisclosure(record)
	}
	m.turnToolDisclosureID = 0
}

// collapseTurnToolDisclosures auto-collapses every disclosure of the turn at
// settlement. Caller must hold m.mu.
func (m *replModel) collapseTurnToolDisclosures() {
	for _, id := range m.turnToolDisclosureIDs {
		if record := m.toolDisclosures[id]; record != nil {
			changed := record.expanded || record.imagesExpanded
			record.expanded = false
			record.imagesExpanded = false
			if changed {
				m.refreshToolDisclosure(record)
				// Image expansion is derived by the shared activity layout rather
				// than stored in the raw tool entry, so force that projection closed.
				m.invalidateVisual()
			}
		}
	}
}

func (m *replModel) entryVisualLineCount(index, width int) int {
	if index < 0 || index >= len(m.transcript) {
		return 0
	}
	if m.quiet && (m.reasoningAt[index] != 0 || m.toolDisclosureAt[index] != 0) {
		return 0
	}
	if width < 1 {
		width = 80
	}
	entry := m.transcript[index]
	if index == m.currentAssistant {
		entry = strings.TrimRight(entry, "\r\n")
		if entry == "" {
			return 0
		}
		entry += m.streamCursorFrame
	}
	followed := index < len(m.transcript)-1 || m.slashHints != ""
	rows, _ := transcriptBlockRowsWithImages(
		entry, followed, width, m.transcriptImages[index],
		m.nativeImages && width >= minimumImageThumbnailCols,
		m.imageCellWidth, m.imageCellHeight,
	)
	return len(rows)
}

func (m *replModel) entryVisualStart(index, width int) int {
	start := 0
	for i := 0; i < index; i++ {
		start += m.entryVisualLineCount(i, width)
	}
	return start
}

// resetToolDisclosure drops the active pointer while leaving the previous
// turn's disclosure in scrollback. Caller must hold m.mu.
func (m *replModel) resetToolDisclosure() {
	if record := m.currentToolDisclosure(); record != nil && record.transcriptIndex < 0 {
		delete(m.toolDisclosures, record.id)
	}
	m.turnToolDisclosureID = 0
}

func (m *replModel) clearToolDisclosures() {
	m.toolDisclosures = make(map[int64]*toolDisclosureRecord)
	m.toolDisclosureAt = make(map[int]int64)
	m.toolDisclosureSeq = 0
	m.turnToolDisclosureID = 0
	m.turnToolDisclosureIDs = nil
	m.toolDisclosurePlacements = nil
	m.imageDisclosurePlacements = nil
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
	label = stripTranscriptImageMarkers(label)
	mod := arrowPulse[int(elapsed/arrowPulsePeriod)%len(arrowPulse)]
	return "  " + styled("→", "run", mod) + " " +
		styledToolText(label) + " " +
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

// newReasoningRecord appends one stable disclosure row for a turn. Later
// reasoning segments update this record instead of adding more transcript
// rows. Caller must hold m.mu.
func (m *replModel) newReasoningRecord(complete bool) *reasoningRecord {
	m.reasoningSeq++
	record := &reasoningRecord{id: m.reasoningSeq, complete: complete}
	if !complete {
		record.expanded = m.turnReasoningOpen
		// The pending shortcut applies only to the first record it creates. An
		// expanded record must not silently pre-arm later reasoning segments.
		m.turnReasoningOpen = false
	}
	m.appendLine("")
	record.transcriptIndex = len(m.transcript) - 1
	m.reasoningRecords[record.id] = record
	m.reasoningAt[record.transcriptIndex] = record.id
	m.reasoningOrder = append(m.reasoningOrder, record.id)
	m.refreshReasoningRecord(record, 80)
	return record
}

func (m *replModel) currentReasoningRecord() *reasoningRecord {
	if m.turnReasoningID == 0 {
		return nil
	}
	return m.reasoningRecords[m.turnReasoningID]
}

// appendThinking adds one streamed provider chunk to this turn's bounded UI
// tail. Complete reasoning remains on the assistant ChatMessage and is not
// duplicated here. Caller must hold m.mu.
func (m *replModel) appendThinking(chunk string) {
	if chunk == "" {
		return
	}
	record := m.currentReasoningRecord()
	resumed := false
	if record == nil {
		// A new disclosure opens at the current transcript position only when
		// assistant prose broke the run (or the turn just started), so an
		// unbroken thinking→tools→thinking continuation keeps aggregating
		// into one indicator instead of stranding an expanded block.
		record = m.newReasoningRecord(false)
		m.turnReasoningID = record.id
		m.turnReasoningIDs = append(m.turnReasoningIDs, record.id)
		m.thinkingSegmentOpen = true
		m.thinkingSegmentStart = time.Now()
		record.active = true
	} else if !m.thinkingSegmentOpen {
		// Resuming after a tool phase paused the segment: same record, fresh
		// clock, one semantic break between the tool-separated passages.
		m.thinkingSegmentOpen = true
		m.thinkingSegmentStart = time.Now()
		record.active = true
		resumed = true
	}
	m.appendReasoningTail(record, chunk, resumed)
	// Width-aware wrapping belongs to the render loop. Provider callbacks only
	// mutate the bounded semantic tail under the model lock.
}

// appendReasoningTail adds text to a bounded rune tail. segmentBreak inserts
// one semantic newline between tool-separated assistant reasoning segments.
func (m *replModel) appendReasoningTail(record *reasoningRecord, text string, segmentBreak bool) {
	if record == nil {
		return
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	// Expanded reasoning can share a visual block with tool image sidecars.
	// Remove reserved slot runes before they can claim those tool images.
	text = stripTranscriptImageMarkers(text)
	addition := []rune(text)
	if segmentBreak && len(record.tail) > 0 && record.tail[len(record.tail)-1] != '\n' {
		withBreak := make([]rune, 1, len(addition)+1)
		withBreak[0] = '\n'
		addition = append(withBreak, addition...)
	}
	if len(addition) >= reasoningTailRetainRunes {
		record.tail = append(record.tail[:0], addition[len(addition)-reasoningTailRetainRunes:]...)
		record.tailVersion++
		record.dirty = true
		return
	}
	// Compact at the hard limit instead of copying the whole tail on every
	// streamed token after the retained target first fills.
	if len(record.tail)+len(addition) > reasoningTailLimitRunes {
		keep := reasoningTailRetainRunes - len(addition)
		next := make([]rune, 0, reasoningTailRetainRunes)
		if keep > 0 && len(record.tail) > keep {
			next = append(next, record.tail[len(record.tail)-keep:]...)
		} else if keep > 0 {
			next = append(next, record.tail...)
		}
		record.tail = append(next, addition...)
		record.tailVersion++
		record.dirty = true
		return
	}
	record.tail = append(record.tail, addition...)
	if len(addition) > 0 {
		record.tailVersion++
		record.dirty = true
	}
}

// finishThinkingSegment closes the current provider segment's disclosure.
// The segment settles in place; a later reasoning segment opens a fresh
// disclosure at its own transcript position. Caller must hold m.mu.
func (m *replModel) finishThinkingSegment() {
	record := m.currentReasoningRecord()
	if record == nil {
		m.thinkingSegmentOpen = false
		m.thinkingSegmentStart = time.Time{}
		return
	}
	// The record may be paused (tool phase mid-run) rather than live; either
	// way it is the current run and prose or settlement closes it here. A
	// paused record already banked its elapsed time.
	if m.thinkingSegmentOpen {
		record.elapsed += time.Since(m.thinkingSegmentStart)
	}
	record.active = false
	record.complete = true
	record.dirty = true
	// Refresh unconditionally: reasoningWidth may be zero in provider
	// callbacks, and refreshReasoningRecord treats that as "keep the
	// existing text", which would strand the live "Thinking…" label.
	// A deliberately expanded segment stays open until the turn settles.
	m.refreshReasoningRecord(record, m.reasoningWidth)
	m.turnReasoningID = 0
	m.thinkingSegmentOpen = false
	m.thinkingSegmentStart = time.Time{}
}

// pauseThinkingSegment banks the running segment's elapsed time and stops its
// live clock without closing the record: the turn's next reasoning chunk
// resumes the same disclosure. Tool phases pause; only assistant prose (or
// turn settlement) closes the run. Caller must hold m.mu.
func (m *replModel) pauseThinkingSegment() {
	if !m.thinkingSegmentOpen {
		return
	}
	if record := m.currentReasoningRecord(); record != nil {
		record.elapsed += time.Since(m.thinkingSegmentStart)
		record.active = false
		record.dirty = true
		m.refreshReasoningRecord(record, m.reasoningWidth)
	}
	m.thinkingSegmentOpen = false
	m.thinkingSegmentStart = time.Time{}
}

// completeThinkingTurn settles any still-open reasoning segment and marks
// whether the provider-generated messages were saved. Caller must hold m.mu.
func (m *replModel) completeThinkingTurn(unsaved bool) {
	m.finishThinkingSegment()
	// Every segment of the turn auto-collapses at settlement; the "not saved"
	// marker lands on each when the turn persisted nothing.
	for _, id := range m.turnReasoningIDs {
		record := m.reasoningRecords[id]
		if record == nil {
			continue
		}
		record.complete = true
		record.expanded = false
		if unsaved {
			record.unsaved = true
		}
		record.dirty = true
		m.refreshReasoningRecord(record, m.reasoningWidth)
	}
	m.resetCurrentThinking()
}

func (m *replModel) resetCurrentThinking() {
	m.turnReasoningID = 0
	m.turnReasoningIDs = nil
	m.turnReasoningOpen = false
	m.thinkingSegmentOpen = false
	m.thinkingSegmentStart = time.Time{}
}

func (m *replModel) clearReasoningRecords() {
	m.reasoningRecords = make(map[int64]*reasoningRecord)
	m.reasoningAt = make(map[int]int64)
	m.reasoningOrder = nil
	m.reasoningPlacements = nil
	m.reasoningSeq = 0
	m.resetCurrentThinking()
}

func (m *replModel) refreshReasoningRecords(width int) {
	widthChanged := width > 0 && width != m.reasoningWidth
	if width > 0 {
		m.reasoningWidth = width
	}
	if widthChanged {
		for _, id := range m.reasoningOrder {
			m.refreshReasoningRecord(m.reasoningRecords[id], width)
		}
		return
	}
	// Refresh every record of the active turn: a turn now spans several
	// per-segment disclosures, and a settled segment may still be dirty.
	for _, id := range m.turnReasoningIDs {
		if record := m.reasoningRecords[id]; record != nil && (record.active || record.dirty) {
			m.refreshReasoningRecord(record, width)
		}
	}
}

func (m *replModel) refreshReasoningRecord(record *reasoningRecord, width int) {
	m.refreshReasoningRecordWithAnchor(record, width, true)
}

func (m *replModel) refreshReasoningRecordWithAnchor(record *reasoningRecord, width int, reanchor bool) {
	if record == nil || record.transcriptIndex < 0 || record.transcriptIndex >= len(m.transcript) {
		return
	}
	width = m.disclosureLayoutWidth(width)
	next := m.reasoningRecordText(record, width)
	record.dirty = false
	if m.transcript[record.transcriptIndex] == next {
		return
	}
	// Reasoning renders inline in the transcript, so growth re-anchors a held
	// viewport like any other transcript mutation.
	if !reanchor {
		m.setTranscriptText(record.transcriptIndex, next)
		return
	}
	m.mutateAnchored(width, matchReasoningBlock(record.id), func(bool) {
		m.setTranscriptText(record.transcriptIndex, next)
	})
}

func (m *replModel) reasoningRecordText(record *reasoningRecord, width int) string {
	glyph := "▸"
	if record.expanded {
		glyph = "▾"
	}

	elapsed := record.elapsed
	if record.active && !m.thinkingSegmentStart.IsZero() && record.id == m.turnReasoningID {
		elapsed += time.Since(m.thinkingSegmentStart)
	}
	label := reasoningDisclosureLabel(record.active, record.unsaved, elapsed)
	header := "  " + inlineActivityControl(glyph, label, record.complete && !record.active)
	if !record.expanded {
		return header
	}

	contentWidth := width - rw.StringWidth(reasoningBlockIndent)
	if contentWidth < 2 {
		return header
	}
	if record.previewWidth != contentWidth || record.previewVersion != record.tailVersion {
		record.previewLines = reasoningTailLines(string(record.tail), contentWidth, reasoningPreviewLines)
		record.previewWidth = contentWidth
		record.previewVersion = record.tailVersion
	}
	lines := record.previewLines
	if len(lines) == 0 {
		return header
	}
	var b strings.Builder
	b.WriteString(header)
	// Keep the disclosure visually stable and worth opening even for a short
	// thought: when the terminal is wide enough for detail, reserve two rows.
	detailRows := max(2, len(lines))
	for i := 0; i < detailRows; i++ {
		line := ""
		if i < len(lines) {
			line = lines[i]
		}
		b.WriteString("\n")
		b.WriteString(reasoningBlockIndent)
		b.WriteString(styled(line, "muted", "italic"))
	}
	return b.String()
}

func reasoningDisclosureLabel(active, unsaved bool, elapsed time.Duration) string {
	// A paused segment (tool phase mid-run) reads "Thought" like a settled
	// one; the header styling, not the label, distinguishes live from done.
	label := "thought"
	if active {
		label = "thinking…"
	}
	if elapsed > 0 {
		label += " " + formatElapsed(elapsed)
	}
	if unsaved {
		label += " · not saved"
	}
	return label
}

// reasoningTailLines wraps the retained text for the current terminal width,
// then returns only its newest physical rows.
func reasoningTailLines(text string, width, limit int) []string {
	if width < 1 || limit < 1 {
		return nil
	}
	lines := make([]string, 0, limit)
	push := func(line string) {
		line = strings.TrimSpace(line)
		if line == "" {
			return
		}
		if len(lines) == limit {
			copy(lines, lines[1:])
			lines[len(lines)-1] = line
			return
		}
		lines = append(lines, line)
	}

	for _, paragraph := range strings.Split(strings.TrimSpace(text), "\n") {
		current := ""
		currentWidth := 0
		flush := func() {
			push(current)
			current = ""
			currentWidth = 0
		}
		for _, word := range strings.Fields(paragraph) {
			wordWidth := rw.StringWidth(word)
			if wordWidth > width {
				flush()
				pieces := splitReasoningWord(word, width)
				for i, piece := range pieces {
					if i < len(pieces)-1 {
						push(piece)
						continue
					}
					current = piece
					currentWidth = rw.StringWidth(piece)
				}
				continue
			}
			if current == "" {
				current, currentWidth = word, wordWidth
				continue
			}
			if currentWidth+1+wordWidth <= width {
				current += " " + word
				currentWidth += 1 + wordWidth
				continue
			}
			flush()
			current, currentWidth = word, wordWidth
		}
		flush()
	}
	return lines
}

func splitReasoningWord(word string, width int) []string {
	var pieces []string
	var chunk []rune
	chunkWidth := 0
	for _, r := range word {
		runeWidth := max(0, rw.RuneWidth(r))
		if len(chunk) > 0 && chunkWidth+runeWidth > width {
			pieces = append(pieces, string(chunk))
			chunk = chunk[:0]
			chunkWidth = 0
		}
		chunk = append(chunk, r)
		chunkWidth += runeWidth
	}
	if len(chunk) > 0 {
		pieces = append(pieces, string(chunk))
	}
	return pieces
}

// anchorForResizedEntry keeps the viewport steady when the entry at visual
// offset start changes height. An entry wholly above the anchor shifts the
// anchor by the height delta; an entry containing the anchor keeps the
// anchor's relative position inside the entry instead of snapping to its top.
func (m *replModel) anchorForResizedEntry(start, oldCount, newCount int) {
	if m.followBottom || oldCount == newCount {
		return
	}
	delta := newCount - oldCount
	switch {
	case start+oldCount <= m.scrollAnchor:
		// Entry is entirely above the anchor: shift by the height change.
		m.scrollAnchor += delta
	case start < m.scrollAnchor:
		// Entry straddles the anchor: preserve the anchor's fractional
		// position within the entry so the viewport does not jump.
		rel := m.scrollAnchor - start
		if oldCount > 0 {
			rel = rel * newCount / oldCount
		}
		m.scrollAnchor = start + rel
	}
	if m.scrollAnchor < 0 {
		m.scrollAnchor = 0
	}
}

func (m *replModel) toggleReasoning(recordID int64, width int) bool {
	record := m.reasoningRecords[recordID]
	if record == nil || record.transcriptIndex < 0 || record.transcriptIndex >= len(m.transcript) {
		return false
	}
	if width > 0 {
		m.reasoningWidth = width
	}
	record.expanded = !record.expanded
	m.refreshReasoningRecord(record, width)
	return true
}

// disclosureLayoutWidth resolves the width inline activity is laid out at:
// the explicit width when usable, else the last renderer width, else 80.
func (m *replModel) disclosureLayoutWidth(width int) int {
	if width < 2 {
		width = m.reasoningWidth
	}
	if width < 2 {
		return 80
	}
	return width
}

// latestTurnReasoningGroup returns the current turn's newest projected inline
// reasoning group. Adjacent thought/tool records share one visual control, so
// the keyboard shortcut must use the same grouping as mouse hit-testing.
func (m *replModel) latestTurnReasoningGroup(width int) []int64 {
	if len(m.turnReasoningIDs) == 0 {
		return nil
	}
	currentTurn := make(map[int64]struct{}, len(m.turnReasoningIDs))
	for _, id := range m.turnReasoningIDs {
		currentTurn[id] = struct{}{}
	}
	blocks := m.transcriptDisplayEntries(m.disclosureLayoutWidth(width))
	for i := len(blocks) - 1; i >= 0; i-- {
		var ids []int64
		for _, id := range blocks[i].reasoningIDs {
			if _, ok := currentTurn[id]; ok && m.reasoningRecords[id] != nil {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			return ids
		}
	}
	// Quiet mode does not project inline activity. Keep the shortcut useful by
	// falling back to the newest current-turn record in that mode.
	for i := len(m.turnReasoningIDs) - 1; i >= 0; i-- {
		id := m.turnReasoningIDs[i]
		if m.reasoningRecords[id] != nil {
			return []int64{id}
		}
	}
	return nil
}

func (m *replModel) toggleLatestReasoning(width int) bool {
	if m.busy {
		if ids := m.latestTurnReasoningGroup(width); len(ids) > 0 {
			if len(ids) == 1 {
				return m.toggleReasoning(ids[0], width)
			}
			return m.toggleReasoningGroup(ids, width)
		}
		// No current-turn record exists yet. Remember the user's choice only
		// until the first record is created; never target an older turn.
		m.turnReasoningOpen = !m.turnReasoningOpen
		return true
	}
	for i := len(m.reasoningOrder) - 1; i >= 0; i-- {
		if m.toggleReasoning(m.reasoningOrder[i], width) {
			return true
		}
	}
	return false
}

func (m *replModel) toggleToolDisclosure(recordID int64) bool {
	record := m.toolDisclosures[recordID]
	if record == nil || record.transcriptIndex < 0 || record.transcriptIndex >= len(m.transcript) {
		return false
	}
	record.expanded = !record.expanded
	m.refreshToolDisclosure(record)
	return true
}

// refreshActiveTools updates each running disclosure row with the current
// breathing-arrow frame and live elapsed time. The arrow pulses every
// arrowPulsePeriod and the timer rolls each second, so the disclosure is
// refreshed whenever either boundary has crossed since the last paint —
// comparing row text alone would freeze the timer for the ~1s stretches
// where formatElapsed's output is unchanged. The header is always visible,
// so this repaints whether or not the detail rows are expanded.
func (m *replModel) refreshActiveTools() {
	if len(m.activeTools) == 0 {
		return
	}
	record := m.currentToolDisclosure()
	if record == nil {
		return
	}
	// The fastest-changing element is the 500ms arrow pulse; repaint once per
	// pulse phase. Elapsed-time changes are subsumed by that finer cadence.
	phase := int(time.Since(m.activeTools[0].started) / arrowPulsePeriod)
	if phase == m.activeToolsPhase {
		return
	}
	m.activeToolsPhase = phase
	for _, at := range m.activeTools {
		if at.row < 0 || at.row >= len(record.rows) {
			continue
		}
		record.rows[at.row].line = runningToolLine(at.label, time.Since(at.started))
	}
	m.refreshToolDisclosure(record)
}

// takeActiveTool stops tracking a finished tool and returns its disclosure row.
// It matches by call ID, falling back to the oldest still-running row when an
// ID is absent. Caller must hold m.mu.
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
	row := m.activeTools[pick].row
	m.activeTools = append(m.activeTools[:pick], m.activeTools[pick+1:]...)
	return row, true
}

func (m *replModel) settleActiveTools(reason string) {
	if len(m.activeTools) == 0 {
		return
	}
	record := m.currentToolDisclosure()
	if record == nil {
		return
	}
	for _, at := range m.activeTools {
		if at.row < 0 || at.row >= len(record.rows) {
			continue
		}
		row := &record.rows[at.row]
		row.line = toolErrorLine(at.label, reason, "")
		row.images = nil
		row.settled = true
	}
	if record.expanded {
		m.refreshToolDisclosure(record)
	}
}

// toolOKLine / toolDeniedLine / toolErrorLine build the final transcript entry
// for a completed tool call. They return the styled string (rather than
// appending) so AppendToolEnd can freeze it over the running line in place.

// styledToolText protects arbitrary labels from gotui's inline-style parser.
// A command may contain an unmatched bracket (for example, grep "["), which
// would otherwise keep the parser nested and expose this row's style markup as
// literal text. Tool rows always flow through parseStyledCells, which restores
// these temporary bracket runes after assigning the intended style.
func styledToolText(text string) string {
	return styledCodeLiteral(stripTranscriptImageMarkers(text), "muted", "")
}

func toolOKLine(label, duration, meta string) string {
	return "  " + styled("✓", "ok", "bold") + " " + styledToolText(toolLineBody(duration, label, meta))
}

func toolDeniedLine(label string) string {
	return "  " + styled("✗", "err", "bold") + " " + styledToolText("denied "+label)
}

// toolErrorLine renders a failed tool call as a red ✗ plus the muted metadata
// (timing · command · exit code) — the same shape as a success line. The
// tool's own output/error text is deliberately not shown; the model still
// receives the full output, this is display only.
func toolErrorLine(label, duration, meta string) string {
	return "  " + styled("✗", "err", "bold") + " " + styledToolText(toolLineBody(duration, label, meta))
}

func hydratedToolLine(label string, msg messages.ChatMessage) string {
	label = stripTranscriptImageMarkers(label)
	if toolWasDenied(msg.Content) {
		return toolDeniedLine(label)
	}
	if msg.IsError() {
		return toolErrorLine(label, "", "failed")
	}
	if succeeded, known := msg.ToolSucceeded(); known {
		if succeeded {
			return toolOKLine(label, "", "")
		}
		return toolErrorLine(label, "", "failed")
	}
	return "  " + styled("·", "muted", "bold") + " " + styledToolText(label)
}

func (m *replModel) appendCompletedToolDisclosure(rows []toolDisclosureRow) *toolDisclosureRecord {
	if len(rows) == 0 {
		return nil
	}
	m.toolDisclosureSeq++
	record := &toolDisclosureRecord{
		id:              m.toolDisclosureSeq,
		transcriptIndex: len(m.transcript),
		rows:            append([]toolDisclosureRow(nil), rows...),
		complete:        true,
	}
	for i := range record.rows {
		record.rows[i].label = stripTranscriptImageMarkers(record.rows[i].label)
		if len(record.rows[i].images) == 0 {
			record.rows[i].line = stripTranscriptImageMarkers(record.rows[i].line)
		}
		if record.rows[i].line == "" {
			record.rows[i].line = "  " + styled("·", "muted", "bold") + " " + styledToolText(record.rows[i].label)
		}
	}
	m.appendLine("")
	record.transcriptIndex = len(m.transcript) - 1
	m.toolDisclosures[record.id] = record
	m.toolDisclosureAt[record.transcriptIndex] = record.id
	m.refreshToolDisclosure(record)
	return record
}

func (m *replModel) appendNoticeLine(text string) {
	m.appendLine(styled(text, "muted", ""))
}

// appendErrorLine reports a failure inline and follows it, so the user sees
// why the composer kept their input.
func (m *replModel) appendErrorLine(text string) {
	m.appendLine(styled("Error: "+text, "err", ""))
	m.followBottom = true
}

func (m *replModel) clearDisplay() {
	m.transcript = nil
	m.transcriptImages = nil
	m.currentAssistant = -1
	m.activeTools = nil
	m.activeToolsPhase = -1
	m.clearToolDisclosures()
	m.clearReasoningRecords()
	m.turnTrailers = make(map[int64]*turnTrailerRecord)
	m.turnTrailerAt = make(map[int]int64)
	m.turnTrailerSeq = 0
	m.turnTrailerPlacements = nil
	m.openTurnTrailerID = 0
	m.turnDock.overlay = turnDockOverlayNone
	for i := range m.queue {
		m.queue[i].transcriptShown = false
	}
	if !m.busy {
		m.runningTools = 0
		m.clearTurnDock()
	}
	m.resetAssistantStream()
	m.scrollAnchor = 0
	m.followBottom = true
	m.invalidateVisual()
}

const resumedTurnLimit = 5

// hydrateHistory makes a resumed context honest about what the model already
// remembers. It shows only recent user turns, keeps assistant prose, and folds
// raw tool exchanges into compact activity rows.
func (m *replModel) hydrateHistory(history []messages.ChatMessage, contextName string) {
	m.clearTurnDock()
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

	var hydratedToolRows []toolDisclosureRow
	var hydratedToolDisclosure *toolDisclosureRecord
	flushTools := func() {
		if len(hydratedToolRows) == 0 {
			return
		}
		if hydratedToolDisclosure == nil {
			hydratedToolDisclosure = m.appendCompletedToolDisclosure(hydratedToolRows)
		} else {
			for i := range hydratedToolRows {
				if hydratedToolRows[i].line == "" {
					hydratedToolRows[i].line = "  " + styled("·", "muted", "bold") + " " +
						styledToolText(hydratedToolRows[i].label)
				}
			}
			hydratedToolDisclosure.rows = append(hydratedToolDisclosure.rows, hydratedToolRows...)
			m.refreshToolDisclosure(hydratedToolDisclosure)
		}
		hydratedToolRows = nil
	}
	applyToolOrder := func(order []durableDisplayToolCall) {
		if len(order) == 0 {
			return
		}
		var existing []toolDisclosureRow
		if hydratedToolDisclosure != nil {
			existing = hydratedToolDisclosure.rows
		}
		used := make([]bool, len(existing))
		ordered := make([]toolDisclosureRow, 0, max(len(order), len(existing)))
		for _, displayCall := range order {
			name := displayCall.Name
			if name == "" {
				name = "tool"
			}
			pick := -1
			for i := range existing {
				if !used[i] && displayCall.ID != "" && existing[i].callID == displayCall.ID {
					pick = i
					break
				}
			}
			if pick < 0 {
				for i := range existing {
					if !used[i] && existing[i].label == name {
						pick = i
						break
					}
				}
			}
			row := toolDisclosureRow{callID: displayCall.ID, label: name, settled: true}
			if pick >= 0 {
				used[pick] = true
				row = existing[pick]
			}
			if displayCall.Denied {
				row.line = toolDeniedLine(name)
				row.images = nil
				row.settled = true
			} else if row.line == "" {
				row.line = "  " + styled("·", "muted", "bold") + " " + styledToolText(name)
			}
			ordered = append(ordered, row)
		}
		for i, row := range existing {
			if !used[i] {
				ordered = append(ordered, row)
			}
		}
		if hydratedToolDisclosure == nil {
			hydratedToolDisclosure = m.appendCompletedToolDisclosure(ordered)
			return
		}
		hydratedToolDisclosure.rows = ordered
		hydratedToolDisclosure.complete = true
		hydratedToolDisclosure.expanded = false
		m.refreshToolDisclosure(hydratedToolDisclosure)
	}

	lastRole := ""
	var lastUserTurn *managedTurnInput
	lastUserContextOnly := false
	var hydratedReasoning *reasoningRecord
	turnInput, turnOutput := 0, 0
	appendHydratedReasoning := func(text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		if hydratedReasoning == nil {
			hydratedReasoning = m.newReasoningRecord(true)
		}
		m.appendReasoningTail(hydratedReasoning, text, len(hydratedReasoning.tail) > 0)
		m.refreshReasoningRecord(hydratedReasoning, 80)
	}
	finishHydratedTurn := func() {
		flushTools()
		m.setHydratedTurnDock(hydratedReasoning, hydratedToolDisclosure, turnInput, turnOutput)
		m.attachTurnDockTrailer()
	}
	for _, msg := range history[start:] {
		switch msg.Role {
		case messages.MessageRoleUser:
			if agentSyntheticMessage(msg) {
				continue
			}
			if lastRole != "" && lastRole != messages.MessageRoleUser {
				finishHydratedTurn()
			} else {
				flushTools()
				m.clearTurnDock()
			}
			hydratedToolDisclosure = nil
			hydratedReasoning = nil
			turnInput, turnOutput = 0, 0
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
			appendHydratedReasoning(msg.Reasoning)
			if tokens := msg.GetInputTokens(); tokens > turnInput {
				turnInput = tokens
			}
			turnOutput += msg.GetOutputTokens()
			if content := msg.GetContent(); content != "" {
				m.appendAssistant(content)
				m.finishAssistantBlock("")
			}
			for _, call := range msg.ToolCalls {
				name := call.Name
				if name == "" {
					name = "tool"
				}
				hydratedToolRows = append(hydratedToolRows, toolDisclosureRow{
					callID: call.ID,
					label:  name,
				})
			}
			lastRole = msg.Role
		case messages.MessageRoleTool:
			inspectionImages := inspectionTranscriptImages(msg, m.artifactStore)
			if len(hydratedToolRows) == 0 {
				name := msg.ToolName
				if name == "" {
					name = "tool"
				}
				hydratedToolRows = append(hydratedToolRows, toolDisclosureRow{
					callID: msg.ToolCallID,
					label:  name,
				})
			}
			pick := -1
			for i := range hydratedToolRows {
				if hydratedToolRows[i].settled {
					continue
				}
				if hydratedToolRows[i].callID == msg.ToolCallID {
					pick = i
					break
				}
				if pick < 0 {
					pick = i
				}
			}
			if pick >= 0 {
				hydratedToolRows[pick].line = hydratedToolLine(hydratedToolRows[pick].label, msg)
				hydratedToolRows[pick].inspectionImages = inspectionImages
				hydratedToolRows[pick].settled = true
			}
			lastRole = msg.Role
		case messages.MessageRoleInternal:
			flushTools()
			displayToolCalls := decodeDisplayToolCalls(msg.Metadata[messages.MetadataKeyDisplayToolCalls])
			if displayReasoning, _ := msg.Metadata[messages.MetadataKeyDisplayReasoning].(string); displayReasoning != "" {
				appendHydratedReasoning(displayReasoning)
			}
			applyToolOrder(displayToolCalls)
			status, _ := msg.Metadata[messages.MetadataKeyTurnStatus].(string)
			switch {
			case status == messages.TurnStatusToolDenied:
				if len(displayToolCalls) == 0 {
					// Compatibility with sessions written before safe tool display
					// metadata existed.
					m.appendLine("  " + styled("✗", "err", "bold") + " " + styled("tool request denied", "muted", ""))
				}
				// A durable internal completion marker settles the preceding user
				// turn without becoming model-visible assistant content.
				lastRole = messages.MessageRoleAssistant
			case status == messages.TurnStatusInterrupted:
				// Everything before the marker is durable completed work; the
				// turn ended early without a final response. Settle it so the
				// preceding user message isn't restored as an unsent draft.
				m.appendLine("  " + styled("turn interrupted · completed work retained", "muted", ""))
				lastRole = messages.MessageRoleAssistant
			case len(displayToolCalls) > 0:
				lastRole = messages.MessageRoleAssistant
			}
		}
	}
	if lastRole != "" && lastRole != messages.MessageRoleUser {
		finishHydratedTurn()
	} else {
		flushTools()
	}
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

// beginManagedTurn echoes a user prompt and marks a turn in flight. Shared by
// the idle submit path and the queued-prompt drain; neither records history
// here (callers do that when the text is first accepted). Caller must hold
// m.mu.
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
		m.setTranscriptEntry(index,
			stripTranscriptImageMarkers(m.transcript[index])+"\n"+renderTranscriptImages(images, "  "),
			images)
	}
}

// appendQueuedInput echoes accepted input without settling the assistant block
// that may still be streaming. The transcript, rather than the status bar, is
// the visible acknowledgement that Polly retained the input.
func (m *replModel) appendQueuedInput(item *queuedREPLInput) {
	if len(m.transcript) > 0 && m.transcript[len(m.transcript)-1] != "" {
		m.appendTranscriptEntry("")
	}
	entry := formattedUserPrompt(item.text) + "\n  " + styled("(queued)", "muted", "")
	item.transcriptIndex = m.appendTranscriptEntry(entry)
	item.transcriptShown = true
	if item.turn != nil {
		m.decorateUserPrompt(item.transcriptIndex, *item.turn)
	}
	m.followBottom = true
}

func (m *replModel) activateQueuedInput(item queuedREPLInput) {
	if item.transcriptShown && item.transcriptIndex >= 0 && item.transcriptIndex < len(m.transcript) {
		m.setTranscriptText(item.transcriptIndex, formattedUserPrompt(item.text))
		if item.turn != nil {
			m.decorateUserPrompt(item.transcriptIndex, *item.turn)
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
	m.setTranscriptText(item.transcriptIndex, formattedUserPrompt(item.text)+"\n  "+styled("(not sent)", "muted", ""))
	if item.turn != nil {
		m.decorateUserPrompt(item.transcriptIndex, *item.turn)
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
	m.startTurnDock()
	m.busy = true
	m.canceling = false
	m.state = turnStateWaiting
	m.runningTools = 0
	m.activeToolsPhase = -1
	m.resetToolDisclosure()
	m.turnToolDisclosureIDs = nil
	m.resetCurrentThinking()
	m.turnStarted = time.Now()
	m.currentPrompt = prompt
	m.currentTurn = cloneManagedTurn(turn)
	if m.currentPersistence == nil {
		m.currentPersistence = newTurnPersistenceAck(false)
	}
	m.turnHasOutput = false
	m.outcomeLabeled = false
	m.lastOutcome = turnOutcomeNone
	// Token counts are per-turn and appear in the dock once reported.
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

// renderInputForTerminal produces the bottom input region: its display text
// (possibly multiple lines), the row count it occupies, the cursor's row/col
// within that region, and whether the cursor should be shown. The
// busy/approval/search overlays are single-line and hide the cursor; the
// editable prompt may span several rows, anchored to the bottom when it
// overflows maxRows. Caller must hold m.mu.
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
	turnDockW   *widgets.Paragraph
	statusW     *widgets.Paragraph
	modalW      *modalParagraph
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
	showStartupLogo    bool
	startupLogoVisible bool

	// histFile is the append handle for persistent input history; nil when
	// history couldn't be opened (best-effort — never fatal).
	histFile *os.File

	// resumeContext is set by the /resume picker before the managed screen
	// exits. The outer conversation loop then closes this runtime and restores
	// the selected session through the normal initialization path.
	resumeContext string
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
	// OverlayBottom, when non-empty, replaces the pane's bottom rows with the
	// turn-detail drawer and/or scrolled-up activity ticker. Covered transcript
	// rows remain reachable by scrolling; opening the drawer never reflows them.
	OverlayBottom [][]ui.Cell
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

	overlay := p.OverlayBottom
	if len(overlay) > height {
		overlay = overlay[len(overlay)-height:]
	}
	for i, row := range overlay {
		y := height - len(overlay) + i
		for x := 0; x < p.Inner.Dx(); x++ {
			buf.SetCell(ui.Cell{Rune: ' ', Style: ui.StyleClear}, image.Pt(x, y).Add(p.Inner.Min))
		}
		for _, cx := range ui.BuildCellWithXArray(row) {
			if cx.X >= p.Inner.Dx() || rw.RuneWidth(cx.Cell.Rune) == 0 {
				continue
			}
			buf.SetCell(cx.Cell, image.Pt(cx.X, y).Add(p.Inner.Min))
		}
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
	m.modelName = config.Model
	m.contextLimit = config.MaxHistoryTokens
	if config.Model != "" {
		m.recentModels = []string{config.Model}
	}
	if contextName == "" {
		contextName = "-"
	}
	m.contextName = contextName
	m.toolCount = toolCount
	m.skillCount = skillCount
	m.quiet = config.Quiet
	return &managedREPL{
		config:          config,
		model:           m,
		quit:            make(chan struct{}, 1),
		suspend:         make(chan struct{}, 1),
		pending:         make(chan managedTurnInput, 1),
		uiTasks:         make(chan func(), 8),
		openImage:       openImageInViewer,
		suspendProcess:  suspendCurrentProcessGroup,
		showStartupLogo: true,
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
	r.startupLogoVisible = r.showStartupLogo
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
	m.turnDock.reasoningIDs = append([]int64(nil), m.turnReasoningIDs...)
	m.turnDock.toolIDs = append([]int64(nil), m.turnToolDisclosureIDs...)
	// A partially persisted turn keeps its completed iterations — reasoning
	// included — so only a turn that saved nothing marks its thinking unsaved.
	progressSaved := turnProgressSaved(err)
	m.completeThinkingTurn(err != nil && !progressSaved)
	m.settleActiveTools(activeToolReason)
	m.activeTools = nil
	m.activeToolsPhase = -1
	m.runningTools = 0
	// Like reasoning, tool activity defaults closed and auto-collapses when the
	// turn settles. Canceled/failed row details remain one click away.
	m.completeToolDisclosure()
	m.collapseTurnToolDisclosures()
	// Only call out data loss. Persisted progress needs no extra success label.
	unsavedSuffix := ""
	if !progressSaved {
		unsavedSuffix = " · not saved"
	}
	switch {
	case err == nil:
		m.finishAssistantBlock("")
		if !m.turnHasOutput {
			m.appendNoticeLine("(no response)")
		}
		m.lastOutcome = turnOutcomeDone
	case errors.Is(err, context.Canceled):
		m.finishAssistantBlock("canceled" + unsavedSuffix)
		m.labelTurnOutcome("canceled" + unsavedSuffix)
		m.lastOutcome = turnOutcomeCanceled
		m.discardQueuedInputs()
		if !m.restoreTurnDraft(m.currentTurn, m.currentPersistence) {
			m.appendNoticeLine("input available with ↑ · current draft preserved")
		}
	default:
		m.finishAssistantBlock("failed" + unsavedSuffix)
		m.labelTurnOutcome("failed" + unsavedSuffix)
		m.appendLine(styled("Error: "+err.Error(), "err", ""))
		m.lastOutcome = turnOutcomeFailed
		m.discardQueuedInputs()
		if !m.restoreTurnDraft(m.currentTurn, m.currentPersistence) {
			m.appendNoticeLine("input available with ↑ · current draft preserved")
		}
	}
	m.settleTurnDock()
	m.attachTurnDockTrailer()
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
	// Detaching advances the turn generation, which also revokes the stuck
	// turn's session write via TurnPersistenceAllowed — newer turns may start
	// appending, so its late results must be dropped. "not saved" stays true.
	m.finishAssistantBlock("canceled · not saved")
	m.labelTurnOutcome("canceled · not saved")
	m.busy = false
	m.canceling = false
	m.currentAssistant = -1
	m.resetAssistantStream()
	m.turnStarted = time.Time{}
	m.toolName = ""
	m.turnDock.reasoningIDs = append([]int64(nil), m.turnReasoningIDs...)
	m.turnDock.toolIDs = append([]int64(nil), m.turnToolDisclosureIDs...)
	m.completeThinkingTurn(true)
	m.settleActiveTools("canceled")
	m.activeTools = nil
	m.activeToolsPhase = -1
	m.runningTools = 0
	m.completeToolDisclosure()
	m.collapseTurnToolDisclosures()
	m.denyApprovalLocked()
	m.state = turnStateIdle
	m.lastOutcome = turnOutcomeCanceled
	m.settleTurnDock()
	m.attachTurnDockTrailer()
	restored := cloneManagedTurn(m.currentTurn)
	restoredPersistence := m.currentPersistence
	m.currentPrompt = ""
	m.currentTurn = managedTurnInput{}
	m.currentPersistence = nil
	m.discardQueuedInputs()
	if !m.restoreTurnDraft(restored, restoredPersistence) {
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
	r.cancelBusyTurn()
	return false
}

// cancelBusyTurn cancels the in-flight turn: freezes the visible partial,
// cancels the turn context, and denies any pending approval so the turn
// goroutine isn't parked on the reply channel. Pending input is marked not sent
// when cancellation settles. Caller must hold m.mu and ensure m.busy &&
// !m.canceling.
func (r *managedREPL) cancelBusyTurn() {
	m := r.model
	m.canceling = true
	// Freeze the visible partial immediately, but do not label it unsaved until
	// the turn actually settles as canceled. Completion and cancel can race; a
	// successful result must never retain a false "not saved" label.
	m.finishAssistantBlock("")
	r.cancelTurn()
	m.denyApprovalLocked()
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
		if ch, ok := printableRune(e); ok {
			m.searchType(ch)
			return false
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
	return max(1, r.frameLayoutFor(ui.TerminalDimensions()).transcriptHeight)
}

// frameLayout is the vertical geometry of one frame: how the terminal height
// splits, top to bottom, between the logo band, the transcript pane, the turn
// dock, the divider, the composer, and the status bar. render and the scroll
// handlers derive it the same way, so scroll deltas match the pane the user
// actually sees.
type frameLayout struct {
	width, height    int
	logoRows         int
	transcriptHeight int
	dockRows         int
	dividerRows      int
	inputRows        int
	statusRows       int
}

// frameLayoutFor splits a w×h terminal. The composer is the only region whose
// height follows its content, so it is measured first and then capped to what
// the fixed chrome leaves; the divider and logo drop out when short, and the
// transcript takes whatever remains. Caller must hold m.mu.
func (r *managedREPL) frameLayoutFor(w, h int) frameLayout {
	m := r.model
	l := frameLayout{width: w, height: h, inputRows: m.inputRows()}
	if !m.quiet {
		l.statusRows = 1
	}
	l.dockRows = turnDockRowCount(h, l.inputRows, l.statusRows, !m.quiet && m.turnDock.visible)
	if room := h - l.statusRows - l.dockRows; room > 1 {
		l.inputRows = min(l.inputRows, room-1)
	} else {
		l.inputRows = 1
	}
	l.dividerRows = dividerRowCount(h, l.inputRows, l.statusRows, l.dockRows, m.quiet)
	content := h - l.inputRows - l.statusRows - l.dockRows - l.dividerRows
	l.logoRows = startupLogoRowCount(content, r.startupLogoVisible, r.images != nil)
	l.transcriptHeight = max(0, content-l.logoRows)
	return l
}

// composerRow maps a row inside the composer to its screen row.
func (l frameLayout) composerRow(row int) int {
	return l.logoRows + l.transcriptHeight + l.dockRows + l.dividerRows + row
}

// transcriptViewport resolves which transcript display rows land on screen
// this frame. A pinned pane shows the last rows; an overlay ticker hides the
// bottom overlayRows. A pane with no room yields an empty window.
func (l frameLayout) transcriptViewport(totalRows, topRow int, pinBottom bool, overlayRows int) transcriptViewport {
	v := transcriptViewport{width: l.width, logoRows: l.logoRows}
	if l.transcriptHeight <= 0 || l.width <= 0 {
		return v
	}
	v.start = topRow
	if pinBottom {
		v.start = max(0, totalRows-l.transcriptHeight)
		if totalRows < l.transcriptHeight {
			v.topPadding = l.transcriptHeight - totalRows
		}
	}
	v.end = v.start + l.transcriptHeight - min(overlayRows, l.transcriptHeight)
	return v
}

// dividerRowCount is the height of the rule separating the transcript from the
// bottom chrome: one row outside quiet mode, dropped entirely when the
// terminal is too short to spare it.
func dividerRowCount(h, inputRows, statusRows, dockRows int, quiet bool) int {
	if quiet || h-inputRows-statusRows-dockRows < 2 {
		return 0
	}
	return 1
}

func turnDockRowCount(h, inputRows, statusRows int, visible bool) int {
	if !visible || h-inputRows-statusRows < 2 {
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

// requestSuspend queues a Ctrl-Z suspension on the UI loop, which restores
// the terminal before stopping the foreground process group.
func (r *managedREPL) requestSuspend() {
	select {
	case r.suspend <- struct{}{}:
	default:
	}
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
	if m.clipboardCapture {
		m.appendNoticeLine("clipboard: waiting for image capture")
		m.followBottom = true
		return false
	}
	if m.busy && defaultReplCommands.busySafeCommand(trimmed) {
		return r.runComposerCommandLocked(trimmed)
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
	case r.pending <- turn:
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

	r.turnDockW = widgets.NewParagraph()
	noBorder(&r.turnDockW.Block)
	r.turnDockW.WrapText = false
	r.turnDockW.TextStyle = ui.NewStyle(ui.ColorGrey)

	r.statusW = widgets.NewParagraph()
	noBorder(&r.statusW.Block)
	r.statusW.WrapText = false
	r.statusW.TextStyle = ui.NewStyle(ui.ColorGrey)
	r.modalW = newModalParagraph()
}

// layout (re)builds the root flex for the current input height. The input row
// count varies with multi-line prompts, so the flex is rebuilt each render
// rather than sized once at setup.
func (r *managedREPL) layout(l frameLayout) {
	flex := widgets.NewFlex()
	noBorder(&flex.Block)
	flex.Direction = widgets.FlexColumn
	if l.logoRows > 0 {
		flex.AddItem(r.logoW, l.logoRows, 0, false)
	}
	flex.AddItem(r.transcriptW, 0, 1, false)
	if l.dockRows > 0 {
		flex.AddItem(r.turnDockW, 1, 0, false)
	}
	if l.dividerRows > 0 {
		flex.AddItem(r.dividerW, 1, 0, false)
	}
	flex.AddItem(r.inputW, l.inputRows, 0, false)
	if l.statusRows > 0 {
		flex.AddItem(r.statusW, 1, 0, false)
	}
	flex.SetRect(0, 0, l.width, l.height)
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

	r.model.mu.Lock()
	if r.model.imageCellWidth != imageCellWidth || r.model.imageCellHeight != imageCellHeight {
		r.model.imageCellWidth = imageCellWidth
		r.model.imageCellHeight = imageCellHeight
		r.model.invalidateVisual()
	}
	r.model.refreshActiveTools()
	r.model.refreshStreamCursor()
	r.model.refreshReasoningRecords(w)
	r.model.refreshExpandedTurnTrailer(w)
	l := r.frameLayoutFor(w, h)
	input, _, curRow, curCol, editable := r.model.renderInputForTerminal(l.inputRows, w)
	transcriptRows := r.model.transcriptRows(w)
	topRow, pinTranscriptBottom := r.model.settleScroll(len(transcriptRows), l.transcriptHeight)
	status := r.model.statusRow(w)
	modalOpen := r.model.modal != nil
	modalText, modalTitle := "", ""
	modalWidth, modalHeight := 0, 0
	if modalOpen {
		modalWidth = modalWidthForTerminal(w, r.model.modal.width)
		maxRows := max(1, h-8)
		modalText = r.model.modal.text(maxRows, modalWidth)
		modalTitle = r.model.modal.title
		modalHeight = min(h, max(3, strings.Count(modalText, "\n")+3))
	}
	var dock string
	if l.dockRows > 0 {
		dock, _ = r.model.turnDockRow(w)
	}
	title := r.model.frameTitle()
	progress := r.model.frameProgress()
	notices := r.model.takeNotices()
	ticker := r.model.activityTicker(len(transcriptRows), topRow, l.transcriptHeight)
	var overlay [][]ui.Cell
	if ticker != "" {
		overlay = append(overlay, ui.ParseStyles(ticker, ui.NewStyle(ui.ColorClear)))
	}
	viewport := l.transcriptViewport(len(transcriptRows), topRow, pinTranscriptBottom, len(overlay))
	imagePlacements := r.model.visibleImagePlacements(viewport)
	r.model.imagePlacements = imagePlacements
	r.model.reasoningPlacements = r.model.visibleReasoningPlacements(viewport)
	r.model.toolDisclosurePlacements = r.model.visibleToolDisclosurePlacements(viewport)
	r.model.imageDisclosurePlacements = r.model.visibleImageDisclosurePlacements(viewport)
	r.model.turnTrailerPlacements = r.model.visibleTurnTrailerPlacements(viewport)
	r.model.mu.Unlock()

	if l.logoRows == imageLogoHeight && r.images != nil {
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

	r.transcriptW.Rows = transcriptRows
	r.transcriptW.UseRows = true
	r.transcriptW.PinBottom = pinTranscriptBottom
	r.transcriptW.TopRow = topRow
	r.transcriptW.OverlayBottom = overlay
	if l.logoRows == imageLogoHeight {
		r.logoW.Rows = make([][]ui.Cell, l.logoRows)
	} else {
		r.logoW.Rows = pollyLogoRows(w)
	}
	r.logoW.TopRow = 0
	r.logoW.OverlayBottom = nil
	r.inputW.Text = input
	r.turnDockW.Text = dock
	r.statusW.Text = status
	if modalOpen {
		x := (w - modalWidth) / 2
		y := (h - modalHeight) / 2
		r.modalW.Text = modalText
		r.modalW.Title = modalTitle
		r.modalW.SetRect(x, y, x+modalWidth, y+modalHeight)
	}
	if l.dividerRows > 0 {
		r.dividerW.Text = styled(strings.Repeat("─", w), "muted", "")
	}

	r.layout(l)
	ui.Clear()
	r.placeCursor(editable && !modalOpen, curCol, l.composerRow(curRow), w)
	if modalOpen {
		ui.Render(r.rootFlex, r.modalW)
	} else {
		ui.Render(r.rootFlex)
	}
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

func modalWidthForTerminal(terminalWidth, preferred int) int {
	if preferred <= 0 {
		preferred = 64
	}
	width := min(preferred, terminalWidth)
	if terminalWidth > 4 {
		width = min(preferred, terminalWidth-4)
	}
	if terminalWidth >= 24 {
		width = max(24, width)
	}
	return width
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

// transcriptRows returns the styled, wrapped transcript for width. Visual
// clipping happens after style parsing and wrapping in transcriptParagraph,
// so wrapped rows remain reachable through scrollback.
func (m *replModel) transcriptRows(width int) [][]ui.Cell {
	if width < 1 {
		width = 1
	}
	if m.nativeImages && m.refreshTranscriptImageSources(width) {
		m.invalidateVisual()
	}
	if m.visualCacheValid && m.visualCacheWidth == width &&
		m.visualCacheCellWidth == m.imageCellWidth && m.visualCacheCellHeight == m.imageCellHeight {
		return m.visualCache
	}

	sources := m.transcriptDisplayEntries(width)
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
			old.key != source.key || old.text != source.text || old.followed != followed ||
			!slices.Equal(old.reasoningIDs, source.reasoningIDs) ||
			!slices.Equal(old.toolDisclosureIDs, source.toolDisclosureIDs) ||
			old.turnTrailerID != source.turnTrailerID ||
			!transcriptImagesEqual(old.images, source.images)
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
			key:               source.key,
			text:              source.text,
			followed:          followed,
			rows:              rows,
			images:            append([]transcriptImage(nil), source.images...),
			imageSpans:        imageSpans,
			reasoningIDs:      append([]int64(nil), source.reasoningIDs...),
			toolDisclosureIDs: append([]int64(nil), source.toolDisclosureIDs...),
			turnTrailerID:     source.turnTrailerID,
			activityFields:    append([]turnDockPlacement(nil), source.activityFields...),
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

func (m *replModel) transcriptDisplayEntries(width int) []transcriptDisplayBlock {
	blocks := make([]transcriptDisplayBlock, 0, len(m.transcript)+1)
	for i, entry := range m.transcript {
		reasoningID := m.reasoningAt[i]
		toolDisclosureID := m.toolDisclosureAt[i]
		turnTrailerID := m.turnTrailerAt[i]
		// Reasoning and tool activity render inline where they occur. In quiet
		// mode the records still back the settled trailer, but nothing extra is
		// projected into the transcript so script output stays clean.
		if m.quiet && (reasoningID != 0 || toolDisclosureID != 0) {
			continue
		}
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
		key := fmt.Sprintf("transcript:%d", i)
		if turnTrailerID != 0 {
			key = fmt.Sprintf("turn-trailer:%d", turnTrailerID)
		} else if reasoningID != 0 {
			key = fmt.Sprintf("reasoning:%d", reasoningID)
		} else if toolDisclosureID != 0 {
			key = fmt.Sprintf("tools:%d", toolDisclosureID)
		}
		block := transcriptDisplayBlock{
			key:           key,
			text:          entry,
			images:        m.transcriptImages[i],
			turnTrailerID: turnTrailerID,
		}
		if reasoningID != 0 {
			block.reasoningIDs = []int64{reasoningID}
		}
		if toolDisclosureID != 0 {
			block.toolDisclosureIDs = []int64{toolDisclosureID}
		}
		blocks = append(blocks, block)
	}
	if m.slashHints != "" {
		blocks = append(blocks, transcriptDisplayBlock{key: "slash", text: styled(m.slashHints, "muted", "")})
	}
	return m.layoutInlineActivityBlocks(blocks, width)
}

func (m *replModel) inlineReasoningField(ids []int64) (turnDockField, bool) {
	var elapsed time.Duration
	active, complete, unsaved, expanded, found := false, true, false, false, false
	for _, id := range ids {
		record := m.reasoningRecords[id]
		if record == nil {
			continue
		}
		found = true
		elapsed += record.elapsed
		if record.active && !m.thinkingSegmentStart.IsZero() && record.id == m.turnReasoningID {
			elapsed += time.Since(m.thinkingSegmentStart)
		}
		active = active || record.active
		complete = complete && record.complete
		unsaved = unsaved || record.unsaved
		expanded = expanded || record.expanded
	}
	if !found {
		return turnDockField{}, false
	}
	label := reasoningDisclosureLabel(active, unsaved, elapsed)
	glyph := "▸"
	if expanded {
		glyph = "▾"
	}
	return turnDockField{
		raw: glyph + " " + label, rendered: inlineActivityControl(glyph, label, complete && !active), overlay: turnDockOverlayThought,
	}, true
}

func (m *replModel) inlineToolField(ids []int64) (turnDockField, bool) {
	total, expanded, complete, found := 0, false, true, false
	for _, id := range ids {
		if record := m.toolDisclosures[id]; record != nil {
			found = true
			total += len(record.rows)
			expanded = expanded || record.expanded
			complete = complete && record.complete
		}
	}
	if !found || total == 0 {
		return turnDockField{}, false
	}
	label := turnToolLabel(total)
	glyph := "▸"
	if expanded {
		glyph = "▾"
	}
	return turnDockField{
		raw: glyph + " " + label, rendered: inlineActivityControl(glyph, label, complete), overlay: turnDockOverlayTools,
	}, true
}

func (m *replModel) inlineImageField(ids []int64) (turnDockField, []transcriptImage, bool) {
	images := m.toolInspectionImages(ids)
	if len(images) == 0 {
		return turnDockField{}, nil, false
	}
	label := turnImageLabel(len(images))
	glyph := "▸"
	if m.toolInspectionExpanded(ids) {
		glyph = "▾"
	}
	return turnDockField{
		raw: glyph + " " + label, rendered: inlineActivityControl(glyph, label, false), overlay: turnDockOverlayImages,
	}, images, true
}

func inlineActivityDetail(text string) string {
	_, detail, ok := strings.Cut(text, "\n")
	if !ok {
		return ""
	}
	return detail
}

// layoutInlineActivityBlocks gives inline controls the exact one-line layout
// used by the trailer. Adjacent thought/tool controls become one row while
// their independently expandable detail remains beneath it.
func (m *replModel) layoutInlineActivityBlocks(blocks []transcriptDisplayBlock, width int) []transcriptDisplayBlock {
	if width < 1 {
		width = 80
	}
	laidOut := make([]transcriptDisplayBlock, 0, len(blocks))
	for _, block := range blocks {
		if !block.isActivity() {
			laidOut = append(laidOut, block)
			continue
		}
		detail := inlineActivityDetail(block.text)
		if len(block.reasoningIDs) > 0 {
			block.activityReasoningDetail = detail
		} else {
			block.activityToolDetail = detail
		}

		if n := len(laidOut); n > 0 {
			previous := &laidOut[n-1]
			if previous.turnTrailerID == 0 && previous.isActivity() {
				if len(previous.images) > 0 && len(block.images) > 0 {
					block.activityToolDetail = offsetTranscriptImageMarkers(block.activityToolDetail, len(previous.images))
				}
				previous.reasoningIDs = append(previous.reasoningIDs, block.reasoningIDs...)
				previous.toolDisclosureIDs = append(previous.toolDisclosureIDs, block.toolDisclosureIDs...)
				if block.activityReasoningDetail != "" {
					if previous.activityReasoningDetail != "" {
						previous.activityReasoningDetail += "\n"
					}
					previous.activityReasoningDetail += block.activityReasoningDetail
				}
				if block.activityToolDetail != "" {
					if previous.activityToolDetail != "" {
						previous.activityToolDetail += "\n"
					}
					previous.activityToolDetail += block.activityToolDetail
				}
				previous.images = append(previous.images, block.images...)
				continue
			}
		}

		laidOut = append(laidOut, block)
	}
	for i := range laidOut {
		block := &laidOut[i]
		if !block.isActivity() {
			continue
		}
		movedOn := !m.busy
		if !movedOn {
			for j := i + 1; j < len(laidOut); j++ {
				if laidOut[j].key == "slash" {
					continue
				}
				movedOn = true
				break
			}
		}
		m.layoutInlineActivityBlock(block, width, movedOn)
	}
	return laidOut
}

func (m *replModel) layoutInlineActivityBlock(block *transcriptDisplayBlock, width int, settled bool) {
	var fields []turnDockField
	if field, ok := m.inlineReasoningField(block.reasoningIDs); ok {
		fields = append(fields, field)
	}
	if field, ok := m.inlineToolField(block.toolDisclosureIDs); ok {
		fields = append(fields, field)
	}
	block.activityImageDetail = ""
	if field, inspectionImages, ok := m.inlineImageField(block.toolDisclosureIDs); ok {
		fields = append(fields, field)
		if m.toolInspectionExpanded(block.toolDisclosureIDs) {
			remaining := maxTranscriptImagesPerBlock - len(block.images)
			if remaining > 0 {
				inspectionImages = inspectionImages[:min(len(inspectionImages), remaining)]
				block.activityImageDetail = offsetTranscriptImageMarkers(
					renderInspectionTranscriptImages(inspectionImages), len(block.images),
				)
				block.images = append(block.images, inspectionImages...)
			}
		}
	}
	for i := range fields {
		glyph, label, ok := strings.Cut(fields[i].raw, " ")
		if ok {
			fields[i].rendered = inlineActivityControl(glyph, label, settled)
		}
	}
	header, placements := renderTurnActivityRow(fields, width)
	block.text = header
	block.activityReasoningDetail = boundedReasoningDetail(block.activityReasoningDetail, reasoningPreviewLines)
	for _, detail := range []string{block.activityReasoningDetail, block.activityToolDetail, block.activityImageDetail} {
		if detail != "" {
			block.text += "\n" + detail
		}
	}
	block.activityFields = placements
	block.key = fmt.Sprintf("activity:r%v:t%v", block.reasoningIDs, block.toolDisclosureIDs)
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

// scrollByWidth moves the scroll anchor by delta display rows (negative = up)
// at the given layout width. Caller must hold m.mu. Disengages followBottom
// on first upward scroll; re-engages when the user scrolls back to the bottom.
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

// settleScroll clamps the held anchor to the rows that exist at this width
// and re-engages followBottom once it reaches the end, returning the frame's
// top row and whether the pane pins to the bottom. Caller must hold m.mu.
func (m *replModel) settleScroll(totalRows, viewportHeight int) (topRow int, pinBottom bool) {
	if m.followBottom {
		return m.scrollAnchor, true
	}
	topRow = m.scrollAnchor
	if maxTop := max(0, totalRows-viewportHeight); topRow >= maxTop {
		topRow = maxTop
		m.followBottom = true
	}
	topRow = max(topRow, 0)
	m.scrollAnchor = topRow
	return topRow, m.followBottom
}

// transcriptViewport is the window of display rows the transcript pane shows
// this frame, plus the screen offset every placement projects through. It is
// resolved once per frame by frameLayout.transcriptViewport.
type transcriptViewport struct {
	start, end int // display rows in [start, end) are on screen
	topPadding int // blank rows above a transcript shorter than the pane
	logoRows   int
	width      int
}

func (v transcriptViewport) contains(row int) bool { return row >= v.start && row < v.end }

// screenY maps a display row inside the window to its screen row.
func (v transcriptViewport) screenY(row int) int { return v.logoRows + v.topPadding + row - v.start }

func (m *replModel) visibleReasoningPlacements(v transcriptViewport) []disclosurePlacement {
	return m.visibleDisclosurePlacements(v, turnDockOverlayThought)
}

func (m *replModel) visibleToolDisclosurePlacements(v transcriptViewport) []disclosurePlacement {
	return m.visibleDisclosurePlacements(v, turnDockOverlayTools)
}

func (m *replModel) visibleImageDisclosurePlacements(v transcriptViewport) []disclosurePlacement {
	return m.visibleDisclosurePlacements(v, turnDockOverlayImages)
}

func (m *replModel) visibleTurnTrailerPlacements(v transcriptViewport) []turnTrailerPlacement {
	var placements []turnTrailerPlacement
	rowOffset := 0
	for _, block := range m.visualBlocks {
		if block.turnTrailerID != 0 && len(block.rows) > 0 {
			row := rowOffset
			if v.contains(row) {
				if record := m.turnTrailers[block.turnTrailerID]; record != nil {
					for _, field := range record.fields {
						if field.X >= v.width {
							continue
						}
						field.Y = v.screenY(row)
						field.Cols = min(field.Cols, v.width-field.X)
						placements = append(placements, turnTrailerPlacement{
							recordID: record.id, turnDockPlacement: field,
						})
					}
				}
			}
		}
		rowOffset += len(block.rows)
	}
	return placements
}

// visibleDisclosurePlacements projects one kind of activity control into
// absolute screen cells for mouse hit-testing.
func (m *replModel) visibleDisclosurePlacements(v transcriptViewport, overlay turnDockOverlay) []disclosurePlacement {
	// Only the block's activity controls are click targets. Truncation may
	// leave no fully visible control; the header never stands in for one.
	var placements []disclosurePlacement
	rowOffset := 0
	for _, block := range m.visualBlocks {
		recordIDs := block.reasoningIDs
		if overlay == turnDockOverlayTools || overlay == turnDockOverlayImages {
			recordIDs = block.toolDisclosureIDs
		}
		row := rowOffset
		rowOffset += len(block.rows)
		if len(recordIDs) == 0 || len(block.rows) == 0 || !v.contains(row) {
			continue
		}
		for _, field := range block.activityFields {
			if field.overlay != overlay || field.X >= v.width {
				continue
			}
			placements = append(placements, disclosurePlacement{
				recordID:  recordIDs[0],
				recordIDs: append([]int64(nil), recordIDs...),
				X:         field.X,
				Y:         v.screenY(row),
				Cols:      min(field.Cols, v.width-field.X),
			})
		}
	}
	return placements
}

func (m *replModel) toggleReasoningAt(x, y, width int) bool {
	for _, placement := range m.reasoningPlacements {
		if y != placement.Y || x < placement.X || x >= placement.X+placement.Cols {
			continue
		}
		if len(placement.recordIDs) > 1 {
			return m.toggleReasoningGroup(placement.recordIDs, width)
		}
		return m.toggleReasoning(placement.recordID, width)
	}
	return false
}

func (m *replModel) toggleToolDisclosureAt(x, y int) bool {
	for _, placement := range m.toolDisclosurePlacements {
		if y != placement.Y || x < placement.X || x >= placement.X+placement.Cols {
			continue
		}
		if len(placement.recordIDs) > 1 {
			return m.toggleToolDisclosureGroup(placement.recordIDs)
		}
		return m.toggleToolDisclosure(placement.recordID)
	}
	return false
}

func (m *replModel) toggleImageDisclosureAt(x, y int) bool {
	for _, placement := range m.imageDisclosurePlacements {
		if y != placement.Y || x < placement.X || x >= placement.X+placement.Cols {
			continue
		}
		ids := placement.recordIDs
		if len(ids) == 0 && placement.recordID != 0 {
			ids = []int64{placement.recordID}
		}
		return m.toggleImageDisclosureGroup(ids)
	}
	return false
}

func activityGroupContains(blockIDs, ids []int64) bool {
	if len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		if !slices.Contains(blockIDs, id) {
			return false
		}
	}
	return true
}

func (m *replModel) toggleReasoningGroup(ids []int64, width int) bool {
	anyExpanded, found := false, false
	validIDs := make([]int64, 0, len(ids))
	for _, id := range ids {
		if record := m.reasoningRecords[id]; record != nil {
			found = true
			anyExpanded = anyExpanded || record.expanded
			validIDs = append(validIDs, id)
		}
	}
	if !found {
		return false
	}
	if width > 0 {
		m.reasoningWidth = width
	}
	layoutWidth := m.disclosureLayoutWidth(width)
	// The group header points down whenever any member is expanded. Its first
	// click therefore collapses the whole group; only an entirely closed group
	// expands on click.
	expand := !anyExpanded
	m.mutateAnchored(layoutWidth, matchActivityGroup(validIDs, true), func(held bool) {
		// Apply the new state to the whole group before refreshing any record:
		// each refresh re-lays-out the merged activity row, so refreshing
		// mid-loop would render intermediate frames from a half-toggled group.
		var changed []*reasoningRecord
		for _, id := range ids {
			if record := m.reasoningRecords[id]; record != nil && record.expanded != expand {
				record.expanded = expand
				changed = append(changed, record)
			}
		}
		for _, record := range changed {
			m.refreshReasoningRecordWithAnchor(record, layoutWidth, !held)
		}
	})
	return true
}

func (m *replModel) toggleToolDisclosureGroup(ids []int64) bool {
	anyExpanded, found := false, false
	validIDs := make([]int64, 0, len(ids))
	for _, id := range ids {
		if record := m.toolDisclosures[id]; record != nil {
			found = true
			anyExpanded = anyExpanded || record.expanded
			validIDs = append(validIDs, id)
		}
	}
	if !found {
		return false
	}
	expand := !anyExpanded
	m.mutateAnchored(m.disclosureLayoutWidth(0), matchActivityGroup(validIDs, false), func(held bool) {
		// Apply-then-refresh: see toggleReasoningGroup.
		var changed []*toolDisclosureRecord
		for _, id := range ids {
			if record := m.toolDisclosures[id]; record != nil && record.expanded != expand {
				record.expanded = expand
				changed = append(changed, record)
			}
		}
		for _, record := range changed {
			m.refreshToolDisclosureWithAnchor(record, !held)
		}
	})
	return true
}

func (m *replModel) toggleImageDisclosureGroup(ids []int64) bool {
	anyExpanded, found := false, false
	validIDs := make([]int64, 0, len(ids))
	for _, id := range ids {
		record := m.toolDisclosures[id]
		if record == nil || len(m.toolInspectionImages([]int64{id})) == 0 {
			continue
		}
		found = true
		anyExpanded = anyExpanded || record.imagesExpanded
		validIDs = append(validIDs, id)
	}
	if !found {
		return false
	}
	expand := !anyExpanded
	m.mutateAnchored(m.disclosureLayoutWidth(0), matchActivityGroup(validIDs, false), func(bool) {
		for _, id := range validIDs {
			m.toolDisclosures[id].imagesExpanded = expand
		}
		m.invalidateVisual()
	})
	return true
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
	if !m.slashHintsHidden && !m.pasting && !m.searching && m.approval == nil && m.modal == nil {
		hint = defaultReplCommands.hintFor(newManagedReplCommandContext(r), text)
	}
	m.setSlashHintLine(hint)
}

func (r *managedREPL) handleEventLocked(e ui.Event) bool {
	m := r.model
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
		return false
	}

	// Selection and credential modals own all remaining input. In particular,
	// key material never passes through paste handling or the composer.
	if m.modal != nil {
		r.handleModalEvent(e)
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
			if m.statusSession.hit(mouse.X, mouse.Y, terminalHeight) {
				r.openResumePicker()
				return false
			}
			if m.toggleTurnTrailerAt(mouse.X, mouse.Y) {
				return false
			}
			// An expanded trailer overlay is modal for one click: clicking
			// anywhere outside its target dismisses it without activating
			// content underneath.
			if m.closeTurnDockOverlay() {
				return false
			}
			if !m.toggleReasoningAt(mouse.X, mouse.Y, terminalWidth) &&
				!m.toggleToolDisclosureAt(mouse.X, mouse.Y) &&
				!m.toggleImageDisclosureAt(mouse.X, mouse.Y) {
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
		if m.busy {
			m.toggleLatestReasoning(0)
		} else {
			m.toggleLatestTurnTrailerOverlay(turnDockOverlayThought)
		}
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

	// An expanded trailer overlay is temporary. Escape closes it before
	// reaching search, approval, or turn-cancel handling.
	if e.ID == "<Escape>" && m.closeTurnDockOverlay() {
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
		if m.ed.empty() && !m.busy {
			r.requestQuit()
			return true
		}
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
		if ch, ok := printableRune(e); ok {
			m.ed.insert(ch)
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

// TurnPersistenceAllowed reports whether this turn may still append to the
// session. It is deliberately activeLocked, not acceptingLocked: an ordinary
// cancellation still persists the turn's completed work. Only a detached turn
// (^C cancellation timed out, generation advanced) is refused — newer turns
// may already be writing, and a late append would land out of order.
func (t *gotuiTurnUI) TurnPersistenceAllowed() bool {
	t.repl.model.mu.Lock()
	defer t.repl.model.mu.Unlock()
	return t.activeLocked()
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
		// Prose is the aggregation boundary: it closes the reasoning run and
		// the tool run before the first token lands, so activity after the
		// prose opens fresh indicators below it.
		t.repl.model.finishThinkingSegment()
		t.repl.model.completeToolDisclosure()
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
		// Tools pause the thinking clock without closing the record; an
		// unbroken continuation resumes the same indicator.
		t.repl.model.pauseThinkingSegment()
		t.repl.model.finishAssistantBlock("")
		t.repl.model.runningTools += len(calls)
		t.repl.model.state = turnStateTool
		t.repl.model.toolName = calls[0].Name
	}
	if !toolDisplayEnabled(t.config) {
		return
	}
	var record *toolDisclosureRecord
	for _, c := range calls {
		record = t.repl.model.appendToolStartRow(c.ID, toolLabel(c))
	}
	t.repl.model.refreshToolDisclosure(record)
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
	label := stripTranscriptImageMarkers(toolLabel(call))
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
	batchDone := m.busy && m.runningTools == 0
	if batchDone {
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
	final = stripTranscriptImageMarkers(final)
	images := discoveredImages
	// Freeze the final line over its running disclosure row. Fall back to a new
	// row if the display was cleared while the tool was in flight.
	record := m.currentToolDisclosure()
	if rowIndex, ok := m.takeActiveTool(call.ID); ok && record != nil && rowIndex >= 0 && rowIndex < len(record.rows) {
		row := &record.rows[rowIndex]
		row.line = final
		row.images = append([]transcriptImage(nil), images...)
		row.settled = true
	} else {
		record = m.ensureToolDisclosure()
		record.rows = append(record.rows, toolDisclosureRow{
			callID:  call.ID,
			label:   label,
			line:    final,
			images:  append([]transcriptImage(nil), images...),
			settled: true,
		})
	}
	m.refreshToolDisclosure(record)
	// The disclosure stays live past the batch: an unbroken continuation's
	// next batch folds into it. Assistant prose or turn settlement closes it.
}

func (t *gotuiTurnUI) AppendToolMedia(call messages.ChatMessageToolCall, images []transcriptImage) {
	if len(images) == 0 || (t.config != nil && t.config.Quiet) {
		return
	}
	t.repl.model.mu.Lock()
	defer t.repl.model.mu.Unlock()
	m := t.repl.model
	if !t.acceptingLocked() {
		return
	}
	record, row := m.toolDisclosureRowForCall(call.ID)
	if row == nil {
		record = m.ensureToolDisclosure()
		record.rows = append(record.rows, toolDisclosureRow{
			callID:  call.ID,
			label:   stripTranscriptImageMarkers(toolLabel(call)),
			line:    toolOKLine(toolLabel(call), "", ""),
			settled: true,
		})
		row = &record.rows[len(record.rows)-1]
	}
	m.mutateAnchored(m.disclosureLayoutWidth(0), matchActivityGroup([]int64{record.id}, false), func(bool) {
		row.inspectionImages = append([]transcriptImage(nil), images...)
		m.refreshToolDisclosureWithAnchor(record, false)
		// The third Images field and its gallery are derived from
		// inspectionImages; neither necessarily changes the canonical raw tool
		// text.
		m.invalidateVisual()
	})
}

func (m *replModel) toolDisclosureRowForCall(callID string) (*toolDisclosureRecord, *toolDisclosureRow) {
	for i := len(m.turnToolDisclosureIDs) - 1; i >= 0; i-- {
		record := m.toolDisclosures[m.turnToolDisclosureIDs[i]]
		if record == nil {
			continue
		}
		for rowIndex := len(record.rows) - 1; rowIndex >= 0; rowIndex-- {
			if callID != "" && record.rows[rowIndex].callID != callID {
				continue
			}
			return record, &record.rows[rowIndex]
		}
	}
	return nil, nil
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
	t.repl.model.lastIn = in
	t.repl.model.lastOut = out
	t.repl.model.turnDock.inputTokens = in
	t.repl.model.turnDock.outputTokens = out
	t.repl.model.mu.Unlock()
}

func (t *gotuiTurnUI) RecordContextUsage(used, limit int, estimated bool) {
	t.repl.model.mu.Lock()
	if t.acceptingLocked() {
		t.repl.model.recordContextUsage(used, limit, estimated)
	}
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

func runManagedREPL(ctx context.Context, config *Config, state *conversationState, showStartupLogo bool) error {
	name, err := state.session.GetName(ctx)
	if err != nil {
		return fmt.Errorf("read session name: %w", err)
	}
	history, err := state.session.GetHistory(ctx)
	if err != nil {
		return fmt.Errorf("read session history: %w", err)
	}
	repl := newManagedREPL(config, name, toolCount(state.toolRegistry), skillCount(state.skillCatalog))
	repl.showStartupLogo = showStartupLogo
	repl.state = state
	repl.model.artifactStore = state.artifactStore
	repl.model.hydrateHistory(history, name)
	// Seed the bar without network traffic. This is explicitly approximate
	// until a provider reports the first real request usage.
	if total, totalErr := state.session.GetTotalTokens(ctx); totalErr == nil {
		limit := config.MaxHistoryTokens
		if md, mdErr := state.session.GetMetadata(ctx); mdErr == nil && md != nil {
			if window := md.ContextWindows[config.Model]; window > 0 {
				limit = llm.ClampContextBudget(limit, window, config.MaxTokens)
			}
		}
		repl.model.recordContextUsage(total, limit, total > 0)
	}
	err = repl.Run(ctx, func(turnCtx context.Context, prompt string, turnUI TurnUI) error {
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
	if err != nil {
		return err
	}
	if repl.resumeContext != "" {
		return &resumeSessionRequest{name: repl.resumeContext}
	}
	return nil
}

func runFallbackREPL(ctx context.Context, config *Config, state *conversationState) error {
	reader := bufio.NewReader(os.Stdin)
	drainSandboxWarningsToWriter(os.Stderr, state)
	writeFallbackSandboxNotice(os.Stderr, config, state)
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
