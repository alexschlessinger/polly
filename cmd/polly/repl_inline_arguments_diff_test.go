package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestInlineArgumentsRequireCompleteRecordedDiff(t *testing.T) {
	const before, after = "before", "after"
	complete := tools.DiffFileChange("a.go", before, after, true, true)
	largeOld := tools.DiffFileChange("a.go", strings.Repeat("x", tools.ChangeMaxFileBytes+1), after, true, true)
	largeContext := strings.Repeat("x", tools.ChangeMaxFileBytes+1)
	largeEdit := tools.DiffFileChange("a.go", largeContext+before, largeContext+after, true, true)
	binary := tools.DiffFileChange("a.go", "binary\x00"+before, after, true, true)
	partialBody := strings.Repeat("long changed line\n", 5000) + "original argument tail"
	partial := tools.DiffFileChange("a.go", "", partialBody, false, true)
	if !largeOld.Truncated || largeOld.Diff != "" || !largeEdit.Truncated || !binary.Binary || binary.Diff != "" {
		t.Fatal("fixture did not exercise omitted oversized/binary diff bodies")
	}
	if !partial.Truncated || partial.Diff == "" || strings.Contains(partial.Diff, "original argument tail") {
		t.Fatal("fixture did not exercise a partially recorded diff")
	}
	collection := func(change tools.FileChange) tools.FileChanges {
		return tools.FileChanges{Root: "/w", Tracked: true, Changes: []tools.FileChange{change}}
	}
	omitted, truncated := collection(complete), collection(complete)
	omitted.Omitted, truncated.Truncated = 1, true
	other := tools.DiffFileChange("other.go", before, after, true, true)
	mixed := collection(binary)
	mixed.Changes = append(mixed.Changes, other)
	headerOnly := complete
	headerOnly.Diff = "--- a/a.go\n+++ b/a.go\n"
	for _, tc := range []struct {
		name, tool, path string
		after            string
		changes          tools.FileChanges
		suppress         bool
	}{
		{"complete write", "write_file", "a.go", after, collection(complete), true},
		{"complete edit", "edit_file", "a.go", after, collection(complete), true},
		{"absolute path", "write_file", "/w/a.go", after, collection(complete), true},
		{"oversized old file", "write_file", "a.go", after, collection(largeOld), false},
		{"oversized edit context", "edit_file", "a.go", after, collection(largeEdit), false},
		{"partial diff", "write_file", "a.go", partialBody, collection(partial), false},
		{"omitted files", "write_file", "a.go", after, omitted, false},
		{"truncated collection", "edit_file", "a.go", after, truncated, false},
		{"binary", "write_file", "a.go", after, collection(binary), false},
		{"binary and unrelated diff", "write_file", "a.go", after, mixed, false},
		{"wrong file", "write_file", "a.go", after, collection(other), false},
		{"headers without body", "edit_file", "a.go", after, collection(headerOnly), false},
		{"no recorded changes", "write_file", "a.go", after, tools.FileChanges{Tracked: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]string{"path": tc.path}
			payloads := map[string]string{"content": tc.after}
			if tc.tool == "edit_file" {
				payloads = map[string]string{"old_string": before, "new_string": tc.after}
			}
			for key, value := range payloads {
				args[key] = value
			}
			encoded, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			call := messages.ChatMessageToolCall{Name: tc.tool, Arguments: string(encoded)}
			row := toolDisclosureRow{}
			row.setCall(call)
			result := toolDataResult(t, call, "changed", tc.changes)
			row.setPresentation(newToolPresentation(toolPresentationInput{call: call, result: result, complete: true}))
			text := plainStyledText(row.inlineArgumentsText())
			// Decode the displayed JSON to verify every argument byte, including
			// the portion beyond a partial diff and the original edit strings.
			var displayed map[string]string
			if err := json.Unmarshal([]byte(strings.TrimPrefix(text, reasoningBlockIndent+"Arguments\n")), &displayed); err != nil {
				t.Fatal(err)
			}
			if displayed["path"] != tc.path {
				t.Fatalf("path = %q, want %q", displayed["path"], tc.path)
			}
			for key, original := range payloads {
				want := original
				if tc.suppress {
					want = "<see recorded diff>"
				}
				if displayed[key] != want {
					t.Fatalf("%s lost original argument or duplicated a complete diff: got %d bytes, want %d", key, len(displayed[key]), len(want))
				}
			}
		})
	}
}

func TestInlineTruncatedWriteKeepsTinyOriginalContentLiveAndSaved(t *testing.T) {
	withDisplayTTY(t)
	const content = "tiny original [content](fg:red)"
	call := messages.ChatMessageToolCall{ID: "write", Name: "write_file", Arguments: `{"path":"a.go","content":"` + content + `"}`}
	changes := tools.FileChanges{Tracked: true, Changes: []tools.FileChange{
		tools.DiffFileChange("a.go", strings.Repeat("x", tools.ChangeMaxFileBytes+1), content, true, true),
	}}
	result := toolDataResult(t, call, "wrote file", changes)
	for _, replay := range []bool{false, true} {
		name := "live"
		if replay {
			name = "saved"
		}
		t.Run(name, func(t *testing.T) {
			r := newManagedREPL(&Config{}, "ctx", 0, 0)
			m := r.model
			if replay {
				m.hydrateHistory([]messages.ChatMessage{
					{Role: messages.MessageRoleUser, Content: "write"},
					{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}},
					result,
					{Role: messages.MessageRoleAssistant, Content: "done"},
				}, "ctx")
			} else {
				m.beginTurn("write")
				tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
				tui.AppendToolStart([]messages.ChatMessageToolCall{call})
				tui.AppendToolEnd(call, result.Content, time.Second, nil)
				tui.AppendToolResult(call, result)
				tui.AppendAssistantText("done")
				r.endTurn(nil)
			}
			m.toggleAllDisclosures(140)
			m.toggleAllDisclosures(140)
			text := strings.Join(transcriptRowsText(m.transcriptRows(140)), "\n")
			if !strings.Contains(text, content) || !strings.Contains(text, "… truncated") || strings.Contains(text, "<see recorded diff>") {
				t.Fatalf("truncation notice hid original write content: %s", text)
			}
		})
	}
}
