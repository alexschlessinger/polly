package markdown

import (
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	ui "github.com/metaspartan/gotui/v5"
)

func TestMutedPreviewUsesResponseMarkdown(t *testing.T) {
	for _, src := range []string{
		"# Heading\n\nParagraph **bold** and *italic*, ~~removed~~ and `code`.\n\nAnother paragraph.",
		"- first\n  - nested\n- second\n\n1. one\n2. two\n\n> quoted text",
		"See [documentation](https://example.com/docs).\n\n```go\nfunc f() {\n\tprintln(\"**literal**\")\n}\n```",
		"| Item | Result |\n|---|---|\n| wide 界 cell | **accepted** |\n| next | nested `code` |",
		"Escaped \\*literal\\* and [untrusted](fg:red)\n\n```text\n    spaces    stay\n```",
	} {
		for _, width := range []int{18, 48, 100} {
			t.Run(fmt.Sprintf("%.20s/%d", src, width), func(t *testing.T) {
				response, _, _, _ := RenderWithWidth(src, "", false, nil, width)
				want := style.VisualRows(response, ui.StyleClear, width)
				got := RenderMutedRows(src, width, nil)
				if len(got) != len(want) {
					t.Fatalf("preview rows = %d, response rows = %d", len(got), len(want))
				}
				for i, line := range got {
					cells := style.ParseCells(line, ui.StyleClear)
					if ui.CellsToString(cells) != ui.CellsToString(want[i]) || len(cells) != len(want[i]) {
						t.Fatalf("row %d: preview %q != response %q", i, ui.CellsToString(cells), ui.CellsToString(want[i]))
					}
					if style.CellsWidth(cells) > width {
						t.Fatalf("row %d overflows %d: %q", i, width, ui.CellsToString(cells))
					}
					for j, cell := range cells {
						if cell.Style.Modifier != want[i][j].Style.Modifier || cell.Style.Fg != ui.StyleParserColorMap["muted"] {
							t.Fatalf("row %d cell %d lost emphasis or muted color: %+v vs %+v", i, j, cell.Style, want[i][j].Style)
						}
					}
					if line != "" && !strings.Contains(line, "fg:muted") {
						t.Fatalf("preview must retain the semantic theme role: %q", line)
					}
				}
			})
		}
	}
}

func TestMutedPreviewCachesCompletedCode(t *testing.T) {
	cache := &CodeCache{}
	const code = "```go\nfunc example() { println(42) }\n```"
	RenderMutedRows(code, 80, cache)
	_, before := cache.Block(0)
	RenderMutedRows(code+"\n\nSubsequent prose.", 80, cache)
	_, after := cache.Block(0)
	if len(before) == 0 || len(after) != len(before) || &before[0] != &after[0] {
		t.Fatal("streaming later prose re-highlighted the completed code block")
	}
}

func TestTrimPreviewPreservesFenceContext(t *testing.T) {
	for _, fence := range []string{"```text", "~~~~text", "  ````go"} {
		marker := strings.TrimLeft(fence, " ")
		marker = strings.TrimRight(marker, "textgo")
		source := fence + "\n" + strings.Repeat("payload line\n", 700) + marker + "\n\n# Next heading\n\n**Important** conclusion."
		tail, prefix := TrimPreview(source, 128)
		if len([]rune(tail)) > 128 || prefix == "" {
			t.Fatalf("fence %q lost its bounded context: %q, %q", fence, prefix, tail)
		}
		got := plainStyledText(strings.Join(RenderMutedRows(prefix+tail, 80, nil), "\n"))
		if strings.Contains(got, "# Next heading") || strings.Contains(got, "**Important**") || !strings.Contains(got, "Important conclusion.") {
			t.Fatalf("fence %q swallowed later Markdown: %q", fence, got)
		}
		// A second cut through the same open block retains the recovered opener.
		open := fence + "\n" + strings.Repeat("payload line\n", 700)
		first, opener := TrimPreview(open, 128)
		second, opener := TrimPreview(opener+first+strings.Repeat("more body\n", 100), 128)
		got = plainStyledText(strings.Join(RenderMutedRows(opener+second+marker+"\n\n**Done**", 80, nil), "\n"))
		if strings.Contains(got, "**Done**") || !strings.HasSuffix(got, "Done") {
			t.Fatalf("repeated cut lost fence %q: %q", fence, got)
		}
	}
}

func TestTrimPreviewFenceBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		keep   int
	}{
		{"indented-first-body-line", "prefix\n\n  ```text\n  payload\n  more payload\n  ```\n\n**Important** conclusion.", 57},
		{"list-first-body-line", "prefix\n\n- ```text\n  payload\n  more payload\n  ```\n\n**Important** conclusion.", 57},
		{"empty-language-fence", "prefix\n\n```text\n```\n\n**Important** conclusion.", 30},
		{"empty-fence", "prefix\n\n```\n```\n\n**Important** conclusion.", 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tail, prefix := TrimPreview(tc.source, tc.keep)
			if len([]rune(tail)) > tc.keep {
				t.Fatalf("tail exceeds %d runes: %q", tc.keep, tail)
			}
			got := plainStyledText(strings.Join(RenderMutedRows(prefix+tail, 80, nil), "\n"))
			if strings.Contains(got, "**Important**") || !strings.HasSuffix(got, "Important conclusion.") {
				t.Fatalf("trimmed fence swallowed later Markdown: prefix=%q tail=%q rendered=%q", prefix, tail, got)
			}
		})
	}
}

func TestMutedPreviewDoesNotCreateImageSlots(t *testing.T) {
	// Even an existing raster reference must stay text-only. Resolving it here
	// would read a local file and create a slot with no owning image sidecar.
	path := filepath.Join(t.TempDir(), "image.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	src := "![image](" + filepath.ToSlash(path) + ")"
	if _, images, _, _ := RenderWithWidth(src, "", false, nil, 80); len(images) != 1 {
		t.Fatal("fixture did not produce a response image")
	}
	state := &renderState{width: 80, textOnly: true}
	rendered := renderDocument(src, state)
	if len(state.images) != 0 || rendered != style.StripImageMarkers(rendered) {
		t.Fatalf("preview created image slots: %q, %+v", rendered, state.images)
	}
	if got, want := plainStyledText(strings.Join(RenderMutedRows(src, 80, nil), "\n")), plainStyledText(RenderDocument(src)); got != want || !strings.Contains(got, "image (") {
		t.Fatalf("image link = %q, want %q", got, want)
	}
	if got := RenderMutedRows(" \n", 80, nil); len(got) != 0 {
		t.Fatalf("empty preview = %q", got)
	}
	if got := RenderMutedRows("text", 0, nil); len(got) != 0 {
		t.Fatalf("zero-width preview = %q", got)
	}
}
