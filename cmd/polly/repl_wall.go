package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"slices"

	"github.com/alexschlessinger/pollytool/sessions"
)

// The agent wall. Where a pane host has tabs, /wall opens one holding a pane
// per running agent, each showing that agent's conversation live: the view
// the inspector shows one of, side by side. A pane of the wall is a pane like
// the inspector's (see repl_panes.go) with a view and a surface of its own,
// so it scrolls, searches, follows links and stops its agent like any view.
//
// The wall follows what is running. An agent that starts takes the pane of
// one that has finished, unless the reader has scrolled or navigated there,
// and otherwise a new pane split from the roomiest one. Closing a pane's view
// (Escape) or its terminal takes that pane off the wall and keeps its agent
// off it, and the wall ends with its last pane, with /wall again, or with
// the run.
//
// Without a host there is nothing to tile, and the agents list opens in the
// frame instead.

const (
	// wallMax is how many panes the wall holds; more agents than that wait
	// for one to free up.
	wallMax = 9
	// A pane is split for another only while both halves stay readable.
	wallMinWidth  = 40
	wallMinHeight = 8
)

type paneWall struct {
	// owner is the workspace whose agents the wall shows.
	owner *replTab
	panes []*satellitePane
	// growing is set while the host is asked for one more pane, and full
	// once it refused one.
	growing, full bool
	// dismissed are the agents whose pane was closed: they stay off the wall.
	dismissed map[string]bool
}

// wallAgent is an agent the wall shows: its member ID and where its
// conversation is read from.
type wallAgent struct {
	id     string
	target sessions.ViewTarget
	label  string
}

// wallAgents lists the visible workspace's agents that are working or
// waiting on the user, in a stable order. It takes the root model's lock.
func (r *managedREPL) wallAgents() []wallAgent {
	var agents []wallAgent
	for id, entry := range r.agentsInspectorEntries(agentProgressWidth) {
		if entry.active || entry.attention {
			agents = append(agents, wallAgent{id: id, target: entry.target, label: entry.label})
		}
	}
	slices.SortFunc(agents, func(a, b wallAgent) int {
		return cmp.Or(cmp.Compare(a.label, b.label), cmp.Compare(a.id, b.id))
	})
	return agents
}

// viewWorkspaces lists every navigation a view can be shown from: each
// tab's, and each wall pane's.
func (r *managedREPL) viewWorkspaces() []*sessionWorkspace {
	var all []*sessionWorkspace
	for _, tab := range r.tabs {
		if tab.workspace != nil {
			all = append(all, tab.workspace)
		}
	}
	if wall := r.panes.wall; wall != nil {
		for _, p := range wall.panes {
			all = append(all, p.view)
		}
	}
	return all
}

// syncWall keeps the wall in step with the agents as a frame begins: it acts
// on /wall, lets go of panes whose view was closed, gives agents that started
// a pane, and places every pane's surface for this frame. Runs on the event
// loop with no model lock held.
func (r *managedREPL) syncWall() {
	h := &r.panes
	if r.wallRequested {
		r.wallRequested = false
		if h.wall != nil {
			r.closeWall()
		} else {
			r.openWall()
		}
	}
	wall := h.wall
	if wall == nil {
		return
	}
	if h.opener == nil || wall.owner != r.rootTab(r.visibleTab()) {
		// Like the view's pane, the wall belongs to the workspace on screen.
		r.closeWall()
		return
	}
	for _, p := range slices.Clone(wall.panes) {
		if !p.view.inspector.open {
			r.dropWallPane(p)
		}
	}
	if h.wall == nil {
		return
	}
	agents := r.wallAgents()
	running, shown := map[string]bool{}, map[string]bool{}
	for _, a := range agents {
		running[a.id] = true
	}
	for _, p := range wall.panes {
		shown[p.member] = true
	}
	for _, a := range agents {
		if shown[a.id] || wall.dismissed[a.id] {
			continue
		}
		if p := wall.spare(running); p != nil {
			r.showInPane(p, a)
		} else if !r.growWall(a) {
			break
		}
	}
	for _, p := range wall.panes {
		if p.screen != nil {
			// Placed before the view is projected, so it wraps for this size.
			p.surface.fill(p.screen)
		}
	}
}

// spare finds a pane whose agent is no longer running and whose reader is
// not in the middle of it: still at the bottom of the conversation the pane
// was opened for.
func (w *paneWall) spare(running map[string]bool) *satellitePane {
	for _, p := range w.panes {
		i := &p.view.inspector
		if !running[p.member] && len(i.history) == 1 && p.view.viewState(i.target).follow {
			return p
		}
	}
	return nil
}

// showInPane points a wall pane at an agent's conversation, as the only
// entry of its history.
func (r *managedREPL) showInPane(p *satellitePane, a wallAgent) {
	p.member = a.id
	r.within(p, func() {
		r.inspect(viewTarget{session: a.target})
		i := &p.view.inspector
		i.history, i.position = i.history[len(i.history)-1:], 0
	})
}

// newWallPane makes a pane for an agent and the command its terminal runs.
func (r *managedREPL) newWallPane(a wallAgent) (*satellitePane, []string, error) {
	exe, err := os.Executable()
	if err == nil {
		err = r.listenPanes()
	}
	var token [16]byte
	if err == nil {
		_, err = rand.Read(token[:])
	}
	if err != nil {
		return nil, nil, err
	}
	p := newSatellitePane(hex.EncodeToString(token[:]))
	p.view, p.surface = &sessionWorkspace{}, newViewSurface()
	r.showInPane(p, a)
	r.panes.await(p.token)
	return p, []string{exe, "pane", r.panes.socket, p.token}, nil
}

// openWall asks the host for a tab tiled with a pane per running agent. The
// answer and the proxies' connections land later, on the event loop.
func (r *managedREPL) openWall() {
	h := &r.panes
	agents := r.wallAgents()
	if len(agents) == 0 {
		r.model.mu.Lock()
		r.model.appendNoticeLine("No agents are running")
		r.model.mu.Unlock()
		return
	}
	opener, ok := h.wallOpener()
	if !ok {
		r.wallHere("The agent wall needs Rex, tmux or Zellij, showing the agents here")
		return
	}
	host := h.opener.words()
	unavailable := host.name + " " + host.tab + " unavailable, showing the agents here · "
	wall := &paneWall{owner: r.rootTab(r.visibleTab())}
	var argvs [][]string
	for _, a := range agents[:min(len(agents), wallMax)] {
		p, argv, err := r.newWallPane(a)
		if err != nil {
			for _, p := range wall.panes {
				r.retireWallPane(p)
			}
			r.wallHere(unavailable + err.Error())
			return
		}
		wall.panes, argvs = append(wall.panes, p), append(argvs, argv)
	}
	h.wall = wall
	if len(agents) > wallMax {
		r.model.mu.Lock()
		r.model.appendNoticeLine(fmt.Sprintf("Showing %d of %d running agents", wallMax, len(agents)))
		r.model.mu.Unlock()
	}
	panes := slices.Clone(wall.panes)
	if !r.background(func() {
		ctx, cancel := context.WithTimeout(r.work.ctx, paneOpenTimeout)
		defer cancel()
		ids, err := opener.tile(ctx, "polly agents", argvs)
		r.postUI(r.work.ctx, func() {
			if err != nil && !paneBehind(err) {
				if h.wall == wall {
					r.closeWall()
					h.wallless = true
					r.wallHere(unavailable + err.Error())
				}
				return
			}
			for n, p := range panes {
				r.wallPaneOpened(p, ids[n])
			}
			if err != nil && h.wall == wall {
				r.model.mu.Lock()
				r.noteBehindLocked("The agent wall opened in a "+host.name+" "+host.tab+" behind this one", err)
				r.model.mu.Unlock()
			}
		})
	}) {
		r.closeWall()
	}
}

// wallPaneOpened records the terminal the host opened for p. A pane let go
// before its proxy connected has nothing else to end it.
func (r *managedREPL) wallPaneOpened(p *satellitePane, id string) {
	p.blockID = id
	if !r.panes.has(p) && p.native == nil {
		r.closePaneBlock(id)
		return
	}
	r.watchPaneConnect(p)
}

// wallHere is the wall without a host: the agents list, in the frame.
func (r *managedREPL) wallHere(notice string) {
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	r.model.appendNoticeLine(notice)
	r.openAgentsInspector()
}

// growWall splits the roomiest pane for one more agent, across its longer
// side: a cell is about twice as tall as it is wide. It reports whether a
// pane is on its way; false leaves the agent waiting for a spare one.
func (r *managedREPL) growWall(a wallAgent) bool {
	h := &r.panes
	wall := h.wall
	if wall.growing || wall.full || len(wall.panes) >= wallMax {
		return false
	}
	var beside *satellitePane
	area := 0
	for _, p := range wall.panes {
		if p.screen == nil || p.blockID == "" {
			continue
		}
		if width, height := p.screen.Size(); width*height > area {
			beside, area = p, width*height
		}
	}
	if beside == nil {
		return false
	}
	width, height := beside.screen.Size()
	stacked := height*2 > width
	if stacked && height/2 < wallMinHeight || !stacked && width/2 < wallMinWidth {
		return false
	}
	opener, ok := h.wallOpener()
	if !ok {
		wall.full = true
		return false
	}
	p, argv, err := r.newWallPane(a)
	if err != nil {
		wall.full = true
		return false
	}
	wall.panes, wall.growing = append(wall.panes, p), true
	anchor := beside.blockID
	if !r.background(func() {
		ctx, cancel := context.WithTimeout(r.work.ctx, paneOpenTimeout)
		defer cancel()
		id, err := opener.grow(ctx, anchor, stacked, argv)
		r.postUI(r.work.ctx, func() {
			wall.growing = false
			if err != nil {
				// The wall keeps the panes it has.
				wall.full = true
				r.dropWallPane(p)
				return
			}
			r.wallPaneOpened(p, id)
		})
	}) {
		wall.growing = false
		r.dropWallPane(p)
		return false
	}
	return true
}

// closeWall ends every pane of the wall. Finishing their screens closes the
// terminals, and the host closes the tab with its last one.
func (r *managedREPL) closeWall() {
	wall := r.panes.wall
	if wall == nil {
		return
	}
	r.panes.wall = nil
	for _, p := range wall.panes {
		r.retireWallPane(p)
	}
}

// dropWallPane takes one pane off the wall; the wall ends with its last.
func (r *managedREPL) dropWallPane(p *satellitePane) {
	wall := r.panes.wall
	if wall == nil {
		return
	}
	n := slices.Index(wall.panes, p)
	if n < 0 {
		return
	}
	wall.panes = slices.Delete(wall.panes, n, n+1)
	if wall.dismissed == nil {
		wall.dismissed = map[string]bool{}
	}
	wall.dismissed[p.member] = true
	if len(wall.panes) == 0 {
		r.panes.wall = nil
	}
	r.retireWallPane(p)
}

func (r *managedREPL) retireWallPane(p *satellitePane) {
	// Closing the view hands what it loaded to the shared cache.
	r.within(p, r.closeInspector)
	r.endPane(p)
}

// comeForward brings polly's own terminal to the front, for something a wall
// pane asked for that is shown there.
func (r *managedREPL) comeForward() {
	opener, block := r.panes.opener, r.panes.block
	if opener == nil || block == "" {
		return
	}
	r.background(func() {
		ctx, cancel := context.WithTimeout(r.work.ctx, paneOpenTimeout)
		defer cancel()
		_ = opener.focus(ctx, block)
	})
}

// refreshWall reads what each pane's view needs before it is painted, as
// refreshInspector does for the visible workspace's.
func (r *managedREPL) refreshWall() {
	if wall := r.panes.wall; wall != nil {
		for _, p := range wall.panes {
			r.within(p, func() { r.refreshInspector(0) })
		}
	}
}
