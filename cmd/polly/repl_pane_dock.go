package main

import (
	"context"
	"os"
	"slices"
	"strings"

	ui "github.com/metaspartan/gotui/v5"
)

// Docked panels. A floating window (see repl_pane_float.go) is an ordinary
// terminal of the host's, so the user can dock it into the tab as a pane, and
// float a pane again. Polly remembers which they did, panel by panel: a panel
// left docked opens as a pane the next time, in this run and later ones, and
// one floated again opens floating. Nothing has to be set.
//
// The host does not say when a window is docked, but docking changes its
// size and often polly's, and both terminals report that: each report has
// polly ask the host where its windows are. So does a window that was not
// where polly left it when it came to be moved.

// envVarPanes names the panels that open as panes, in the user's
// configuration file: polly writes it as the user docks and floats them.
const envVarPanes = "POLLYTOOL_PANES"

// The share of polly's own width a panel docked beside it takes.
const (
	dockLeast = 0.2
	dockMost  = 0.5
)

// kindOf says which panel p shows: the inspected view has no dialog.
func (h *paneHost) kindOf(p *satellitePane) floatKind {
	if p.panel != nil {
		return p.panel.kind
	}
	return floatView
}

// panelWindows returns every open panel's window in canonical order, docked or
// floating.
func (h *paneHost) panelWindows() []*satellitePane {
	var windows []*satellitePane
	for _, kind := range panelOrder {
		if p := h.panelOf(kind); p != nil {
			windows = append(windows, p)
		}
	}
	return windows
}

// loadDockedPanels reads the panels the user keeps docked: from the
// environment, or else from the configuration file.
func loadDockedPanels() map[floatKind]bool {
	value, ok := os.LookupEnv(envVarPanes)
	if !ok {
		value, _ = userConfigValue(envVarPanes)
	}
	docks := map[floatKind]bool{}
	for _, name := range strings.Split(value, ",") {
		trimmed := strings.TrimSpace(name)
		for kind, spec := range panelSpecs {
			if trimmed == spec.name {
				docks[kind] = true
			}
		}
	}
	return docks
}

// saveDockedPanels writes them to the configuration file, beside the other
// defaults saved there.
func saveDockedPanels(docks map[floatKind]bool) error {
	var names []string
	for kind, spec := range panelSpecs {
		if docks[kind] {
			names = append(names, spec.name)
		}
	}
	slices.Sort(names)
	path, err := userConfigPath()
	if err != nil {
		return err
	}
	return writeUserConfig(path, map[string]string{envVarPanes: strings.Join(names, ",")})
}

// hostWindow asks the host for p's window, running argv: a pane of the tab
// where the user keeps that panel docked, and a floating window otherwise.
// done hears the outcome on the event loop. False means the work could not
// be started, and done never runs.
func (r *managedREPL) hostWindow(p *satellitePane, argv []string, label string, done func(id string, err error)) bool {
	h := &r.panes
	opener, ok := h.windows()
	if !ok {
		return false
	}
	kind := h.kindOf(p)
	if h.docks[kind] {
		// Beside polly, or under the closest preceding docked panel in
		// canonical order (falling back to any docked panel to stay in one
		// right-hand column).
		targetOrder := slices.Index(panelOrder, kind)
		under := ""
		for _, other := range h.panelWindows() {
			if other == p || !other.docked || other.blockID == "" {
				continue
			}
			otherOrder := slices.Index(panelOrder, h.kindOf(other))
			if otherOrder < targetOrder {
				under = other.blockID
			} else if under == "" {
				under = other.blockID
			}
		}
		columns, _ := ui.TerminalDimensions()
		share := min(dockMost, max(dockLeast, float64(p.want.columns)/float64(max(1, columns))))
		p.docked = true
		return r.background(func() {
			ctx, cancel := context.WithTimeout(r.work.ctx, paneOpenTimeout)
			defer cancel()
			id, err := opener.dock(ctx, argv, label, under, share)
			r.postUI(r.work.ctx, func() { done(id, err) })
		})
	}
	spot := h.spotFor(p, max(floatFooterRows, h.footerRows))
	return r.background(func() {
		ctx, cancel := context.WithTimeout(r.work.ctx, paneOpenTimeout)
		defer cancel()
		id, err := opener.float(ctx, argv, label, spot)
		r.postUI(r.work.ctx, func() {
			done(id, err)
		})
	})
}

// locateWindows asks the host which of the panels' windows are docked, and
// remembers what the user changed: a window's terminal changed size, which
// docking and floating do. One check runs at a time; a report that arrives
// meanwhile has it run once more.
func (r *managedREPL) locateWindows() {
	h := &r.panes
	opener, ok := h.windows()
	if !ok || r.work == nil {
		return
	}
	if h.locating {
		h.relocate = true
		return
	}
	var windows []*satellitePane
	var ids []string
	for _, p := range h.panelWindows() {
		if p.blockID != "" {
			windows, ids = append(windows, p), append(ids, p.blockID)
		}
	}
	if len(ids) == 0 {
		return
	}
	h.locating = true
	if !r.background(func() {
		ctx, cancel := context.WithTimeout(r.work.ctx, paneOpenTimeout)
		defer cancel()
		states, err := opener.docked(ctx, ids)
		r.postUI(r.work.ctx, func() {
			h.locating = false
			changed := false
			for n, p := range windows {
				if err != nil || n >= len(states) || !h.has(p) || states[n] == dockUnknown {
					continue
				}
				docked := states[n] == dockDocked
				if docked == p.docked {
					continue
				}
				// The user docked it, or floated it where they want it: either
				// way it is theirs to place now, and where it opens next.
				p.docked, p.loose = docked, true
				if kind := h.kindOf(p); h.docks[kind] != p.docked {
					if h.docks == nil {
						h.docks = map[floatKind]bool{}
					}
					h.docks[kind], changed = p.docked, true
				}
			}
			if changed {
				if err := saveDockedPanels(h.docks); err != nil {
					r.model.mu.Lock()
					r.model.appendNoticeLine("Docked panels not saved for later launches · " + err.Error())
					r.model.mu.Unlock()
				}
			}
			if h.relocate {
				h.relocate = false
				r.locateWindows()
			}
		})
	}) {
		h.locating = false
	}
}
