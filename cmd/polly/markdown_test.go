package main

import (
	"slices"
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
	got := renderMarkdown("# Release plan\n\n## Renderer\n\n### Details\n\n#### Fallback\n\n##### Narrow\n\n###### Notes")
	wantPlain := "▌ RELEASE PLAN\n\n▎ Renderer\n\n▏ Details\n\n▏ Fallback\n\n┊ Narrow\n\n· Notes"
	if plain := plainStyledText(got); plain != wantPlain {
		t.Fatalf("heading plaintext = %q, want %q", plain, wantPlain)
	}
	for _, want := range []string{
		"[▌ ](fg:accent,mod:bold)[RELEASE PLAN](fg:accent,mod:bold)",
		"[▎ ](fg:accent,mod:bold)[Renderer](fg:accent,mod:bold)",
		"[▏ ](fg:accent)[Details](fg:accent)",
		"[▏ ](fg:muted)[Fallback](fg:muted)",
		"[┊ ](fg:muted)[Narrow](fg:muted)",
		"[· ](fg:muted)[Notes](fg:muted)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("heading render %q missing %q", got, want)
		}
	}
}

func TestRenderMarkdownH1UppercasesOnlyPlainText(t *testing.T) {
	got := renderMarkdown("# *Release* [Docs](https://example.com/Guide) with `eBPF`")
	want := "▌ RELEASE Docs (https://example.com/Guide) WITH eBPF"
	if plain := plainStyledText(got); plain != want {
		t.Fatalf("H1 plaintext = %q, want %q", plain, want)
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

func TestRenderMarkdownCodeBlockExpandsLiteralTabs(t *testing.T) {
	got := plainStyledText(renderMarkdown("```go\nfunc main() {\n\tif true {\n\t\tprintln(\"x\")\n\t}\n}\n```"))
	if strings.ContainsRune(got, '\t') {
		t.Fatalf("rendered code contains a literal tab: %q", got)
	}
	for _, want := range []string{"│     if true {", "│         println(\"x\")", "│     }"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered code missing %q: %q", want, got)
		}
	}
}

func TestExpandCodeTabsUsesCodeRelativeTabStops(t *testing.T) {
	got := expandCodeTabs("a\tb\nabcd\tc\n界\tx")
	want := "a   b\nabcd    c\n界  x"
	if got != want {
		t.Fatalf("expandCodeTabs() = %q, want %q", got, want)
	}
}

func TestRenderMarkdownCodeBlockPreservesMarkdownImageLiteral(t *testing.T) {
	got := renderMarkdown("```markdown\n![headcam-try5](/tmp/headcam-try5.png)\n```")
	plain := plainStyledText(got)
	if !strings.Contains(plain, "│ ![headcam-try5](/tmp/headcam-try5.png)") {
		t.Fatalf("markdown code literal was mangled: %q", plain)
	}
	if strings.Contains(plain, "fg:code") {
		t.Fatalf("gotui style markup leaked into code literal: %q", plain)
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

func TestRenderMarkdownTableAligned(t *testing.T) {
	got := renderMarkdown("| Name | Qty |\n|---|---|\n| apple | 3 |\n| kiwi | 12 |")
	want := []string{
		"│ Name   Qty",
		"│ ─────  ───",
		"│ apple  3",
		"│ kiwi   12",
	}
	if plain := strings.Split(plainStyledText(got), "\n"); !slices.Equal(plain, want) {
		t.Fatalf("table = %q, want %q", plain, want)
	}
	for _, markup := range []string{"[Name](mod:bold)", "[Qty](mod:bold)", "[│ ](fg:muted)", "[─────  ───](fg:muted)"} {
		if !strings.Contains(got, markup) {
			t.Fatalf("table markup missing %q in %q", markup, got)
		}
	}
	if strings.Contains(got, "[apple](mod:bold)") {
		t.Fatalf("body cell rendered bold: %q", got)
	}
}

func TestRenderMarkdownTableAlignment(t *testing.T) {
	got := plainStyledText(renderMarkdown("| L | R | C |\n|:--|--:|:-:|\n| a | b | c |\n| aa | bb | cc |"))
	want := []string{
		"│ L    R  C",
		"│ ──  ──  ──",
		"│ a    b  c",
		"│ aa  bb  cc",
	}
	if plain := strings.Split(got, "\n"); !slices.Equal(plain, want) {
		t.Fatalf("aligned table = %q, want %q", plain, want)
	}
}

func TestRenderMarkdownTableMeasuresRenderedCells(t *testing.T) {
	// Wide runes count display cells; a link measures as its rendered
	// "label (dest)" form, not its source text.
	got := plainStyledText(renderMarkdown("| 名前 | Link |\n|---|---|\n| ab | [x](https://e.co) |"))
	want := []string{
		"│ 名前  Link",
		"│ ────  ────────────────",
		"│ ab    x (https://e.co)",
	}
	if plain := strings.Split(got, "\n"); !slices.Equal(plain, want) {
		t.Fatalf("measured table = %q, want %q", plain, want)
	}
}

func TestRenderMarkdownTableRaggedRows(t *testing.T) {
	got := plainStyledText(renderMarkdown("| a | b |\n|---|---|\n| x |\n| 1 | 2 | 3 |"))
	want := []string{
		"│ a  b",
		"│ ─  ─",
		"│ x",
		"│ 1  2",
	}
	if plain := strings.Split(got, "\n"); !slices.Equal(plain, want) {
		t.Fatalf("ragged table = %q, want %q", plain, want)
	}
}

func TestStreamedTableAlignsAtSettle(t *testing.T) {
	m := newReplModel()
	m.appendAssistant("| a | b |\n")
	// Without the delimiter row this is still a paragraph of literal pipes.
	if got := plainStyledText(m.transcript[0].text); !strings.Contains(got, "| a | b |") {
		t.Fatalf("pre-delimiter render = %q, want literal pipes", got)
	}

	m.appendAssistant("|---|---|\n| one | 2 |\n")
	got := plainStyledText(m.transcript[0].text)
	for _, want := range []string{"│ a │ b", "│ one │ 2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("streaming render = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "─") {
		t.Fatalf("streaming render already aligned: %q", got)
	}
	if !m.streamDeferredTable {
		t.Fatal("streaming table did not defer the aligned render")
	}

	// Nothing is held back (pipe rows are not holdback constructs), so only
	// the deferred-table flag forces the settle re-render.
	m.finishAssistantBlock("")
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
	got := plainStyledText(m.transcript[0].text)
	if !strings.Contains(got, "│ a  b") || !strings.Contains(got, "─") {
		t.Fatalf("completed mid-stream table not aligned: %q", got)
	}
	if m.streamDeferredTable {
		t.Fatal("table with a following block should not defer")
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

	got := plainStyledText(m.transcript[0].text)
	for _, want := range []string{"▎ Plan", "two", "• run", "• ship", "│ func main() {}", "Done — see docs (https://x.dev)."} {
		if !strings.Contains(got, want) {
			t.Fatalf("final render %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "**") {
		t.Fatalf("emphasis markers leaked: %q", got)
	}
}
