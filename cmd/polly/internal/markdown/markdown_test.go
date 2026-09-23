package markdown

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	ui "github.com/metaspartan/gotui/v5"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
)

// plainStyledText parses styled markup to its visible runes.
func plainStyledText(s string) string {
	var rendered strings.Builder
	for _, c := range style.ParseCells(s, ui.NewStyle(ui.ColorWhite)) {
		if c.Rune != '\u200b' {
			rendered.WriteRune(c.Rune)
		}
	}
	return rendered.String()
}

func TestRenderMarkdownInlineStyles(t *testing.T) {
	got := RenderDocument("plain **bold** *it* `code` ~~gone~~")
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
	got := RenderDocument("# Release plan\n\n## Renderer\n\n### Details\n\n#### Fallback\n\n##### Narrow\n\n###### Notes")
	wantPlain := "Release plan\n\nRenderer\n\nDetails\n\nFallback\n\nNarrow\n\nNotes"
	if plain := plainStyledText(got); plain != wantPlain {
		t.Fatalf("heading plaintext = %q, want %q", plain, wantPlain)
	}
	// Headings rank by weight alone; no marker glyph can collide with the
	// user gutter or the code fence.
	for _, want := range []string{
		"[Release plan](fg:accent,mod:bold)",
		"[Renderer](mod:bold)",
		"[Details](fg:muted,mod:bold)",
		"[Fallback](fg:muted)",
		"[Narrow](fg:muted)",
		"[Notes](fg:muted)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("heading render %q missing %q", got, want)
		}
	}
}

func TestRenderMarkdownH1KeepsCase(t *testing.T) {
	got := RenderDocument("# *Release* [Docs](https://example.com/Guide) with `eBPF`")
	want := "Release Docs (https://example.com/Guide) with eBPF"
	if plain := plainStyledText(got); plain != want {
		t.Fatalf("H1 plaintext = %q, want %q", plain, want)
	}
}

func TestRenderMarkdownListsAndQuotes(t *testing.T) {
	got := plainStyledText(RenderDocument("- alpha\n- beta\n\n1. one\n2. two\n\n> quoted line"))
	for _, want := range []string{"• alpha", "• beta", "1. one", "2. two", "▏ quoted line"} {
		if !strings.Contains(got, want) {
			t.Fatalf("blocks %q missing %q", got, want)
		}
	}
}

func TestRenderMarkdownNestedListIndents(t *testing.T) {
	got := plainStyledText(RenderDocument("- outer\n  - inner"))
	if !strings.Contains(got, "• outer") || !strings.Contains(got, "  • inner") {
		t.Fatalf("nested list = %q", got)
	}
}

func TestRenderMarkdownLinks(t *testing.T) {
	got := RenderDocument("see [docs](https://example.com/page)")
	if !strings.Contains(got, "[docs](fg:accent)") {
		t.Fatalf("link label not accented: %q", got)
	}
	if !strings.Contains(plainStyledText(got), "(https://example.com/page)") {
		t.Fatalf("destination missing: %q", got)
	}
	// Autolink-style: no point repeating the URL after itself.
	auto := plainStyledText(RenderDocument("<https://example.com>"))
	if strings.Count(auto, "example.com") != 1 {
		t.Fatalf("autolink repeated its destination: %q", auto)
	}
}

func TestRenderMarkdownCodeBlockHighlights(t *testing.T) {
	got := RenderDocument("```go\nfunc main() { return }\n// done\n```")
	if !strings.Contains(got, "╭─ go") {
		t.Fatalf("fence header missing: %q", got)
	}
	if !strings.Contains(got, "[func](fg:syn-keyword)") {
		t.Fatalf("keyword not highlighted: %q", got)
	}
	if !strings.Contains(got, "[// done](fg:syn-comment)") {
		t.Fatalf("comment not highlighted: %q", got)
	}
	if lines := strings.Split(got, "\n"); !strings.HasPrefix(plainStyledText(lines[1]), "│ ") {
		t.Fatalf("code line missing gutter: %q", lines[1])
	}
}

// Every chroma category polly styles gets a syntax-token role, which under the
// default theme resolves to the semantic role it borrowed before it had a name.
func TestRenderMarkdownCodeBlockTokenRoles(t *testing.T) {
	tests := []struct {
		name   string
		lang   string
		source string
		want   []string
	}{
		{
			name:   "go",
			lang:   "go",
			source: "// done\nfunc main() {\n\tx := 42\n\tprintln(\"hi\")\n}",
			want:   []string{"fg:syn-comment", "fg:syn-keyword", "fg:syn-string", "fg:syn-number", "fg:syn-func"},
		},
		{
			name:   "json",
			lang:   "json",
			source: "{\"count\": 42, \"name\": \"polly\"}",
			want:   []string{"fg:syn-string", "fg:syn-number"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RenderDocument("```" + tt.lang + "\n" + tt.source + "\n```")
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Fatalf("%s block missing %s: %q", tt.lang, want, got)
				}
			}
		})
	}
}

func TestRenderMarkdownUnknownLanguageFallsBack(t *testing.T) {
	got := RenderDocument("```notareallang\nweird **not bold** text\n```")
	plain := plainStyledText(got)
	if !strings.Contains(plain, "weird **not bold** text") {
		t.Fatalf("code content must stay literal: %q", plain)
	}
}

func TestRenderMarkdownCodeBlockExpandsLiteralTabs(t *testing.T) {
	got := plainStyledText(RenderDocument("```go\nfunc main() {\n\tif true {\n\t\tprintln(\"x\")\n\t}\n}\n```"))
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
	got := RenderDocument("```markdown\n![headcam-try5](/tmp/headcam-try5.png)\n```")
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
	got := RenderDocument("array[3](see note)")
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
		if got := tc.in[:SafeVisibleLen(tc.in)]; got != tc.want {
			t.Errorf("safeVisibleLen(%q) shows %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSafeVisibleLenFenceBodyIsInert(t *testing.T) {
	in := "```go\nx := a * b **not emphasis\n"
	if got := in[:SafeVisibleLen(in)]; got != in {
		t.Fatalf("fence body should be fully visible, got %q", got)
	}
	// A trailing backtick-run line may become the closing fence: held.
	in2 := "```go\nx := 1\n``"
	if got := in2[:SafeVisibleLen(in2)]; got != "```go\nx := 1\n" {
		t.Fatalf("partial closing fence should be held, got %q", got)
	}
}

func TestSafeVisibleLenCapBoundsLatency(t *testing.T) {
	in := "start **" + strings.Repeat("x", holdbackCap+10)
	if got := in[:SafeVisibleLen(in)]; got != in {
		t.Fatalf("past the cap everything should show, got %d of %d bytes", len(got), len(in))
	}
}

func TestRenderMarkdownTableAligned(t *testing.T) {
	got := RenderDocument("| Name | Qty |\n|---|---|\n| apple | 3 |\n| kiwi | 12 |")
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
	got := plainStyledText(RenderDocument("| L | R | C |\n|:--|--:|:-:|\n| a | b | c |\n| aa | bb | cc |"))
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
	got := plainStyledText(RenderDocument("| 名前 | Link |\n|---|---|\n| ab | [x](https://e.co) |"))
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
	got := plainStyledText(RenderDocument("| a | b |\n|---|---|\n| x |\n| 1 | 2 | 3 |"))
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

func TestHighlightCodeLinesWithoutLanguageIsPlainCode(t *testing.T) {
	code := "func main() { return }\n// done"
	if got, want := HighlightCodeLines(code, ""), styledLines(code, "code", ""); !slices.Equal(got, want) {
		t.Fatalf("HighlightCodeLines(code, \"\") = %q, want %q", got, want)
	}
}

func TestHighlightDiffUsesTokenRoles(t *testing.T) {
	lines := HighlightCodeLines("+added\n-removed\n context", "diff")
	if len(lines) < 3 || !strings.Contains(lines[0], "fg:syn-add") || !strings.Contains(lines[1], "fg:syn-del") {
		t.Fatalf("diff highlighting: %q", lines)
	}
	if !strings.Contains(lines[2], "fg:code") {
		t.Fatalf("diff context must stay code: %q", lines[2])
	}
}

func TestLongStreamingCodeBlockHighlightsInTheBackground(t *testing.T) {
	var src strings.Builder
	src.WriteString("Code:\n\n```go\n")
	cache := &CodeCache{Background: true}
	render := func() (streamed, exact []string) {
		got, _, _, _ := RenderWithWidth(src.String(), "", true, cache, 80)
		want, _, _, _ := RenderWithWidth(src.String(), "", true, nil, 80)
		return strings.Split(got, "\n"), strings.Split(want, "\n")
	}
	lines := 0
	grow := func(text string) {
		src.WriteString(text + "\n")
		lines++
	}
	for lines < backgroundCodeLines {
		if lines == backgroundCodeLines-4 {
			grow("/* a comment")
		} else {
			grow(fmt.Sprintf("var v%d = %d", lines, lines))
		}
		if streamed, exact := render(); !slices.Equal(streamed, exact) || cache.HighlightPending() {
			t.Fatalf("a %d-line block was not highlighted in place", lines)
		}
	}

	// Past the limit, new lines show as plain code behind the last highlight.
	grow("var past = 1")
	streamed, exact := render()
	last := len(streamed) - 1
	if !slices.Equal(streamed[:last], exact[:last]) || streamed[last] != gutterLines(styledLines("var past = 1", "code", ""))[0] {
		t.Fatalf("the line past the limit was not plain behind the highlight:\n%s", strings.Join(streamed[last-1:], "\n"))
	}
	pass := cache.NextHighlight()
	if pass == nil || cache.NextHighlight() != nil || !cache.HighlightPending() {
		t.Fatal("the long block did not get exactly one pass")
	}

	// The comment closes across the old boundary while the pass runs.
	grow("*/")
	for range 30 {
		grow(fmt.Sprintf("var v%d = %d", lines, lines))
	}
	render()
	pass.Run()
	if !cache.Install(pass) {
		t.Fatal("a pass over a prefix of the grown block did not land")
	}
	if !cache.HighlightPending() {
		t.Fatal("the block grew past the pass without asking for another")
	}
	next := cache.NextHighlight()
	next.Run()
	if !cache.Install(next) {
		t.Fatal("the catch-up pass did not land")
	}
	if streamed, exact := render(); !slices.Equal(streamed, exact) || cache.HighlightPending() {
		t.Fatal("the caught-up block differs from highlighting it whole")
	}

	// A pass still out when the message settles lands nowhere.
	grow("var tail = 1")
	render()
	stale := cache.NextHighlight()
	settledSrc := src.String() + "```\n"
	want, _, _, _ := RenderWithWidth(settledSrc, "", false, nil, 80)
	if settled, _, _, _ := RenderWithWidth(settledSrc, "", false, cache, 80); settled != want {
		t.Fatal("the settled render kept plain lines")
	}
	stale.Run()
	if cache.Install(stale) || cache.HighlightPending() {
		t.Fatal("a pass from before the settle landed")
	}
	if settled, _, _, _ := RenderWithWidth(settledSrc, "", false, cache, 80); settled != want {
		t.Fatal("the settled render is not stable")
	}
}

func TestShortOrPlainStreamingCodeNeedsNoPass(t *testing.T) {
	src := "```\n" + strings.Repeat("plain text\n", 3*backgroundCodeLines)
	cache := &CodeCache{Background: true}
	streamed, _, _, _ := RenderWithWidth(src, "", true, cache, 80)
	want, _, _, _ := RenderWithWidth(src, "", true, nil, 80)
	if streamed != want || cache.NextHighlight() != nil {
		t.Fatal("a block with no language waited for a pass")
	}
	src = "```go\n" + strings.Repeat("x := 1\n", 3*backgroundCodeLines)
	scrollback := &CodeCache{}
	streamed, _, _, _ = RenderWithWidth(src, "", true, scrollback, 80)
	want, _, _, _ = RenderWithWidth(src, "", true, nil, 80)
	if streamed != want || scrollback.NextHighlight() != nil {
		t.Fatal("a cache without Background left highlighting to a pass")
	}
}

func TestRenderReportsWidthDependence(t *testing.T) {
	if _, _, _, sized := RenderWithWidth("# Title\n\n```go\nx := 1\n```\n", "", false, nil, 80); sized {
		t.Fatal("prose and code reported as width-dependent")
	}
	if _, _, _, sized := RenderWithWidth("> | a | b |\n> |---|---|\n> | 1 | 2 |\n", "", false, nil, 0); !sized {
		t.Fatal("quoted table not reported as width-dependent")
	}
}
