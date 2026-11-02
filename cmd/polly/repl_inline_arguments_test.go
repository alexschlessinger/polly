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
	"github.com/alexschlessinger/pollytool/tools"
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
					prefix := style.ParseCells(style.Rail, ui.StyleClear)
					for _, block := range m.visual.blocks {
						for _, link := range block.toolOutputLinks {
							inCommand := false
							for _, cells := range block.rows[link.Y+1 : link.Y+link.Rows] {
								text := ui.CellsToString(cells)
								if text == plainStyledText(style.Rail)+"Command" {
									inCommand = true
									continue
								}
								if text == plainStyledText(style.Rail)+"Output" {
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

func TestInlineArgumentsAreCompleteSafeAndNonredundant(t *testing.T) {
	for _, tc := range []struct {
		name, tool, args string
		diff             bool
		want, absent     []string
	}{
		{"read", "read_file", `{"path":"/workspace/a/long/absolute/path.go","query":"[literal](fg:red)","offset":25,"limit":12}`, false, []string{`"path": "/workspace/a/long/absolute/path.go"`, `"query": "[literal](fg:red)"`, `"offset": 25`, `"limit": 12`}, nil},
		{"custom", "lookup", `{"options":{"id":9007199254740993,"enabled":true,"api_key":"secret"},"headers":[{"Authorization":"secret"}]}`, false, []string{"9007199254740993", `"enabled": true`, "<redacted>"}, []string{"secret"}},
		{"bash options", "bash", `{"command":"cd staging && go test ./...","timeout":45}`, false, []string{"Command", "cd staging && go test ./...", "Arguments", `"timeout": 45`}, nil},
		{"malformed", "lookup", `[literal](fg:red) not JSON`, false, []string{"[literal](fg:red) not JSON"}, nil},
		{"write", "write_file", `{"path":"a.go","content":"` + strings.Repeat("large write payload ", 1000) + `"}`, true, []string{`"path": "a.go"`, `"content": "<see recorded diff>"`}, []string{"large write payload"}},
		{"edit", "edit_file", `{"path":"a.go","old_string":"before","new_string":"after","replace_all":true}`, true, []string{`"path": "a.go"`, `"replace_all": true`, `"old_string": "<see recorded diff>"`, `"new_string": "<see recorded diff>"`}, []string{"before", "after"}},
		{"failed edit", "edit_file", `{"path":"a.go","old_string":"before","new_string":"after"}`, false, []string{`"old_string": "before"`, `"new_string": "after"`}, []string{"<see recorded diff>"}},
		{"empty", "lookup", `{}`, false, nil, []string{"Arguments"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := messages.ChatMessageToolCall{Name: tc.tool, Arguments: tc.args}
			row := toolDisclosureRow{}
			row.setCall(call)
			if tc.diff {
				var args struct {
					Path, Content string
					Old           string `json:"old_string"`
					New           string `json:"new_string"`
				}
				if err := json.Unmarshal([]byte(tc.args), &args); err != nil {
					t.Fatal(err)
				}
				old, next := args.Old, args.New
				if tc.tool == "write_file" {
					next = args.Content
				}
				changes := tools.FileChanges{Tracked: true, Changes: []tools.FileChange{tools.DiffFileChange(args.Path, old, next, true, true)}}
				result := toolDataResult(t, call, "changed", changes)
				row.setPresentation(newToolPresentation(toolPresentationInput{call: call, result: result, complete: true}))
			}
			text := plainStyledText(row.inlineArgumentsText())
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Fatalf("arguments lost %q: %s", want, text)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(text, absent) {
					t.Fatalf("arguments leaked %q: %s", absent, text)
				}
			}
		})
	}
}
