package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/sessions"
	ui "github.com/metaspartan/gotui/v5"
)

type viewKind uint8

const (
	conversationViewKind viewKind = iota
	toolViewKind
	thoughtViewKind
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
type toolView struct{ viewRenderer }
type thoughtView struct{ viewRenderer }

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

func viewFor(kind viewKind) View {
	switch kind {
	case toolViewKind:
		return toolView{}
	case thoughtViewKind:
		return thoughtView{}
	default:
		return conversationView{}
	}
}

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
	return fmt.Sprintf("%s/%d/%s", id, t.kind, t.item)
}

type viewState struct {
	promptExpanded bool
	top            int
	follow         bool
	search         string
	lastRows       int // -1 until a newly selected inspector item has rendered
	anchor         viewAnchor
	sections       map[string]viewSection
	revision       uint64
}

func (s *viewState) resetScroll() {
	s.top, s.follow, s.lastRows = 0, false, -1
	s.anchor = viewAnchor{}
}

type viewGeometry struct {
	width, cellWidth, cellHeight int
	nativeImages                 bool
}

type viewSource struct {
	model    *replModel // isolated display snapshot; never the execution model
	info     *sessions.SessionView
	tool     *inspectedTool
	thought  *inspectedThought
	revision string
}

var errViewItemUnavailable = errors.New("selected item is no longer available")

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

func (toolView) Project(ctx context.Context, source viewSource, state viewState) (*replModel, error) {
	if source.tool == nil {
		return nil, errViewItemUnavailable
	}
	t := source.tool
	m := newReplModel()
	if source.info != nil {
		m.artifactStore = source.info.Artifacts
	}
	// The tool's state lives in the inspector header; the body is two titled
	// payloads, arguments then output, under one gutter. A bash call shows
	// the command it runs, as the approval block does, not its JSON envelope.
	arguments := strings.TrimSpace(t.call.Arguments)
	title, lines := "arguments", []string{styled("(none)", "muted", "")}
	if cmd, ok := bashCommandOf(t.call); ok {
		title, lines = "command", highlightCodeLines(cmd, "bash")
	} else if arguments != "" {
		lang := ""
		if json.Valid([]byte(arguments)) {
			lang = "json"
			title += " · json"
		}
		lines = highlightCodeLines(strings.TrimRight(readableResult(arguments), "\n"), lang)
	}
	m.appendLine(strings.Join(renderFence(title, lines), "\n"))
	if !t.complete {
		m.appendLine(styled("╭─ output", "muted", ""))
		m.appendNoticeLine("Running… output appears when this tool finishes")
		return m, nil
	}
	if !t.available {
		m.appendLine(styled("╭─ output", "muted", ""))
		m.appendNoticeLine("Output unavailable in saved history")
		return m, nil
	}
	body := t.result.GetContent()
	for _, part := range t.result.Parts {
		if part.Artifact == nil || part.Artifact.Kind != artifacts.KindText {
			continue
		}
		if m.artifactStore == nil {
			m.appendErrorLine("Full output artifact unavailable; showing stored preview.")
			break
		}
		reader, err := m.artifactStore.Open(ctx, part.Artifact.ID)
		if err != nil {
			m.appendErrorLine("Full output unavailable: " + err.Error())
			break
		}
		data, err := io.ReadAll(reader)
		closeErr := reader.Close()
		if err != nil {
			return nil, fmt.Errorf("read tool output: %w", err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close tool output: %w", closeErr)
		}
		body = string(data)
		break
	}
	if body == "" {
		m.appendLine(styled("╭─ output", "muted", ""))
		m.appendNoticeLine("No text output")
	} else {
		text := strings.TrimRight(stripTranscriptImageMarkers(readableResult(body)), "\n")
		raw := strings.Split(text, "\n")
		for n := range raw {
			raw[n] = styleEscape(raw[n])
		}
		m.appendLine(strings.Join(renderFence("output · "+resultLineMeta(text), raw), "\n"))
	}
	images := inspectionTranscriptImages(t.result, m.artifactStore)
	if len(images) > 0 {
		idx := m.appendTranscriptEntry(renderInspectionTranscriptImages(images))
		m.setTranscriptImages(idx, images)
	}
	return m, nil
}

func readableResult(text string) string {
	if !json.Valid([]byte(text)) {
		return text
	}
	var b bytes.Buffer
	if json.Indent(&b, []byte(text), "", "  ") == nil {
		return b.String()
	}
	return text
}

func (thoughtView) Project(_ context.Context, source viewSource, state viewState) (*replModel, error) {
	if source.thought == nil {
		return nil, errViewItemUnavailable
	}
	m := newReplModel()
	text := stripTranscriptImageMarkers(source.thought.text)
	if strings.TrimSpace(text) == "" {
		m.appendNoticeLine("Waiting for thoughts…")
	} else {
		m.appendLine(styleEscape(text))
	}
	return m, nil
}

// sessionWorkspace owns navigation, not child execution. Drafts stay on the
// runtime or in agentDrafts; neither they nor navigation metadata are evicted.
type sessionWorkspace struct {
	inspector        inspectorState
	states           map[string]*viewState
	agentDrafts      map[string]string
	agentSubmissions map[string]string
}

type inspectorState struct {
	open, maximized bool
	// focused routes the navigation keys to the inspector instead of the
	// composer. Tab on an empty composer sets it; Esc, typing, closing, or
	// leaving the workspace clears it.
	focused     bool
	target      viewTarget
	history     []viewTarget
	position    int
	current     *viewInstance
	generation  uint64
	searching   bool
	searchInput lineEditor
}

type viewInstance struct {
	bytes              int64
	target             viewTarget
	view               View
	model              *replModel
	info               *sessions.SessionView
	revision           string
	geometry           viewGeometry
	loading            bool
	stateRevision      uint64
	navigationRevision string
	failures           int
	unavailable        bool
	retryAt            time.Time
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
