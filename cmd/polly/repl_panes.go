package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/internal/scratch"
	"github.com/alexschlessinger/pollytool/rex"
	"github.com/gdamore/tcell/v3"
	"github.com/gdamore/tcell/v3/tty"
	ui "github.com/metaspartan/gotui/v5"
)

// Panes. Inside Rex, the inspected view opens in a Rex pane instead of the
// frame polly paints beside the conversation: polly asks Rex for a window
// floating over its tab (see repl_pane_float.go) running `polly pane`, which
// relays that terminal back here (see pane_proxy.go), and the event loop
// paints it as a second screen. The pane has no state of its own. It is one more place the view's surface can be
// (see repl_surface.go), so it shows what this process knows, live, and its
// keys and clicks reach the same handlers the frame's do.
//
// One pane exists at a time and it follows the visible workspace: it is open
// exactly while that workspace's view is. The event loop owns every field
// below except the token the listener checks.

const (
	// paneWriteTimeout bounds a write to a pane whose terminal stopped
	// reading, so a stuck pane is dropped instead of freezing the main screen.
	paneWriteTimeout = 2 * time.Second
	paneHelloTimeout = 5 * time.Second
	paneOpenTimeout  = 10 * time.Second
	// paneProbeTime is how long a command has to show that it is a terminal
	// program; editors do within a tenth of a second.
	paneProbeTime  = 2 * time.Second
	paneProbeEvery = 100 * time.Millisecond
)

// paneConnectTimeout bounds how long a newly opened host terminal has for its
// proxy to connect before polly retires the block and falls back.
var paneConnectTimeout = 3 * time.Second

var errPaneConnectTimeout = errors.New("pane proxy did not connect in time")

// panePlace says where a host puts a new terminal.
type panePlace int

const (
	// paneTab is a tab of its own, in front: another session.
	paneTab panePlace = iota
	// paneIfTerminal is a split beside polly that takes the keys, but only
	// if the command takes over its terminal: an editor, which may equally
	// be a window of its own that wants no pane.
	paneIfTerminal
)

// paneOpener is the core terminal-host contract: open a terminal running argv,
// close it, bring a terminal to the front, and name the tab polly is in.
// Optional capabilities—floating/docked windows (windowHost), tiled agent
// walls (wallHost), and host command-palette prompts (promptHost)—are
// discovered by interface assertion on the host.
//
// A terminal that is meant to be in front (a tab, the wall, one focused) and
// that the host has but would not show is reported with a *paneBehindError:
// the call did what it could, and the user has to be told where to look.
type paneOpener interface {
	// words is what the host and its tabs are called, for the notices that
	// say what it could not do.
	words() hostWords
	// open runs argv at place and reports the terminal's id, empty when the
	// command came and went, or kept running without wanting a terminal.
	open(ctx context.Context, argv []string, place panePlace, label string) (id string, err error)
	close(ctx context.Context, id string) error
	// focus brings a terminal to the front with the keys, whichever polly
	// opened it.
	focus(ctx context.Context, id string) error
	// title names the tab polly is in, unless someone else named it: the
	// name it has must be the host's own, polly's placeholder, or was. An
	// empty now gives the tab back the host's name.
	title(ctx context.Context, was, now string) error
}

// dockState reports where a host currently holds a panel terminal: absent or
// unreadable (dockUnknown), floating over the tab (dockFloating), or docked
// into the tab's layout (dockDocked).
type dockState int

const (
	dockUnknown dockState = iota
	dockFloating
	dockDocked
)

// windowHost is implemented by hosts that can float and dock panel terminals
// alongside polly's own pane.
type windowHost interface {
	// float runs argv in a window that floats over polly's own pane, without
	// the keys. spot is its top left corner and its size, each as a share of
	// that pane's width and height.
	float(ctx context.Context, argv []string, label string, spot [4]float64) (id string, err error)
	// arrange moves floating windows to new spots over polly's pane, and
	// reports for each whether it was moved or left alone: one that is no
	// longer floating where polly put it, because the user moved, resized or
	// docked it, stays where it is.
	arrange(ctx context.Context, moves []paneMove) ([]bool, error)
	// dock runs argv in a pane of the tab's own, without the keys: under the
	// terminal named, or with none beside polly, taking share of its width.
	dock(ctx context.Context, argv []string, label, under string, share float64) (id string, err error)
	// docked reports whether each terminal is docked, floating, or absent
	// (dockUnknown) so a closed pane is not mistaken for one the user floated.
	docked(ctx context.Context, ids []string) ([]dockState, error)
}

// wallHost is implemented by hosts that can tile multiple agent panes in a
// dedicated tab and split existing panes as new agents start.
type wallHost interface {
	// tile opens a tab in front holding a terminal per argv, in a grid, and
	// reports their ids in the same order.
	tile(ctx context.Context, label string, argvs [][]string) (ids []string, err error)
	// grow splits the terminal beside for one more: under it when stacked,
	// to its right otherwise.
	grow(ctx context.Context, beside string, stacked bool, argv []string) (id string, err error)
}

// promptHost is implemented by hosts that can present a pick list or input
// prompt in their own UI (such as Rex's command palette).
type promptHost interface {
	// ask puts a question to the user in the host's own list and reports the
	// answer, which is not given when they closed the question or ctx ended.
	// An error means the host could not ask: errAskLater when that is nothing
	// to tell the user about.
	ask(ctx context.Context, q paneQuestion) (paneAnswer, error)
}

// hostWords names a host and what it calls a tab: Rex has tabs, and tmux
// has windows.
type hostWords struct{ name, tab string }

// paneTabLabel is what a tab of polly's is called until its session has a
// title to name it with.
const paneTabLabel = "polly"

// paneMove takes the floating window id to another spot over polly's pane.
type paneMove struct {
	id string
	to [4]float64
}

// paneQuestion is something the host asks the user for polly: which of
// choices, or with text set a line to type, starting from value.
type paneQuestion struct {
	title, hint string
	choices     []paneChoice
	// typed lets what the user types answer in place of a choice.
	typed bool
	text  bool
	// value is the line the user starts from, and confirm what submitting it
	// is called.
	value, confirm string
	// wait is how long the question stays up unanswered: paneAskTime unless
	// set.
	wait time.Duration
}

// paneAnswer is the choice's id, or the text typed. It is not given when the
// user closed the question or let it lapse.
type paneAnswer struct {
	given    bool
	id, text string
}

// paneChoice is one row of a list the host shows: detail is short and sits
// at the row's far end, and icon is one of the paneIcon names.
type paneChoice struct {
	id, title, subtitle, detail, icon string
}

// The icons a row can carry. The names are Rex's; a host draws the ones it
// has.
const (
	paneIconLive    = "terminal" // running now
	paneIconPast    = "clock"    // saved, and not open anywhere
	paneIconCommand = "code"
	paneIconSkill   = "star"
	paneIconPage    = "document"
	paneIconSetup   = "gear"
)

// errAskLater is a host that cannot ask right now but may the next time: it
// is showing another question, or has not been set up to.
var errAskLater = errors.New("the host has no way to ask now")

// errAskUnconfigured marks the specific case where the host's ask action (e.g.
// Rex's polly_ask) is not installed; askHost backs off briefly so every
// command does not pay a redundant IPC round-trip before falling back.
var errAskUnconfigured = fmt.Errorf("host ask action not configured: %w", errAskLater)

var askUnconfiguredBackoff = 30 * time.Second

// paneBehindError is a terminal the host opened or focused but would not
// bring to the front. Its text is the host's reason, for the user to act on.
type paneBehindError struct {
	reason string
	cause  error
}

func (e *paneBehindError) Error() string {
	if e.reason != "" {
		return e.reason
	}
	return e.cause.Error()
}

func (e *paneBehindError) Unwrap() error { return e.cause }

// paneBehind reports whether err is only that: the terminal is there.
func paneBehind(err error) bool {
	var behind *paneBehindError
	return errors.As(err, &behind)
}

var rexReasonNoise = regexp.MustCompile(`^stream response "[^"]*" failed: |\s*\(status=\d+\s+code=[a-z0-9_]+\)$`)

// rexBehindError wraps a Rex refusal to bring a tab forward, stripping Rex's
// transport framing so the user sees the app's own reason.
func rexBehindError(err error) error {
	var api *rex.APIError
	if errors.As(err, &api) && api.Message != "" {
		return &paneBehindError{reason: rexReasonNoise.ReplaceAllString(api.Message, ""), cause: err}
	}
	return &paneBehindError{cause: err}
}

// rexPlaced remembers where polly placed a floating layer in Rex, alongside
// polly's own pane rect then, so a tab/split resize is not mistaken for a user
// drag.
type rexPlaced struct {
	at, home rex.Rect
}

// rexPanes opens panes in Rex, around anchor: the block polly runs in.
type rexPanes struct {
	anchor string
	client *rex.Client

	mu     sync.Mutex
	placed map[string]rexPlaced
}

func newRexPanes(anchor string) *rexPanes {
	return &rexPanes{anchor: anchor, client: rex.NewClient(), placed: map[string]rexPlaced{}}
}

func (h *rexPanes) putPlaced(id string, p rexPlaced) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.placed == nil {
		h.placed = map[string]rexPlaced{}
	}
	h.placed[id] = p
}

func (h *rexPanes) getPlaced(id string) (rexPlaced, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.placed[id]
	return p, ok
}

func (h *rexPanes) forgetPlaced(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.placed, id)
}

func (h *rexPanes) cli() *rex.Client {
	if h.client != nil {
		return h.client
	}
	return rex.NewClient()
}

func (h *rexPanes) words() hostWords { return hostWords{name: "Rex", tab: "tab"} }

func (h *rexPanes) open(ctx context.Context, argv []string, place panePlace, label string) (string, error) {
	c := h.cli()
	terminal := &rex.TerminalOptions{Shell: "none", Command: argv}
	switch place {
	case paneTab:
		// A block ID also names the session that holds it.
		res, err := c.NewWindow(ctx, rex.NewWindowRequest{SessionID: h.anchor, WindowLabel: label, Layout: rex.ShellLayout(label, terminal)})
		if err != nil {
			return "", err
		}
		if len(res.BlockIDs) == 0 {
			return "", errors.New("rex created no block")
		}
		return res.BlockIDs[0], h.show(ctx, c, res.BlockIDs[0])
	case paneIfTerminal:
		return h.openIfTerminal(ctx, c, terminal, label)
	}
	return "", fmt.Errorf("rex opens nothing at place %d", place)
}

// openIfTerminal starts the command in a block that is in no window, and
// watches what it does to its terminal. A program that takes the screen or
// reads keys as they are typed is a terminal program, and its block is placed
// beside polly with the keys. One that does neither within paneProbeTime has
// a window of its own, and its block stays out of sight until it exits. No
// list of editors is consulted: the program shows what it is.
//
// A command that fails is not told from one that finished: Rex reports
// status 0 for both here.
func (h *rexPanes) openIfTerminal(ctx context.Context, c *rex.Client, terminal *rex.TerminalOptions, label string) (string, error) {
	// Rex keeps a terminal whose program exits within a quarter second, for
	// its error to be read. Nobody can read one that is in no window, so
	// this one always closes with its program.
	never := uint32(0)
	terminal.Exit = &rex.TerminalExitOptions{QuickExitThresholdMS: &never}
	block, err := c.NewBlock(ctx, rex.NewBlockRequest{SessionID: h.anchor, Label: label, Options: terminal})
	if err != nil {
		return "", err
	}
	id := block.BlockID
	for deadline := time.Now().Add(paneProbeTime); time.Now().Before(deadline); {
		select {
		case <-ctx.Done():
			return "", nil
		case <-time.After(paneProbeEvery):
		}
		process, err := c.Process(ctx, h.anchor, id)
		if err != nil {
			// The block closed with its program: it came and went.
			return "", nil
		}
		if process.LastExit != nil {
			return "", nil
		}
		var input struct {
			Canonical *bool `json:"canonical"`
		}
		_ = c.CallBlock(ctx, h.anchor, id, rex.CreatorTerminal, "input_mode", nil, &input)
		modes, _ := c.Modes(ctx, h.anchor, id)
		if modes != nil && modes.Screen == "alternate" || input.Canonical != nil && !*input.Canonical {
			if _, err := c.MoveBlock(ctx, rex.MoveBlockRequest{SessionID: h.anchor, BlockID: id, AnchorBlockID: h.anchor, Direction: rex.DirectionHorizontal, Side: rex.SideAfter}); err != nil {
				_, _ = c.CloseBlock(ctx, h.anchor, id)
				return "", err
			}
			_, _ = c.FocusBlock(ctx, h.anchor, id)
			return id, nil
		}
	}
	return "", nil
}

func (h *rexPanes) close(ctx context.Context, id string) error {
	h.forgetPlaced(id)
	_, err := h.cli().CloseBlock(ctx, id, id)
	return err
}

func (h *rexPanes) focus(ctx context.Context, id string) error {
	// A block ID also names the session that holds it, so this reaches a
	// polly in another Rex session too.
	c := h.cli()
	if _, err := c.FocusBlock(ctx, id, id); err != nil {
		return err
	}
	return h.show(ctx, c, id)
}

// show has the Rex app the user is looking at show the tab that holds block.
// The app's tab selection is its own: a window made the session's active
// one, which is all new_window and focus_block do, changes nothing on
// screen. So the app that owns polly's terminal, the one displaying it, is
// asked to select the tab itself. It agrees only with Remote Control turned
// on in its settings; its refusal comes back as a *paneBehindError.
func (h *rexPanes) show(ctx context.Context, c *rex.Client, block string) error {
	app := h.app(ctx, c)
	if app == "" {
		return nil // Nobody is looking: there is no front to bring it to.
	}
	view, err := c.InspectSession(ctx, block)
	if err != nil {
		return rexBehindError(err)
	}
	for _, window := range view.Windows {
		if !windowHolds(window, block) {
			continue
		}
		if err := c.SelectWindow(ctx, app, view.SessionID, window.WindowID); err != nil {
			return rexBehindError(err)
		}
		return nil
	}
	return nil
}

// app finds the Rex app showing polly: the one that owns polly's terminal,
// which an app takes when it displays one. "" when no app does, as in a
// session nobody has open: another app's screen is not polly's to change.
func (h *rexPanes) app(ctx context.Context, c *rex.Client) string {
	size, err := c.Size(ctx, h.anchor, h.anchor)
	if err != nil || size.Owner == "" {
		return ""
	}
	apps, _ := c.ListClients(ctx, "", "app")
	for _, app := range apps {
		if app.ClientID == size.Owner {
			return app.ClientID
		}
	}
	return ""
}

// paneAskTime is how long a question stays up unanswered.
const paneAskTime = 5 * time.Minute

// rexAskAction is the action Rex's configuration file has to define for
// polly's questions to show in the command palette, and rexAskTitle what Rex
// lists it as. Rex asks its apps a question only for such an action, so
// docs/CLI.md gives the user the few lines to add; without them polly asks
// in its own screen.
const (
	rexAskAction = "polly_ask"
	rexAskTitle  = "Polly: Ask"
)

// ask puts the question in the command palette of the connected Rex apps.
func (h *rexPanes) ask(ctx context.Context, q paneQuestion) (paneAnswer, error) {
	c, wait := h.cli(), cmp.Or(q.wait, paneAskTime)
	answer := paneAnswer{given: true}
	var err error
	if q.text {
		answer.text, err = c.Input(ctx, h.anchor, rexAskAction, rex.InputRequest{Title: q.title, Placeholder: q.hint, Value: q.value, Confirm: q.confirm, Timeout: wait})
	} else {
		items := make([]rex.PickItem, 0, min(len(q.choices), rex.MaxPickItems))
		for _, choice := range q.choices[:cap(items)] {
			items = append(items, rex.PickItem{ID: choice.id, Title: choice.title, Subtitle: choice.subtitle, Detail: choice.detail, Icon: choice.icon})
		}
		var row rex.PickAnswer
		row, err = c.Pick(ctx, h.anchor, rexAskAction, rex.PickRequest{Title: q.title, Placeholder: q.hint, Items: items, AllowOther: q.typed, Timeout: wait})
		answer.id, answer.text = row.ID, row.Text
	}
	var prompt *rex.PromptError
	switch {
	case err == nil:
		return answer, nil
	case errors.As(err, &prompt) && prompt.Dismissed():
		return paneAnswer{}, nil
	case errors.Is(err, rex.ErrNoAskAction):
		// The action may be added and the configuration reloaded any time, so
		// back off briefly instead of disabling permanently or stalling every
		// command with a redundant IPC round-trip.
		return paneAnswer{}, errAskUnconfigured
	case errors.As(err, &prompt) && (prompt.Reason == rex.PromptBusy || prompt.Reason == rex.PromptNoClient):
		// Another question is up or no app is connected right now: retry next time.
		return paneAnswer{}, errAskLater
	}
	return paneAnswer{}, err
}

// home finds the tab polly is in, whichever tab is in front, and the part of
// it polly's own pane takes: a floating window's bounds are shares of the
// tab, and polly's windows belong over polly.
func (h *rexPanes) home(ctx context.Context, c *rex.Client) (rex.WindowView, rex.Rect, error) {
	view, err := c.InspectSession(ctx, h.anchor)
	if err != nil {
		return rex.WindowView{}, rex.Rect{}, err
	}
	for _, window := range view.Windows {
		if !windowHolds(window, h.anchor) {
			continue
		}
		for _, layer := range window.Layers {
			if rect, ok := blockRect(layer.Layout, h.anchor); ok && layer.Kind != "floating" {
				return window, rect, nil
			}
		}
		// Polly itself floats, or is zoomed: the whole tab will do.
		return window, rex.Rect{0, 0, 1, 1}, nil
	}
	return rex.WindowView{}, rex.Rect{}, errors.New("rex has no tab holding polly")
}

// blockRect finds a terminal's place in a layout.
func blockRect(node *rex.LayoutNodeView, block string) (rex.Rect, bool) {
	if node == nil {
		return rex.Rect{}, false
	}
	if node.Split == nil {
		return node.Rect, node.BlockID == block
	}
	if rect, ok := blockRect(node.Split.Before, block); ok {
		return rect, true
	}
	return blockRect(node.Split.After, block)
}

// over is a spot over the pane at rect, in the tab's terms.
func over(rect rex.Rect, spot [4]float64) rex.Rect {
	return rex.Rect{rect[0] + spot[0]*rect[2], rect[1] + spot[1]*rect[3], spot[2] * rect[2], spot[3] * rect[3]}
}

func (h *rexPanes) float(ctx context.Context, argv []string, label string, spot [4]float64) (string, error) {
	c := h.cli()
	window, rect, err := h.home(ctx, c)
	if err != nil {
		return "", err
	}
	at := over(rect, spot)
	res, err := c.NewLayer(ctx, rex.NewLayerRequest{
		SessionID: h.anchor,
		WindowID:  window.WindowID,
		Bounds:    at,
		Layout:    rex.ShellLayout(label, &rex.TerminalOptions{Shell: "none", Command: argv}),
	})
	if err != nil {
		return "", err
	}
	if len(res.BlockIDs) == 0 {
		return "", errors.New("rex created no block")
	}
	id := res.BlockIDs[0]
	h.putPlaced(id, rexPlaced{at: at, home: rect})
	return id, nil
}

func (h *rexPanes) arrange(ctx context.Context, moves []paneMove) ([]bool, error) {
	c := h.cli()
	window, rect, err := h.home(ctx, c)
	if err != nil {
		return nil, err
	}
	floating := make(map[string]rex.LayerView)
	for _, layer := range window.Layers {
		// A window polly opened holds one terminal. Docked, it is in the
		// tab's own layer among the others.
		if layer.Kind == "floating" && len(layer.Blocks) == 1 {
			floating[layer.Blocks[0].BlockID] = layer
		}
	}
	moved := make([]bool, len(moves))
	for n, move := range moves {
		layer, ok := floating[move.id]
		if !ok {
			h.forgetPlaced(move.id)
			continue
		}
		if left, known := h.getPlaced(move.id); known && !nearSpot(layer.Bounds, left.at) && nearSpot(rect, left.home) {
			// The user moved or resized this window while polly's own pane
			// stayed put: leave it theirs.
			h.forgetPlaced(move.id)
			continue
		}
		at := over(rect, move.to)
		if _, err := c.ResizeLayer(ctx, h.anchor, layer.LayerID, at); err == nil {
			h.putPlaced(move.id, rexPlaced{at: at, home: rect})
			moved[n] = true
		}
	}
	return moved, nil
}

func (h *rexPanes) dock(ctx context.Context, argv []string, label, under string, share float64) (string, error) {
	c := h.cli()
	req := rex.NewSplitRequest{
		SessionID: h.anchor,
		AnchorID:  h.anchor,
		Direction: rex.DirectionHorizontal,
		Side:      rex.SideAfter,
		Ratio:     1 - share,
		Layout:    rex.ShellLayout(label, &rex.TerminalOptions{Shell: "none", Command: argv}),
	}
	if under != "" {
		req.AnchorID, req.Direction, req.Ratio = under, rex.DirectionVertical, 0
	}
	res, err := c.NewSplit(ctx, req)
	if err != nil {
		return "", err
	}
	if len(res.BlockIDs) == 0 {
		return "", errors.New("rex created no block")
	}
	if under == "" {
		// Rex 0.1.0-preview.23 creates every split at one half whatever ratio
		// it is given; the border moves once the split exists.
		_, _ = c.ResizePane(ctx, rex.ResizePaneRequest{
			SessionID:     h.anchor,
			Direction:     rex.DirectionHorizontal,
			BeforeBlockID: h.anchor,
			AfterBlockID:  res.BlockIDs[0],
			Ratio:         1 - share,
		})
	}
	return res.BlockIDs[0], nil
}

func (h *rexPanes) docked(ctx context.Context, ids []string) ([]dockState, error) {
	view, err := h.cli().InspectSession(ctx, h.anchor)
	if err != nil {
		return nil, err
	}
	states := make([]dockState, len(ids))
	for _, window := range view.Windows {
		for _, layer := range window.Layers {
			for _, block := range layer.Blocks {
				if n := slices.Index(ids, block.BlockID); n >= 0 {
					if layer.Kind == "floating" {
						states[n] = dockFloating
					} else {
						states[n] = dockDocked
					}
				}
			}
		}
	}
	for n, id := range ids {
		if states[n] != dockFloating {
			h.forgetPlaced(id)
		}
	}
	return states, nil
}

func (h *rexPanes) title(ctx context.Context, was, now string) error {
	c := h.cli()
	view, err := c.InspectSession(ctx, h.anchor)
	if err != nil {
		return err
	}
	for _, window := range view.Windows {
		if !windowHolds(window, h.anchor) {
			continue
		}
		// Rex names a window it was given no label for after the end of its ID.
		short, unnamed := strings.CutPrefix(window.Label, "Window ")
		unnamed = unnamed && strings.HasSuffix(window.WindowID, short)
		if window.Label == now || now == "" && unnamed {
			return nil
		}
		if window.Label != "" && !unnamed && window.Label != paneTabLabel && window.Label != was {
			return nil // Someone named this tab; it keeps that name.
		}
		_, err := c.RenameWindow(ctx, h.anchor, window.WindowID, now)
		return err
	}
	return nil
}

func windowHolds(window rex.WindowView, block string) bool {
	for _, layer := range window.Layers {
		for _, b := range layer.Blocks {
			if b.BlockID == block {
				return true
			}
		}
	}
	return false
}

func (h *rexPanes) tile(ctx context.Context, label string, argvs [][]string) ([]string, error) {
	blocks := make([]*rex.Layout, len(argvs))
	for n, argv := range argvs {
		blocks[n] = rex.ShellLayout(label, &rex.TerminalOptions{Shell: "none", Command: argv})
	}
	// Columns of near-equal height, as close to square as the count allows.
	columns := 1
	for columns*columns < len(blocks) {
		columns++
	}
	stacks := make([]*rex.Layout, 0, columns)
	for left := columns; left > 0; left-- {
		n := (len(blocks) + left - 1) / left
		stacks = append(stacks, evenSplit(blocks[:n], rex.VerticalSplit))
		blocks = blocks[n:]
	}
	c := h.cli()
	res, err := c.NewWindow(ctx, rex.NewWindowRequest{SessionID: h.anchor, WindowLabel: label, Layout: evenSplit(stacks, rex.HorizontalSplit)})
	if err != nil {
		return nil, err
	}
	if len(res.BlockIDs) != len(argvs) {
		return nil, fmt.Errorf("rex created %d blocks for %d panes", len(res.BlockIDs), len(argvs))
	}
	return res.BlockIDs, h.show(ctx, c, res.BlockIDs[0])
}

// evenSplit lays parts out in a row or a stack of equal shares: the first
// takes its share and the rest divide what is left.
func evenSplit(parts []*rex.Layout, split func(float64, *rex.Layout, *rex.Layout) *rex.Layout) *rex.Layout {
	if len(parts) == 1 {
		return parts[0]
	}
	return split(1/float64(len(parts)), parts[0], evenSplit(parts[1:], split))
}

func (h *rexPanes) grow(ctx context.Context, beside string, stacked bool, argv []string) (string, error) {
	direction := rex.DirectionHorizontal
	if stacked {
		direction = rex.DirectionVertical
	}
	res, err := h.cli().NewSplit(ctx, rex.NewSplitRequest{
		SessionID: beside,
		AnchorID:  beside,
		Direction: direction,
		Side:      rex.SideAfter,
		Layout:    rex.ShellLayout(paneTabLabel, &rex.TerminalOptions{Shell: "none", Command: argv}),
	})
	if err != nil {
		return "", err
	}
	if len(res.BlockIDs) == 0 {
		return "", errors.New("rex created no block")
	}
	return res.BlockIDs[0], nil
}

type paneHost struct {
	// opener is set by an interactive Run inside Rex, tmux, or Zellij; without
	// one, panels and tabs stay on polly's own screen.
	opener paneOpener
	// Per-capability degradation flags: a failure in one area (such as /wall
	// or opening a tab) leaves the host's other capabilities intact.
	tabless   bool
	wallless  bool
	dockless  bool
	floatless bool
	listless  bool

	listener net.Listener
	socket   string
	// block is the terminal polly itself runs in, and store the session
	// database it reads: what another polly is told, and asked about.
	block, store string
	// pane shows the visible workspace's view; wall holds the panes of the
	// agent wall, each with a view of its own (see repl_wall.go); floats
	// are the windows of the panels that float (see repl_pane_float.go).
	// focused is the own-screen panel that currently owns keyboard focus,
	// distinct from blocking dialogs in r.model.modal.
	pane    *satellitePane
	wall    *paneWall
	floats  []*satellitePane
	focused *satellitePane
	events  chan paneEvent
	// titled is what polly last named its tab, and titling is set while the
	// host is being asked to.
	titled  string
	titling bool
	// asked is the question the host is showing for polly, askRetryAt defers
	// repeated IPC probes when the host's ask action is not installed, and
	// behindTold is set once the user has read why the host leaves new tabs
	// behind.
	asked      *hostAsk
	askRetryAt time.Time
	behindTold bool
	// arranging is set while the host is moving floating windows, reflow when
	// polly's own pane changed size, and footerRows tracks the live composer +
	// status height so external floating windows stay above multi-line prompts
	// and docked forms.
	arranging, reflow bool
	footerRows        int
	// keysFor is the panel whose window, opened again by Alt-P, takes the
	// keys when its terminal connects, if that is before keysUntil.
	keysFor   floatKind
	keysUntil time.Time
	// arrangeFails counts the moves the host has failed in a row, and
	// arrangeAfter is when it may be asked again (see syncFloatLayout).
	arrangeFails int
	arrangeAfter time.Time
	// docks are the panels the user keeps docked (see repl_pane_dock.go);
	// locating is set while the host is asked where the windows are, and
	// relocate when it has to be asked again.
	docks              map[floatKind]bool
	locating, relocate bool
	// own is where the last frame put the windows of polly's own screen (see
	// repl_pane_own.go).
	own ownLayout
	// waking is the timer that brings a waiting question to the host when it
	// is due.
	waking *time.Timer

	// awaited holds the tokens of the relayed panes whose proxies have yet to
	// connect, and execs the commands other panes are waiting to become, by
	// token. The listener's goroutines read both.
	mu      sync.Mutex
	awaited map[string]bool
	execs   map[string]paneExec
}

// windows returns the host's floating/docked window capability, if any.
func (h *paneHost) windows() (windowHost, bool) {
	if h.opener == nil {
		return nil, false
	}
	w, ok := h.opener.(windowHost)
	return w, ok
}

// wallOpener returns the host's agent-wall capability when available.
func (h *paneHost) wallOpener() (wallHost, bool) {
	if h.opener == nil || h.wallless {
		return nil, false
	}
	w, ok := h.opener.(wallHost)
	return w, ok
}

// prompts returns the host's command-palette prompt capability when available.
func (h *paneHost) prompts() (promptHost, bool) {
	if h.opener == nil || h.listless {
		return nil, false
	}
	p, ok := h.opener.(promptHost)
	return p, ok
}

// satellitePane is an open pane: screen is nil until its proxy connects.
type satellitePane struct {
	token        string
	blockID      string
	done         chan struct{}
	connectTimer *time.Timer
	native       tcell.Screen
	screen       tcell.Screen
	painter      framePainter
	// pointer, hover and marked are the pane's own: the main screen's pointer
	// is never over it.
	pointer      image.Point
	pointerKnown bool
	hover        hoverTarget
	marked       []image.Point

	// A pane of the wall shows a view of its own rather than the visible
	// workspace's: view is its navigation, surface where it paints, drag its
	// scrollbar's, and member the agent it was opened for.
	view    *sessionWorkspace
	surface viewSurface
	drag    scrollDragState
	member  string
	// panel is set on a window a dialog floats in.
	panel *floatPanel
	// A panel's window asks for the room in want. Floating, polly last put
	// it at spot over its own pane. It is loose once the user has moved it,
	// and docked while it is a pane of the tab: theirs to place either way.
	want          floatWant
	spot          [4]float64
	loose, docked bool
	// own is a window polly draws on its own screen (see repl_pane_own.go):
	// rect is where the last frame put it, closeAt the mark that closes it,
	// and pinAt the word that docks it or floats it.
	own                  bool
	rect, closeAt, pinAt image.Rectangle
}

func newSatellitePane(token string) *satellitePane {
	return &satellitePane{token: token, done: make(chan struct{})}
}

// paneLayer paints the view's surface and its scrollbar thumb, which the
// frame's chrome layer paints when the view is framed.
type paneLayer struct {
	ui.Drawable
	surface *viewSurface
}

func (l *paneLayer) Draw(buf *ui.Buffer) {
	l.Drawable.Draw(buf)
	l.surface.scrollbar.draw(buf)
}

// paneEvent is one event from a pane's terminal; lost reports that the
// terminal is gone.
type paneEvent struct {
	pane *satellitePane
	ev   ui.Event
	lost bool
}

// paneTty is a tcell terminal backed by a pane proxy's connection: input and
// size arrive in frames, output goes back as it is written.
type paneTty struct {
	conn net.Conn
	in   *io.PipeReader
	inW  *io.PipeWriter

	mu     sync.Mutex
	size   tty.WindowSize
	resize chan<- bool
}

func newPaneTty(conn net.Conn, size paneSize) *paneTty {
	in, inW := io.Pipe()
	t := &paneTty{conn: conn, in: in, inW: inW}
	t.setSize(size)
	go t.demux()
	return t
}

// demux feeds input frames to Read and applies resizes until the connection
// ends, which Read then reports to tcell.
func (t *paneTty) demux() {
	for {
		kind, payload, err := readPaneFrame(t.conn)
		if err != nil {
			_ = t.inW.CloseWithError(err)
			return
		}
		switch kind {
		case paneFrameInput:
			if _, err := t.inW.Write(payload); err != nil {
				return
			}
		case paneFrameResize:
			if size, ok := decodePaneSize(payload); ok {
				t.setSize(size)
			}
		}
	}
}

func (t *paneTty) setSize(size paneSize) {
	if size.cols == 0 || size.rows == 0 {
		size.cols, size.rows = 80, 24
	}
	t.mu.Lock()
	t.size = tty.WindowSize{Width: int(size.cols), Height: int(size.rows), PixelWidth: int(size.pixelW), PixelHeight: int(size.pixelH)}
	resize := t.resize
	t.mu.Unlock()
	if resize != nil {
		select {
		case resize <- true:
		default:
		}
	}
}

func (t *paneTty) Start() error { return nil }
func (t *paneTty) Stop() error  { return nil }
func (t *paneTty) Drain() error { return nil }

func (t *paneTty) NotifyResize(ch chan<- bool) {
	t.mu.Lock()
	t.resize = ch
	t.mu.Unlock()
}

func (t *paneTty) WindowSize() (tty.WindowSize, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.size, nil
}

func (t *paneTty) Read(p []byte) (int, error) { return t.in.Read(p) }

func (t *paneTty) Write(p []byte) (int, error) {
	_ = t.conn.SetWriteDeadline(time.Now().Add(paneWriteTimeout))
	n, err := t.conn.Write(p)
	if err != nil {
		// Ending the connection ends demux, and Read reports the pane lost.
		_ = t.conn.Close()
	}
	return n, err
}

func (t *paneTty) Close() error {
	_ = t.in.Close()
	return t.conn.Close()
}

// startPanes arms pane support for an interactive run inside a host: tmux or
// Zellij, whose pane polly's terminal is even when they run inside Rex
// themselves, or else Rex. The environment names every host around polly,
// one inside another, and only the innermost has polly's terminal: tmux says
// which terminal its pane is, so a tmux whose pane is not polly's own is
// around another host, and is passed over.
func (r *managedREPL) startPanes() {
	h := &r.panes
	// Which panels the user keeps docked holds on polly's own screen too.
	h.docks = loadDockedPanels()
	if !paneProxySupported || r.headless != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), paneHelloTimeout)
	defer cancel()
	var tmux *tmuxPanes
	if pane, server := os.Getenv("TMUX_PANE"), os.Getenv("TMUX"); pane != "" && server != "" {
		if host := newTmuxPanes(pane, server); host.mine(ctx) {
			tmux = host
		}
	}
	if tmux != nil {
		h.opener, h.block = tmux, tmux.block()
		// Only a recent tmux floats panes; tmux has no promptHost.
		h.floatless = !tmux.floats(ctx)
	} else if pane, session := os.Getenv("ZELLIJ_PANE_ID"), os.Getenv("ZELLIJ_SESSION_NAME"); pane != "" && session != "" {
		host := newZellijPanes(pane, session)
		// Everything asked of Zellij starts from its list of panes, which an
		// older one does not give: polly's own screen does without it.
		if _, _, err := host.home(ctx); err != nil {
			return
		}
		h.opener, h.block = host, host.block()
	} else if anchor := os.Getenv("REX_BLOCK"); anchor != "" {
		h.opener, h.block = newRexPanes(anchor), anchor
	} else {
		return
	}
	h.store, _ = defaultStorePath()
	// Listening from the start lets another polly find the sessions open
	// here (see askPanes). Panes are still tried without it.
	_ = r.listenPanes()
}

// closePanes ends the pane and the listener as the run ends.
func (r *managedREPL) closePanes() {
	r.closePane()
	r.closeWall()
	r.closeFloats()
	if timer := r.panes.waking; timer != nil {
		timer.Stop()
	}
	r.untitlePane()
	r.panes.opener = nil
	r.panes.mu.Lock()
	r.panes.execs = nil
	r.panes.mu.Unlock()
	if r.panes.listener != nil {
		_ = r.panes.listener.Close()
		r.panes.listener = nil
	}
}

// await admits the proxy of the pane token names, once.
func (h *paneHost) await(token string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.awaited == nil {
		h.awaited = map[string]bool{}
	}
	h.awaited[token] = true
}

func (h *paneHost) forget(token string) {
	h.mu.Lock()
	delete(h.awaited, token)
	h.mu.Unlock()
}

// byToken finds the open pane token names.
func (h *paneHost) byToken(token string) *satellitePane {
	if h.pane != nil && h.pane.token == token {
		return h.pane
	}
	for _, p := range h.floats {
		if p.token == token {
			return p
		}
	}
	if h.wall != nil {
		for _, p := range h.wall.panes {
			if p.token == token {
				return p
			}
		}
	}
	return nil
}

// has reports whether p is still open.
func (h *paneHost) has(p *satellitePane) bool {
	return p != nil && h.byToken(p.token) == p
}

func (h *paneHost) setExec(token string, x paneExec) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.execs == nil {
		h.execs = map[string]paneExec{}
	}
	h.execs[token] = x
}

// takeExec spends the command waiting under token: one pane becomes it.
func (h *paneHost) takeExec(token string) (paneExec, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	x, ok := h.execs[token]
	delete(h.execs, token)
	return x, ok
}

// claim reports whether token names a pane awaiting its proxy, and spends
// it: one connection per pane.
func (h *paneHost) claim(token string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for awaited := range h.awaited {
		if subtle.ConstantTimeCompare([]byte(awaited), []byte(token)) == 1 {
			delete(h.awaited, awaited)
			return true
		}
	}
	return false
}

// listen opens the socket pane proxies connect to. It sits in the scratch
// root, which only this user can enter and no sandboxed command can reach.
func (r *managedREPL) listenPanes() error {
	h := &r.panes
	if h.listener != nil {
		return nil
	}
	root, err := scratch.EnsureRoot()
	if err != nil {
		return err
	}
	socket := filepath.Join(root, fmt.Sprintf("pane-%d.sock", os.Getpid()))
	// A polly that was killed leaves its socket behind, perhaps under this
	// pid. Nothing answers at those; a living polly's is left alone.
	_ = os.Remove(socket)
	stale, _ := filepath.Glob(filepath.Join(root, "pane-*.sock"))
	for _, path := range stale {
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err != nil {
			_ = os.Remove(path)
			continue
		}
		_ = conn.Close()
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	// The root already keeps other users out; the socket need not rely on it.
	_ = os.Chmod(socket, 0600)
	h.listener, h.socket = listener, socket
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go r.greetPane(conn)
		}
	}()
	return nil
}

// greetPane turns a proxy's connection into a screen off the event loop,
// since starting one waits on the terminal's answers, and hands it over.
func (r *managedREPL) greetPane(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(paneHelloTimeout))
	kind, payload, err := readPaneFrame(conn)
	_ = conn.SetReadDeadline(time.Time{})
	if err == nil && kind == paneFrameWho {
		r.answerWho(conn, payload)
		return
	}
	if err == nil && kind == paneFrameActivate {
		r.handleActivatePeer(conn, payload)
		return
	}
	size, ok := decodePaneSize(payload)
	if err != nil || kind != paneFrameHello || !ok {
		_ = conn.Close()
		return
	}
	token, term, _ := strings.Cut(string(payload[paneSizeBytes:]), "\x00")
	_ = conn.SetWriteDeadline(time.Now().Add(paneWriteTimeout))
	if x, ok := r.panes.takeExec(token); ok {
		if data, err := json.Marshal(x); err == nil {
			_, _ = conn.Write(append([]byte{paneModeExec}, data...))
		}
		_ = conn.Close()
		return
	}
	if !r.panes.claim(token) {
		_ = conn.Close()
		return
	}
	if _, err := conn.Write([]byte{paneModeRelay}); err != nil {
		_ = conn.Close()
		return
	}
	var opts []tcell.TerminfoScreenOption
	if term != "" {
		opts = append(opts, tcell.OptTerm(term))
	}
	screen, err := tcell.NewTerminfoScreenFromTty(newPaneTty(conn, size), opts...)
	if err == nil {
		if err = screen.Init(); err != nil {
			screen = nil
		}
	}
	if err != nil {
		_ = conn.Close()
	}
	if !r.postUI(r.work.ctx, func() { r.attachPane(token, screen, err) }) && screen != nil {
		screen.Fini()
	}
}

func (r *managedREPL) attachPane(token string, screen tcell.Screen, err error) {
	p := r.panes.byToken(token)
	if p == nil {
		if screen != nil {
			screen.Fini()
		}
		return
	}
	if p.connectTimer != nil {
		p.connectTimer.Stop()
		p.connectTimer = nil
	}
	if err != nil {
		switch {
		case p.panel != nil:
			r.failFloat(p, err)
		case p.view != nil:
			r.dropWallPane(p)
		default:
			r.failPane(p, err)
		}
		return
	}
	// Motion reports carry hover and scrollbar drags, as on the main screen.
	screen.EnableMouse(tcell.MouseButtonEvents | tcell.MouseMotionEvents)
	p.native = screen
	p.screen = themedScreen{p.painter.track(screen)}
	if p == r.panes.pane {
		r.syncViewPane()
	}
	if h := &r.panes; p.view == nil && h.kindOf(p) == h.keysFor && time.Now().Before(h.keysUntil) {
		// Alt-P closed the window that had the keys to open this one.
		h.keysUntil = time.Time{}
		r.focusHostPane(p.blockID)
	}
	go func() {
		var held tcell.ButtonMask
		for ev := range screen.EventQ() {
			out := paneEvent{pane: p}
			if _, failed := ev.(*tcell.EventError); failed {
				out.lost = true
			} else if converted, ok := convertTcellEvent(ev); ok {
				if mouse, isMouse := ev.(*tcell.EventMouse); isMouse {
					converted = markMouseDrag(&held, mouse, converted)
				}
				out.ev = converted
			} else {
				continue
			}
			select {
			case r.panes.events <- out:
			case <-p.done:
				return
			}
		}
	}()
}

// launch asks the host for a terminal at place that becomes the command x,
// and reports the outcome to done on the event loop: the command is running
// when the error is nil or paneBehind. False means there is no host to ask,
// and done never runs: the caller does without one.
func (r *managedREPL) launch(x paneExec, place panePlace, label string, done func(error)) bool {
	h := &r.panes
	if h.opener == nil || place == paneTab && h.tabless {
		return false
	}
	exe, err := os.Executable()
	if err == nil {
		err = r.listenPanes()
	}
	var raw [16]byte
	if err == nil {
		_, err = rand.Read(raw[:])
	}
	if err != nil {
		return false
	}
	token := hex.EncodeToString(raw[:])
	h.setExec(token, x)
	opener, argv := h.opener, []string{exe, "pane", h.socket, token}
	return r.background(func() {
		ctx, cancel := context.WithTimeout(r.work.ctx, paneOpenTimeout)
		defer cancel()
		_, err := opener.open(ctx, argv, place, label)
		if err != nil && !paneBehind(err) {
			h.takeExec(token)
		}
		r.postUI(r.work.ctx, func() { done(err) })
	})
}

// syncViewPane keeps the pane in step with the visible workspace's view: open
// while the view is, on a screen of its own. Every render starts here, so a
// view opened, closed or left behind by a tab switch is followed within the
// frame. Until a new pane's proxy connects the view stays in polly's frame so
// it never vanishes during startup or failure; a pane that cannot be opened
// hands the view back to the frame for good.
func (r *managedREPL) syncViewPane() {
	h := &r.panes
	want := h.hosts(floatView) && len(r.tabs) > 0 && r.workspace().inspector.open
	if want && h.pane == nil {
		r.openPane()
	} else if !want && h.pane != nil {
		r.closePane()
	}
	if detached := h.pane != nil && h.pane.screen != nil; detached != r.viewDetached {
		r.viewDetached = detached
		// The frame's placement, hitboxes and focus mean nothing on the other
		// screen, and the other screen's mean nothing in the frame.
		r.surface.place(image.Rectangle{}, image.Rectangle{})
		r.surface.buttons, r.surface.scrollbar, r.scrollDrag = nil, scrollbar{}, scrollDragState{}
		if len(r.tabs) > 0 {
			r.workspace().inspector.focused = false
		}
	}
	if p := h.pane; p != nil && p.screen != nil {
		// Placed before the view is projected, so it wraps for this size.
		r.surface.fill(p.screen)
	}
}

// fill places the surface on the whole of a pane's screen, less a column for
// the scrollbar when there is room for one.
func (s *viewSurface) fill(screen tcell.Screen) {
	width, height := screen.Size()
	inner, track := image.Rect(0, 0, width, height), image.Rectangle{}
	if width > 2 {
		inner.Max.X--
		track = image.Rect(width-1, 0, width, height)
	}
	s.place(inner, track)
}

// openPane asks the host for a terminal running the proxy. The answer and the
// proxy's connection both land later, on the event loop.
func (r *managedREPL) openPane() {
	h := &r.panes
	exe, err := os.Executable()
	if err == nil {
		err = r.listenPanes()
	}
	var token [16]byte
	if err == nil {
		_, err = rand.Read(token[:])
	}
	if err != nil {
		r.failPane(nil, err)
		return
	}
	p := newSatellitePane(hex.EncodeToString(token[:]))
	p.want = viewWant
	h.pane = p
	h.await(p.token)
	if !r.hostWindow(p, []string{exe, "pane", h.socket, p.token}, panelSpecs[floatView].label, func(id string, err error) {
		if err != nil {
			r.failPane(p, err)
			return
		}
		p.blockID = id
		if r.panes.pane != p && p.native == nil {
			// Closed before its proxy connected: nothing else ends it.
			r.closePaneBlock(id)
			return
		}
		r.watchPaneConnect(p)
	}) {
		r.closePane()
	}
}

// failPane gives up on the host's panes of the kind that failed for this run,
// docked ones or floating ones: the next frame shows the view on polly's own
// screen, where it stays. p is the pane that failed, nil when none got as far
// as existing; a pane already replaced is not the run's failure.
func (r *managedREPL) failPane(p *satellitePane, err error) {
	h := &r.panes
	if p != nil && h.pane != p {
		return
	}
	what := "pane"
	if p != nil && !p.docked {
		h.floatless, what = true, "floating window"
	} else {
		h.dockless = true
	}
	name := h.opener.words().name
	r.closePane()
	r.model.mu.Lock()
	r.model.appendNoticeLine(name + " " + what + " unavailable, showing the view here · " + err.Error())
	r.model.mu.Unlock()
}

// closePane ends the pane. Finishing its screen restores the terminal and
// closes the connection; the proxy then exits and Rex closes its block.
func (r *managedREPL) closePane() {
	p := r.panes.pane
	if p == nil {
		return
	}
	r.panes.pane = nil
	r.endPane(p)
}

// watchPaneConnect starts a timeout for p's proxy to attach once the host has
// created its terminal; expiry retires the unclaimed token, closes the block,
// and falls back to polly's own screen.
func (r *managedREPL) watchPaneConnect(p *satellitePane) {
	if p == nil || p.screen != nil || p.own || !r.panes.has(p) || r.work == nil {
		return
	}
	if p.connectTimer != nil {
		p.connectTimer.Stop()
	}
	p.connectTimer = time.AfterFunc(paneConnectTimeout, func() {
		r.postUI(r.work.ctx, func() {
			p.connectTimer = nil
			if !r.panes.has(p) || p.screen != nil || p.own {
				return
			}
			switch {
			case p.panel != nil:
				r.failFloat(p, errPaneConnectTimeout)
			case p.view != nil:
				r.dropWallPane(p)
			default:
				r.failPane(p, errPaneConnectTimeout)
			}
		})
	})
}

// endPane lets go of a pane that is no longer in the host's books.
func (r *managedREPL) endPane(p *satellitePane) {
	if p.connectTimer != nil {
		p.connectTimer.Stop()
		p.connectTimer = nil
	}
	r.panes.forget(p.token)
	close(p.done)
	if p.native != nil {
		p.native.Fini()
	} else if p.blockID != "" {
		r.closePaneBlock(p.blockID)
	}
}

func (r *managedREPL) closePaneBlock(id string) {
	opener := r.panes.opener
	if opener == nil {
		return
	}
	r.background(func() {
		ctx, cancel := context.WithTimeout(r.work.ctx, paneOpenTimeout)
		defer cancel()
		// The pane is usually gone already: its proxy exits when refused.
		_ = opener.close(ctx, id)
	})
}

// within runs fn with p's view as the inspected one. The view pane shows the
// visible workspace's, which it already is. A pane of the wall brings its own
// navigation and surface, and everything that reads the inspector reads those
// until fn returns: the view's code is the same wherever a view is shown.
func (r *managedREPL) within(p *satellitePane, fn func()) {
	if p.view == nil {
		fn()
		return
	}
	surface, scoped, detached, drag := r.surface, r.scoped, r.viewDetached, r.scrollDrag
	r.surface, r.scoped, r.viewDetached, r.scrollDrag = p.surface, p, true, p.drag
	defer func() {
		p.surface, p.drag = r.surface, r.scrollDrag
		r.surface, r.scoped, r.viewDetached, r.scrollDrag = surface, scoped, detached, drag
	}()
	fn()
}

// viewScope remembers which view is the inspected one now, for work that
// finishes after the handler has returned: the func it returns runs fn with
// that view inspected again, or not at all once its pane has gone.
func (r *managedREPL) viewScope() func(fn func()) {
	p := r.scoped
	return func(fn func()) {
		if p == nil {
			fn()
		} else if r.panes.has(p) {
			r.within(p, fn)
		}
	}
}

// renderPanes paints each pane's view on its screen, after the main frame and
// from the same state. The surfaces were placed as the frame began.
func (r *managedREPL) renderPanes() {
	if p := r.panes.pane; p != nil {
		r.renderPane(p)
	}
	r.renderFloats()
	if wall := r.panes.wall; wall != nil {
		for _, p := range wall.panes {
			r.within(p, func() { r.renderPane(p) })
		}
	}
}

func (r *managedREPL) renderPane(p *satellitePane) {
	if p.screen == nil || !r.viewDetached {
		return
	}
	width, height := p.screen.Size()
	if width <= 0 || height <= 0 {
		return
	}
	// The pane's terminal draws no images of ours yet, so the view is wrapped
	// with captions and its placements only serve clicks.
	r.renderInspector(r.surface.inner, viewGeometry{width: r.surface.inner.Dx()})
	r.setInspectorScrollbar(p.pointer, p.pointerKnown)
	group := &frameGroup{Block: *ui.NewBlock()}
	noBorder(&group.Block)
	group.SetRect(0, 0, width, height)
	header, body, _ := r.surface.split()
	if !header.Empty() {
		setWidgetRect(r.surface.header, header)
		group.items = append(group.items, r.surface.header)
	}
	if !body.Empty() {
		setWidgetRect(r.surface.body, body)
		group.items = append(group.items, r.surface.body)
	}
	if themed, ok := p.screen.(themedScreen); ok {
		themed.syncSurface()
	}
	p.painter.draw(p.screen, &paneLayer{Drawable: group, surface: &r.surface})
	// Hitboxes are fresh now; the underline follows this frame's layout.
	p.hover = hoverTarget{}
	if p.pointerKnown {
		p.hover = r.viewHoverTarget(p.pointer)
	}
	mark := p.hover.rect
	if action, ok := r.selectedInspectorAction(); ok {
		mark = action.mark
	}
	p.marked = underlineRect(p.screen, p.marked, mark)
	p.screen.Show()
}

// handlePaneEvent applies an event from the pane's terminal to the view. The
// pane's screen has the view's keys outright, so there is no focus to take
// or give back, and no composer for a typed rune to go to. It reports whether
// anything may have changed; pointer motion within one target has not.
func (r *managedREPL) handlePaneEvent(e paneEvent) bool {
	p := e.pane
	if !r.panes.has(p) || p.screen == nil {
		return false
	}
	if e.ev.Type == ui.ResizeEvent && p.view == nil {
		// Docking a window, or floating one, is what resizes it most.
		r.locateWindows()
	}
	if !e.lost && (e.ev.Type == ui.KeyboardEvent || (e.ev.Type == ui.MouseEvent && (e.ev.ID == "<MouseLeft>" || e.ev.ID == "<MouseWheelUp>" || e.ev.ID == "<MouseWheelDown>"))) {
		now := time.Now()
		r.model.mu.Lock()
		r.keyAt = now
		r.paneInputAt = now
		r.model.mu.Unlock()
	}
	if p.panel != nil {
		return r.floatInput(p, e)
	}
	if e.lost {
		// The terminal is gone, usually closed in Rex: that closes the view.
		if p.view != nil {
			r.dropWallPane(p)
		} else {
			r.closePane()
			r.closeInspector()
		}
		return true
	}
	changed := false
	r.within(p, func() { changed = r.paneInput(p, e.ev) })
	return changed
}

func (r *managedREPL) paneInput(p *satellitePane, ev ui.Event) bool {
	if !r.viewDetached || !r.workspace().inspector.open {
		return false
	}
	// The view's handlers expect the dispatcher's lock, as on the main screen.
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	switch ev.Type {
	case ui.ResizeEvent:
		return true
	case ui.KeyboardEvent:
		if r.viewSearchKey(ev) {
			return true
		}
		switch ev.ID {
		case "<Escape>", "<C-c>":
			r.closeInspector()
		case "<C-o>":
			r.toggleViewDisclosures()
		case "<Tab>":
			if p != r.panes.pane {
				return r.navigateView(ev) // A pane of the wall is no panel's.
			}
			r.tabToNextPanel(floatView, true)
		case "<M-p>":
			if p != r.panes.pane {
				return r.navigateView(ev)
			}
			r.redockHostPanel(p)
		default:
			return r.navigateView(ev)
		}
		return true
	case ui.MouseEvent:
		mouse, ok := ev.Payload.(ui.Mouse)
		if !ok {
			return false
		}
		point := image.Pt(mouse.X, mouse.Y)
		moved := !p.pointerKnown || point != p.pointer
		overTrack := p.pointerKnown && p.pointer.In(r.surface.scrollbar.track)
		p.pointer, p.pointerKnown = point, true
		if r.handleScrollbar(ev, false) {
			return true
		}
		// Held-button motion is not a click.
		if !mouse.Drag && r.viewContentMouse(ev, point) {
			return true
		}
		// What is left is the pointer moving: repaint when what it is over did.
		return moved && (r.viewHoverTarget(point) != p.hover || overTrack != point.In(r.surface.scrollbar.track))
	}
	return false
}
