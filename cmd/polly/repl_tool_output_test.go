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

func TestInlineToolOutputClickReplayAndResize(t *testing.T) {
	withDisplayTTY(t)
	for _, replay := range []bool{false, true} {
		t.Run(fmt.Sprint(replay), func(t *testing.T) {
			r, screen := affordanceTestREPL(t)
			t.Cleanup(func() { _ = r.work.close() })
			m := r.model
			calls := []messages.ChatMessageToolCall{
				{ID: "one", Name: "bash", Arguments: `{"command":"printf output"}`},
				{ID: "two", Name: "lookup", Arguments: `{}`},
			}
			bodies := []string{"first-line\n  [brackets]\nlast-line", `{"name":"polly","count":2}`}
			if replay {
				history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "check"}, {Role: messages.MessageRoleAssistant, Reasoning: "look at this", ToolCalls: calls}}
				for i, call := range calls {
					history = append(history, messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: bodies[i]})
				}
				history = append(history, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done"})
				m.hydrateHistory(history, "ctx")
			} else {
				m.beginTurn("check")
				tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
				tui.ShowThinking("look at this")
				tui.AppendToolStart(calls)
				for i, call := range calls {
					tui.AppendToolEnd(call, bodies[i], time.Second, nil)
				}
				tui.AppendAssistantText("done")
				r.endTurn(nil)
			}
			m.toggleAllDisclosures(140)
			for _, width := range []int{140, 48, 80, 180} {
				screen.SetSize(width, 50)
				r.render()
				if len(m.toolOutputLinks) != 2 {
					t.Fatalf("width %d: tool links=%+v", width, m.toolOutputLinks)
				}
				for _, original := range append([]toolOutputLink(nil), m.toolOutputLinks...) {
					var link toolOutputLink
					for _, current := range m.toolOutputLinks {
						if current.key == original.key {
							link = current
						}
					}
					_, row := m.toolOutputRow(link)
					if row == nil {
						t.Fatal("link lost identity")
					}
					cell, _, _ := screen.Get(link.X, link.Y)
					if cell != "✓" && cell != "·" {
						t.Fatalf("width %d: tool target points to %q", width, cell)
					}
					if !row.outputExpanded {
						r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: link.X, Y: link.Y}})
					}
					r.render()
				}
				text := strings.Join(transcriptRowsText(m.transcriptRows(width)), "\n")
				for _, want := range []string{"first-line", "[brackets]", "last-line", `"name": "polly"`, `"count": 2`} {
					if !strings.Contains(text, want) {
						t.Fatalf("width %d: missing %q:\n%s", width, want, text)
					}
				}
				if r.workspace().inspector.open {
					t.Fatal("tool click opened a panel")
				}
			}
			link := m.toolOutputLinks[0]
			r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: link.X, Y: link.Y}})
			r.render()
			text := strings.Join(transcriptRowsText(m.transcriptRows(180)), "\n")
			if strings.Contains(text, "first-line") || !strings.Contains(text, `"name": "polly"`) {
				t.Fatalf("closing one output changed its sibling: %s", text)
			}
		})
	}
}

func TestInlineToolOutputRepeatedCallIDs(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = r.work.close() })
	call := messages.ChatMessageToolCall{ID: "reused", Name: "read_file", Arguments: `{"path":"a.go"}`}
	var history []messages.ChatMessage
	for _, body := range []string{"old-body", "new-body"} {
		history = append(history, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "read"}, messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}}, messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: body}, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done"})
	}
	r.model.hydrateHistory(history, "ctx")
	r.model.toggleAllDisclosures(140)
	screen.SetSize(140, 40)
	r.render()
	if len(r.model.toolOutputLinks) != 2 {
		t.Fatal("missing replayed rows")
	}
	link := r.model.toolOutputLinks[1]
	r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: link.X, Y: link.Y}})
	r.render()
	text := strings.Join(transcriptRowsText(r.model.transcriptRows(140)), "\n")
	if !strings.Contains(text, "new-body") || strings.Contains(text, "old-body") {
		t.Fatalf("reused call ID opened the wrong output: %s", text)
	}
}

type gatedInlineOutputStore struct {
	artifacts.Store
	gate    chan struct{}
	started chan struct{}
	opens   atomic.Int32
}

func (s *gatedInlineOutputStore) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	if s.opens.Add(1) == 1 {
		close(s.started)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.gate:
		return s.Store.Open(ctx, id)
	}
}

func runInlineOutputTask(t *testing.T, r *managedREPL) {
	t.Helper()
	select {
	case task := <-r.uiTasks:
		task()
	case <-time.After(3 * time.Second):
		t.Fatal("output load did not return")
	}
}

func TestInlineToolOutputArtifactLoadsOnDemandAndRejectsStaleResult(t *testing.T) {
	withDisplayTTY(t)
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprint(replace), func(t *testing.T) {
			r, screen := affordanceTestREPL(t)
			t.Cleanup(func() { _ = r.work.close() })
			m := r.model
			store := &gatedInlineOutputStore{Store: testArtifactStore(t), gate: make(chan struct{}), started: make(chan struct{})}
			m.artifactStore = store
			ref, err := store.Put(context.Background(), artifacts.Blob{Kind: artifacts.KindText, Data: []byte("full-artifact-body\nsecond line")})
			if err != nil {
				t.Fatal(err)
			}
			m.beginTurn("read")
			call := messages.ChatMessageToolCall{ID: "read", Name: "read_file", Arguments: `{"path":"a.go"}`}
			tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
			tui.AppendToolStart([]messages.ChatMessageToolCall{call})
			tui.AppendToolEnd(call, "preview-only", time.Second, nil)
			tui.AppendToolResult(call, messages.ChatMessage{Content: "preview-only", Parts: []messages.ContentPart{{Type: "artifact", Artifact: &ref}}})
			m.toggleAllDisclosures(140)
			screen.SetSize(140, 40)
			r.render()
			if store.opens.Load() != 0 {
				t.Fatal("opening the batch read the artifact")
			}
			link := m.toolOutputLinks[0]
			r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: link.X, Y: link.Y}})
			select {
			case <-store.started:
			case <-time.After(time.Second):
				t.Fatal("click did not load output")
			}
			if replace {
				tui.AppendToolResult(call, messages.ChatMessage{Content: "newer-result"})
			}
			close(store.gate)
			runInlineOutputTask(t, r)
			r.render()
			text := strings.Join(transcriptRowsText(m.transcriptRows(140)), "\n")
			want := "full-artifact-body"
			if replace {
				want = "newer-result"
			}
			if !strings.Contains(text, want) || (replace && strings.Contains(text, "full-artifact-body")) {
				t.Fatalf("wrong artifact result: %s", text)
			}
			for range 2 {
				link = m.toolOutputLinks[0]
				r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: link.X, Y: link.Y}})
				r.render()
			}
			if store.opens.Load() != 1 {
				t.Fatal("reopening read the artifact again")
			}
		})
	}
}

func TestInlineToolOutputFormatting(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"\x1b[31mhello\x1b[0m\r\n[brackets]\tworld", "hello\n[brackets]  world"},
		{`"hello\nworld"`, "hello\nworld"},
		{`"\u001b[31mhello\u001b[0m"`, "hello"},
		{"", "No text output"},
		{`{broken`, "{broken"},
	} {
		if got := plainStyledText(formatInlineToolOutput(tc.input, "")); got != tc.want {
			t.Fatalf("format %q = %q, want %q", tc.input, got, tc.want)
		}
	}
	for _, body := range []string{strings.Repeat("x\n", inlineOutputLines+10), strings.Repeat("x", inlineOutputBytes+10)} {
		if text := plainStyledText(formatInlineToolOutput(body, "")); !strings.Contains(text, "additional output omitted") {
			t.Fatal("large output has no truncation marker")
		}
	}
}

func TestAgentInlineToolOutputSurvivesProjectionRefresh(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = r.work.close() })
	store := &gatedInlineOutputStore{Store: testArtifactStore(t), gate: make(chan struct{}), started: make(chan struct{})}
	ref, err := store.Put(context.Background(), artifacts.Blob{Kind: artifacts.KindText, Data: []byte("full-agent-output")})
	if err != nil {
		t.Fatal(err)
	}
	agent := newReplModel()
	agent.artifactStore = store
	call := messages.ChatMessageToolCall{ID: "read", Name: "read_file", Arguments: `{"path":"a.go"}`}
	agent.hydrateHistory([]messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "read"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}},
		{Role: messages.MessageRoleTool, ToolCallID: call.ID, Content: "preview", Parts: []messages.ContentPart{{Type: "artifact", Artifact: &ref}}},
	}, "agent")
	inspectTestAgent(r, agent)
	v := waitInspector(t, r, 160)
	v.model.toggleAllDisclosures(60)
	rememberViewSections(v.model, r.workspace().viewState(r.workspace().inspector.target))
	screen.SetSize(160, 40)
	r.render()
	link := v.model.toolOutputLinks[0]
	r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: link.X, Y: link.Y}})
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("agent click did not load output")
	}
	// Replace the projection while the output is still being fetched.
	agent.appendLine("new agent message")
	v = waitInspector(t, r, 160)
	if _, row := v.model.toolOutputRow(link); row == nil || !row.outputExpanded || !row.outputLoading {
		t.Fatal("refresh lost the open output or its pending load")
	}
	close(store.gate)
	runInlineOutputTask(t, r)
	v = waitInspector(t, r, 160)
	text := strings.Join(transcriptRowsText(v.model.transcriptRows(60)), "\n")
	if !strings.Contains(text, "full-agent-output") || !strings.Contains(text, "new agent message") {
		t.Fatalf("refresh lost the output or conversation: %s", text)
	}
	if store.opens.Load() != 1 {
		t.Fatal("agent projection duplicated the artifact read")
	}
	for _, record := range agent.toolDisclosures.all() {
		for _, row := range record.rows {
			if row.outputExpanded {
				t.Fatal("pane disclosure modified the running agent")
			}
		}
	}
}

func TestMainInlineToolOutputKeepsAgentPanel(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = r.work.close() })
	m := r.model
	m.beginTurn("check")
	call := messages.ChatMessageToolCall{ID: "one", Name: "bash", Arguments: `{"command":"echo ok"}`}
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "inline-body", time.Second, nil)
	m.toggleAllDisclosures(160)
	agent := newReplModel()
	agent.appendLine("agent conversation")
	inspectTestAgent(r, agent)
	waitInspector(t, r, 160)
	screen.SetSize(160, 40)
	r.render()
	link := m.toolOutputLinks[0]
	r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: link.X, Y: link.Y}})
	r.render()
	if !r.workspace().inspector.open || !m.currentToolDisclosure().rows[0].outputExpanded {
		t.Fatal("inline output click closed the agent panel")
	}
}
