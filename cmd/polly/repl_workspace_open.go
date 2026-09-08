package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/alexschlessinger/pollytool/sessions"
)

type workspaceEntry struct {
	root, selected *sessions.SessionView
	orphan         bool
}

// Follow stored parent identities, never a last-known display name. Missing
// ancestors leave the highest surviving conversation as a standalone root.
func resolveWorkspaceEntry(ctx context.Context, store sessions.ViewStore, name string) (*workspaceEntry, error) {
	selected, err := store.ReadView(ctx, sessions.ViewTarget{Name: name}, "")
	if err != nil {
		return nil, err
	}
	e := &workspaceEntry{root: selected, selected: selected}
	seen := map[string]bool{selected.ID: true}
	for e.root.Metadata.Parent != "" {
		if e.root.ParentID == "" {
			e.orphan = true
			break
		}
		parent, err := store.ReadView(ctx, sessions.ViewTarget{ID: e.root.ParentID}, "")
		if errors.Is(err, sessions.ErrSessionNotFound) {
			e.orphan = true
			break
		}
		if err != nil {
			return nil, err
		}
		if seen[parent.ID] {
			return nil, fmt.Errorf("session ancestry contains a cycle")
		}
		seen[parent.ID] = true
		e.root = parent
	}
	return e, nil
}

func (r *managedREPL) beginWorkspaceOpen(name string) bool {
	if r.state == nil || r.opener == nil {
		return false
	}
	store, ok := r.state.sessionStore.(sessions.ViewStore)
	if !ok {
		return false
	}
	if !r.canOpenLocked() {
		return true
	}
	r.opening = name
	ctx, cancel := context.WithCancel(r.runCtx)
	r.openCancel = cancel
	opener := r.opener
	// Snapshot the local runtime IDs before leaving the event loop.
	local := make(map[string]bool)
	for _, tab := range r.tabs {
		local[tab.viewID()] = true
	}
	go func() {
		e, err := resolveWorkspaceEntry(ctx, store, name)
		res := openResult{name: name, err: err, workspaceEntry: e}
		if err == nil {
			res.name = e.root.Metadata.Name
			if !local[e.root.ID] && !e.root.InUse {
				openCtx := context.WithValue(ctx, childViewIdentityKey{}, e.root.ID)
				_, settings, prepareErr := opener.prepare(openCtx, res.name, func(notice string) { res.notices = append(res.notices, notice) })
				res.err = prepareErr
				if res.err == nil {
					res.state, res.err = opener.open(openCtx, res.name, settings, false)
				}
			}
		}
		if res.err == nil && !local[e.root.ID] {
			if res.state != nil {
				res.name, res.display, res.err = r.newTabModelContext(ctx, res.state)
				if res.err != nil {
					_ = res.state.Close()
					res.state = nil
				}
			} else {
				res.display = prepareChildDisplay(e.root, r.config, 80)
			}
		}
		r.openDone <- res
	}()
	return true
}

func (r *managedREPL) addReadOnlyWorkspace(info *sessions.SessionView, store sessions.SessionStore, prepared ...*replModel) *replTab {
	var m *replModel
	if len(prepared) > 0 {
		m = prepared[0]
	}
	if m == nil {
		m = prepareChildDisplay(info, r.config, 80)
	}
	infoCopy := *info
	infoCopy.History = nil
	info = &infoCopy
	m.appendNoticeLine("Read-only · open in another polly · sending requires its session lease")
	tab := &replTab{name: info.Metadata.Name, model: m, childView: info, viewTarget: sessions.ViewTarget{ID: info.ID}, state: r.childViewState(store, info), detachedWorkspace: true}
	tab.workspaceRoot = true
	if len(r.tabs) == 1 && r.tabs[0].state == nil {
		r.tabs[0] = tab
	} else {
		r.tabs = append(r.tabs, tab)
	}
	r.showTab(r.tabIndexOfModel(m))
	return tab
}

func (r *managedREPL) finishWorkspaceOpen(res openResult) error {
	e := res.workspaceEntry
	var root *replTab
	for _, t := range r.tabs {
		if t.viewID() == e.root.ID {
			root = t
			break
		}
	}
	if root != nil {
		if res.state != nil {
			_ = res.state.Close()
		}
		r.showTab(r.tabIndexOfModel(root.model))
	} else if res.state != nil {
		res.state.workspaceEntry = e
		var err error
		if res.display != nil {
			err = r.addPreparedTab(res.state, res.name, res.display)
		} else {
			err = r.addTab(res.state)
		}
		if err != nil {
			_ = res.state.Close()
			return err
		}
		res.state.workspaceEntry = nil
		root = r.visibleTab()
		root.detachedWorkspace = e.orphan
	} else {
		root = r.addReadOnlyWorkspace(e.root, r.state.sessionStore, res.display)
	}
	if e.orphan {
		root.model.mu.Lock()
		root.model.appendNoticeLine("Parent unavailable")
		root.model.mu.Unlock()
	}
	if e.selected.ID != e.root.ID {
		r.inspect(viewTarget{session: sessions.ViewTarget{ID: e.selected.ID, Name: e.selected.Metadata.Name}})
	}
	return nil
}
