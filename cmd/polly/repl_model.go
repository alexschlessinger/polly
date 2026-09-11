package main

import (
	"context"
	"image"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
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
	// turnOutcomeIncomplete marks a turn cut short by a token or iteration cap.
	turnOutcomeIncomplete
)

// transcriptEntry is one transcript block: its rendered text and the explicit
// local image references it carries (nil when none), kept in one value so the
// two cannot drift apart.
type transcriptEntry struct {
	text          string
	images        []style.Image
	initialPrompt bool
	// Completed assistant Markdown is materialized on the next visible paint.
	markdown string
	// markdownSource survives materialization so pane resizing can reflow tables.
	markdownSource string
	markdownWidth  int
	codeCache      *markdown.CodeCache
}

// replModel is the mutex-protected state for the TUI. Mutated from both the
// main event loop and any in-flight turn goroutine, so every read/write
// holds mu.
type replModel struct {
	mu                sync.Mutex
	inspectorWrap     bool   // soft code wrapping belongs to tool inspectors only
	toolBaseDir       string // the inspected conversation's own execution root
	affordances       affordanceState
	inspections       inspectionSource
	bashInspector     *bashInspectorCommand // immutable tool-inspector display data
	bashSetupExpanded bool

	// transcript is the accumulated content rendered into the upper pane.
	// Each entry is a logical "block" (user prompt, assistant turn, notice,
	// tool line) whose text may contain inline style markup; entries are
	// joined with "\n" at render time. Grown only by appendTranscriptEntry,
	// shrunk only by deleteTranscriptEntry, reset only by clearDisplay,
	// rewritten in place only by setTranscriptText/setTranscriptEntry/
	// setTranscriptImages — so every mutation invalidates the visual cache.
	// A direct write outside those owners is a bug.
	transcript            []transcriptEntry
	userPromptSeen        bool
	collapseInitialPrompt bool // display-only agent inspector projection
	initialPromptExpanded bool
	// displayCleared records that /clear or Ctrl+L emptied the transcript, so
	// it no longer projects the session's saved history: a child view must
	// not be cached as that history's display until it is rebuilt from it.
	displayCleared  bool
	imageBaseDir    string
	nativeImages    bool
	imageCellWidth  int
	imageCellHeight int
	artifactStore   artifacts.Store
	// imagePlacements is the last rendered frame's native thumbnail geometry,
	// in absolute screen cells, kept for mouse-click hit-testing.
	imagePlacements []termimg.Placement

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
	// streamShown is how many of its bytes are currently rendered. The visible
	// prefix renders at most once per frame, reusing code highlighting, while unclosed inline
	// markup near the tail is held back (safeVisibleLen) so text lands on
	// screen already styled instead of visibly transforming.
	streamRaw        strings.Builder
	streamShown      int
	streamTypewriter assistantTypewriter
	streamCodeCache  *markdown.CodeCache
	markdownPending  bool
	markdownWidth    int

	visual transcriptVisualCache

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
	toolDisclosures       transcriptRegistry[*toolDisclosureRecord]
	turnToolDisclosureID  int64
	turnToolDisclosureIDs []int64 // every disclosure opened this turn
	// disclosurePlacements is the last rendered frame's activity controls
	// per kind, in absolute screen cells, for mouse hit-testing.
	disclosurePlacements [activityKindCount][]disclosurePlacement
	agentLinkPlacements  []agentLink
	// settledAgentsShown lists the workflows whose settled members are
	// expanded under their heading; the default folds them into a count.
	settledAgentsShown map[string]bool
	inspectionLinks    []inspectionLink
	turnDock           turnDockState
	turnTrailers       transcriptRegistry[*turnTrailerRecord]
	modal              *replModal

	ed            lineEditor
	busy          bool
	canceling     bool
	turnID        int64
	pasting       bool // inside a bracketed paste; runes go in verbatim
	approval      *approvalState
	approvalQueue []*approvalState
	// swarmParent is the identity the swarm snapshot's decisions are read
	// for; empty in views that never learn it.
	swarmParent     string
	approvalsClosed bool
	hist            promptHistory
	// Inline answers belong to the request and batch index shown by the last
	// input frame, which may differ after a concurrent cancellation.
	paintedApproval      *approvalState
	paintedApprovalIndex int

	// queue holds inputs submitted while a turn is in flight (the prompt stays
	// editable during a turn). Commands remain text-only; prompts carry the
	// exact prepared message accepted from the composer.
	queue []queuedREPLInput

	// Scrollback. When followBottom is true, the render trims to the most
	// recent lines that fit. When false, scrollAnchor names the absolute
	// transcript-line index of the top visible row.
	followBottom bool
	scrollAnchor int

	status     sessionStatus
	parentLink image.Rectangle
	quiet      bool
	// masthead heads the transcript flow in root sessions; see mastheadBlock.
	masthead mastheadState
	// hoverHint names the wordless target under the pointer in the status
	// row's idle slot; the frame that paints the status row sets it.
	hoverHint string

	// hidden is set while this model's tab is off screen. Streamed text is
	// then kept raw and rendered when the tab shows again, since markdown
	// rendering per chunk only serves a visible transcript.
	hidden bool

	state       turnState
	toolName    string
	turnStarted time.Time
	lastIn      int
	lastOut     int
	lastElapsed time.Duration
	lastOutcome turnOutcome
	completion  *turnCompletion

	// Reasoning disclosures are semantic per-turn records rather than free-form
	// transcript strings. The UI retains only a bounded tail; successful turns
	// already persist their complete provider reasoning on ChatMessage and
	// hydrate it back into a fresh bounded record after restart.
	reasoningRecords     transcriptRegistry[*reasoningRecord]
	reasoningOrder       []int64 // creation order, for Ctrl-O
	turnReasoningID      int64
	turnReasoningIDs     []int64 // every reasoning record opened this turn
	turnReasoningOpen    bool    // pending Ctrl-O pre-arm before the first chunk
	thinkingSegmentOpen  bool
	thinkingSegmentStart time.Time
	reasoningWidth       int // last renderer width; avoids terminal access from provider callbacks

	// focusKnown/focused mirror the terminal's focus reports (tcell
	// EnableFocus). Desktop notifications fire only when the terminal has
	// explicitly said the user is elsewhere; with no reports they stay silent.
	focusKnown bool
	focused    bool

	// Notifications have their own short lock so painting one tab never waits
	// for another tab's transcript mutations.
	notificationMu sync.Mutex
	notices        []string

	// signals are what happened on this tab while it was hidden — a turn
	// settling, a tool call needing approval — for the event loop to relay
	// into the visible transcript as notice lines named after the tab.
	// Showing the tab drops them: the user now sees the tab itself.
	signals []tabSignal
	// unseenOutcome is how the turn that settled while the tab was hidden
	// ended, kept for the status-row badge until the tab is shown.
	unseenOutcome turnOutcome

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
	cells             []ui.Cell
	followed          bool
	rows              [][]ui.Cell
	images            []style.Image
	imageSpans        []transcriptImageSpan
	reasoningIDs      []int64
	toolDisclosureIDs []int64
	turnTrailerID     int64
	activityFields    []turnDockPlacement
	agentLinks        []agentLink
}

// reasoningRecord is the display projection of one user turn's provider
// reasoning. tail is intentionally bounded; the durable ChatMessage remains
// the authoritative complete copy for successful turns.
type reasoningRecord struct {
	transcriptAnchor
	inspectionKey  string
	tail           []rune
	tailVersion    uint64
	previewVersion uint64
	previewWidth   int
	previewLines   []string
	dirty          bool
	expanded       bool
	active         bool
	complete       bool
	unsaved        bool
	elapsed        time.Duration
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
	inspectionKey    string
	callID           string
	toolName         string
	agent            *agentActivity
	label            string
	line             string
	inline           *inlineToolLine
	bash             *bashSummary
	file             *inlineFileSummary
	images           []style.Image
	inspectionImages []style.Image
	settled          bool
}

type toolDisclosureRecord struct {
	transcriptAnchor
	rows []toolDisclosureRow
	// displayRows snapshots the visible rows with the canonical transcript
	// update, so width projection cannot observe a half-applied mutation.
	displayRows    []toolDisclosureRow
	expanded       bool
	imagesExpanded bool
	agentsExpanded bool
	complete       bool
}

type approvalState struct {
	ctx       context.Context
	requester string
	calls     []messages.ChatMessageToolCall
	index     int
	out       []bool
	reply     chan []bool
}

func newReplModel() *replModel {
	baseDir, _ := os.Getwd()
	m := &replModel{
		currentAssistant: -1,
		reasoningWidth:   80,
		imageBaseDir:     baseDir,
		toolBaseDir:      baseDir,
		hist:             promptHistory{idx: -1, match: -1},
		state:            turnStateIdle,
		followBottom:     true,
		activeToolsPhase: -1,
	}
	m.ed.goalCol = -1
	return m
}
