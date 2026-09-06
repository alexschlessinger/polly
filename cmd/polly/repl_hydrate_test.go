package main

import (
	"fmt"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

// The durable display order wins over replay order: rows are matched by call
// ID first, then by tool name when either side lacks an ID, denied calls show
// as denied whatever their row held, and rows the record never mentions keep
// their place at the end. Labels carry argument summaries and never take
// part in the pairing.
func TestHistoryHydratorAppliesDurableToolOrder(t *testing.T) {
	m := newReplModel()
	h := historyHydrator{m: m}
	h.toolRows = []toolDisclosureRow{
		{callID: "b", toolName: "read", label: "read main.go", line: "read-line", settled: true},
		{callID: "", toolName: "bash", label: "bash ls", line: "bash-line", settled: true},
		{callID: "z", toolName: "extra", label: "extra", line: "extra-line", settled: true},
	}
	h.flushTools()

	h.applyToolOrder([]durableDisplayToolCall{
		{ID: "a", Name: "bash"},
		{ID: "b", Name: "read"},
		{ID: "c", Name: "write", Denied: true},
	})

	if h.tools == nil || !h.tools.complete || h.tools.expanded {
		t.Fatalf("disclosure after ordering = %#v", h.tools)
	}
	assertToolRows(t, h.tools.rows,
		[]string{"bash ls", "read main.go", "write", "extra"},
		[]string{"bash-line", "read-line", toolDeniedLine("write"), "extra-line"})
}

// Hydrated rows read like they did live (tool name plus argument summary),
// so the durable record pairs them by tool name: an ID-less allowed call (an
// OpenAI-compatible server may omit IDs) still finds its row. The name
// fallback only bridges a missing ID, so a denied call whose exchange was
// stripped never takes the row of a same-named sibling the record names by
// ID.
func TestHistoryHydratorPairsRowsByToolName(t *testing.T) {
	okLine := toolOKLine("bash ls -la", "", "")
	cases := []struct {
		name       string
		call       messages.ChatMessageToolCall
		order      string
		wantLabels []string
		wantLines  []string
	}{
		{
			name:       "id-less allowed call beside a stripped denied call",
			call:       messages.ChatMessageToolCall{Name: "bash", Arguments: `{"command":"ls -la"}`},
			order:      `[{"name":"bash"},{"id":"x","name":"read","denied":true}]`,
			wantLabels: []string{"bash ls -la", "read"},
			wantLines:  []string{okLine, toolDeniedLine("read")},
		},
		{
			name:       "denied call listed before its same-named sibling",
			call:       messages.ChatMessageToolCall{ID: "a", Name: "bash", Arguments: `{"command":"ls -la"}`},
			order:      `[{"id":"d","name":"bash","denied":true},{"id":"a","name":"bash"}]`,
			wantLabels: []string{"bash", "bash ls -la"},
			wantLines:  []string{toolDeniedLine("bash"), okLine},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := historyHydrator{m: newReplModel()}
			h.assistant(messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{tc.call}})
			result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: tc.call.ID, Content: "ok"}
			result.SetToolSucceeded(true)
			h.tool(result)
			h.internal(messages.ChatMessage{Role: messages.MessageRoleInternal, Metadata: map[string]any{
				messages.MetadataKeyDisplayToolCalls: tc.order,
				messages.MetadataKeyTurnStatus:       messages.TurnStatusToolDenied,
			}})
			if h.tools == nil {
				t.Fatal("no disclosure after ordering")
			}
			assertToolRows(t, h.tools.rows, tc.wantLabels, tc.wantLines)
		})
	}
}

func assertToolRows(t *testing.T, got []toolDisclosureRow, wantLabels, wantLines []string) {
	t.Helper()
	if len(got) != len(wantLabels) {
		t.Fatalf("rows = %d, want %d: %#v", len(got), len(wantLabels), got)
	}
	for i := range got {
		if got[i].label != wantLabels[i] || got[i].line != wantLines[i] || !got[i].settled {
			t.Fatalf("row %d = %#v, want label %q line %q", i, got[i], wantLabels[i], wantLines[i])
		}
	}
}

// A result settles the row with its call ID, or the oldest waiting row when
// the ID matches nothing; a result that arrives before any call opens a row
// named after its tool.
func TestHistoryHydratorSettlesToolResults(t *testing.T) {
	h := historyHydrator{m: newReplModel()}
	h.assistant(messages.ChatMessage{
		Role:      messages.MessageRoleAssistant,
		ToolCalls: []messages.ChatMessageToolCall{{ID: "one", Name: "bash"}, {ID: "two", Name: "read"}},
	})
	h.tool(messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: "two", Content: "ok"})
	if h.toolRows[0].settled || !h.toolRows[1].settled {
		t.Fatalf("result for call two settled the wrong row: %#v", h.toolRows)
	}
	h.tool(messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: "unknown", Content: "ok"})
	if len(h.toolRows) != 2 || !h.toolRows[0].settled {
		t.Fatalf("unmatched result should settle the oldest waiting row: %#v", h.toolRows)
	}

	orphan := historyHydrator{m: newReplModel()}
	orphan.tool(messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: "x", ToolName: "grep", Content: "ok"})
	if len(orphan.toolRows) != 1 || orphan.toolRows[0].label != "grep" || !orphan.toolRows[0].settled {
		t.Fatalf("orphan result rows = %#v", orphan.toolRows)
	}
}

func TestResumedHistoryWindow(t *testing.T) {
	var history []messages.ChatMessage
	for i := 0; i < resumedTurnLimit+3; i++ {
		history = append(history,
			messages.ChatMessage{Role: messages.MessageRoleUser, Content: "q"},
			messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "a"},
		)
	}
	start, total, show := resumedHistoryWindow(history)
	if total != resumedTurnLimit+3 || show != resumedTurnLimit || start != 6 {
		t.Fatalf("window = (start %d, total %d, show %d)", start, total, show)
	}
	want := fmt.Sprintf("resumed context · showing last %d of %d turns", show, total)
	if got := resumedNotice("", total, show); got != want {
		t.Fatalf("notice = %q, want %q", got, want)
	}
	if got := resumedNotice("ctx", 1, 1); got != "resumed ctx · 1 turn" {
		t.Fatalf("single-turn notice = %q", got)
	}
}
