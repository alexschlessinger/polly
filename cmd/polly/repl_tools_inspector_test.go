package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	ui "github.com/metaspartan/gotui/v5"
)

func TestToolListIndependentExpansionAndLiveCompletion(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	first := messages.ChatMessageToolCall{ID: "first", Name: "bash", Arguments: `{"command":"cd /private/tmp && echo first"}`}
	second := messages.ChatMessageToolCall{ID: "second", Name: "read_file", Arguments: `{"path":"second.go"}`}
	r.model.appendToolCallStart(first)
	r.model.appendToolCallStart(second)
	r.inspectCommand("tools")
	v := waitInspector(t, r, 140)
	keys := []string{r.model.inspections.tools[0].key, r.model.inspections.tools[1].key}
	if text := inspectorText(v); !strings.Contains(text, "echo first") || !strings.Contains(text, "second.go") || strings.Contains(text, "output") {
		t.Fatalf("missing compact previews: %s", text)
	}
	r.inspectorAction(toolInspectorBlock(keys[1], "title"))
	v = waitInspector(t, r, 140)
	blocks := v.model.toolInspector.blocks(80)
	if len(blocks) < 2 || blocks[0].key != toolInspectorBlock(keys[0], "title") || blocks[1].key != toolInspectorBlock(keys[1], "title") {
		t.Fatal("opening a call inserted spacing above its preview")
	}
	text := inspectorText(v)
	for _, want := range []string{"echo first", "second.go", "Running…"} {
		if !strings.Contains(text, want) {
			t.Fatalf("independent expansion lost %q: %s", want, text)
		}
	}
	if v.model.toolInspector.items[0].expanded {
		t.Fatal("opening another tool opened the first tool")
	}
	// Completing a tool other than the clicked target updates its own row,
	// while both tools retain their independent disclosure state.
	r.model.inspections.finishTool(first, "first failure details", 2*time.Second, fmt.Errorf("failed"))
	r.model.inspections.finishTool(second, "second contents", time.Second, nil)
	v = waitInspector(t, r, 140)
	text = inspectorText(v)
	for _, want := range []string{"failed · 2.0s", "1.0s", "second contents", "echo first"} {
		if !strings.Contains(text, want) {
			t.Fatalf("completion lost %q: %s", want, text)
		}
	}
	if strings.Contains(text, "first failure details") || strings.Contains(text, "Running…") {
		t.Fatal("failure auto-expanded or completed output stayed pending")
	}
	r.inspectorAction(toolInspectorBlock(keys[0], "title"))
	v = waitInspector(t, r, 140)
	if text = inspectorText(v); !strings.Contains(text, "first failure details") || !strings.Contains(text, "cd /private/tmp") || !strings.Contains(text, "second contents") {
		t.Fatalf("opening first call changed another: %s", text)
	}
	r.inspectorAction(toolInspectorBlock(keys[0], "title"))
	v = waitInspector(t, r, 140)
	if text = inspectorText(v); strings.Contains(text, "first failure details") || !strings.Contains(text, "second contents") {
		t.Fatalf("collapsing first call changed another: %s", text)
	}
}

func TestToolListCtrlOOpensAndClosesEveryCall(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	first := messages.ChatMessageToolCall{ID: "first", Name: "bash", Arguments: `{"command":"echo first"}`}
	second := messages.ChatMessageToolCall{ID: "second", Name: "read_file", Arguments: `{"path":"second.go"}`}
	r.model.appendToolCallStart(first)
	r.model.appendToolCallStart(second)
	r.model.inspections.finishTool(first, "first output", time.Second, nil)
	r.model.inspections.finishTool(second, "second output", time.Second, nil)
	r.inspectCommand("tools")
	v := waitInspector(t, r, 140)
	if text := inspectorText(v); strings.Contains(text, "first output") || strings.Contains(text, "second output") {
		t.Fatalf("tool list opened with every call expanded: %s", text)
	}
	// The shortcut addresses the focused view: the tools list, not the
	// conversation beside it.
	r.workspace().inspector.focused = true
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if v = waitInspector(t, r, 140); !v.model.toolInspector.items[0].expanded || !v.model.toolInspector.items[1].expanded {
		t.Fatal("Ctrl-O did not open every call")
	}
	if text := inspectorText(v); !strings.Contains(text, "first output") || !strings.Contains(text, "second output") {
		t.Fatalf("expanding every call lost output: %s", text)
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if v = waitInspector(t, r, 140); v.model.toolInspector.items[0].expanded || v.model.toolInspector.items[1].expanded {
		t.Fatal("second Ctrl-O did not close every call")
	}
	if text := inspectorText(v); strings.Contains(text, "first output") || strings.Contains(text, "second output") {
		t.Fatalf("collapsing every call kept output: %s", text)
	}
}

// The shortcut's expansion is sticky in the tools list too: calls that appear
// in the inspected conversation after the press open with the list, until the
// press that closes everything clears it.
func TestToolListCtrlOHoldsExpansionForLaterCalls(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	first := messages.ChatMessageToolCall{ID: "first", Name: "bash", Arguments: `{"command":"echo first"}`}
	r.model.appendToolCallStart(first)
	r.model.inspections.finishTool(first, "first output", time.Second, nil)
	r.inspectCommand("tools")
	v := waitInspector(t, r, 140)
	r.workspace().inspector.focused = true
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if v = waitInspector(t, r, 140); !v.model.toolInspector.items[0].expanded {
		t.Fatal("Ctrl-O did not open the call")
	}
	// A call that arrives later opens with the list.
	second := messages.ChatMessageToolCall{ID: "second", Name: "read_file", Arguments: `{"path":"second.go"}`}
	r.model.appendToolCallStart(second)
	r.model.inspections.finishTool(second, "second output", time.Second, nil)
	v = waitInspector(t, r, 140)
	if len(v.model.toolInspector.items) < 2 || !v.model.toolInspector.items[1].expanded {
		t.Fatalf("later call did not inherit the sticky expansion: %#v", v.model.toolInspector.items)
	}
	// The closing press clears the hold: the next arrival stays closed.
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	v = waitInspector(t, r, 140)
	if v.model.toolInspector.items[0].expanded || v.model.toolInspector.items[1].expanded {
		t.Fatal("second Ctrl-O did not close every call")
	}
	third := messages.ChatMessageToolCall{ID: "third", Name: "bash", Arguments: `{"command":"echo third"}`}
	r.model.appendToolCallStart(third)
	r.model.inspections.finishTool(third, "third output", time.Second, nil)
	v = waitInspector(t, r, 140)
	if v.model.toolInspector.items[2].expanded {
		t.Fatal("call after the closing press inherited an expansion")
	}
}

func TestSavedToolListSpansWholeConversation(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	saved := testAcquireSession(t, store, "saved-tools")
	defer saved.Close()
	var history []messages.ChatMessage
	for n := 0; n < 8; n++ {
		call := messages.ChatMessageToolCall{ID: "repeated", Name: fmt.Sprintf("tool_%d", n), Arguments: fmt.Sprintf(`{"step":%d}`, n)}
		result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: fmt.Sprintf("result %d", n)}
		result.SetToolDuration(time.Second)
		history = append(history, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "next"}, messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}}, result)
	}
	testAddMessages(t, saved, history)
	r.inspect(viewTarget{session: sessions.ViewTarget{Name: "saved-tools"}, kind: toolViewKind, item: "tool:8:repeated"})
	v := waitInspector(t, r, 140)
	if len(v.model.toolInspector.items) != 8 {
		t.Fatal("tool list inherited the transcript's bounded turn window")
	}
	text := inspectorText(v)
	last := -1
	for n := 0; n < 8; n++ {
		at := strings.Index(text, fmt.Sprintf("tool_%d", n))
		if at <= last {
			t.Fatal("saved tool ordering changed")
		}
		last = at
	}
	first, lastKey := v.model.inspections.tools[0].key, v.model.inspections.tools[7].key
	r.inspectorAction(toolInspectorBlock(first, "title"))
	r.inspectorAction(toolInspectorBlock(lastKey, "title"))
	v = waitInspector(t, r, 140)
	text = inspectorText(v)
	if !strings.Contains(text, "result 0") || !strings.Contains(text, "result 7") || strings.Contains(text, "result 1") {
		t.Fatalf("repeated call IDs mixed section state or output: %s", text)
	}
	if len(r.tabs) != 1 || r.visibleTab().name != "root" {
		t.Fatal("saved inspection created or switched an execution tab")
	}
}

func TestToolListClockTicksWithoutReprojection(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	r.model.appendToolCallStart(messages.ChatMessageToolCall{ID: "clock", Name: "bash"})
	r.inspectCommand("tools")
	v := waitInspector(t, r, 140)
	m := v.model
	m.toolInspector.items[0].tool.started = time.Now().Add(-12 * time.Second)
	m.toolInspectorTick = 0
	text := strings.Join(transcriptRowsText(v.view.Rows(m, 80)), "\n")
	if !strings.Contains(text, "12.") {
		t.Fatalf("clock did not refresh in the cached list: %s", text)
	}
	if next := waitInspector(t, r, 140); next.model != m {
		t.Fatal("clock-only update rebuilt the projection")
	}
}
