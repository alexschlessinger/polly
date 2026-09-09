package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
)

func TestStreamedTableAlignsAtSettle(t *testing.T) {
	m := newReplModel()
	m.appendAssistant("| a | b |\n")
	// Without the delimiter row this is still a paragraph of literal pipes.
	m.renderPendingMarkdown()
	if got := plainStyledText(m.transcript[0].text); !strings.Contains(got, "| a | b |") {
		t.Fatalf("pre-delimiter render = %q, want literal pipes", got)
	}

	m.appendAssistant("|---|---|\n| one | 2 |\n")
	m.renderPendingMarkdown()
	got := plainStyledText(m.transcript[0].text)
	for _, want := range []string{"│ a │ b", "│ one │ 2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("streaming render = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "─") {
		t.Fatalf("streaming render already aligned: %q", got)
	}
	// Settlement must align the table even though pipe rows were not held back.
	m.finishAssistantBlock("")
	m.renderPendingMarkdown()
	final := strings.Split(plainStyledText(m.transcript[0].text), "\n")
	want := []string{
		"│ a    b",
		"│ ───  ─",
		"│ one  2",
	}
	if !slices.Equal(final, want) {
		t.Fatalf("settled table = %q, want %q", final, want)
	}
}

func TestStreamedTableWithFollowingBlockAlignsImmediately(t *testing.T) {
	m := newReplModel()
	m.appendAssistant("| a | b |\n|---|---|\n| x | y |\n\nafter\n")
	m.renderPendingMarkdown()
	got := plainStyledText(m.transcript[0].text)
	if !strings.Contains(got, "│ a  b") || !strings.Contains(got, "─") {
		t.Fatalf("completed mid-stream table not aligned: %q", got)
	}
}

func TestStreamedMarkdownEndToEnd(t *testing.T) {
	m := newReplModel()
	for _, chunk := range []string{
		"## Pl", "an\n\nUse ", "**two** steps:\n", "- run `go ", "test`\n- ship\n",
		"```go\nfunc main()", " {}\n```\n", "Done — see [docs](https://x.dev).",
	} {
		m.appendAssistant(chunk)
	}
	m.finishAssistantBlock("")

	m.renderPendingMarkdown()

	got := plainStyledText(m.transcript[0].text)
	for _, want := range []string{"Plan", "two", "• run", "• ship", "│ func main() {}", "Done — see docs (https://x.dev)."} {
		if !strings.Contains(got, want) {
			t.Fatalf("final render %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "**") {
		t.Fatalf("emphasis markers leaked: %q", got)
	}
}

func TestRenderMarkdownTableClippedPastHeaderKeepsWidths(t *testing.T) {
	source := "| Name | Qty |\n|---|---|\n| apple | 3 |\n| kiwi | 12 |"
	doc := markdown.NewDocument(source, "", false, nil)
	cut := strings.Index(source, "| kiwi")
	var head, tail []string
	for _, row := range must2(doc.Render(0, cut, 1000)) {
		head = append(head, lineCellsOutput(row, false))
	}
	for _, row := range must2(doc.Render(cut, len(source), 1000)) {
		tail = append(tail, lineCellsOutput(row, false))
	}
	full := strings.Split(plainStyledText(markdown.RenderDocument(source)), "\n")
	if got := append(head, tail...); !slices.Equal(got, full) {
		t.Fatalf("clipped table = %q, want %q", got, full)
	}
	if len(tail) != 1 || strings.Contains(tail[0], "─") {
		t.Fatalf("continuation grew a header rule: %q", tail)
	}
}
