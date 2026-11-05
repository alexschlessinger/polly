package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

func TestActivityCaretStaysFlushBelowUserPrompt(t *testing.T) {
	withDisplayTTY(t)
	for _, width := range []int{24, 100} {
		for _, prompt := range []string{"check branch", "check\nbranch"} {
			t.Run(fmt.Sprintf("width=%d/prompt=%q", width, prompt), func(t *testing.T) {
				r := newManagedREPL(&Config{}, "ctx", 0, 0)
				m := r.model
				m.beginTurn(prompt)
				tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
				tui.ShowThinking("Checking the diff.")
				call := messages.ChatMessageToolCall{ID: "read", Name: "read_file", Arguments: `{"path":"AGENTS.md"}`}
				tui.AppendToolStart([]messages.ChatMessageToolCall{call})
				tui.AppendToolEnd(call, "instructions", time.Millisecond, nil)
				headerRow := strings.Count(prompt, "\n") + 1

				for _, settled := range []bool{false, true} {
					if settled {
						tui.AppendAssistantText("Done.")
						r.endTurn(nil)
					}
					for _, expanded := range []bool{true, false} {
						rows := (conversationView{}).Rows(m, width)
						text := transcriptRowsText(rows)
						glyph := "▸"
						if expanded {
							glyph = "▾"
						}
						if len(text) <= headerRow || !strings.HasPrefix(text[headerRow], glyph+" thought") {
							t.Fatalf("settled=%v expanded=%v: activity is not flush below the prompt: %q", settled, expanded, text)
						}
						if expanded && (len(text) <= headerRow+1 || text[headerRow+1] != "  Checking the diff.") {
							t.Fatalf("expanded detail should have a two-space indent: %q", text)
						}
						hits := m.visibleDisclosurePlacements(fullViewport(len(rows), width), activityAll)
						if len(hits) == 0 || hits[0].X != 0 || hits[0].Y != headerRow {
							t.Fatalf("caret hitbox is not flush below the prompt: %#v", hits)
						}
						m.disclosurePlacements[activityAll] = hits
						// Click the caret itself, not a label farther along the row.
						if !m.toggleDisclosureAt(0, headerRow, width) {
							t.Fatal("flush-left caret did not toggle the activity")
						}
					}
				}
			})
		}
	}
}
