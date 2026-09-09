package main

import (
	"context"
	"fmt"
	"image"
	"os"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
	"golang.org/x/term"
)

type managedREPL struct {
	config *Config

	// state backs the session slash commands (/clear, /context, /tools,
	// /skills). Nil in unit tests that exercise only the editor/event layer;
	// command handlers guard against that.
	state *conversationState

	model *replModel

	logoW                              *transcriptParagraph
	transcriptW                        *transcriptParagraph
	dividerW                           *style.LiteralParagraph
	inputW                             *style.LiteralParagraph
	turnDockW                          *style.LiteralParagraph
	statusW                            *style.LiteralParagraph
	modalW                             *modalParagraph
	rootFlex                           ui.Drawable
	chrome                             chromeGeometry
	orbit                              frameOrbit
	inspectorScrollbar, modalScrollbar scrollbar
	scrollDrag                         scrollDragState
	chromeHoverChanged                 bool

	quit    chan struct{}
	suspend chan struct{}
	pending chan pendingTurn
	// tabEvents wakes the loop when a tab's turn settles, its lease ends, or
	// a cancel grace expires; the loop then scans every tab (settleTabs).
	tabEvents chan struct{}

	// Leaving. quitting is set once every turn has been told to stop;
	// quitDeadline then bounds how long the loop waits for them to settle
	// before returning anyway. quitWarned records the idle Ctrl-C that
	// reported turns running in other tabs, so the next one quits.
	quitting     bool
	quitDeadline <-chan time.Time
	quitWarned   bool

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
	uiTasks          chan func()
	work             *replWork
	childViews       childViewCache
	childViewRequest *childViewNavigation
	// replacingTab is the last workspace a /close is replacing; finishOpen
	// closes it once the fresh session holds the screen.
	replacingTab        *replTab
	inspectorRatio      float64
	inspectorRefreshAt  time.Time
	inspectorW          *transcriptParagraph
	inspectorHeaderW    *style.LiteralParagraph
	inspectorHeaderRows int
	inspectorButtons    []inspectorButton
	inspectorDragging   bool
	mousePosition       image.Point
	mousePositionKnown  bool
	// hover is the target under the pointer as of the last paint, and
	// hoverCells the screen cells its underline occupies; see repl_hover.go.
	hover            hoverTarget
	hoverCells       []image.Point
	workspaceActions []func()

	// fx drives window-level terminal effects (title, taskbar progress,
	// desktop notifications); nil outside a managed-screen Run (unit tests).
	fx          *terminalFX
	affordanceW *affordanceLayer
	// images owns native Kitty/Sixel placements. Nil means captions/paths only.
	images *termimg.Manager

	// startupLogoVisible reserves a small header above the transcript until the
	// first real turn starts. The composer and status remain live from frame one.
	showStartupLogo    bool
	startupLogoVisible bool

	// histFile is the append handle for persistent input history; nil when
	// history couldn't be opened (best-effort — never fatal).
	histFile *os.File

	// runTurn executes interactive tab turns. Swarm members run independently.
	runTurn turnRunner
	// spawnRequests are the /spawn commands recorded by handlers for the
	// event loop to apply.
	spawnRequests []spawnRequest
	// pickerExpanded names the sessions whose agents the resume picker
	// lists; the picker shares the map so the choice outlives a modal.
	pickerExpanded map[string]bool
	// sessionsPicker is the most recently opened sessions picker; it is live
	// while its modal is the visible model's.
	sessionsPicker *sessionsPicker

	// runCtx is the Run loop's context, kept for work that event handlers
	// start (opening sessions). Background outside Run.
	runCtx context.Context

	// Tabs (see repl_tabs.go). Every open session is a tab with its own
	// screen model and turn; r.model and r.state mirror the visible one.
	// showTabRequest (-1 when none) and closeTabRequest are recorded by
	// handlers and applied by the event loop.
	tabs            []*replTab
	showTabRequest  int
	closeTabRequest bool

	// Opening sessions (/resume, /new). opener builds the runtime; nil in
	// unit tests, where a selection only records itself. opening names the
	// session being opened until it lands or fails; while set, the composer
	// holds new turns. openDone hands the opened runtime to the event loop.
	opener     *sessionOpener
	opening    string
	openCancel context.CancelFunc
	openDone   chan openResult
}

// sessionSettings returns the live session's settings, or nil when no
// session is attached (unit tests of the screen alone).
func (r *managedREPL) sessionSettings() *Settings {
	if r.state == nil {
		return nil
	}
	return &r.state.settings
}

// currentModel is the live session's model, or "" without a session.
func (r *managedREPL) currentModel() string {
	if settings := r.sessionSettings(); settings != nil {
		return settings.Model
	}
	return ""
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
	if err := messages.ValidateImageMessage(turn.userMessage); err != nil {
		return managedTurnInput{}, err
	}
	return turn, nil
}

// sandboxNoticeLine reports exceptional sandbox posture at REPL startup.
// An active sandbox with no issues needs no notice.
func sandboxNoticeLine(config *Config, state *conversationState) string {
	return currentSandboxPosture(config, state).noticeString()
}

func newManagedREPL(config *Config, contextName string, toolCount, skillCount int) *managedREPL {
	m := newReplModel()
	if contextName == "" {
		contextName = "-"
	}
	m.status = newSessionStatus(&config.Launch, contextName, toolCount, skillCount)
	m.quiet = config.Quiet
	return &managedREPL{
		config: config,
		model:  m,
		// The screen starts on a tab with no session behind it; the first
		// session to land (addTab) takes its place.
		tabs:            []*replTab{{name: contextName, model: m, workspaceRoot: true}},
		quit:            make(chan struct{}, 1),
		suspend:         make(chan struct{}, 1),
		pending:         make(chan pendingTurn, 1),
		uiTasks:         make(chan func(), 8),
		work:            newREPLWork(),
		openDone:        make(chan openResult, 1),
		tabEvents:       make(chan struct{}, 1),
		showTabRequest:  -1,
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
