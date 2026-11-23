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

type conversationView struct{ viewRenderer }

// Rows paints a conversation on the event loop. Background projections may warm
// transcriptRows, but must leave theme-dependent reasoning previews for paint.
func (v conversationView) Rows(m *replModel, width int) [][]ui.Cell {
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
	agents       *agentsInspectorState
	agentsParent *viewTarget
	// depth is the view's bulk state, what Ctrl-O last set; chunks holds the
	// chunks folded or opened by hand since, which win over it, and
	// sections the outputs and thoughts opened by a click. Never persisted.
	depth    disclosureDepth
	chunks   map[string]bool
	top      int
	follow   bool
	search   string
	lastRows int // -1 until a newly selected inspector item has rendered
	// lastWidth and lastTotal are the width and row count of the last paint,
	// so a re-wrap can carry the seen/unseen state across instead of reading
	// the changed row count as new output.
	lastWidth, lastTotal int
	anchor               viewAnchor
	sections             map[string]viewSection
	// A proposal can prepend a sibling or separate a previously adjacent
	// batch with prose. Keep the runtime calls' host identities as fallback
	// evidence for a fold whose heading key changes when the proposal lands.
	runtimeFolds map[string]bool
	revision     uint64
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
	applyViewSections(m, state)
	m.renderPendingMarkdown()
	return m, nil
}

// sessionWorkspace owns navigation, not child execution; its navigation
// metadata is never evicted.
type sessionWorkspace struct {
	inspector inspectorState
	states    map[string]*viewState
	// resolved maps a name-keyed session target to the identity the store
	// answered with, so a later open through the same link starts from the
	// identity and finds its cached view instead of reading cold.
	resolved map[string]sessions.ViewTarget
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
	// saved is an immutable checkpoint display. Live revisions clone it and
	// hydrate only the bounded continuation, never all saved history per token.
	saved         *replModel
	savedRevision string
	savedBytes    int64
	live          inspectorLiveStatus
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
		// An inspected view opens expanded: the reader came to see the work,
		// so its calls are listed. The main conversation opens collapsed.
		s = &viewState{follow: true, depth: depthExpanded}
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
