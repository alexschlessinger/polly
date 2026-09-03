package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	tcell "github.com/gdamore/tcell/v3"
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

// replModel is the mutex-protected state for the MVP TUI. Mutated from both
// the main event loop and any in-flight turn goroutine, so every read/write
// holds mu.
// transcriptEntry is one transcript block: its rendered text and the explicit
// local image references it carries (nil when none), kept in one value so the
// two cannot drift apart.
type transcriptEntry struct {
	text   string
	images []transcriptImage
}

type replModel struct {
	mu sync.Mutex

	// transcript is the accumulated content rendered into the upper pane.
	// Each entry is a logical "block" (user prompt, assistant turn, notice,
	// tool line) whose text may contain inline style markup; entries are
	// joined with "\n" at render time. Grown only by appendTranscriptEntry,
	// shrunk only by deleteTranscriptEntry, reset only by clearDisplay,
	// rewritten in place only by setTranscriptText/setTranscriptEntry/
	// setTranscriptImages — so every mutation invalidates the visual cache.
	// A direct write outside those owners is a bug.
	transcript      []transcriptEntry
	imageBaseDir    string
	nativeImages    bool
	imageCellWidth  int
	imageCellHeight int
	artifactStore   artifacts.Store
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

	ed        lineEditor
	busy      bool
	canceling bool
	turnID    int64
	pasting   bool // inside a bracketed paste; runes go in verbatim
	approval  *approvalState
	hist      promptHistory

	// queue holds inputs submitted while a turn is in flight (the prompt stays
	// editable during a turn). Commands remain text-only; prompts carry the
	// exact prepared message accepted from the composer.
	queue []queuedREPLInput

	// Scrollback. When followBottom is true, the render trims to the most
	// recent lines that fit. When false, scrollAnchor names the absolute
	// transcript-line index of the top visible row.
	followBottom bool
	scrollAnchor int

	status sessionStatus
	quiet  bool

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
		hist:             promptHistory{idx: -1, match: -1},
		state:            turnStateIdle,
		followBottom:     true,
		activeToolsPhase: -1,
	}
	m.ed.goalCol = -1
	return m
}

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

	// runCtx is the Run loop's context, kept for work that event handlers
	// start (session switches). Background outside Run.
	runCtx context.Context

	// Session switching (/resume). opener builds the runtime for the chosen
	// session; nil in unit tests, where a selection only records itself.
	// onStateChange tells the owner which session is live so shutdown closes
	// that one. switchTarget names the session a switch is heading for, from
	// the request until the switch lands or fails; while set, the composer
	// refuses new turns. switchSaved restores config when the open fails.
	// switchDone hands the opened runtime back to the event loop.
	opener         *sessionOpener
	onStateChange  func(*conversationState)
	switchTarget   string
	switchInFlight bool
	switchSaved    *Config
	switchCancel   context.CancelFunc
	switchDone     chan switchResult
}

// switchResult is the outcome of opening a session for an in-place switch.
type switchResult struct {
	name  string
	state *conversationState
	err   error
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
	if contextName == "" {
		contextName = "-"
	}
	m.status = newSessionStatus(config, contextName, toolCount, skillCount)
	m.quiet = config.Quiet
	return &managedREPL{
		config:          config,
		model:           m,
		quit:            make(chan struct{}, 1),
		suspend:         make(chan struct{}, 1),
		pending:         make(chan managedTurnInput, 1),
		uiTasks:         make(chan func(), 8),
		switchDone:      make(chan switchResult, 1),
		runCtx:          context.Background(),
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
	r.model.visual.invalidate()
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

	r.runCtx = ctx
	defer r.drainSwitch()

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
		case <-r.sessionDone():
			// Lease loss or store shutdown ends the run with its typed cause,
			// as it did when the run context was parented on the session.
			r.cancelTurn()
			r.releaseApproval()
			return context.Cause(r.state.session.Context())
		case res := <-r.switchDone:
			r.finishSwitch(res)
			r.render()
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
			turnDone = r.continueAfterTurn(ctx, runTurn)
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

	turnCtx, cancel := r.turnContext(ctx)
	r.turnCancel = cancel
	done := make(chan error, 1)
	tui := &gotuiTurnUI{repl: r, config: r.config, state: r.state, turnID: turnID, reuseUser: reuseUser, turn: cloneManagedTurn(turn), persistence: persistence}
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
	return r.continueAfterTurn(ctx, runTurn)
}

// continueAfterTurn runs whatever waited on the turn that just settled. A
// pending session switch outranks queued input, which belonged to the session
// being left and was discarded when the switch was requested.
func (r *managedREPL) continueAfterTurn(ctx context.Context, runTurn func(context.Context, string, TurnUI) error) chan error {
	if r.switchTarget == "" {
		return r.startNextQueued(ctx, runTurn)
	}
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	if !r.switchInFlight {
		r.beginSwitchLocked()
	}
	return nil
}

// turnContext parents a turn on the live session's lease context, so losing
// the lease cancels the turn with its typed cause, and bridges the run
// context in for signals. Cancel reports context.Canceled, the user-cancel
// cause the turn UI labels as canceled.
func (r *managedREPL) turnContext(ctx context.Context) (context.Context, context.CancelFunc) {
	parent := ctx
	if r.state != nil && r.state.session != nil {
		parent = r.state.session.Context()
	}
	turnCtx, cancel := context.WithCancelCause(parent)
	stop := context.AfterFunc(ctx, func() { cancel(context.Cause(ctx)) })
	return turnCtx, func() {
		stop()
		cancel(context.Canceled)
	}
}

// sessionDone is the live session's lease context, or nil (never ready) when
// the REPL has no session, as in unit tests.
func (r *managedREPL) sessionDone() <-chan struct{} {
	if r.state == nil || r.state.session == nil {
		return nil
	}
	return r.state.session.Context().Done()
}

// requestSwitchLocked starts moving the REPL to the named session. A running
// turn is canceled first and the switch proceeds once it settles; queued
// input is dropped because it belonged to the session being left. Caller
// must hold m.mu.
func (r *managedREPL) requestSwitchLocked(name string) {
	m := r.model
	if r.switchTarget != "" {
		m.appendNoticeLine("already switching to " + r.switchTarget)
		return
	}
	if r.opener == nil {
		m.appendNoticeLine("session switching unavailable")
		return
	}
	r.switchTarget = name
	if m.busy {
		m.appendNoticeLine("switching to " + name + " once the current turn is canceled")
		if !m.canceling {
			r.cancelBusyTurn()
		}
		return
	}
	r.beginSwitchLocked()
}

// beginSwitchLocked resolves the target's settings onto config on the UI
// goroutine, then opens its runtime off it. Caller must hold m.mu and ensure
// no turn is in flight.
func (r *managedREPL) beginSwitchLocked() {
	m := r.model
	name := r.switchTarget
	m.discardQueuedInputs()
	saved := *r.config
	r.switchSaved = &saved
	resolved, err := r.opener.prepare(r.runCtx, name, m.appendNoticeLine)
	if err != nil {
		r.failSwitchLocked(err)
		return
	}
	m.appendNoticeLine("opening " + resolved + "…")
	ctx, cancel := context.WithCancel(r.runCtx)
	r.switchCancel = cancel
	r.switchInFlight = true
	open := r.opener.open
	go func() {
		state, err := open(ctx, resolved)
		r.switchDone <- switchResult{name: resolved, state: state, err: err}
	}()
}

// finishSwitch lands an opened session: the new runtime becomes live before
// the previous one is closed, so an attach failure leaves the REPL where it
// was. Runs on the event loop without m.mu.
func (r *managedREPL) finishSwitch(res switchResult) {
	r.switchInFlight = false
	if r.switchCancel != nil {
		r.switchCancel()
		r.switchCancel = nil
	}
	if res.err != nil {
		r.model.mu.Lock()
		r.failSwitchLocked(res.err)
		r.model.mu.Unlock()
		return
	}
	previous := r.state
	if err := r.attachState(res.state); err != nil {
		_ = res.state.Close()
		r.model.mu.Lock()
		r.failSwitchLocked(err)
		r.model.mu.Unlock()
		return
	}
	r.switchTarget = ""
	r.switchSaved = nil
	r.startupLogoVisible = false
	if previous != nil {
		if err := previous.Close(); err != nil {
			r.model.mu.Lock()
			r.model.appendNoticeLine("closing previous session: " + err.Error())
			r.model.mu.Unlock()
		}
	}
}

// failSwitchLocked abandons a switch, restoring the config the target's
// settings had overwritten. Caller must hold m.mu.
func (r *managedREPL) failSwitchLocked(err error) {
	name := r.switchTarget
	r.switchTarget = ""
	r.switchInFlight = false
	if r.switchSaved != nil {
		*r.config = *r.switchSaved
		r.switchSaved = nil
	}
	reason := err.Error()
	if errors.Is(err, sessions.ErrSessionInUse) {
		reason = "it is open in another polly"
	}
	r.model.appendErrorLine("could not open " + name + ": " + reason)
}

// drainSwitch runs when the event loop exits with an open still in flight:
// the runtime it produces has no owner, so wait for it and close it rather
// than leak its lease and tool processes.
func (r *managedREPL) drainSwitch() {
	if !r.switchInFlight {
		return
	}
	if r.switchCancel != nil {
		r.switchCancel()
		r.switchCancel = nil
	}
	res := <-r.switchDone
	r.switchInFlight = false
	if res.state != nil {
		_ = res.state.Close()
	}
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
	if !m.slashHintsHidden && !m.pasting && !m.hist.searching && m.approval == nil && m.modal == nil {
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
			if m.status.sessionField.hit(mouse.X, mouse.Y, terminalHeight) {
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
	if m.hist.searching {
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
		m.hist.startSearch()
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
