package main

import (
	"fmt"
	"image"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	ui "github.com/metaspartan/gotui/v5"
)

func TestToolAndThoughtDetailsCannotOpenPanel(t *testing.T) {
	withDisplayTTY(t)
	for _, source := range []string{"live", "saved", "saved-agent"} {
		t.Run(source, func(t *testing.T) {
			r, screen := affordanceTestREPL(t)
			t.Cleanup(func() { _ = r.work.close() })
			m := r.model
			call := messages.ChatMessageToolCall{ID: "read", Name: "read_file", Arguments: `{"path":"replay-marker.go"}`}
			history := []messages.ChatMessage{
				{Role: messages.MessageRoleUser, Content: "review this code"},
				{Role: messages.MessageRoleAssistant, Reasoning: "reasoning-marker checks the code", ToolCalls: []messages.ChatMessageToolCall{call}},
				{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: "file contents"},
				{Role: messages.MessageRoleAssistant, Content: "done"},
			}
			switch source {
			case "live":
				m.beginTurn("review this code")
				tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
				tui.ShowThinking(history[1].Reasoning)
				tui.AppendToolStart([]messages.ChatMessageToolCall{call})
				tui.AppendToolEnd(call, "file contents", time.Second, nil)
				tui.AppendAssistantText("done")
				r.endTurn(nil)
			case "saved":
				m.hydrateHistory(history, "replayed")
			case "saved-agent":
				m = prepareChildDisplay(&sessions.SessionView{Metadata: &sessions.Metadata{Name: "replayed-agent"}, History: history}, r.config, 140)
				r.model, r.visibleTab().model = m, m
			}
			m.toggleAllDisclosures(140)
			for _, width := range []int{140, 80, 180} {
				screen.SetSize(width, 40)
				r.render()
				for _, marker := range []string{"reasoning-marker", "replay-marker.go"} {
					point := image.Pt(-1, -1)
					for y := 0; y < 38; y++ {
						var row strings.Builder
						for x := 0; x < width; x++ {
							cell, _, _ := screen.Get(x, y)
							if cell == "" {
								cell = " "
							}
							row.WriteString(cell)
						}
						// The activity line names the latest call too; the
						// detail rows under it are the ones in question.
						if x := strings.Index(row.String(), marker); x >= 0 && !strings.ContainsAny(row.String(), "▸▾") {
							point = image.Pt(x+1, y)
							break
						}
					}
					if point.X < 0 {
						t.Fatalf("width %d did not render %s", width, marker)
					}
					hoverAt(t, r, point)
					if marker == "reasoning-marker" && !r.hover.rect.Empty() {
						t.Fatalf("width %d retained hover for %s: %v", width, marker, r.hover.rect)
					}
					r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: point.X, Y: point.Y}})
					if r.workspace().inspector.open {
						t.Fatalf("width %d clicking %s opened panel", width, marker)
					}
				}
			}
		})
	}
}

func TestInspectorCommandIsUnregistered(t *testing.T) {
	for _, arg := range []string{"", " tools", " thoughts", " changes", " find", " maximize"} {
		handled, _, err := defaultReplCommands.dispatch("/inspect"+arg, &replCommandContext{registry: defaultReplCommands})
		if handled || err != nil {
			t.Fatalf("%s still registered: handled=%v err=%v", fmt.Sprintf("/inspect%s", arg), handled, err)
		}
	}
}

func TestAgentsStatusTogglesOpenPanel(t *testing.T) {
	withDisplayTTY(t)
	for _, approval := range []bool{false, true} {
		t.Run(fmt.Sprintf("approval=%v", approval), func(t *testing.T) {
			fixture, screen := affordanceTestREPL(t)
			t.Cleanup(func() { _ = fixture.work.close() })
			r, ids := agentsInspectorFixture(t)
			if approval {
				r.model.approval.calls = []messages.ChatMessageToolCall{{ID: "pending", Name: "read_file"}}
			} else {
				r.model.approval = nil
			}
			pending := r.model.approval
			r.setupWidgets()
			r.model.ed.setText("preserve draft")
			screen.SetSize(160, 40)
			clickBadge := func() {
				t.Helper()
				r.render()
				field := r.model.status.agentsField
				if field.Cols == 0 {
					t.Fatal("agents status has no click target")
				}
				r.handleEvent(mouseEvent("<MouseLeft>", image.Pt(field.X, 39)))
			}
			clickBadge()
			w := r.workspace()
			if !w.inspector.open || w.inspector.target.kind != agentsViewKind {
				t.Fatal("first status click did not open Agents")
			}
			waitInspector(t, r, 160)
			state := w.viewState(w.inspector.target)
			state.agents.selected = ids[1]
			w.inspector.focused, w.inspector.searching = true, true
			w.inspector.keyboardAction = "button:agents-open:" + ids[1]
			clickBadge()
			if w.inspector.open || w.inspector.focused || w.inspector.searching || w.inspector.current != nil || w.inspector.keyboardAction != "" {
				t.Fatal("second status click did not fully close Agents")
			}
			clickBadge()
			if !w.inspector.open || w.inspector.target.kind != agentsViewKind {
				t.Fatal("third status click did not reopen Agents")
			}
			waitInspector(t, r, 160)
			if w.viewState(w.inspector.target).agents.selected != ids[1] {
				t.Fatal("reopening Agents lost the selected agent")
			}
			if r.model.ed.text() != "preserve draft" || r.model.approval != pending || r.model.modal != nil {
				t.Fatal("toggling Agents changed the draft, approval, or modal")
			}
		})
	}
}

func TestAgentsStatusStillOpensPanel(t *testing.T) {
	withDisplayTTY(t)
	fixture, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = fixture.work.close() })
	r, ids := agentsInspectorFixture(t)
	r.model.approval = nil
	r.setupWidgets()
	r.model.ed.setText("preserve draft")
	screen.SetSize(160, 40)
	r.render()
	field := r.model.status.agentsField
	if field.Cols == 0 {
		t.Fatal("agents status has no click target")
	}
	r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: field.X, Y: 39}})
	if !r.workspace().inspector.open || r.workspace().inspector.target.kind != agentsViewKind {
		t.Fatal("agents status did not open the panel")
	}
	waitInspector(t, r, 160)
	r.inspectorAction("agents-open:" + ids[0])
	waitInspector(t, r, 160)
	if r.workspace().inspector.target.session.ID != ids[0] || r.model.ed.text() != "preserve draft" {
		t.Fatal("agent did not open or changed the draft")
	}
}
