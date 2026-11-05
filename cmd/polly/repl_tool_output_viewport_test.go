package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
)

type viewportOutputStore struct {
	artifacts.Store
	started chan string
	gate    chan struct{}
	body    string
	opens   atomic.Int32
}

func (s *viewportOutputStore) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	s.opens.Add(1)
	s.started <- id
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.gate:
		return io.NopCloser(strings.NewReader(s.body)), nil
	}
}

type viewportOutputFixture struct {
	r     *managedREPL
	m     *replModel
	state *viewState
	store *viewportOutputStore
}

func newViewportOutputFixture(t *testing.T, pane string, count int, dense, detailed bool, preview, body string) viewportOutputFixture {
	t.Helper()
	r, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = r.work.close() })
	screen.SetSize(160, 36)
	store := &viewportOutputStore{started: make(chan string, count+10), gate: make(chan struct{}), body: body}
	m := r.model
	if pane == "watch" {
		m = newReplModel()
	}
	m.artifactStore = store
	history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "inspect outputs"}}
	var calls []messages.ChatMessageToolCall
	var results []messages.ChatMessage
	for n := range count {
		call := messages.ChatMessageToolCall{ID: fmt.Sprintf("call-%04d", n), Name: "bash"}
		ref := artifacts.Ref{ID: fmt.Sprintf("artifact-%04d", n), Kind: artifacts.KindText}
		result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: preview, Parts: []messages.ContentPart{{Type: "artifact", Artifact: &ref}}}
		if dense {
			calls = append(calls, call)
			results = append(results, result)
		} else {
			history = append(history, messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}}, result, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: fmt.Sprintf("finished %d", n)})
		}
	}
	if dense {
		history = append(history, messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: calls})
		history = append(history, results...)
		history = append(history, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "finished"})
	}
	m.hydrateHistory(history, "ctx")
	m.renderPendingMarkdown()
	fixture := viewportOutputFixture{r: r, m: m, store: store}
	if pane == "watch" {
		watchTestAgent(r, m)
		s := r.workspace().viewState(r.workspace().inspector.target)
		if detailed {
			s.depth = depthFull
			openAllOutputs(m)
			rememberViewSections(m, s)
		}
		s.follow, s.top = false, 0
		fixture.m, fixture.state = waitInspector(t, r, 160).model, s
	} else {
		openAllLines(m)
		if detailed {
			m.depth = depthFull
			openAllOutputs(m)
		}
		m.followBottom, m.scrollAnchor = false, 0
	}
	return fixture
}

func (f viewportOutputFixture) layout() (frameLayout, transcriptViewport) {
	l := f.r.frameLayoutFor(160, 36)
	if f.state == nil {
		rows := (conversationView{}).Rows(f.m, l.mainWidth())
		top, pin := f.m.settleScroll(len(rows), l.transcriptHeight)
		overlay := 0
		if f.m.activityTicker(len(rows), top, l.transcriptHeight) != "" {
			overlay = 1
		}
		return l, l.transcriptViewport(len(rows), top, pin, overlay)
	}
	g := f.r.viewGeometryFor(l.chrome, l.width)
	inner := l.chrome.inner
	header := f.r.inspectorHeader(g.width, inner.Dy(), inner.Min.X, inner.Min.Y)
	height := max(0, inner.Dy()-header.rows)
	rows := (conversationView{}).Rows(f.m, g.width)
	top := f.state.top
	if f.state.follow {
		top = max(0, len(rows)-height)
	}
	layout := frameLayout{width: g.width, transcriptHeight: height}
	return l, layout.transcriptViewport(len(rows), top, f.state.follow, 0)
}

func viewportOutputLinks(m *replModel) []toolOutputLink {
	var links []toolOutputLink
	offset := 0
	for _, block := range m.visual.blocks {
		for _, link := range block.toolOutputLinks {
			link.Y += offset
			links = append(links, link)
		}
		offset += len(block.rows)
	}
	return links
}

func viewportLoadingOutputs(m *replModel) map[string]bool {
	loading := make(map[string]bool)
	for _, record := range m.toolDisclosures.all() {
		for _, row := range record.rows {
			if row.outputLoading {
				loading[row.sectionKey] = true
			}
		}
	}
	return loading
}

func waitViewportOutputReads(t *testing.T, store *viewportOutputStore, count int) {
	t.Helper()
	for range count {
		select {
		case <-store.started:
		case <-time.After(3 * time.Second):
			t.Fatal("visible output did not start its artifact read")
		}
	}
}

func TestInlineToolOutputViewportLargeHistory(t *testing.T) {
	withDisplayTTY(t)
	for _, pane := range []string{"main", "watch"} {
		for _, dense := range []bool{false, true} {
			for _, follow := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/dense=%v/follow=%v", pane, dense, follow), func(t *testing.T) {
					f := newViewportOutputFixture(t, pane, 1000, dense, true, "preview one\npreview two\npreview three", "full output")
					if f.state == nil {
						f.m.followBottom = follow
					} else {
						f.state.follow = follow
					}
					_, viewport := f.layout()
					want := make(map[string]bool)
					for _, link := range viewportOutputLinks(f.m) {
						if link.action == "" && link.Y < viewport.end && link.Y+link.Rows > viewport.start {
							want[link.key] = true
						}
					}
					if len(want) == 0 || len(want) > 20 {
						t.Fatalf("fixture visible outputs = %d", len(want))
					}
					var revision uint64
					if f.state != nil {
						revision = f.state.revision
					}
					f.r.render()
					if f.state != nil && f.state.revision != revision+1 {
						t.Fatalf("loading should advance side state once: before=%d after=%d", revision, f.state.revision)
					}
					loading := viewportLoadingOutputs(f.m)
					if len(loading) != len(want) {
						t.Fatalf("paint started %d reads, want only %d visible outputs", len(loading), len(want))
					}
					for key := range loading {
						if !want[key] {
							t.Fatalf("paint loaded offscreen output %q", key)
						}
					}
					waitViewportOutputReads(t, f.store, len(want))
					if f.state != nil {
						f.m = waitInspector(t, f.r, 160).model
					}
					for range 3 {
						f.r.render()
					}
					if got := f.store.opens.Load(); got != int32(len(want)) {
						t.Fatalf("repeated paint duplicated reads: got %d, want %d", got, len(want))
					}
					// Scroll to a distant output, using the current layout after the
					// Loading rows were inserted. Old pending reads remain owned.
					links := viewportOutputLinks(f.m)
					destination := links[len(links)/2]
					if f.state == nil {
						f.m.followBottom, f.m.scrollAnchor = false, destination.Y
					} else {
						f.state.follow, f.state.top = false, destination.Y
					}
					f.r.render()
					loading = viewportLoadingOutputs(f.m)
					if !loading[destination.key] || len(loading) <= len(want) || len(loading) > 40 {
						t.Fatalf("scroll did not load only the next viewport: loading=%d destination=%v", len(loading), loading[destination.key])
					}
					waitViewportOutputReads(t, f.store, len(loading)-len(want))
				})
			}
		}
	}
}

func TestInlineToolOutputViewportHeaderAboveViewport(t *testing.T) {
	withDisplayTTY(t)
	for _, pane := range []string{"main", "watch"} {
		t.Run(pane, func(t *testing.T) {
			f := newViewportOutputFixture(t, pane, 2, true, true, strings.Repeat("preview line\n", 70), "full output")
			f.layout()
			link := viewportOutputLinks(f.m)[0]
			top := link.Y + 10
			if f.state == nil {
				f.m.followBottom, f.m.scrollAnchor = false, top
			} else {
				f.state.follow, f.state.top = false, top
			}
			f.r.render()
			if len(f.m.toolOutputLinks) != 1 || !f.m.toolOutputLinks[0].rect().Empty() || f.m.toolOutputLinks[0].body.Empty() {
				t.Fatalf("fixture wants a clickable body without its header: %+v", f.m.toolOutputLinks)
			}
			body := f.m.toolOutputLinks[0]
			actions := f.r.inlineKeyboardActions()
			if f.state != nil {
				actions = f.r.inspectorKeyboardActions()
			}
			for _, action := range actions {
				if action.key == body.keyboardKey() {
					t.Fatal("clipped header remained a keyboard target")
				}
			}
			loading := viewportLoadingOutputs(f.m)
			if len(loading) != 1 || !loading[link.key] {
				t.Fatalf("visible body did not load its clipped header's output: %v", loading)
			}
			waitViewportOutputReads(t, f.store, 1)
			if f.state == nil && f.m.scrollAnchor != top || f.state != nil && f.state.top != top {
				t.Fatal("adding the Loading line moved the held viewport")
			}
			f.r.handleEvent(mouseEvent("<MouseLeft>", body.body.Min))
			if _, row := f.m.toolOutputRow(link); row == nil || row.outputExpanded {
				t.Fatal("clicking the visible body did not collapse its offscreen header's output")
			}
		})
	}
}

func TestInlineToolOutputViewportHiddenPanes(t *testing.T) {
	withDisplayTTY(t)
	t.Run("maximized watch hides main", func(t *testing.T) {
		f := newViewportOutputFixture(t, "main", 1000, true, true, "preview", "full output")
		agent := newReplModel()
		agent.appendLine("agent conversation")
		watchTestAgent(f.r, agent)
		f.r.workspace().inspector.maximized = true
		waitInspector(t, f.r, 160)
		if !f.r.frameLayoutFor(160, 36).chrome.main.Empty() {
			t.Fatal("fixture did not hide main")
		}
		f.r.render()
		if len(viewportLoadingOutputs(f.m)) != 0 || f.store.opens.Load() != 0 {
			t.Fatal("hidden main started artifact reads")
		}
	})
	t.Run("no watch body height", func(t *testing.T) {
		f := newViewportOutputFixture(t, "watch", 1000, true, true, "preview", "full output")
		l, _ := f.layout()
		l.chrome.inner.Max.Y = l.chrome.inner.Min.Y + 1
		f.r.renderInspector(l)
		if len(viewportLoadingOutputs(f.m)) != 0 || f.store.opens.Load() != 0 {
			t.Fatal("zero-height Watch body started artifact reads")
		}
	})
}

func TestInlineToolOutputViewportExplicitClickAndPages(t *testing.T) {
	withDisplayTTY(t)
	for _, pane := range []string{"main", "watch"} {
		t.Run(pane, func(t *testing.T) {
			f := newViewportOutputFixture(t, pane, 1, true, false, "preview", strings.Repeat("page line\n", inlineOutputLines+5))
			close(f.store.gate)
			f.r.render()
			if len(viewportLoadingOutputs(f.m)) != 0 || f.store.opens.Load() != 0 {
				t.Fatal("ordinary open disclosure fetched a collapsed output")
			}
			link := f.m.toolOutputLinks[0]
			f.r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: link.X, Y: link.Y}})
			waitViewportOutputReads(t, f.store, 1)
			runInlineOutputTask(t, f.r)
			if f.state != nil {
				f.m = waitInspector(t, f.r, 160).model
			}
			_, row := f.m.toolOutputRow(link)
			if row == nil || !row.outputExpanded || !row.output.loaded || !row.output.more {
				t.Fatal("explicit click did not load the first output page")
			}
			for _, action := range []string{"next", "previous"} {
				if f.state == nil {
					f.m.followBottom = true
				} else {
					f.state.follow = true
				}
				f.r.render()
				var control toolOutputLink
				for _, candidate := range f.m.toolOutputLinks {
					if candidate.action == action {
						control = candidate
					}
				}
				if control.key == "" {
					t.Fatalf("missing visible %s page control", action)
				}
				f.r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: control.X, Y: control.Y}})
				waitViewportOutputReads(t, f.store, 1)
				if f.state != nil {
					// A paint can also queue a refreshed projection; allow those
					// tasks to land without assuming the output task is first.
					settleInspectorWork(f.r)
					f.m = waitInspector(t, f.r, 160).model
				} else {
					runInlineOutputTask(t, f.r)
				}
				_, row = f.m.toolOutputRow(link)
				if row == nil || !row.outputExpanded || row.outputLoading || !row.output.loaded || (row.output.offset > 0) != (action == "next") {
					t.Fatalf("%s control did not install its output page", action)
				}
			}
			for range 3 {
				f.r.render()
			}
			if got := f.store.opens.Load(); got != 3 {
				t.Fatalf("click and two page controls made %d reads, want 3", got)
			}
		})
	}
}
