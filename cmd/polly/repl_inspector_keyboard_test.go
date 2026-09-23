package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
)

func TestToolInspectorKeyboardSelectionSurvivesCompletion(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	first := messages.ChatMessageToolCall{ID: "first", Name: "read_file", Arguments: `{"path":"first.go"}`}
	second := messages.ChatMessageToolCall{ID: "second", Name: "read_file", Arguments: `{"path":"second.go"}`}
	r.model.appendToolCallStart(first)
	r.model.appendToolCallStart(second)
	r.inspectCommand("tools")
	waitInspector(t, r, 140)
	key := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}); waitInspector(t, r, 140) }
	key("<Tab>")
	key("<Up>")
	key("<Enter>")
	v := r.workspace().inspector.current
	if !v.model.toolInspector.items[0].expanded || v.model.toolInspector.items[1].expanded {
		t.Fatal("Enter should expand only the selected tool")
	}
	r.model.inspections.finishTool(second, "second output", time.Second, nil)
	waitInspector(t, r, 140)
	key("<Left>")
	key("<Down>")
	key("<Right>")
	v = r.workspace().inspector.current
	if v.model.toolInspector.items[0].expanded || !v.model.toolInspector.items[1].expanded || !strings.Contains(inspectorText(v), "second output") {
		t.Fatal("selection or independent expansion was lost across completion")
	}
}

func TestChangesInspectorKeyboardSelectionSurvivesRefresh(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	report := &fileChanges{tracked: true, changes: []fileChange{
		{path: "main.go", diff: "@@ -1 +1 @@\n-old\n+new"},
		{path: "nested/main.go", diff: "@@ -1 +1 @@\n-before\n+after"},
	}}
	r.model.setWorkspaceChanges(report)
	r.openChangesInspector()
	waitInspector(t, r, 140)
	key := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}); waitInspector(t, r, 140) }
	key("<Tab>")
	if !r.inspectorFocused() {
		t.Fatal("Tab did not focus Changes")
	}
	key("<Down>")
	key("<Enter>")
	list := r.workspace().inspector.current.model.changesInspector
	if list.selected != "nested/main.go" || list.items[0].expanded || !list.items[1].expanded {
		t.Fatalf("wrong selected diff: %+v", list)
	}
	// A refresh can insert a path before the selected row.
	r.model.setWorkspaceChanges(&fileChanges{tracked: true, changes: append([]fileChange{{path: "added.go"}}, report.changes...)})
	waitInspector(t, r, 140)
	key("<Left>")
	list = r.workspace().inspector.current.model.changesInspector
	if list.selected != "nested/main.go" || list.items[2].expanded {
		t.Fatal("refresh lost selected path")
	}
	key("<Up>")
	key("<Right>")
	list = r.workspace().inspector.current.model.changesInspector
	if list.selected != "main.go" || !list.items[1].expanded || list.items[2].expanded {
		t.Fatal("independent diff expansion failed")
	}
	key("<C-o>")
	list = r.workspace().inspector.current.model.changesInspector
	for _, item := range list.items {
		if !item.expanded {
			t.Fatal("Ctrl-O did not expand all diffs")
		}
	}
	key("<Tab>")
	if r.inspectorFocused() {
		t.Fatal("Tab did not return to composer")
	}
}

func TestChangesInspectorKeyboardEmptyAndRemovedSelection(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	r.model.setWorkspaceChanges(&fileChanges{tracked: true, changes: []fileChange{{path: "gone.go"}}})
	r.openChangesInspector()
	waitInspector(t, r, 140)
	r.workspace().inspector.focused = true
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Right>"})
	waitInspector(t, r, 140)
	r.model.setWorkspaceChanges(&fileChanges{tracked: true, changes: []fileChange{{path: "remaining.go"}}})
	waitInspector(t, r, 140)
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	list := waitInspector(t, r, 140).model.changesInspector
	if list.selected != "remaining.go" || !list.items[0].expanded {
		t.Fatal("removed selection did not fall back to remaining file")
	}
	r.model.setWorkspaceChanges(&fileChanges{tracked: true})
	waitInspector(t, r, 140)
	r.model.ed.setText("keep this draft")
	for _, key := range []string{"<Up>", "<Down>", "<Left>", "<Right>", "<Enter>"} {
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: key})
	}
	if r.model.ed.text() != "keep this draft" {
		t.Fatal("empty inspector consumed composer draft")
	}
}

func TestChangesInspectorKeyboardRapidSelection(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	r.model.setWorkspaceChanges(&fileChanges{tracked: true, changes: []fileChange{{path: "one"}, {path: "two"}, {path: "three"}}})
	r.openChangesInspector()
	waitInspector(t, r, 140)
	r.workspace().inspector.focused = true
	// Input can arrive before the asynchronous projection catches up.
	for _, key := range []string{"<Down>", "<Down>", "<Enter>"} {
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: key})
	}
	list := waitInspector(t, r, 140).model.changesInspector
	if list.selected != "three" || !list.items[2].expanded || list.items[1].expanded {
		t.Fatalf("rapid navigation lost selection: %+v", list)
	}
}

func TestInspectorTabFocusPreservesDraft(t *testing.T) {
	for _, kind := range []string{"tools", "changes"} {
		t.Run(kind, func(t *testing.T) {
			r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
			r.model.appendToolCallStart(messages.ChatMessageToolCall{ID: "read", Name: "read_file", Arguments: `{ "path": "main.go" }`})
			r.model.setWorkspaceChanges(&fileChanges{tracked: true, changes: []fileChange{{path: "main.go"}}})
			r.inspectCommand(kind)
			waitInspector(t, r, 140)
			for _, draft := range []string{"unfinished prompt", "/inspect"} {
				r.model.ed.setText(draft)
				r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Tab>"})
				if !r.inspectorFocused() || r.model.ed.text() != draft {
					t.Fatal("Tab did not focus inspector while preserving draft")
				}
				r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Tab>"})
				if r.inspectorFocused() || r.model.ed.text() != draft {
					t.Fatal("Tab did not return to composer while preserving draft")
				}
			}
		})
	}
}

func TestEveryInspectorKeyboardActions(t *testing.T) {
	withDisplayTTY(t)
	fixture, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = fixture.work.close() })
	for _, kind := range []viewKind{conversationViewKind, toolViewKind, thoughtViewKind, swarmViewKind, agentsViewKind, changesViewKind} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
			r.setupWidgets()
			r.showTab(0)
			r.model.appendThinking("A thought to inspect.")
			r.model.appendToolCallStart(messages.ChatMessageToolCall{ID: "call", Name: "read_file", Arguments: `{"path":"main.go"}`})
			r.model.setWorkspaceChanges(&fileChanges{tracked: true, changes: []fileChange{{path: "main.go"}}})
			target := tabViewTarget(r.visibleTab())
			target.kind = kind
			if kind == thoughtViewKind {
				target.item = r.model.inspections.thoughts[0].key
			}
			if kind == toolViewKind {
				target.item = r.model.inspections.tools[0].key
			}
			r.inspect(target)
			screen.SetSize(140, 40)
			waitInspector(t, r, 140)
			r.render()
			r.model.ed.setText("preserve draft")
			key := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}); r.render() }
			key("<Tab>")
			event, ok := headlessKeyEvent("s-tab")
			if !ok {
				t.Fatal("missing replay key")
			}
			r.handleEvent(event)
			r.render()
			action, ok := r.selectedInspectorAction()
			if !ok || action.key != "button:parent" {
				t.Fatalf("first action: %+v %v", action, ok)
			}
			if underlinedRun(t, screen, action.rect.Min.Y) == "" {
				t.Fatal("keyboard action is not visibly marked")
			}
			// Selection follows the action through a split-to-full-width resize.
			screen.SetSize(80, 24)
			waitInspector(t, r, 80)
			r.render()
			action, ok = r.selectedInspectorAction()
			if !ok || action.key != "button:parent" || !action.rect.In(r.chrome.inner) {
				t.Fatal("resize lost action or retained stale geometry")
			}
			key("<Enter>")
			if r.workspace().inspector.open || r.model.ed.text() != "preserve draft" {
				t.Fatal("parent action failed or touched the draft")
			}
		})
	}
}

func TestInspectorKeyboardConversationDisclosuresAndLinks(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = r.work.close() })
	screen.SetSize(140, 40)
	r.model.appendToolCallStart(messages.ChatMessageToolCall{ID: "read", Name: "read_file", Arguments: `{"path":"main.go"}`})
	r.inspect(tabViewTarget(r.visibleTab()))
	waitInspector(t, r, 140)
	r.render()
	r.workspace().inspector.focused = true
	choose := func(prefix string) {
		t.Helper()
		for range 30 {
			r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<S-Tab>"})
			r.render()
			if action, ok := r.selectedInspectorAction(); ok && strings.HasPrefix(action.key, prefix) {
				return
			}
		}
		t.Fatalf("no keyboard action matching %q: %+v", prefix, r.inspectorKeyboardActions())
	}
	choose("disclosure:")
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	waitInspector(t, r, 140)
	r.render()
	choose("inspection:")
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	waitInspector(t, r, 140)
	if r.workspace().inspector.target.kind != toolViewKind {
		t.Fatal("conversation tool link did not open Tools")
	}
}

// A keyboard-selected open thought is marked on every row it wraps to, as
// hovering it is.
func TestInspectorKeyboardMarksEveryRowOfAnOpenThought(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = r.work.close() })
	screen.SetSize(140, 40)
	r.model.appendThinking("first line of thought\nsecond line of thought\nthird line of thought")
	r.inspect(tabViewTarget(r.visibleTab()))
	waitInspector(t, r, 140)
	r.render()
	r.workspace().inspector.focused = true
	choose := func(prefix string) inspectorKeyboardAction {
		t.Helper()
		for range 30 {
			r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<S-Tab>"})
			r.render()
			if action, ok := r.selectedInspectorAction(); ok && strings.HasPrefix(action.key, prefix) {
				return action
			}
		}
		t.Fatalf("no keyboard action matching %q: %+v", prefix, r.inspectorKeyboardActions())
		return inspectorKeyboardAction{}
	}
	choose(fmt.Sprintf("disclosure:%d:", activityThought))
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	waitInspector(t, r, 140)
	r.render()
	action := choose(fmt.Sprintf("inspection:%d:", thoughtViewKind))
	frame := screenSnapshot(t, screen)
	for i, want := range []string{"first", "second", "third"} {
		if got := frameUnderlinedRun(frame, action.rect.Min.Y+i); !strings.Contains(got, want+" line of thought") {
			t.Fatalf("thought row %d underline = %q", i, got)
		}
	}
}

func TestInspectorKeyboardMissingActionDoesNotActivateAnother(t *testing.T) {
	withDisplayTTY(t)
	fixture, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = fixture.work.close() })
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root", "child")
	r.setupWidgets()
	r.showTab(0)
	child := r.tabs[1]
	child.model.busy = true
	r.inspect(tabViewTarget(child))
	screen.SetSize(140, 40)
	waitInspector(t, r, 140)
	r.render()
	r.workspace().inspector.focused = true
	for range 2 {
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<S-Tab>"})
		r.render()
	}
	if action, ok := r.selectedInspectorAction(); !ok || action.key != "button:stop" {
		t.Fatal("Stop was not keyboard accessible")
	}
	child.model.busy = false
	r.render()
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	if !r.workspace().inspector.open || len(r.workspaceActions) != 0 {
		t.Fatal("missing Stop action activated another control")
	}
}

func TestInspectorSwarmSectionsAndAgentReturn(t *testing.T) {
	r, ids := agentsInspectorFixture(t)
	r.openAgentsInspector()
	waitInspector(t, r, 140)
	r.workspace().inspector.focused = true
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Right>"})
	waitInspector(t, r, 140)
	if r.workspace().inspector.target.session.ID != ids[0] {
		t.Fatal("Right did not open the first agent")
	}
	// Navigation must work even while a root approval is waiting.
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Left>"})
	if r.workspace().inspector.target.kind != agentsViewKind {
		t.Fatal("Left did not return to Agents")
	}
	r.inspectorAction("swarm_members")
	for _, want := range swarmInspectorSections[1:] {
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Right>"})
		if got := r.workspace().inspector.target.item; got != want {
			t.Fatalf("section=%q want %q", got, want)
		}
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Left>"})
	if r.workspace().inspector.target.item != "previews" {
		t.Fatal("Left did not return to previous section")
	}
}

func TestReadOnlyInspectorKeepsKeysDuringApproval(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	r.model.appendThinking(strings.Repeat("thought\n", 50))
	r.inspectCommand("thoughts")
	waitInspector(t, r, 140)
	i := &r.workspace().inspector
	i.focused = true
	r.model.ed.setText("draft")
	r.model.approval = &approvalState{}
	state := r.workspace().viewState(i.target)
	state.top = 10
	state.follow = false
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Home>"})
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	if state.top != 0 || r.model.approval == nil || r.model.ed.text() != "draft" {
		t.Fatal("inspector keys reached the waiting approval or draft")
	}
}
