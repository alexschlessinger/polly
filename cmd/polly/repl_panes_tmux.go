package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// tmux as a pane host. Inside tmux polly asks it for what it asks Rex for
// (see repl_panes.go): a window for another session, a pane beside itself,
// and, where tmux has them, floating panes over its own pane for the panels
// that float. A pane runs the same proxy, so everything painted in one is
// painted by this process.
//
// A terminal is a tmux pane, named by its pane ID. Floating panes are placed
// in the window's cells, which is where polly's own pane is measured too. A
// tmux without floating panes still docks panes and opens windows, and the
// panels that float do so on polly's own screen (see repl_pane_own.go).
// tmux has no list of its own to ask in, so questions stay on polly's screen.

// tmuxBlockPrefix starts the name polly gives other pollys for its pane: the
// server's socket and the pane, since a pane ID means nothing to another
// server.
const tmuxBlockPrefix = "tmux:"

// tmuxPanes opens panes in tmux, around anchor: the pane polly runs in, on
// the server at socket. run performs one tmux command line and reports what
// it printed.
type tmuxPanes struct {
	anchor, socket string
	run            func(ctx context.Context, args ...string) (string, error)

	// placed is where polly last put each floating pane, and the size the
	// window had then.
	mu     sync.Mutex
	placed map[string]tmuxPlace
}

// tmuxPlace is a pane's cells in its window, its left and top and its size,
// and the size of the window. floating says whether the pane floats over the
// window's layout.
type tmuxPlace struct {
	at       [4]float64
	window   [2]int
	floating bool
}

// newTmuxPanes is the host for the pane and server the environment names:
// tmux sets TMUX to its socket, its PID and the session.
func newTmuxPanes(anchor, env string) *tmuxPanes {
	socket, _, _ := strings.Cut(env, ",")
	return &tmuxPanes{anchor: anchor, socket: socket, run: tmuxRun, placed: map[string]tmuxPlace{}}
}

// put records where polly left a floating pane.
func (h *tmuxPanes) put(id string, place tmuxPlace) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.placed[id] = place
}

// tmuxRun runs tmux, which finds its server in polly's own environment.
func tmuxRun(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "tmux", args...)
	var out, complaint bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &complaint
	if err := cmd.Run(); err != nil {
		if text := strings.TrimSpace(complaint.String()); text != "" {
			return "", fmt.Errorf("%s: %w", text, err)
		}
		return "", fmt.Errorf("tmux %s: %w", args[0], err)
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

func (h *tmuxPanes) words() hostWords { return hostWords{name: "tmux", tab: "window"} }

// block is polly's own pane as another polly can find it.
func (h *tmuxPanes) block() string { return tmuxBlockPrefix + h.socket + ":" + h.anchor }

// mine reports whether polly's own terminal is the anchor pane's: it is not
// when something else with terminals of its own runs in that pane, and polly
// in that.
func (h *tmuxPanes) mine(ctx context.Context) bool {
	terminal, err := h.show(ctx, h.anchor, "#{pane_tty}")
	return err == nil && ownTerminal(terminal)
}

// floats reports whether this tmux has floating panes.
func (h *tmuxPanes) floats(ctx context.Context) bool {
	_, err := h.run(ctx, "list-commands", "new-pane")
	return err == nil
}

// show asks tmux for a format's value for a pane.
func (h *tmuxPanes) show(ctx context.Context, pane, format string) (string, error) {
	return h.run(ctx, "display-message", "-p", "-t", pane, format)
}

// created runs a command that makes a pane running argv and reports the
// pane's ID.
func (h *tmuxPanes) created(ctx context.Context, argv []string, command ...string) (string, error) {
	id, err := h.run(ctx, append(append(command, "-P", "-F", "#{pane_id}", "--"), argv...)...)
	if err == nil && !strings.HasPrefix(id, "%") {
		err = fmt.Errorf("tmux created no pane (%q)", id)
	}
	return id, err
}

// session is the target for a new window of polly's session.
func (h *tmuxPanes) session(ctx context.Context) (string, error) {
	id, err := h.show(ctx, h.anchor, "#{session_id}")
	return id + ":", err
}

func (h *tmuxPanes) open(ctx context.Context, argv []string, place panePlace, label string) (string, error) {
	session, err := h.session(ctx)
	if err != nil {
		return "", err
	}
	switch place {
	case paneTab:
		return h.created(ctx, argv, "new-window", "-t", session, "-n", label)
	case paneIfTerminal:
		return h.openIfTerminal(ctx, argv, session, label)
	}
	return "", fmt.Errorf("tmux opens nothing at place %d", place)
}

// openIfTerminal starts the command in a window behind polly's and watches
// what it does to its terminal. A program that takes the screen is a
// terminal program, and its pane is moved beside polly with the keys. One
// that has not within paneProbeTime has a window of its own on the desktop,
// and its tmux window stays in the background until it exits. No list of
// editors is consulted: the program shows what it is.
func (h *tmuxPanes) openIfTerminal(ctx context.Context, argv []string, session, label string) (string, error) {
	id, err := h.created(ctx, argv, "new-window", "-d", "-t", session, "-n", label)
	if err != nil {
		return "", err
	}
	for deadline := time.Now().Add(paneProbeTime); time.Now().Before(deadline); {
		select {
		case <-ctx.Done():
			return "", nil
		case <-time.After(paneProbeEvery):
		}
		screen, err := h.show(ctx, id, "#{alternate_on}")
		if err != nil {
			// The pane closed with its program: it came and went.
			return "", nil
		}
		if screen == "1" {
			if _, err := h.run(ctx, "join-pane", "-h", "-s", id, "-t", h.anchor); err != nil {
				_ = h.close(ctx, id)
				return "", err
			}
			return id, nil
		}
	}
	return "", nil
}

func (h *tmuxPanes) close(ctx context.Context, id string) error {
	h.mu.Lock()
	delete(h.placed, id)
	h.mu.Unlock()
	_, err := h.run(ctx, "kill-pane", "-t", id)
	return err
}

// focus brings a pane to the front: its window in its session, the pane in
// its window, and the session on the client polly is shown on. id is a pane
// of this server, or another polly's block on it.
func (h *tmuxPanes) focus(ctx context.Context, id string) error {
	if named, ok := strings.CutPrefix(id, tmuxBlockPrefix); ok {
		socket, pane, _ := cutLast(named, ":")
		if socket != h.socket {
			return errors.New("that polly is in another tmux server")
		}
		id = pane
	}
	if !strings.HasPrefix(id, "%") {
		return fmt.Errorf("%q is no tmux pane", id)
	}
	if _, err := h.run(ctx, "select-window", "-t", id); err != nil {
		return err
	}
	if _, err := h.run(ctx, "select-pane", "-t", id); err != nil {
		return err
	}
	// With nobody attached there is no client to switch, and no front.
	_, _ = h.run(ctx, "switch-client", "-t", id)
	return nil
}

// cutLast cuts s around the last sep.
func cutLast(s, sep string) (before, after string, found bool) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}

// place asks tmux where a pane is.
func (h *tmuxPanes) place(ctx context.Context, pane string) (tmuxPlace, error) {
	var place tmuxPlace
	out, err := h.show(ctx, pane, "#{pane_floating_flag} #{pane_left} #{pane_top} #{pane_width} #{pane_height} #{window_width} #{window_height}")
	if err != nil {
		return place, err
	}
	fields := strings.Fields(out)
	if len(fields) != 7 {
		return place, fmt.Errorf("tmux described pane %s as %q", pane, out)
	}
	var cells [6]int
	for n, field := range fields[1:] {
		if cells[n], err = strconv.Atoi(field); err != nil {
			return place, fmt.Errorf("tmux described pane %s as %q: %w", pane, out, err)
		}
	}
	for n := range place.at {
		place.at[n] = float64(cells[n])
	}
	place.window, place.floating = [2]int{cells[4], cells[5]}, fields[0] == "1"
	return place, nil
}

// tmuxCells is a spot over polly's own pane in the window's cells: a floating
// pane is given the corner and the size of its frame, at least big enough to
// have a row inside it.
func tmuxCells(home, spot [4]float64) (x, y, width, height string) {
	cell := func(v float64) string { return strconv.Itoa(int(v + 0.5)) }
	return cell(home[0] + spot[0]*home[2]), cell(home[1] + spot[1]*home[3]), cell(max(3, spot[2]*home[2])), cell(max(3, spot[3]*home[3]))
}

func (h *tmuxPanes) float(ctx context.Context, argv []string, label string, spot [4]float64) (string, error) {
	home, err := h.place(ctx, h.anchor)
	if err != nil {
		return "", err
	}
	x, y, width, height := tmuxCells(home.at, spot)
	// A new pane takes the keys, and they go straight back to the pane that
	// had them. -A keeps the pane in sight while polly's is zoomed.
	command := []string{"new-pane", "-A", "-t", h.anchor, "-T", label, "-x", width, "-y", height, "-X", x, "-Y", y, "-P", "-F", "#{pane_id}", "--"}
	args := make([]string, 0, len(command)+len(argv)+3)
	args = append(args, command...)
	args = append(args, argv...)
	args = append(args, ";", "last-pane", "-t", h.anchor)
	id, err := h.run(ctx, args...)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(id, "%") {
		return "", fmt.Errorf("tmux created no pane (%q)", id)
	}
	// Record where the pane landed if tmux can describe it; do not leak an
	// already-created pane if the follow-up query misses.
	if place, err := h.place(ctx, id); err == nil {
		h.put(id, place)
	}
	return id, nil
}

// arrange moves the floating panes polly placed. One that is docked now is
// the user's, and so is one that is not where polly left it, unless the
// window has changed size since: tmux moves floating panes itself to keep
// them inside a window that shrinks.
func (h *tmuxPanes) arrange(ctx context.Context, moves []paneMove) ([]bool, error) {
	home, err := h.place(ctx, h.anchor)
	if err != nil {
		return nil, err
	}
	moved := make([]bool, len(moves))
	for n, move := range moves {
		now, err := h.place(ctx, move.id)
		h.mu.Lock()
		left, known := h.placed[move.id]
		if err != nil || !now.floating {
			delete(h.placed, move.id)
		}
		h.mu.Unlock()
		if err != nil || !now.floating || known && now.at != left.at && now.window == left.window {
			continue
		}
		x, y, width, height := tmuxCells(home.at, move.to)
		if _, err := h.run(ctx, "move-pane", "-t", move.id, "-X", x, "-Y", y); err != nil {
			continue
		}
		if _, err := h.run(ctx, "resize-pane", "-t", move.id, "-x", width, "-y", height); err != nil {
			continue
		}
		if now, err := h.place(ctx, move.id); err == nil {
			h.put(move.id, now)
			moved[n] = true
		}
	}
	return moved, nil
}

func (h *tmuxPanes) dock(ctx context.Context, argv []string, label, under string, share float64) (string, error) {
	command := []string{"split-window", "-d", "-h", "-t", h.anchor, "-l", strconv.Itoa(int(min(0.9, max(0.1, share))*100+0.5)) + "%"}
	if under != "" {
		command = []string{"split-window", "-d", "-v", "-t", under}
	}
	id, err := h.created(ctx, argv, command...)
	if err == nil {
		_, _ = h.run(ctx, "select-pane", "-t", id, "-T", label)
	}
	return id, err
}

func (h *tmuxPanes) docked(ctx context.Context, ids []string) ([]dockState, error) {
	states := make([]dockState, len(ids))
	for n, id := range ids {
		place, err := h.place(ctx, id)
		if err != nil {
			h.mu.Lock()
			delete(h.placed, id)
			h.mu.Unlock()
			continue
		}
		if place.floating {
			states[n] = dockFloating
		} else {
			states[n] = dockDocked
			h.mu.Lock()
			delete(h.placed, id)
			h.mu.Unlock()
		}
	}
	return states, nil
}

// title names the window polly is in. tmux names a window after what runs in
// it until someone renames it, which turns its automatic-rename option off:
// so a window still named automatically, or carrying a name polly gave, is
// polly's to name, and giving the name back turns the option on again.
func (h *tmuxPanes) title(ctx context.Context, was, now string) error {
	out, err := h.show(ctx, h.anchor, "#{window_id}\t#{automatic-rename}\t#{window_name}")
	if err != nil {
		return err
	}
	fields := strings.SplitN(out, "\t", 3)
	if len(fields) != 3 {
		return fmt.Errorf("tmux described polly's window as %q", out)
	}
	window, automatic, name := fields[0], fields[1] == "1", fields[2]
	ours := automatic || name == paneTabLabel || was != "" && name == was
	switch {
	case !ours, name == now && !automatic, now == "" && automatic:
		return nil // Someone named this window, or it has the name already.
	case now == "":
		_, err = h.run(ctx, "set-option", "-w", "-u", "-t", window, "automatic-rename")
	default:
		_, err = h.run(ctx, "rename-window", "-t", window, "--", now)
	}
	return err
}

func (h *tmuxPanes) tile(ctx context.Context, label string, argvs [][]string) ([]string, error) {
	session, err := h.session(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(argvs))
	for n, argv := range argvs {
		var id string
		if n == 0 {
			id, err = h.created(ctx, argv, "new-window", "-t", session, "-n", label)
		} else {
			id, err = h.created(ctx, argv, "split-window", "-d", "-t", ids[0])
		}
		if err != nil {
			if len(ids) > 0 {
				_, _ = h.run(ctx, "kill-window", "-t", ids[0])
			}
			return nil, err
		}
		ids = append(ids, id)
		// Evened out as it grows, so the next split has room.
		_, _ = h.run(ctx, "select-layout", "-t", ids[0], "tiled")
	}
	return ids, nil
}

func (h *tmuxPanes) grow(ctx context.Context, beside string, stacked bool, argv []string) (string, error) {
	direction := "-h"
	if stacked {
		direction = "-v"
	}
	return h.created(ctx, argv, "split-window", "-d", direction, "-t", beside)
}
