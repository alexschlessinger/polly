package main

import (
	"strings"
	"testing"
)

func TestRenderMarkdownInlineStyles(t *testing.T) {
	got := renderMarkdown("plain **bold** *it* `code` ~~gone~~")
	if plain := plainStyledText(got); plain != "plain bold it code gone" {
		t.Fatalf("plaintext = %q", plain)
	}
	for _, want := range []string{"[bold](mod:bold)", "[it](mod:italic)", "[code](fg:code)", "[gone](mod:strike)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("markup %q missing %q", got, want)
		}
	}
}

func TestRenderMarkdownHeadings(t *testing.T) {
	got := renderMarkdown("## Setup")
	if !strings.Contains(got, "[## ](fg:muted)") || !strings.Contains(got, "[Setup](mod:bold)") {
		t.Fatalf("heading render = %q", got)
	}
}

func TestRenderMarkdownListsAndQuotes(t *testing.T) {
	got := plainStyledText(renderMarkdown("- alpha\n- beta\n\n1. one\n2. two\n\n> quoted line"))
	for _, want := range []string{"• alpha", "• beta", "1. one", "2. two", "▏ quoted line"} {
		if !strings.Contains(got, want) {
			t.Fatalf("blocks %q missing %q", got, want)
		}
	}
}

func TestRenderMarkdownNestedListIndents(t *testing.T) {
	got := plainStyledText(renderMarkdown("- outer\n  - inner"))
	if !strings.Contains(got, "• outer") || !strings.Contains(got, "  • inner") {
		t.Fatalf("nested list = %q", got)
	}
}

func TestRenderMarkdownLinks(t *testing.T) {
	got := renderMarkdown("see [docs](https://example.com/page)")
	if !strings.Contains(got, "[docs](fg:accent)") {
		t.Fatalf("link label not accented: %q", got)
	}
	if !strings.Contains(plainStyledText(got), "(https://example.com/page)") {
		t.Fatalf("destination missing: %q", got)
	}
	// Autolink-style: no point repeating the URL after itself.
	auto := plainStyledText(renderMarkdown("<https://example.com>"))
	if strings.Count(auto, "example.com") != 1 {
		t.Fatalf("autolink repeated its destination: %q", auto)
	}
}

func TestRenderMarkdownCodeBlockHighlights(t *testing.T) {
	got := renderMarkdown("```go\nfunc main() { return }\n// done\n```")
	if !strings.Contains(got, "╭─ go") {
		t.Fatalf("fence header missing: %q", got)
	}
	if !strings.Contains(got, "[func](fg:accent)") {
		t.Fatalf("keyword not highlighted: %q", got)
	}
	if !strings.Contains(got, "[// done](fg:muted)") {
		t.Fatalf("comment not muted: %q", got)
	}
	if lines := strings.Split(got, "\n"); !strings.HasPrefix(plainStyledText(lines[1]), "│ ") {
		t.Fatalf("code line missing gutter: %q", lines[1])
	}
}

func TestRenderMarkdownUnknownLanguageFallsBack(t *testing.T) {
	got := renderMarkdown("```notareallang\nweird **not bold** text\n```")
	plain := plainStyledText(got)
	if !strings.Contains(plain, "weird **not bold** text") {
		t.Fatalf("code content must stay literal: %q", plain)
	}
}

func TestRenderMarkdownEscapesStyleMarkup(t *testing.T) {
	// Model text that looks like gotui markup but isn't a markdown link must
	// not inject styles.
	got := renderMarkdown("array[3](see note)")
	cellsText := plainStyledText(got)
	if cellsText != "array[3](see note)" {
		t.Fatalf("bracket text mangled: %q", cellsText)
	}
}

func TestSafeVisibleLenHoldsUnclosedInlineMarkup(t *testing.T) {
	cases := []struct {
		in   string
		want string // visible prefix
	}{
		{"plain text", "plain text"},                   // nothing open
		{"start **bo", "start "},                       // unclosed bold
		{"start **bold** done", "start **bold** done"}, /* closed */
		{"a `co", "a "},                                // unclosed code span
		{"a `code` b", "a `code` b"},                   // closed span
		{"see [lab", "see "},                           // possible link label
		{"see [lab](ur", "see "},                       // link destination in progress
		{"see [lab](url) x", "see [lab](url) x"},
		{"done.\n**bo", "done.\n"},             // completed lines always show
		{"snake_case_name", "snake_case_name"}, // intraword _ is not emphasis
		{"2*3 = 6", "2*3 = 6"},                 // intraword * is not held
		{"esc \\*lit", "esc \\*lit"},           // escaped delimiter
	}
	for _, tc := range cases {
		if got := tc.in[:safeVisibleLen(tc.in)]; got != tc.want {
			t.Errorf("safeVisibleLen(%q) shows %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSafeVisibleLenFenceBodyIsInert(t *testing.T) {
	in := "```go\nx := a * b **not emphasis\n"
	if got := in[:safeVisibleLen(in)]; got != in {
		t.Fatalf("fence body should be fully visible, got %q", got)
	}
	// A trailing backtick-run line may become the closing fence: held.
	in2 := "```go\nx := 1\n``"
	if got := in2[:safeVisibleLen(in2)]; got != "```go\nx := 1\n" {
		t.Fatalf("partial closing fence should be held, got %q", got)
	}
}

func TestSafeVisibleLenCapBoundsLatency(t *testing.T) {
	in := "start **" + strings.Repeat("x", holdbackCap+10)
	if got := in[:safeVisibleLen(in)]; got != in {
		t.Fatalf("past the cap everything should show, got %d of %d bytes", len(got), len(in))
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

	got := plainStyledText(m.transcript[0])
	for _, want := range []string{"## Plan", "two", "• run", "• ship", "│ func main() {}", "Done — see docs (https://x.dev)."} {
		if !strings.Contains(got, want) {
			t.Fatalf("final render %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "**") {
		t.Fatalf("emphasis markers leaked: %q", got)
	}
}
