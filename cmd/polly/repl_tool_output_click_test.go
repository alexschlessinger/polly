package main

import (
	"fmt"
	"image"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

func TestInlineToolBodyClickClosesOnlyItsRow(t *testing.T) {
	withDisplayTTY(t)
	for _, pane := range []string{"main", "watch"} {
		for _, kind := range []string{"output", "command", "diff"} {
			for _, width := range []int{80, 160} {
				t.Run(fmt.Sprintf("%s/%s/%d", pane, kind, width), func(t *testing.T) {
					r, screen := affordanceTestREPL(t)
					t.Cleanup(func() { _ = r.work.close() })
					screen.SetSize(width, 40)
					m := r.model
					if pane == "watch" {
						m = newReplModel()
					}
					call := messages.ChatMessageToolCall{ID: "first", Name: "read_file", Arguments: `{"path":"a.go"}`}
					switch kind {
					case "command":
						call.Name, call.Arguments = "bash", `{"command":"cd staging && printf 'a long command with several words to wrap across a narrow pane'"}`
					case "diff":
						call.Name = "write_file"
					}
					result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: strings.Repeat("output words ", 12)}
					if kind == "diff" {
						result = toolDataResult(t, call, "changed", editChanges("a.go"))
					}
					sibling := messages.ChatMessageToolCall{ID: "second", Name: "lookup", Arguments: `{}`}
					m.hydrateHistory([]messages.ChatMessage{
						{Role: messages.MessageRoleUser, Content: "inspect"},
						{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call, sibling}},
						result,
						{Role: messages.MessageRoleTool, ToolCallID: sibling.ID, ToolName: sibling.Name, Content: "sibling output"},
						{Role: messages.MessageRoleAssistant, Content: "done"},
					}, "ctx")
					if pane == "watch" {
						watchTestAgent(r, m)
						m = waitInspector(t, r, width).model
					}
					// An agent's view already opens open; the main pane starts folded.
					if pane != "watch" {
						m.toggleAllDisclosures(width)
					}
					m.toggleAllDisclosures(width)
					if pane == "watch" {
						rememberViewSections(m, r.workspace().viewState(r.workspace().inspector.target))
					}
					r.render()
					if len(m.toolOutputLinks) != 2 {
						t.Fatalf("tool links = %+v, want two", m.toolOutputLinks)
					}
					link := m.toolOutputLinks[0]
					if link.body.Empty() || link.body.Dy() < 2 {
						t.Fatalf("expanded row has no clickable body: %+v", link)
					}
					// Body clicks reach across the pane, not only the short header.
					point := image.Pt(link.body.Max.X-1, link.body.Min.Y+1)
					if kind == "output" && point.X < link.X+link.Cols {
						t.Fatalf("fixture body point %v should be past its header %+v", point, link)
					}
					r.handleEvent(mouseEvent("<MouseLeft>", point))
					if pane == "watch" {
						m = waitInspector(t, r, width).model
						state := r.workspace().viewState(r.workspace().inspector.target)
						if state.sections[link.key].outputExpanded {
							t.Fatal("body collapse was not saved in inspector state")
						}
					}
					record, row := m.toolOutputRow(link)
					if record == nil || row == nil || !record.expanded || row.outputExpanded {
						t.Fatalf("body click did not collapse only the tool output: record=%+v row=%+v", record, row)
					}
					if !record.rows[1].outputExpanded {
						t.Fatal("body click collapsed the sibling output")
					}
				})
			}
		}
	}
}

func TestInlineToolBodyClickDoesNotReopenStaleTarget(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = r.work.close() })
	screen.SetSize(100, 30)
	m := r.model
	m.hydrateHistory([]messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "inspect"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "one", Name: "lookup"}}},
		{Role: messages.MessageRoleTool, ToolCallID: "one", ToolName: "lookup", Content: "output"},
	}, "ctx")
	m.toggleAllDisclosures(100)
	m.toggleAllDisclosures(100)
	r.render()
	link := m.toolOutputLinks[0]
	if link.body.Empty() {
		t.Fatal("expanded row has no clickable body")
	}
	point := link.body.Min
	if !r.toggleToolOutputAt(m, point, nil) {
		t.Fatal("first body click did not collapse the output")
	}
	if r.toggleToolOutputAt(m, point, nil) {
		t.Fatal("a stale body target accepted another toggle before repaint")
	}
	if _, row := m.toolOutputRow(link); row == nil || row.outputExpanded {
		t.Fatal("stale body target reopened the output")
	}
}
