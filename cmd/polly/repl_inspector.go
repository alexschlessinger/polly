package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
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
	if target.session.ID == "" {
		if resolved, ok := w.resolved[viewTarget{session: target.session}.key()]; ok {
			target.session = resolved
		}
	}
	if target.kind == conversationViewKind && (!i.open || (viewTarget{session: i.target.session}).key() != (viewTarget{session: target.session}).key()) {
		w.viewState(target).agentsParent = nil
	}
	// A store-resolved target aliases its name-keyed state under the ID key,
	// so a second click through the original link is the same selection.
	same := i.target.key() == target.key() || w.states[target.key()] != nil && w.states[target.key()] == w.states[i.target.key()]
	if i.open && same {
		return
	}
	r.retireInspector(w)
	// Every switch into a view starts at its bottom, following new output.
	w.viewState(target).resetScroll()
	if len(i.history) > 0 {
		i.history = i.history[:i.position+1]
	}
	i.history = append(i.history, target)
	i.position = len(i.history) - 1
	i.target, i.open = target, true
	i.generation++
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
	w.inspector.open, w.inspector.searching, w.inspector.focused = false, false, false
	w.inspector.keyboardAction = ""
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
	if i.history[next].kind != agentsViewKind {
		w.viewState(i.history[next]).resetScroll()
	}
	i.position, i.target, i.open = next, i.history[next], true
	i.generation++
}

// targetsVisibleTab reports whether target names the workspace's main
// session, by identity when the target carries one and by name otherwise.
func (r *managedREPL) targetsVisibleTab(target viewTarget) bool {
	root := r.visibleTab()
	if target.session.ID != "" {
		return target.session.ID == root.viewID()
	}
	return target.session.Name != "" && target.session.Name == root.name
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

// refreshInspector runs before paint with no model locks held. Live source
// snapshots are copied under one lock; layout and I/O happen on a worker.
func (r *managedREPL) refreshInspector(width int) {
	w := r.workspace()
	i := &w.inspector
	if !i.open {
		return
	}
	if i.target.kind == agentsViewKind {
		r.refreshAgentsInspector(r.inspectorGeometry(width))
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
	live := r.inspectionTab(i.target)
	if i.target.kind == swarmViewKind {
		live = nil
	}
	if i.target.kind == conversationViewKind {
		v.view = conversationView{collapseInitialPrompt: !r.targetsVisibleTab(i.target)}
		if v.model != nil {
			v.model.setInitialPromptExpanded(state.promptExpanded)
		}
	}
	if v.unavailable || v.failures > 0 && time.Now().Before(v.retryAt) {
		if v.model != nil && v.geometry != geometry {
			v.view.Rows(v.model, geometry.width)
			v.geometry = geometry
		}
		return
	}
	previousOutputs := inlineToolOutputSnapshots(v.model)
	var source viewSource
	if live != nil {
		m := live.model
		m.mu.Lock()
		if i.target.kind == conversationViewKind {
			// A hidden tab's running tool rows tick only when something
			// paints them; the inspector is that something.
			m.refreshActiveTools()
		}
		revision := fmt.Sprintf("live:%p:%s:%q:%d:%d:%d:%d:%d", m, live.name, m.status.description, m.visual.revision, m.streamRaw.Len(), m.displayCatalog.version, m.turnReasoningID, m.thinkingSegmentStart.UnixNano())
		source.info = &sessions.SessionView{ID: live.viewID(), Metadata: &sessions.Metadata{Name: live.name, Title: m.status.title, TitleSource: m.status.titleSource, Parent: live.parentName, Description: m.status.description}, Artifacts: m.artifactStore}
		// The saved view's ParentID is the stable ancestry; a runtime parent
		// tab only stands in when the tab was never read from the store.
		if live.childView != nil && live.childView.ParentID != "" {
			source.info.ParentID = live.childView.ParentID
		} else if live.parent != nil {
			source.info.ParentID = live.parent.viewID()
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
			// A live view needs the display clock, without inheriting execution
			// state. Saved and retired display snapshots keep their banked time.
			source.model.turnReasoningID = m.turnReasoningID
			source.model.thinkingSegmentStart = m.thinkingSegmentStart
			// A hidden live stream has not materialized Markdown into its entry.
			if m.currentAssistant >= 0 && m.currentAssistant < len(source.model.transcript) {
				source.model.transcript[m.currentAssistant].markdown = m.streamRaw.String()
				source.model.markdownPending = true
			}
		}
		source.model.workspaceChanges = m.workspaceChanges
		source.model.toolBaseDir = m.toolBaseDir
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
	swarmState := r.state
	if !r.background(func() {
		var err error
		if target.kind == swarmViewKind {
			if swarmState == nil || swarmState.swarm == nil {
				err = fmt.Errorf("swarm runtime unavailable")
			} else {
				state, e := swarmState.swarm.State(r.work.ctx)
				err = e
				if err == nil {
					parent := swarmState.swarm.ParentState(state)
					body := swarmInspectorTextFor(state, &parent, target.item, swarmState.swarm.ID)
					source.model = newReplModel()
					source.model.appendLine(style.Escape(body))
					data, _ := json.Marshal(state)
					source.revision = fmt.Sprintf("swarm:%x", sha256.Sum256(data))
					source.info = &sessions.SessionView{ID: target.session.ID, Metadata: &sessions.Metadata{Name: target.session.Name}, Revision: source.revision}
				}
			}
		}
		if source.model == nil && err == nil {
			if reader == nil {
				err = fmt.Errorf("saved views unavailable")
			} else {
				source.info, err = reader.ReadView(r.work.ctx, target.session, knownRevision)
				if err == nil && !source.info.Unchanged {
					if target.kind == conversationViewKind {
						source.model = prepareChildDisplay(source.info, r.config, geometry.width)
						resolveToolBaseDir(r.work.ctx, reader, source.info, source.model)
						if source.model.hasAgentRows() {
							// Saved spawn rows link to their sessions the way a
							// live tab's do; otherwise nested agents are reachable
							// only through the Agents picker.
							if summaries, e := reader.(sessions.SessionStore).ListSummaries(r.work.ctx); e == nil {
								source.model.hydrateAgentSessions(source.info.Metadata.Name, summaries)
							}
						}
						if swarmState != nil && swarmState.swarm != nil && swarmState.swarm.ID == source.info.ID {
							if snapshot, e := swarmState.swarm.State(r.work.ctx); e == nil {
								source.model.hydrateSwarmAgents(snapshot)
							}
						}
						source.revision = source.info.Revision
					}
				}
			}
		}
		var model *replModel
		var size int64
		unchanged := err == nil && source.info != nil && source.info.Unchanged
		reused := err == nil && !unchanged && sameLayout && source.revision == previousRevision
		if err == nil && !unchanged && !reused {
			carryInlineToolOutputs(source.model, previousOutputs)
			model, err = v.view.Project(r.work.ctx, source, state)
			if err == nil {
				model.nativeImages, model.imageCellWidth, model.imageCellHeight = geometry.nativeImages, geometry.cellWidth, geometry.cellHeight
				model.refreshReasoningRecords(geometry.width)
				v.view.Rows(model, geometry.width)
				store := model.artifactStore
				model.artifactStore = nil
				size = childViewSize(model)
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
			// A disclosure toggled while this projection was in flight applied
			// to the current model; landing the older snapshot would undo it.
			// The next paint reprojects at the newer state.
			if w.viewState(i.target).revision != state.revision {
				return
			}
			if err == nil {
				v.failures, v.unavailable = 0, false
			}
			if unchanged || reused {
				v.info = source.info
				return
			}
			if err != nil {
				v.revision = ""
				v.failures = min(v.failures+1, 6)
				v.unavailable = errors.Is(err, sessions.ErrSessionNotFound) || reader == nil && live == nil
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
					if target.session.ID == "" {
						if w.resolved == nil {
							w.resolved = make(map[string]sessions.ViewTarget)
						}
						w.resolved[viewTarget{session: target.session}.key()] = i.target.session
					}
				}
			}
			latest := w.viewState(i.target)
			model.setInitialPromptExpanded(latest.promptExpanded)
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
