package main

import (
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestStatusRowOmitsToolChanges(t *testing.T) {
	for _, changes := range []tools.FileChanges{
		editChanges("main.go"),
		{Tracked: true, Changes: []tools.FileChange{{Path: "image.png", Binary: true}}},
		{Reason: "not a git repository"},
	} {
		m := newReplModel()
		m.status.contextName = "session"
		m.status.modelName = "openai/gpt-5.4"
		m.status.recordContextUsage(1000, 10000)
		call := messages.ChatMessageToolCall{ID: "edit", Name: "edit_file"}
		m.displayCatalog.setResult(call, toolDataResult(t, call, "Edited", changes))
		for _, width := range []int{40, 200} {
			got := strings.TrimSpace(plainStyledText(m.statusRow(width)))
			if want := "gpt-5.4 · session · 1.0k/10.0k"; got != want {
				t.Fatalf("width %d, changes %+v: status = %q, want %q", width, changes, got, want)
			}
		}
	}
}

func TestStatusRowIgnoresSavedWorkspaceChanges(t *testing.T) {
	// Older sessions can retain a workspace report reference, even when its
	// artifact is unavailable. It is no longer read or shown by a display.
	info := &sessions.SessionView{Metadata: &sessions.Metadata{
		Name:             "saved",
		WorkspaceChanges: &artifacts.Ref{ID: strings.Repeat("a", 64)},
	}}
	m := prepareChildDisplay(info, &Config{}, 80)
	if got := strings.TrimSpace(plainStyledText(m.statusRow(80))); got != "saved" {
		t.Fatalf("saved workspace changes leaked into status: %q", got)
	}
}

// sessionChangeStats checks that bounded transcript hydration preserves the
// historical per-tool counts. Session-wide totals are not runtime UI state.
func sessionChangeStats(tools []displayTool) (additions, deletions, files int) {
	seen := make(map[string]bool)
	for _, tool := range tools {
		changes := tool.pres.changes
		if changes == nil || !changes.tracked {
			continue
		}
		for _, change := range changes.changes {
			additions += change.additions
			deletions += change.deletions
			if !seen[change.path] {
				seen[change.path] = true
				files++
			}
		}
	}
	return additions, deletions, files
}
