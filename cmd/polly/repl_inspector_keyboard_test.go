package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
)

func TestInspectorTabFocusPreservesDraft(t *testing.T) {
	for _, kind := range []string{"conversation", "agents"} {
		t.Run(kind, func(t *testing.T) {
			r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
			r.model.appendToolCallStart(messages.ChatMessageToolCall{ID: "read", Name: "read_file", Arguments: `{ "path": "main.go" }`})
			r.model.setWorkspaceChanges(&fileChanges{tracked: true, changes: []fileChange{{path: "main.go"}}})
			if kind == "agents" {
				r.openAgentsInspector()
			} else {
				r.inspect(tabViewTarget(r.visibleTab()))
			}
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
	for _, kind := range []viewKind{conversationViewKind, swarmViewKind, agentsViewKind} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
			r.setupWidgets()
			r.showTab(0)
			r.model.appendThinking("A thought to inspect.")
			r.model.appendToolCallStart(messages.ChatMessageToolCall{ID: "call", Name: "read_file", Arguments: `{"path":"main.go"}`})
			r.model.setWorkspaceChanges(&fileChanges{tracked: true, changes: []fileChange{{path: "main.go"}}})
			target := tabViewTarget(r.visibleTab())
			target.kind = kind
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
			actions := r.inspectorKeyboardActions()
			r.handleEvent(event)
			r.render()
			// The close button lives on the frame, outside the key cycle; a
			// view with nothing to act on selects nothing and Esc closes it.
			if len(actions) == 0 {
				if _, ok := r.selectedInspectorAction(); ok {
					t.Fatal("selected an action in a view without any")
				}
				// The first Esc hands the keys back; the second closes.
				key("<Escape>")
				key("<Escape>")
				if r.workspace().inspector.open || r.model.ed.text() != "preserve draft" {
					t.Fatal("Esc did not close or touched the draft")
				}
				return
			}
			first := actions[0].key
			if kind == conversationViewKind && first != "button:parent" {
				t.Fatalf("conversation's first action = %q, want its title", first)
			}
			action, ok := r.selectedInspectorAction()
			if !ok || action.key != first {
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
			if !ok || action.key != first || !action.rect.In(r.chrome.inner) {
				t.Fatal("resize lost action or retained stale geometry")
			}
			key("<Enter>")
			if r.model.ed.text() != "preserve draft" {
				t.Fatal("first action touched the draft")
			}
			if kind == conversationViewKind && r.workspace().inspector.open {
				t.Fatal("the root conversation's title did not close the inspector")
			}
			key("<Escape>")
			key("<Escape>")
			if r.workspace().inspector.open {
				t.Fatal("Esc did not close the inspector")
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

}

// A keyboard-selected open thought is marked on every row it wraps to, as
// hovering it is.

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
	// The title, then the status row's Stop.
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
	r.inspect(tabViewTarget(r.visibleTab()))
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
