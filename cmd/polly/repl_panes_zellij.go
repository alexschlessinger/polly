package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Zellij as a pane host. Inside Zellij polly asks it for what it asks Rex
// for (see repl_panes.go): a tab for another session, a pane beside itself,
// and floating panes over its own pane for the panels that float. A pane
// runs the same proxy, so everything painted in one is painted by this
// process.
//
// A terminal is a Zellij pane, named terminal_N. A floating pane is pinned,
// which keeps it in sight while the keys are in a pane under it, and placed
// in the tab's cells, where polly's own pane is measured too. Zellij has no
// list of its own to ask in, so questions stay on polly's screen; it splits
// a pane only beside the one that asks and at half its size, so docked panels
// stand side by side; and it does not say whether a program has taken the
// screen, so every editor is given a pane.

// zellijBlockPrefix starts the name polly gives other pollys for its pane:
// the session and the pane, since a pane ID means nothing to another
// session.
const zellijBlockPrefix = "zellij:"

// zellijOwnTab is the name Zellij gives a tab nobody has named.
var zellijOwnTab = regexp.MustCompile(`^Tab #\d+$`)

// zellijPanes opens panes in Zellij, around anchor: the pane polly runs in,
// in session. run performs one zellij command line and reports what it
// printed.
type zellijPanes struct {
	anchor, session string
	run             func(ctx context.Context, args ...string) (string, error)

	// placed is where polly last put each floating pane, and where its own
	// pane was then.
	mu     sync.Mutex
	placed map[string]zellijPlaced
}

type zellijPlaced struct{ at, home [4]float64 }

// zellijPane is a pane as `zellij action list-panes --json` describes it.
type zellijPane struct {
	ID       int    `json:"id"`
	Plugin   bool   `json:"is_plugin"`
	Floating bool   `json:"is_floating"`
	X        int    `json:"pane_x"`
	Y        int    `json:"pane_y"`
	Rows     int    `json:"pane_rows"`
	Columns  int    `json:"pane_columns"`
	TabID    int    `json:"tab_id"`
	TabName  string `json:"tab_name"`
}

func (p zellijPane) at() [4]float64 {
	return [4]float64{float64(p.X), float64(p.Y), float64(p.Columns), float64(p.Rows)}
}

// zellijPaneID is a terminal pane's name from the number Zellij gives a pane
// in its environment and in its listings, or from the name itself.
func zellijPaneID(id string) string {
	if _, err := strconv.Atoi(id); err == nil {
		return "terminal_" + id
	}
	return id
}

// newZellijPanes is the host for the pane and session the environment names.
func newZellijPanes(pane, session string) *zellijPanes {
	return &zellijPanes{anchor: zellijPaneID(pane), session: session, run: zellijRun, placed: map[string]zellijPlaced{}}
}

// zellijRun runs zellij, which finds its session in polly's own environment.
func zellijRun(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "zellij", args...)
	var out, complaint bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &complaint
	if err := cmd.Run(); err != nil {
		if text := strings.TrimSpace(complaint.String()); text != "" {
			return "", errors.New(text)
		}
		return "", fmt.Errorf("zellij %s: %w", strings.Join(args[:min(2, len(args))], " "), err)
	}
	return strings.TrimSpace(out.String()), nil
}

func (h *zellijPanes) words() hostWords { return hostWords{name: "Zellij", tab: "tab"} }

// block is polly's own pane as another polly can find it.
func (h *zellijPanes) block() string { return zellijBlockPrefix + h.session + ":" + h.anchor }

// action performs one of Zellij's actions.
func (h *zellijPanes) action(ctx context.Context, args ...string) (string, error) {
	return h.run(ctx, append([]string{"action"}, args...)...)
}

// panes lists the session's terminal panes by name. A Zellij too old to
// list them, or to be told which pane to act on, fails here.
func (h *zellijPanes) panes(ctx context.Context) (map[string]zellijPane, error) {
	out, err := h.action(ctx, "list-panes", "--json")
	if err != nil {
		return nil, err
	}
	var listed []zellijPane
	if err := json.Unmarshal([]byte(out), &listed); err != nil {
		return nil, fmt.Errorf("zellij listed its panes as %.60q: %w", out, err)
	}
	panes := make(map[string]zellijPane, len(listed))
	for _, pane := range listed {
		if !pane.Plugin {
			panes["terminal_"+strconv.Itoa(pane.ID)] = pane
		}
	}
	return panes, nil
}

// home is polly's own pane.
func (h *zellijPanes) home(ctx context.Context) (zellijPane, map[string]zellijPane, error) {
	panes, err := h.panes(ctx)
	if err != nil {
		return zellijPane{}, nil, err
	}
	home, ok := panes[h.anchor]
	if !ok {
		return zellijPane{}, nil, fmt.Errorf("zellij lists no pane %s", h.anchor)
	}
	return home, panes, nil
}

// created runs an action that makes a pane running argv and reports the
// pane's name.
func (h *zellijPanes) created(ctx context.Context, argv []string, flags ...string) (string, error) {
	out, err := h.action(ctx, append(append([]string{"new-pane", "--close-on-exit", "--no-focus"}, flags...), append([]string{"--"}, argv...)...)...)
	if err != nil {
		return "", err
	}
	if id := zellijPaneID(out); strings.HasPrefix(id, "terminal_") {
		return id, nil
	}
	return "", fmt.Errorf("zellij created no pane (%.60q)", out)
}

// tab opens a tab in front running argv, and reports its ID.
func (h *zellijPanes) tab(ctx context.Context, label string, argv []string) (int, error) {
	out, err := h.action(ctx, append([]string{"new-tab", "--name", label, "--close-on-exit", "--"}, argv...)...)
	if err != nil {
		return 0, err
	}
	id, err := strconv.Atoi(out)
	if err != nil {
		return 0, fmt.Errorf("zellij opened no tab (%.60q)", out)
	}
	return id, nil
}

// tabPanes is the panes of one tab, by name.
func (h *zellijPanes) tabPanes(ctx context.Context, tab int) []string {
	var names []string
	panes, _ := h.panes(ctx)
	for name, pane := range panes {
		if pane.TabID == tab {
			names = append(names, name)
		}
	}
	return names
}

func (h *zellijPanes) open(ctx context.Context, argv []string, place panePlace, label string) (string, error) {
	switch place {
	case paneTab:
		tab, err := h.tab(ctx, label, argv)
		if err != nil {
			return "", err
		}
		if panes := h.tabPanes(ctx, tab); len(panes) > 0 {
			slices.Sort(panes)
			return panes[0], nil
		}
		return "", nil
	case paneIfTerminal:
		// Zellij does not say what a program does to its terminal, so the
		// command has a pane beside polly, with the keys, whatever it is.
		out, err := h.action(ctx, append([]string{"new-pane", "--close-on-exit", "--direction", "right", "--name", label, "--"}, argv...)...)
		return zellijPaneID(out), err
	}
	return "", fmt.Errorf("zellij opens nothing at place %d", place)
}

func (h *zellijPanes) close(ctx context.Context, id string) error {
	h.mu.Lock()
	delete(h.placed, id)
	h.mu.Unlock()
	_, err := h.action(ctx, "close-pane", "--pane-id", id)
	return err
}

// focus brings a pane to the front: its tab, and the pane in it. id is a
// pane of this session, or another polly's block in it.
func (h *zellijPanes) focus(ctx context.Context, id string) error {
	if named, ok := strings.CutPrefix(id, zellijBlockPrefix); ok {
		session, pane, _ := cutLast(named, ":")
		if session != h.session {
			return errors.New("that polly is in another Zellij session")
		}
		id = pane
	}
	if !strings.HasPrefix(id, "terminal_") {
		return fmt.Errorf("%q is no Zellij pane", id)
	}
	panes, err := h.panes(ctx)
	if err != nil {
		return err
	}
	pane, ok := panes[id]
	if !ok {
		return fmt.Errorf("zellij lists no pane %s", id)
	}
	if _, err := h.action(ctx, "go-to-tab-by-id", strconv.Itoa(pane.TabID)); err != nil {
		return err
	}
	_, err = h.action(ctx, "focus-pane-id", id)
	return err
}

// zellijCells is a spot over polly's own pane in the tab's cells, as the
// flags that place a floating pane take it.
func zellijCells(home, spot [4]float64) []string {
	x, y, width, height := tmuxCells(home, spot)
	return []string{"--x", x, "--y", y, "--width", width, "--height", height}
}

func (h *zellijPanes) float(ctx context.Context, argv []string, label string, spot [4]float64) (string, error) {
	home, _, err := h.home(ctx)
	if err != nil {
		return "", err
	}
	// Pinned, the pane stays in sight while the keys are in the panes under
	// it, which is where they stay.
	id, err := h.created(ctx, argv, append([]string{"--floating", "--pinned", "true", "--name", label}, zellijCells(home.at(), spot)...)...)
	if err != nil {
		return "", err
	}
	h.record(ctx, id, home.at())
	return id, nil
}

// record notes where a floating pane polly placed came to, with polly's own
// pane at home.
func (h *zellijPanes) record(ctx context.Context, id string, home [4]float64) {
	var at [4]float64
	if panes, err := h.panes(ctx); err == nil {
		at = panes[id].at()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.placed[id] = zellijPlaced{at: at, home: home}
}

// arrange moves the floating panes polly placed. One that is docked now is
// the user's, and so is one that is not where polly left it, unless polly's
// own pane has moved or changed size since: then the tab was laid out again,
// and Zellij moved its floating panes with it.
func (h *zellijPanes) arrange(ctx context.Context, moves []paneMove) ([]bool, error) {
	home, panes, err := h.home(ctx)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	for id := range h.placed {
		if pane, ok := panes[id]; !ok || !pane.Floating {
			delete(h.placed, id)
		}
	}
	h.mu.Unlock()
	moved := make([]bool, len(moves))
	for n, move := range moves {
		now, listed := panes[move.id]
		h.mu.Lock()
		left, known := h.placed[move.id]
		h.mu.Unlock()
		if !listed || !now.Floating || known && now.at() != left.at && home.at() == left.home {
			continue
		}
		if _, err := h.action(ctx, append([]string{"change-floating-pane-coordinates", "--pane-id", move.id}, zellijCells(home.at(), move.to)...)...); err != nil {
			continue
		}
		h.record(ctx, move.id, home.at())
		moved[n] = true
	}
	return moved, nil
}

// dock splits polly's own pane for a pane at its right. Zellij splits only
// the pane that asks, in half: under and share have no say here.
func (h *zellijPanes) dock(ctx context.Context, argv []string, label, _ string, _ float64) (string, error) {
	return h.created(ctx, argv, "--direction", "right", "--name", label)
}

func (h *zellijPanes) docked(ctx context.Context, ids []string) ([]dockState, error) {
	panes, err := h.panes(ctx)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	for id := range h.placed {
		if pane, ok := panes[id]; !ok || !pane.Floating {
			delete(h.placed, id)
		}
	}
	h.mu.Unlock()
	states := make([]dockState, len(ids))
	for n, id := range ids {
		pane, ok := panes[id]
		if !ok {
			continue
		}
		if pane.Floating {
			states[n] = dockFloating
		} else {
			states[n] = dockDocked
		}
	}
	return states, nil
}

// title names the tab polly is in, unless someone named it: a tab still
// carrying the name Zellij gave it, or one polly gave, is polly's to name,
// and giving the name back undoes the renaming.
func (h *zellijPanes) title(ctx context.Context, was, now string) error {
	home, _, err := h.home(ctx)
	if err != nil {
		return err
	}
	tab, name := strconv.Itoa(home.TabID), home.TabName
	unnamed := zellijOwnTab.MatchString(name)
	ours := unnamed || name == paneTabLabel || was != "" && name == was
	switch {
	case !ours, name == now, now == "" && unnamed:
		return nil // Someone named this tab, or it has the name already.
	case now == "":
		_, err = h.action(ctx, "undo-rename-tab", "--tab-id", tab)
	default:
		_, err = h.action(ctx, "rename-tab", "--tab-id", tab, now)
	}
	return err
}

func (h *zellijPanes) tile(ctx context.Context, label string, argvs [][]string) ([]string, error) {
	tab, err := h.tab(ctx, label, argvs[0])
	if err != nil {
		return nil, err
	}
	first := h.tabPanes(ctx, tab)
	if len(first) != 1 {
		return nil, fmt.Errorf("zellij opened a tab with %d panes", len(first))
	}
	ids := first
	for _, argv := range argvs[1:] {
		// Zellij lays a tab's panes out itself as they are added.
		id, err := h.created(ctx, argv, "--tab-id", strconv.Itoa(tab))
		if err != nil {
			_, _ = h.action(ctx, "close-tab", "--tab-id", strconv.Itoa(tab))
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// grow adds a pane to the tab that holds beside. Where it goes there is
// Zellij's to say.
func (h *zellijPanes) grow(ctx context.Context, beside string, _ bool, argv []string) (string, error) {
	panes, err := h.panes(ctx)
	if err != nil {
		return "", err
	}
	pane, ok := panes[beside]
	if !ok {
		return "", fmt.Errorf("zellij lists no pane %s", beside)
	}
	return h.created(ctx, argv, "--tab-id", strconv.Itoa(pane.TabID))
}
