package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	rw "github.com/mattn/go-runewidth"
)

func TestInlineBashFitsPaneThroughCompletionResizeAndReload(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("run tests")
	command := "cd /workspace/polly && GOCACHE=/tmp/polly-cache go test ./cmd/polly -run TestExpandedToolDisclosure -count=1 | tail -40"
	args, _ := json.Marshal(map[string]string{"command": command})
	call := messages.ChatMessageToolCall{ID: "test", Name: "bash", Arguments: string(args)}
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	record := m.currentToolDisclosure()
	m.toggleToolDisclosure(record.id)
	assertRows := func(model *replModel, glyph string) {
		t.Helper()
		for _, width := range []int{24, 48, 80, 160, 32, 160} {
			var detail string
			for _, block := range model.transcriptDisplayEntries(width) {
				if len(block.toolDisclosureIDs) > 0 {
					detail = plainStyledText(inlineActivityDetail(block.text))
				}
			}
			if strings.Count(detail, "\n") != 0 || rw.StringWidth(detail) > width || !strings.Contains(detail, glyph+" $ ") {
				t.Fatalf("width %d should show one Bash row: %q", width, detail)
			}
			if !strings.HasSuffix(detail, "s") {
				t.Fatalf("width %d lost duration: %q", width, detail)
			}
			// Exercise the actual wrapped transcript as well as the projection.
			found := 0
			for _, line := range transcriptRowsText(model.transcriptRows(width)) {
				if strings.Contains(line, glyph+" $ ") {
					found++
					if !strings.HasSuffix(strings.TrimSpace(line), "s") {
						t.Fatalf("duration wrapped off row at %d: %q", width, line)
					}
				}
			}
			if found != 1 {
				t.Fatalf("width %d has %d physical Bash rows", width, found)
			}
		}
	}
	assertRows(m, "→")
	tui.AppendToolEnd(call, strings.Repeat("output\n", 28), 1200*time.Millisecond, nil)
	assertRows(m, "✓")
	wide := toolDisclosureTextAtWidth(record, 200)
	narrow := toolDisclosureTextAtWidth(record, 48)
	if !strings.Contains(plainStyledText(wide), "28 lines") || strings.Contains(plainStyledText(narrow), "28 lines") {
		t.Fatalf("output counts did not yield to command: wide=%q narrow=%q", wide, narrow)
	}
	if got := m.displayCatalog.toolForCall(call.ID).call.Arguments; got != call.Arguments {
		t.Fatalf("inline rendering changed inspector arguments: %q", got)
	}
	result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: "bash", Content: "output"}
	result.SetToolSucceeded(true)
	result.SetToolDuration(1200 * time.Millisecond)
	reloaded := newReplModel()
	reloaded.hydrateHistory([]messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "run tests"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}},
		result,
		{Role: messages.MessageRoleAssistant, Content: "done"},
	}, "ctx")
	for _, record := range reloaded.toolDisclosures.all() {
		reloaded.toggleToolDisclosure(record.id)
	}
	assertRows(reloaded, "✓")
}

func TestInlineBashRetainsFailureAndLiteralSyntax(t *testing.T) {
	call := messages.ChatMessageToolCall{Name: "bash", Arguments: `{"command":"grep '[' cmd/polly/repl_inline_tool.go"}`}
	row := toolDisclosureRow{label: toolLabel(call)}
	row.setCall(call)
	for _, meta := range []string{"exit 2", "denied", "canceled"} {
		row.setLine(inlineToolLine{glyph: "✗", tone: "err", modifier: "bold", meta: meta, duration: "1.2s"})
		for width := 1; width < 100; width++ {
			line := row.inlineLine(width)
			plain := plainStyledText(line)
			if rw.StringWidth(plain) > width || strings.Contains(plain, "fg:") || strings.Contains(plain, "mod:") {
				t.Fatalf("width %d invalid styled row: %q", width, plain)
			}
			if width >= 32 && (!strings.Contains(plain, meta) || !strings.Contains(plain, "1.2s")) {
				t.Fatalf("width %d lost failure metadata: %q", width, plain)
			}
		}
		if got := plainStyledText(row.inlineLine(100)); !strings.Contains(got, "grep '['") {
			t.Fatalf("literal bracket changed: %q", got)
		}
	}
	// The canonical transcript still carries the same safe literal styling.
	if got := plainStyledText(row.line); !strings.Contains(got, "bash grep '['") || strings.Contains(got, "fg:") {
		t.Fatalf("canonical row changed: %q", got)
	}
}
