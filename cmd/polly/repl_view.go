package main

import (
	"context"
	"fmt"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
	ui "github.com/metaspartan/gotui/v5"
)

type viewKind uint8

const (
	conversationViewKind viewKind = iota
	swarmViewKind
	agentsViewKind
)

// View renders content. It deliberately has no execution, lease, or input API.
// The main transcript and the inspector use the same conversationView.
type View interface {
	Project(context.Context, viewSource, viewState) (*replModel, error)
	Rows(*replModel, int) [][]ui.Cell
}

type viewRenderer struct{}

func (viewRenderer) Rows(m *replModel, width int) [][]ui.Cell { return m.transcriptRows(width) }

type conversationView struct {
	viewRenderer
	collapseInitialPrompt bool
}

func (v conversationView) Rows(m *replModel, width int) [][]ui.Cell {
	if m.collapseInitialPrompt != v.collapseInitialPrompt {
		m.collapseInitialPrompt = v.collapseInitialPrompt
		m.visual.invalidate()
	}
	// Advance live thought labels even when no source content has changed.
	// Only the affected display rows are invalidated; projections stay cached.
	m.refreshReasoningRecords(width)
	return m.transcriptRows(width)
}

func viewFor(kind viewKind) View { return conversationView{} }

type viewTarget struct {
	session sessions.ViewTarget
	kind    viewKind
	item    string
}

func (t viewTarget) key() string {
	id := t.session.ID
	if id == "" {
		id = "name:" + t.session.Name + "/" + t.session.Parent + "/" + t.session.SpawnCallID
	}
	item := t.item
	return fmt.Sprintf("%s/%d/%s", id, t.kind, item)
}

type viewState struct {
	agents         *agentsInspectorState
	agentsParent   *viewTarget
	promptExpanded bool
	// expandAll is the inspected view's Ctrl-O sticky expand-all, the view
	// counterpart of replModel.expandDisclosures: while set, sections that
	// arrive in the view later open by default. Explicit per-section closes
	// still win. Never persisted.
	expandAll bool
	top       int
	follow    bool
	search    string
	lastRows  int // -1 until a newly selected inspector item has rendered
	// lastWidth and lastTotal are the width and row count of the last paint,
	// so a re-wrap can carry the seen/unseen state across instead of reading
	// the changed row count as new output.
	lastWidth, lastTotal int
	anchor               viewAnchor
	sections             map[string]viewSection
	revision             uint64
}

// resetScroll starts a newly shown view at its bottom, following new output.
func (s *viewState) resetScroll() {
	s.top, s.follow, s.lastRows = 0, true, -1
	s.anchor = viewAnchor{}
}

type viewGeometry struct {
	width, cellWidth, cellHeight int
	nativeImages                 bool
}

type viewSource struct {
	model    *replModel // isolated display snapshot; never the execution model
	info     *sessions.SessionView
	revision string
}

func (conversationView) Project(_ context.Context, source viewSource, state viewState) (*replModel, error) {
	if source.model == nil {
		return nil, fmt.Errorf("conversation unavailable")
	}
	m := source.model
	m.setInitialPromptExpanded(state.promptExpanded)
	applyViewSections(m, state)
	m.renderPendingMarkdown()
	return m, nil
}

// sessionWorkspace owns navigation, not child execution. Drafts stay on the
// runtime or in agentDrafts; neither they nor navigation metadata are evicted.
type sessionWorkspace struct {
	inspector inspectorState
	states    map[string]*viewState
	// resolved maps a name-keyed session target to the identity the store
	// answered with, so a later open through the same link starts from the
	// identity and finds its cached view instead of reading cold.
	resolved           map[string]sessions.ViewTarget
	agentDrafts        map[string]string
	agentDraftVersions map[string]uint64
	agentSubmissions   map[string]string
}

func (w *sessionWorkspace) setAgentDraft(key, text string) {
	if w.agentDrafts == nil {
		w.agentDrafts = make(map[string]string)
	}
	if w.agentDraftVersions == nil {
		w.agentDraftVersions = make(map[string]uint64)
	}
	w.agentDrafts[key] = text
	w.agentDraftVersions[key]++
}

type inspectorState struct {
	open, maximized bool
	// focused routes the navigation keys to the inspector instead of the
	// composer. Tab sets it; Esc, typing, closing, or
	// leaving the workspace clears it.
	keyboardAction        string
	keyboardTarget        string
	keyboardPaintedTarget string
	focused               bool
	target                viewTarget
	history               []viewTarget
	position              int
	current               *viewInstance
	generation            uint64
	searching             bool
	searchInput           lineEditor
}

type viewInstance struct {
	agentsActions []string
	bytes         int64
	target        viewTarget
	view          View
	model         *replModel
	info          *sessions.SessionView
	revision      string
	geometry      viewGeometry
	loading       bool
	stateRevision uint64
	failures      int
	unavailable   bool
	retryAt       time.Time
}

func (w *sessionWorkspace) viewState(target viewTarget) *viewState {
	if w.states == nil {
		w.states = make(map[string]*viewState)
	}
	s := w.states[target.key()]
	if s == nil {
		s = &viewState{follow: true}
		w.states[target.key()] = s
	}
	return s
}

func (r *managedREPL) workspace() *sessionWorkspace {
	t := r.visibleTab()
	if t.workspace == nil {
		t.workspace = &sessionWorkspace{}
	}
	return t.workspace
}
