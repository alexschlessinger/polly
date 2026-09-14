package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func TestToolListIndependentSectionsAndLiveCompletion(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	first := messages.ChatMessageToolCall{ID: "first", Name: "bash", Arguments: `{"command":"cd /private/tmp && echo first"}`}
	second := messages.ChatMessageToolCall{ID: "second", Name: "read_file", Arguments: `{"path":"second.go"}`}
	r.model.appendToolCallStart(first)
	r.model.appendToolCallStart(second)
	r.inspectCommand("tools")
	v := waitInspector(t, r, 140)
	keys := []string{r.model.inspections.tools[0].key, r.model.inspections.tools[1].key}
	for _, action := range []string{toolInspectorBlock(keys[0], "setup"), toolInspectorBlock(keys[0], "command"), toolInspectorBlock(keys[1], "arguments"), toolInspectorBlock(keys[1], "output")} {
		r.inspectorAction(action)
	}
	v = waitInspector(t, r, 140)
	text := inspectorText(v)
	for _, want := range []string{"cd /private/tmp", "echo first", "second.go", "Running…"} {
		if !strings.Contains(text, want) {
			t.Fatalf("independent sections lost %q: %s", want, text)
		}
	}
	if v.model.toolInspector.items[0].sections.output {
		t.Fatal("opening another tool's output opened the first tool")
	}
	// Completing a tool other than the clicked target updates its own row,
	// while both tools retain their independent disclosure state.
	r.model.inspections.finishTool(first, "first failure details", 2*time.Second, fmt.Errorf("failed"))
	r.model.inspections.finishTool(second, "second contents", time.Second, nil)
	v = waitInspector(t, r, 140)
	text = inspectorText(v)
	for _, want := range []string{"failed · 2.0s", "completed · 1.0s", "second contents", "echo first"} {
		if !strings.Contains(text, want) {
			t.Fatalf("completion lost %q: %s", want, text)
		}
	}
	if strings.Contains(text, "first failure details") || strings.Contains(text, "Running…") {
		t.Fatal("failure auto-expanded or completed output stayed pending")
	}
	r.inspectorAction(toolInspectorBlock(keys[0], "command"))
	v = waitInspector(t, r, 140)
	if text = inspectorText(v); strings.Contains(text, "echo first") || !strings.Contains(text, "cd /private/tmp") || !strings.Contains(text, "second contents") {
		t.Fatalf("collapsing command changed another section: %s", text)
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
	r.inspectorAction(toolInspectorBlock(first, "output"))
	r.inspectorAction(toolInspectorBlock(lastKey, "output"))
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
	if !strings.Contains(text, "running · 12.") {
		t.Fatalf("clock did not refresh in the cached list: %s", text)
	}
	if next := waitInspector(t, r, 140); next.model != m {
		t.Fatal("clock-only update rebuilt the projection")
	}
}
