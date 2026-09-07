package main

import (
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
)

func tabViewTarget(tab *replTab) viewTarget {
	return viewTarget{session: sessions.ViewTarget{ID: tab.viewID(), Name: tab.name}}
}

// These navigation functions are loop-owned and safe while the main model is
// locked. Storage and other models are inspected later, outside that lock.
func (r *managedREPL) inspect(target viewTarget) {
	w := r.workspace()
	i := &w.inspector
	if i.open && i.target.key() == target.key() {
		return
	}
	r.retireInspector(w)
	if len(i.history) > 0 {
		i.history = i.history[:i.position+1]
	}
	i.history = append(i.history, target)
	i.position = len(i.history) - 1
	i.target, i.open = target, true
	i.generation++
	r.startupLogoVisible = false
}

func (r *managedREPL) retireInspector(w *sessionWorkspace) {
	i := &w.inspector
	v := i.current
	if v != nil && v.model != nil {
		rememberViewPosition(v.model, w.viewState(i.target))
	}
	if v != nil && v.model != nil && v.info != nil && !v.loading && v.revision != "" {
		r.childViews.put(&cachedChildView{key: "inspector:" + v.target.key(), info: v.info, model: v.model, view: v, bytes: v.bytes, used: r.childViews.visit()})
	}
	i.current = nil
}

func (r *managedREPL) closeInspector() {
	w := r.workspace()
	r.retireInspector(w)
	w.inspector.open, w.inspector.searching = false, false
	w.inspector.generation++
}

func (r *managedREPL) inspectorHistory(delta int) {
	w := r.workspace()
	i := &w.inspector
	next := i.position + delta
	if next < 0 || next >= len(i.history) {
		return
	}
	r.retireInspector(w)
	i.position, i.target, i.open = next, i.history[next], true
	i.generation++
}

func (r *managedREPL) inspectorSequence(delta int) {
	w := r.workspace()
	i := &w.inspector
	if i.current == nil || i.current.model == nil {
		return
	}
	s := i.current.model.inspections
	var keys []string
	switch i.target.kind {
	case toolViewKind:
		for _, item := range s.tools {
			keys = append(keys, item.key)
		}
	case thoughtViewKind:
		for _, item := range s.thoughts {
			keys = append(keys, item.key)
		}
	default:
		return
	}
	for n, key := range keys {
		if key != i.target.item {
			continue
		}
		if next := n + delta; next >= 0 && next < len(keys) {
			t := i.target
			t.item = keys[next]
			r.inspect(t)
		}
		return
	}
}

func (r *managedREPL) inspectionTab(target viewTarget) *replTab {
	for _, tab := range r.tabs {
		if target.session.ID != "" {
			if tab.viewID() == target.session.ID {
				return tab
			}
		} else if target.session.Name != "" && tab.name == target.session.Name {
			return tab
		}
	}
	return nil
}

func (r *managedREPL) inspectorGeometry(width int) viewGeometry {
	i := &r.workspace().inspector
	if i.open && !i.maximized && width >= 120 {
		ratio := r.inspectorRatio
		if ratio == 0 {
			ratio = .5
		}
		left := max(50, min(width-51, int(float64(width-1)*ratio)))
		width -= left + 1
	}
	g := viewGeometry{width: width}
	if r.images != nil {
		g.cellWidth, g.cellHeight = r.images.cellDimensions()
		g.nativeImages = true
	}
	return g
}

// refreshInspector runs before paint with no model locks held. Live source
// snapshots are copied under one lock; layout and I/O happen on a worker.
func (r *managedREPL) refreshInspector(width int) {
	w := r.workspace()
	i := &w.inspector
	if !i.open {
		return
	}
	geometry := r.inspectorGeometry(width)
	state := *w.viewState(i.target)
	state.sections = maps.Clone(state.sections)
	if i.current == nil {
		if cached := r.childViews.take("inspector:" + i.target.key()); cached != nil && cached.view != nil {
			i.current = cached.view
		}
		if i.current == nil && i.target.kind == conversationViewKind && i.target.session.ID != "" {
			if cached := r.childViews.take(i.target.session.ID); cached != nil {
				m := cached.model
				i.current = &viewInstance{target: i.target, view: conversationView{}, model: m, info: cached.info, revision: cached.info.Revision, bytes: cached.bytes, geometry: viewGeometry{width: m.visual.width, nativeImages: m.nativeImages, cellWidth: m.imageCellWidth, cellHeight: m.imageCellHeight}}
			}
		}
		if i.current == nil {
			i.current = &viewInstance{target: i.target, view: viewFor(i.target.kind)}
		}
	}
	v := i.current
	if v.loading {
		return
	}
	if v.unavailable || v.failures > 0 && time.Now().Before(v.retryAt) {
		if v.model != nil && v.geometry != geometry {
			v.view.Rows(v.model, geometry.width)
			v.geometry = geometry
		}
		return
	}
	var source viewSource
	live := r.inspectionTab(i.target)
	if live != nil {
		m := live.model
		m.mu.Lock()
		revision := fmt.Sprintf("live:%p:%s:%q:%d:%d:%d", m, live.name, m.status.description, m.visual.revision, m.streamRaw.Len(), m.inspections.version)
		source.info = &sessions.SessionView{ID: live.viewID(), Metadata: &sessions.Metadata{Name: live.name, Parent: live.parentName, Description: m.status.description}, Artifacts: m.artifactStore}
		if live.parent != nil {
			source.info.ParentID = live.parent.viewID()
		} else if live.childView != nil {
			source.info.ParentID = live.childView.ParentID
		}
		tool, thought := m.inspections.selected(i.target)
		if i.target.kind != conversationViewKind {
			var version uint64
			if tool != nil {
				version = tool.version
			}
			if thought != nil {
				version = thought.version
			}
			revision = fmt.Sprintf("live:%p:%d:%s:%d", m, m.inspections.epoch, i.target.item, version)
			navigationRevision := fmt.Sprintf("%p:%d:%d", m, m.inspections.epoch, m.inspections.version)
			if v.model != nil && v.navigationRevision != navigationRevision {
				v.setNavigation(m.inspections)
				v.navigationRevision = navigationRevision
			}
		}
		// Titles and sequence status can change without changing the displayed
		// result. Updating them must not cause artifact reads or formatting.
		v.info = source.info
		if v.revision == revision && v.geometry == geometry && v.stateRevision == state.revision {
			m.mu.Unlock()
			return
		}
		if i.target.kind == conversationViewKind {
			source.model = childDisplayCopy(m)
			// A hidden live stream has not materialized Markdown into its entry.
			if m.currentAssistant >= 0 && m.currentAssistant < len(source.model.transcript) {
				source.model.transcript[m.currentAssistant].markdown = m.streamRaw.String()
				source.model.markdownPending = true
			}
		} else {
			source.model = newReplModel()
			source.model.inspections = m.inspections.navigation()
			if tool != nil {
				copy := *tool
				copy.result = cloneChatMessage(tool.result)
				source.tool = &copy
			}
			if thought != nil {
				copy := *thought
				copy.writer = nil
				source.thought = &copy
			}
		}
		source.model.artifactStore = m.artifactStore
		m.mu.Unlock()
		source.revision = revision
	} else if v.revision != "" && v.geometry == geometry && v.stateRevision == state.revision && time.Since(r.inspectorRefreshAt) < time.Second {
		return
	}
	r.inspectorRefreshAt = time.Now()
	var reader sessions.ViewStore
	if r.state != nil {
		reader, _ = r.state.sessionStore.(sessions.ViewStore)
	}
	target, generation := i.target, i.generation
	knownRevision := ""
	if live == nil && v.model != nil && v.info != nil && v.revision != "" && v.geometry == geometry && v.stateRevision == state.revision {
		knownRevision = v.info.Revision
	}
	previousRevision := v.revision
	sameLayout := v.model != nil && v.geometry == geometry && v.stateRevision == state.revision
	v.loading = true
	if !r.background(func() {
		var err error
		if source.model == nil {
			if reader == nil {
				err = fmt.Errorf("saved views unavailable")
			} else {
				source.info, err = reader.ReadView(r.work.ctx, target.session, knownRevision)
				if err == nil && !source.info.Unchanged {
					if target.kind == conversationViewKind {
						source.model = prepareChildDisplay(source.info, r.config, geometry.width)
						source.revision = source.info.Revision
					} else {
						source.model = newReplModel()
						source.model.hydrateInspections(source.info.History)
						source.tool, source.thought = source.model.inspections.selected(target)
						source.revision = source.itemRevision()
					}
				}
			}
		}
		var model *replModel
		var size int64
		unchanged := err == nil && source.info != nil && source.info.Unchanged
		reused := err == nil && !unchanged && sameLayout && source.revision == previousRevision
		if err == nil && !unchanged && !reused {
			model, err = v.view.Project(r.work.ctx, source, state)
			if err == nil {
				if model != source.model {
					model.inspections = source.model.inspections.navigation()
				}
				model.nativeImages, model.imageCellWidth, model.imageCellHeight = geometry.nativeImages, geometry.cellWidth, geometry.cellHeight
				model.refreshReasoningRecords(geometry.width)
				model.refreshExpandedTurnTrailer(geometry.width)
				v.view.Rows(model, geometry.width)
				store := model.artifactStore
				model.artifactStore = nil
				size = childViewSize(model)
				if target.kind != conversationViewKind {
					size += model.inspections.navigationBytes()
				}
				model.artifactStore = store
			}
		}
		if source.info != nil {
			source.info.History = nil
		}
		if err != nil {
			model = newReplModel()
			model.appendErrorLine(err.Error())
			v.view.Rows(model, geometry.width)
		}
		r.postUI(r.work.ctx, func() {
			if !i.open || i.generation != generation || i.current != v {
				return
			}
			v.loading = false
			if err == nil {
				v.failures, v.unavailable = 0, false
			}
			if unchanged || reused {
				v.info = source.info
				if reused && target.kind != conversationViewKind {
					v.setNavigation(source.model.inspections)
				}
				return
			}
			if err != nil {
				v.revision = ""
				v.failures = min(v.failures+1, 6)
				v.unavailable = errors.Is(err, sessions.ErrSessionNotFound) || errors.Is(err, errViewItemUnavailable) || reader == nil && live == nil
				v.retryAt = time.Now().Add(min(30*time.Second, time.Second<<uint(v.failures-1)))
			} else {
				v.revision, v.info = source.revision, source.info
				if source.info.ID != "" {
					// Keep the original key for UI state while replacing all future
					// store lookups with the resolved immutable identity.
					i.target.session.ID = source.info.ID
					i.target.session.Name = source.info.Metadata.Name
					v.target.session.ID = source.info.ID
					v.target.session.Name = source.info.Metadata.Name
					w.states[i.target.key()] = w.viewState(target)
					i.history[i.position] = i.target
				}
			}
			latest := w.viewState(i.target)
			if v.model != nil {
				rememberViewPosition(v.model, latest)
			}
			restoreViewPosition(model, latest)
			v.model, v.geometry = model, geometry
			v.stateRevision = state.revision
			v.bytes = size
		})
	}) {
		v.loading = false
	}
}
