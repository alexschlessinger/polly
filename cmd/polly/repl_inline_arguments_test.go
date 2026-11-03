package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
)

func TestInlineBashDetailsRetainOriginalCommandLiveAndSaved(t *testing.T) {
	withDisplayTTY(t)
	for _, replay := range []bool{false, true} {
		name := "live"
		if replay {
			name = "saved"
		}
		t.Run(name, func(t *testing.T) {
			for _, command := range []string{
				"cd /workspace/staging-checkout && MODE=staging go test ./...",
				"cd /workspace/staging-checkout\nMODE=staging go test ./...\n" + strings.Repeat("printf 'wide 界 é [literal](fg:red) with spaces'\n", approvalViewMaxLines+2) + "printf 'last command'",
			} {
				r := newManagedREPL(&Config{}, "ctx", 0, 0)
				m := r.model
				args, err := json.Marshal(map[string]string{"command": command})
				if err != nil {
					t.Fatal(err)
				}
				call := messages.ChatMessageToolCall{ID: "bash", Name: "bash", Arguments: string(args)}
				if replay {
					m.hydrateHistory([]messages.ChatMessage{
						{Role: messages.MessageRoleUser, Content: "test"},
						{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}},
						{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: "test output"},
						{Role: messages.MessageRoleAssistant, Content: "done"},
					}, "ctx")
				} else {
					m.beginTurn("test")
					tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
					tui.AppendToolStart([]messages.ChatMessageToolCall{call})
					tui.AppendToolEnd(call, "test output", time.Second, nil)
					tui.AppendAssistantText("done")
					r.endTurn(nil)
				}
				m.toggleAllDisclosures(240)
				preview := strings.Join(transcriptRowsText(m.transcriptRows(240)), "\n")
				if strings.Contains(preview, "Command") || strings.Contains(preview, "/workspace/staging-checkout") || strings.Contains(preview, "MODE=staging") {
					t.Fatalf("opened preview stopped being concise: %s", preview)
				}
				m.toggleAllDisclosures(240)
				for _, width := range []int{240, 48, 18, 80} {
					m.transcriptRows(width)
					var got []ui.Cell
					prefix := style.ParseCells(toolBodyRail, ui.StyleClear)
					for _, block := range m.visual.blocks {
						for _, link := range block.toolOutputLinks {
							inCommand := false
							for _, cells := range block.rows[link.Y+1 : link.Y+link.Rows] {
								text := ui.CellsToString(cells)
								if text == activityIndent+"  command" {
									inCommand = true
									continue
								}
								if strings.HasPrefix(text, activityIndent+"  output") {
									break
								}
								if !inCommand {
									continue
								}
								if len(cells) < len(prefix) || !slices.Equal(cells[:len(prefix)], prefix) || style.CellsWidth(cells) > width {
									t.Fatalf("width %d: command escaped rail or overflowed: %q", width, text)
								}
								got = append(got, cells[len(prefix):]...)
							}
						}
					}
					want := style.ParseCells(strings.Join(markdown.HighlightCodeLines(command, "bash"), ""), ui.StyleClear)
					if !slices.Equal(got, want) {
						t.Fatalf("width %d: command lost text, spacing, or styling:\ngot %q\nwant %q", width, ui.CellsToString(got), ui.CellsToString(want))
					}
				}
				if r.workspace().inspector.open {
					t.Fatal("command details reopened the old inspector UI")
				}
			}
		})
	}
}

// A row's label is the call. Only a Bash row whose label lost part of its
// command shows the command as written; nothing dumps its arguments.
func TestInlineDetailsShowCommandsOnlyWhereTheLabelIsLossy(t *testing.T) {
	for _, tc := range []struct {
		name, tool, args string
		want, absent     []string
	}{
		{"read", "read_file", `{"path":"/workspace/a/long/absolute/path.go","offset":25,"limit":12}`, nil, []string{"Arguments", "path.go", "25"}},
		{"custom", "lookup", `{"options":{"id":9007199254740993,"api_key":"secret"}}`, nil, []string{"Arguments", "9007199254740993", "secret"}},
		{"edit", "edit_file", `{"path":"a.go","old_string":"before","new_string":"after"}`, nil, []string{"Arguments", "before", "after"}},
		{"plain command", "bash", `{"command":"go test ./..."}`, nil, []string{"command", "go test"}},
		{"folded setup", "bash", `{"command":"cd staging && MODE=ci go test ./..."}`, []string{"command", "cd staging && MODE=ci go test ./..."}, []string{"Arguments"}},
		{"several lines", "bash", "{\"command\":\"go vet ./...\\ngo test ./...\"}", []string{"command", "go vet ./...", "go test ./..."}, nil},
		{"malformed", "lookup", `[literal](fg:red) not JSON`, nil, []string{"[literal]", "Arguments"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := toolDisclosureRow{}
			row.setCall(messages.ChatMessageToolCall{Name: tc.tool, Arguments: tc.args})
			text := plainStyledText(row.inlineCommandText(200))
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Fatalf("details lost %q: %q", want, text)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(text, absent) {
					t.Fatalf("details leaked %q: %q", absent, text)
				}
			}
		})
	}
	// A label that fits only after shortening gets the command back under it.
	row := toolDisclosureRow{}
	row.setCall(messages.ChatMessageToolCall{Name: "bash", Arguments: `{"command":"go test ./cmd/polly/internal/style/... ./cmd/polly/internal/markdown/..."}`})
	if wide, narrow := row.inlineCommandText(200), row.inlineCommandText(40); wide != "" || !strings.Contains(plainStyledText(narrow), "internal/markdown/...") {
		t.Fatalf("shortened label wants its command: wide=%q narrow=%q", wide, narrow)
	}
}
