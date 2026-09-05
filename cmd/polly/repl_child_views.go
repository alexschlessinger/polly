package main

import (
	"context"
	"reflect"
	"strings"
	"sync"

	"github.com/alexschlessinger/pollytool/sessions"
)

type childViewIdentityKey struct{}

type childViewNavigation struct {
	store    sessions.SessionStore
	target   sessions.ViewTarget
	activity *agentActivity
	cached   *cachedChildView
	parent   *replTab
}

// requestChildViewLocked only records navigation. A cache hit performs no
// filesystem/database work before paint. Cold reads and refreshes run later.
func (r *managedREPL) requestChildViewLocked(activity *agentActivity, callID string) bool {
	if _, ok := r.state.sessionStore.(sessions.ViewStore); !ok {
		return false
	}
	parent := r.visibleTab()
	target := sessions.ViewTarget{ID: activity.viewID, Name: activity.session, Parent: parent.name, SpawnCallID: callID}
	for i, tab := range r.tabs {
		if target.ID != "" && tab.viewID() == target.ID || target.ID == "" && tab.parent == parent && tab.childView != nil && tab.childView.Metadata != nil && tab.childView.Metadata.SpawnCallID == callID {
			r.requestShowTabLocked(i)
			return true
		}
	}
	cached := r.childViews.take(target.ID)
	if cached != nil {
		target.ID = cached.info.ID
		activity.viewID = target.ID
	}
	r.childViewRequest = &childViewNavigation{store: r.state.sessionStore, target: target, activity: activity, cached: cached, parent: parent}
	return true
}

func (r *managedREPL) showChildView(req *childViewNavigation) {
	r.startupLogoVisible = false
	m := newReplModel()
	m.hidden, m.quiet = true, r.config.Quiet
	info := &sessions.SessionView{ID: req.target.ID, Metadata: &sessions.Metadata{Name: req.target.Name, Parent: req.parent.name, SpawnCallID: req.target.SpawnCallID}}
	if req.cached != nil {
		m, info = req.cached.model, req.cached.info
	} else {
		m.status = newSessionStatus(&r.config.Launch, req.target.Name, 0, 0)
		m.appendNoticeLine("Loading conversation…")
	}
	tab := &replTab{name: info.Metadata.Name, model: m, childView: info, viewTarget: req.target, viewLoading: req.cached == nil, parent: req.parent, parentName: info.Metadata.Parent, delivered: true}
	tab.state = r.childViewState(req.store, info)
	tab.model.artifactStore = info.Artifacts
	r.tabs = append(r.tabs, tab)
	r.showTab(len(r.tabs) - 1)
	r.refreshChildView(tab, req.activity)
}

func (r *managedREPL) childViewState(store sessions.SessionStore, info *sessions.SessionView) *conversationState {
	settings := r.config.Launch.clone()
	if info.Metadata != nil {
		for _, spec := range settingSpecs {
			if spec.fromMeta != nil {
				spec.fromMeta(&settings, info.Metadata)
			}
		}
	}
	return &conversationState{sessionStore: store, settings: settings, artifactStore: info.Artifacts}
}

func prepareChildDisplay(info *sessions.SessionView, cfg *Config, width int) *replModel {
	m := newReplModel()
	m.hidden, m.quiet = true, cfg.Quiet
	settings := cfg.Launch.clone()
	for _, spec := range settingSpecs {
		if spec.fromMeta != nil {
			spec.fromMeta(&settings, info.Metadata)
		}
	}
	m.status = newSessionStatus(&settings, info.Metadata.Name, len(info.Metadata.ActiveTools), len(info.Metadata.ActiveSkills))
	m.status.parentName = info.Metadata.Parent
	m.artifactStore = info.Artifacts
	m.hydrateHistory(info.History, info.Metadata.Name)
	m.renderPendingMarkdown()
	m.refreshReasoningRecords(width)
	m.transcriptRows(width)
	return m
}

func (r *managedREPL) refreshChildView(tab *replTab, activity *agentActivity) {
	store := tab.state.sessionStore.(sessions.ViewStore)
	target, revision := tab.viewTarget, tab.childView.Revision
	width := max(80, tab.model.visual.width)
	used := tab.viewUsed
	r.background(func() {
		info, err := store.ReadView(r.work.ctx, target, revision)
		var next *replModel
		var cached *cachedChildView
		if err == nil && !info.Unchanged {
			next = prepareChildDisplay(info, r.config, width)
			if next.hasAgentRows() {
				if summaries, e := tabStoreSummaries(store, r.work.ctx); e == nil {
					next.hydrateAgentSessions(info.Metadata.Name, summaries)
				}
			}
			info.History = nil
			cached = buildCachedChildView(info, next, used)
		}
		r.postUI(r.work.ctx, func() {
			if r.quitting {
				return
			}
			if err == nil {
				activity.viewID = info.ID
			} else if target.ID != "" {
				r.childViews.take(target.ID)
			}
			if r.tabIndexOfModel(tab.model) < 0 {
				if cached != nil {
					r.admitChildDisplay(cached)
				}
				return
			}
			// A follow-up may already have promoted the tab to a leased runtime.
			if tab.childView == nil || tab.viewOpening {
				return
			}
			tab.viewLoading = false
			if err != nil {
				tab.childView.Revision = "" // a missing/stale snapshot must not reenter the cache
				tab.model.mu.Lock()
				tab.model.appendErrorLine("could not refresh conversation: " + err.Error())
				tab.model.mu.Unlock()
				return
			}
			tab.childView, tab.viewTarget = info, sessions.ViewTarget{ID: info.ID}
			tab.name, tab.parentName = info.Metadata.Name, info.Metadata.Parent
			tab.state = r.childViewState(tab.state.sessionStore, info)
			if next != nil {
				r.replaceChildDisplay(tab, next)
			}
			tab.model.mu.Lock()
			tab.model.status.contextName, tab.model.status.parentName = tab.name, tab.parentName
			tab.model.artifactStore = info.Artifacts
			tab.model.mu.Unlock()
			if r.model == tab.model {
				r.state = tab.state
			}
			if draft := tab.viewSubmit; draft != nil {
				tab.viewSubmit = nil
				tab.model.mu.Lock()
				if tab.model.ed.text() == *draft {
					r.activateChildViewLocked(tab)
				}
				tab.model.mu.Unlock()
			}
		})
	})
}

// Keep the model object stable: clipboard completions and other UI callbacks
// may still target it. Only replace display state, preserving draft input,
// focus, paste/modal state, and the user's latest scroll position.
func (r *managedREPL) replaceChildDisplay(tab *replTab, next *replModel) {
	m := tab.model
	m.mu.Lock()
	defer m.mu.Unlock()
	m.transcript, m.markdownPending, m.visual = next.transcript, next.markdownPending, next.visual
	m.status = next.status
	m.lastIn, m.lastOut, m.lastElapsed, m.lastOutcome = next.lastIn, next.lastOut, next.lastElapsed, next.lastOutcome
	m.toolDisclosures, m.toolDisclosureAt, m.toolDisclosureSeq = next.toolDisclosures, next.toolDisclosureAt, next.toolDisclosureSeq
	m.turnToolDisclosureID, m.turnToolDisclosureIDs = next.turnToolDisclosureID, next.turnToolDisclosureIDs
	m.reasoningRecords, m.reasoningAt, m.reasoningOrder = next.reasoningRecords, next.reasoningAt, next.reasoningOrder
	m.reasoningSeq, m.reasoningWidth = next.reasoningSeq, next.reasoningWidth
	m.turnReasoningID, m.turnReasoningIDs = next.turnReasoningID, next.turnReasoningIDs
	m.turnTrailers, m.turnTrailerAt, m.turnTrailerSeq = next.turnTrailers, next.turnTrailerAt, next.turnTrailerSeq
	m.turnDock, m.openTurnTrailerID = next.turnDock, next.openTurnTrailerID
	mergeChildAttachments(m, next)
}

func mergeChildAttachments(m, next *replModel) {
	for id, attachment := range next.attachments {
		if previous, exists := m.attachments[id]; exists {
			if previous.Artifact == nil || attachment.Artifact == nil || previous.Artifact.ID != attachment.Artifact.ID {
				if m.ambiguousAttachments == nil {
					m.ambiguousAttachments = make(map[int]bool)
				}
				m.ambiguousAttachments[id] = true
			}
			continue
		}
		if m.attachments == nil {
			m.attachments = make(map[int]composerAttachment)
		}
		m.attachments[id] = attachment
	}
	for id, ambiguous := range next.ambiguousAttachments {
		if ambiguous {
			if m.ambiguousAttachments == nil {
				m.ambiguousAttachments = make(map[int]bool)
			}
			m.ambiguousAttachments[id] = true
		}
	}
	m.attachmentSeq = max(m.attachmentSeq, next.attachmentSeq)
}

func (r *managedREPL) admitChildDisplay(view *cachedChildView) {
	for _, tab := range r.tabs {
		if tab.viewID() == view.info.ID {
			return
		}
	}
	r.childViews.put(view)
}

func (t *replTab) viewID() string {
	if t.viewTarget.ID != "" {
		return t.viewTarget.ID
	}
	if t.state != nil {
		if identity, ok := t.state.session.(sessions.ViewIdentity); ok {
			return identity.ViewID()
		}
	}
	return ""
}

func buildCachedChildView(info *sessions.SessionView, model *replModel, used uint64) *cachedChildView {
	model.mu.Lock()
	display := childDisplayCopy(model)
	model.mu.Unlock()
	infoCopy := *info
	infoCopy.History = nil
	size := childViewSize(display) + retainedViewBytes(reflect.ValueOf(info.Metadata))
	display.artifactStore = info.Artifacts
	return &cachedChildView{info: &infoCopy, model: display, bytes: size, used: used}
}

func tabStoreSummaries(store sessions.ViewStore, ctx context.Context) ([]sessions.SessionSummary, error) {
	return store.(sessions.SessionStore).ListSummaries(ctx)
}

// Prepare before releasing the writer's lease so the cached revision really
// describes the retired display. Admission happens only after runtime close.
// Capture only stable identities and the model; workers never read tabs.
func (r *managedREPL) retiredChildViewTask(tab *replTab) func() *cachedChildView {
	if tab.parentName == "" || tab.state == nil {
		return nil
	}
	store, ok := tab.state.sessionStore.(sessions.ViewStore)
	if !ok {
		return nil
	}
	if tab.childView != nil {
		if tab.viewLoading || tab.viewOpening || tab.childView.Revision == "" {
			return nil
		}
		info, model, used := tab.childView, tab.model, tab.viewUsed
		return func() *cachedChildView { return buildCachedChildView(info, model, used) }
	}
	identity, ok := tab.state.session.(sessions.ViewIdentity)
	if !ok {
		return nil
	}
	id, model, used := identity.ViewID(), tab.model, tab.viewUsed
	return func() *cachedChildView {
		info, err := store.ReadView(r.work.ctx, sessions.ViewTarget{ID: id}, "")
		if err != nil {
			return nil
		}
		info.History = nil
		model.mu.Lock()
		display := childDisplayCopy(model)
		model.mu.Unlock()
		display.artifactStore = info.Artifacts
		display.renderPendingMarkdown()
		display.refreshReasoningRecords(max(80, display.visual.width))
		display.transcriptRows(max(80, display.visual.width))
		return buildCachedChildView(info, display, used)
	}
}

func childViewLocalCommand(line string) bool {
	name := strings.Fields(line)
	if len(name) == 0 {
		return true
	}
	switch name[0] {
	case "/help", "/parent", "/tab", "/close", "/new", "/resume", "/exit", "/quit", "/clear", "/attach":
		return true
	}
	return false
}

// Enter keeps its draft until a runtime has been acquired for this exact id.
// Sending again while startup is pending cannot schedule a duplicate turn.
func (r *managedREPL) activateChildViewLocked(tab *replTab) {
	if r.quitting || tab.viewOpening {
		return
	}
	if tab.viewLoading {
		draft := tab.model.ed.text()
		tab.viewSubmit = &draft
		return
	}
	if r.opener == nil {
		tab.model.appendErrorLine("opening sessions is unavailable")
		return
	}
	tab.viewOpening = true
	info, sessionStore, opener := tab.childView, tab.state.sessionStore, r.opener
	store := sessionStore.(sessions.ViewStore)
	draft := tab.model.ed.text()
	tab.model.appendNoticeLine("Preparing conversation…")
	if !r.background(func() {
		fresh, err := store.ReadView(r.work.ctx, sessions.ViewTarget{ID: info.ID}, "")
		var state *conversationState
		var next *replModel
		if err == nil && fresh.InUse {
			err = sessions.ErrSessionInUse
		}
		if err == nil {
			ctx := context.WithValue(r.work.ctx, childViewIdentityKey{}, fresh.ID)
			settings := r.childViewState(sessionStore, fresh).settings
			state, err = opener.open(ctx, fresh.Metadata.Name, settings, false)
			if err == nil {
				_, next, err = r.newTabModelContext(ctx, state)
			}
		}
		// Ownership transfers only when the event loop accepts this runtime.
		// A result queued immediately before shutdown still closes its lease.
		var handoff sync.Mutex
		adopted := false
		done := make(chan struct{})
		defer func() {
			handoff.Lock()
			defer handoff.Unlock()
			if state != nil && !adopted {
				_ = state.Close()
			}
		}()
		if r.postUI(r.work.ctx, func() {
			handoff.Lock()
			defer handoff.Unlock()
			defer close(done)
			tab.viewOpening = false
			if r.work.ctx.Err() != nil || r.quitting || r.tabIndexOfModel(tab.model) < 0 {
				return
			}
			if err != nil {
				tab.model.mu.Lock()
				tab.model.appendErrorLine("could not prepare conversation: " + err.Error())
				tab.model.mu.Unlock()
				return
			}
			adopted = true
			tab.state, tab.childView, tab.keepOpen = state, nil, true
			tab.name, tab.parentName = fresh.Metadata.Name, fresh.Metadata.Parent
			if fresh.Revision != info.Revision {
				r.replaceChildDisplay(tab, next)
			}
			m := tab.model
			m.mu.Lock()
			defer m.mu.Unlock()
			mergeChildAttachments(m, next)
			m.artifactStore, m.status.contextName, m.status.parentName = state.artifactStore, tab.name, tab.parentName
			tab.stopWatch = context.AfterFunc(state.session.Context(), r.wakeTabs)
			if r.model == m {
				r.state = state
			}
			if m.ed.text() == draft {
				// A submitted follow-up belongs to its child even if the user
				// returned to the caller while the runtime was starting.
				previousModel, previousState := r.model, r.state
				r.model, r.state = m, state
				r.submitComposerLocked()
				r.model, r.state = previousModel, previousState
			}
		}) {
			select {
			case <-done:
			case <-r.work.ctx.Done():
			}
		}
	}) {
		tab.viewOpening = false
	}
}
