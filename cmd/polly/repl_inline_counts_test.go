package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

func TestInlineActivityCountsMatchWindowedAndRevealedCalls(t *testing.T) {
	withDisplayTTY(t)
	var calls []messages.ChatMessageToolCall
	add := func(name, args string) {
		calls = append(calls, messages.ChatMessageToolCall{ID: fmt.Sprint(len(calls)), Name: name, Arguments: args})
	}
	// Forty calls, but only fifteen distinct read paths and four edited
	// paths. Each shell call contains several commands and still owns one row.
	for i := range 25 {
		add("read_file", fmt.Sprintf(`{"path":"file-%02d.go"}`, i%15))
	}
	for i := range 5 {
		add("edit_file", fmt.Sprintf(`{"path":"file-%02d.go","old_string":"old","new_string":"new"}`, i%4))
	}
	for range 9 {
		add("bash", `{"command":"cd staging && go build ./... && go test ./..."}`)
	}
	add("inspect", `{}`)

	for _, source := range []string{"live", "history"} {
		t.Run(source, func(t *testing.T) {
			const width = 200
			r := newManagedREPL(&Config{}, "ctx", 0, 0)
			m := r.model
			m.depth = depthCollapsed // the checks below open a folded line by hand
			if source == "live" {
				m.beginTurn("inspect")
				tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
				tui.AppendToolStart(calls)
				for _, call := range calls {
					tui.AppendToolEnd(call, "ok", time.Millisecond, nil)
				}
				tui.AppendAssistantText("Done.")
				r.endTurn(nil)
			} else {
				history := []messages.ChatMessage{
					{Role: messages.MessageRoleUser, Content: "inspect"},
					{Role: messages.MessageRoleAssistant, ToolCalls: calls},
				}
				for _, call := range calls {
					result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: "ok"}
					result.SetToolSucceeded(true)
					history = append(history, result)
				}
				history = append(history, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "Done."})
				m.hydrateHistory(history, "ctx")
			}

			checkHeader := func() transcriptDisplayBlock {
				t.Helper()
				blocks := activityBlocks(m, width)
				if len(blocks) != 1 {
					t.Fatalf("activity blocks = %d, want 1", len(blocks))
				}
				lines := strings.Split(plainStyledText(blocks[0].text), "\n")
				want := "40 tool calls · edited 4 files"
				if !strings.Contains(lines[0], want) || strings.Contains(lines[0], "inspect") {
					t.Fatalf("activity header = %q, want %q", lines[0], want)
				}
				// A folded line names its latest call on the row under it; an
				// open one leaves it to its rows.
				record := m.toolDisclosures.get(blocks[0].toolDisclosureIDs[0])
				if call := len(lines) > 1 && strings.TrimSpace(lines[1]) == "✓ inspect"; call == record.expanded {
					t.Fatalf("latest call row shown=%v while expanded=%v: %q", call, record.expanded, lines)
				}
				return blocks[0]
			}
			block := checkHeader()
			m.toggleDisclosureGroup(activityTools, block.toolDisclosureIDs, width)
			block = checkHeader()
			// One run of forty calls shows its newest activityRunRows, the
			// first marked as cut, its rail the cut's control ahead of the
			// row's own target.
			targets := block.activityToolTargets
			if detail := plainStyledText(block.activityToolDetail); strings.Contains(detail, "earlier") || !strings.HasPrefix(detail, reasoningCutIndent) || len(targets) != activityRunRows+1 {
				t.Fatalf("forty calls want the newest %d visible, the first marked: %q / %+v", activityRunRows, detail, targets)
			}
			if cut := targets[0]; cut.action != openCutAction || cut.line != 0 || cut.x != -activityRailCols || cut.cols != activityRailCols {
				t.Fatalf("the cut run wants its rail control on its first row: %+v", targets)
			}
			for _, target := range targets[1:] {
				if target.action != "" {
					t.Fatalf("calls want their own row targets after the cut control: %+v", targets)
				}
			}
			expandLines(m, nil)
			block = checkHeader()
			if len(block.activityToolTargets) != len(calls) || strings.Count(plainStyledText(block.activityToolDetail), "✓ $") != 9 {
				t.Fatalf("revealed calls do not match summary: %q / %d targets", plainStyledText(block.activityToolDetail), len(block.activityToolTargets))
			}
		})
	}
}
