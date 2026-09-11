package markdown

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	ui "github.com/metaspartan/gotui/v5"
)

const responsiveTableExample = `| Skill | Purpose | Extras |
| --- | --- | --- |
| deploy | Deploy Linksnaps to production: clean-tree check, tests, build/push images, deploy, verify, tag repos, publish GitHub releases | deploy.sh |
| release-linksnaps | Coordinate a full frontend and backend release: inventory/classify open PRs, build local candidates, validate with tests and browser driver | agents/, assets/, scripts/ |
| run-linksnaps | Build, run, drive and screenshot the Vue app: start linkapi, dev server, log in as test account | driver.mjs |
| analyze-browser-profile | Parse a browser performance profile for JS hotspots, layout/style recalc, GC, slow events | analyze.py |
| pull | Pull latest Linksnaps and LinkAPI and describe what changed | — |`

func responsiveLines(t *testing.T, source string, width int, streaming bool) []string {
	t.Helper()
	rendered, _, _ := RenderWithWidth(source, "", streaming, nil, width)
	var lines []string
	for _, line := range strings.Split(rendered, "\n") {
		cells := style.ParseCells(line, ui.StyleClear)
		if got := style.CellsWidth(cells); got > width {
			t.Fatalf("width %d: line uses %d cells: %q", width, got, ui.CellsToString(cells))
		}
		lines = append(lines, ui.CellsToString(cells))
	}
	return lines
}

func TestResponsiveTableExample(t *testing.T) {
	for _, width := range []int{180, 100, 60, 42, 30, 16} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			lines := responsiveLines(t, responsiveTableExample, width, false)
			text := strings.Join(lines, "\n")
			if width >= 42 && strings.Contains(text, "Purpose:") {
				t.Fatalf("unexpected stacked layout: %s", text)
			}
			if width < 42 && !strings.Contains(text, "Purpose:") {
				t.Fatalf("missing stacked labels: %s", text)
			}
		})
	}
}

func TestResponsiveTableCompactAndThreshold(t *testing.T) {
	source := "| a | b |\n|---|---|\n| one | 2 |"
	want := []string{"│ a    b", "│ ───  ─", "│ one  2"}
	if got := responsiveLines(t, source, 80, false); !slices.Equal(got, want) {
		t.Fatalf("compact = %q", got)
	}
	source = "| abcdefghijkl | mnopqrstuvwx |\n|---|---|\n| 1 | 2 |"
	if got := strings.Join(responsiveLines(t, source, 28, false), "\n"); strings.Contains(got, "abcdefghijkl:") {
		t.Fatal("stacked at exact fit")
	}
	if got := strings.Join(responsiveLines(t, source, 27, false), "\n"); !strings.Contains(got, "abcdefghijkl:") {
		t.Fatal("did not stack below threshold")
	}
}

func TestResponsiveTableWrapAndStyle(t *testing.T) {
	source := "| Name | Description |\n|---|---|\n| **alpha** | one two three four five six seven eight nine ten |\n| beta | short |"
	lines := responsiveLines(t, source, 30, false)
	text := strings.Join(lines, "\n")
	if !strings.Contains(text, "│ alpha  one two three four") || !strings.Contains(text, "│        five six seven eight") || !strings.Contains(text, "│\n│ beta") {
		t.Fatalf("cell wrapping: %s", text)
	}
	wrapped := wrapTableCell(style.Styled("alpha beta gamma", "accent", "bold"), 8)
	for _, line := range wrapped {
		for _, cell := range style.ParseCells(line, ui.StyleClear) {
			if cell.Style.Fg != ui.StyleParserColorMap["accent"] || cell.Style.Modifier != ui.ModifierBold {
				t.Fatalf("lost style: %+v", cell)
			}
		}
	}
	for _, value := range []string{"e\u0301e\u0301e\u0301", "中文日本語", "👩‍💻👩‍💻", "[very-long-identifier]"} {
		got := wrapTableCell(style.Escape(value), 4)
		var joined string
		for _, line := range got {
			joined += ui.CellsToString(style.ParseCells(line, ui.StyleClear))
		}
		if joined != value {
			t.Fatalf("lost graphemes: %q => %q", value, joined)
		}
	}
}

func TestResponsiveTableStackedEmptyAndNested(t *testing.T) {
	source := "| | Long description |\n|---|---|\n| | value with many words |\n| x | |"
	text := strings.Join(responsiveLines(t, source, 15, false), "\n")
	if !strings.Contains(text, "Column 1:") || !strings.Contains(text, "value with") {
		t.Fatalf("stacked content: %s", text)
	}
	for _, prefix := range []string{"> ", "  "} {
		nested := prefix + strings.ReplaceAll(responsiveTableExample, "\n", "\n"+prefix)
		if prefix == "  " {
			nested = "- table\n\n" + nested
		}
		responsiveLines(t, nested, 60, false)
	}
}

func TestResponsiveTableLiveAndLegacy(t *testing.T) {
	source := "| a | b |\n|---|---|\n| one | 2 |"
	if got, want := responsiveLines(t, source, 30, true), responsiveLines(t, source, 30, false); !slices.Equal(got, want) {
		t.Fatalf("live %q != settled %q", got, want)
	}
	rendered, _, deferred := RenderWithCache(source, "", true, nil)
	if !deferred || !strings.Contains(ui.CellsToString(style.ParseCells(rendered, ui.StyleClear)), "│ one │ 2") {
		t.Fatal("changed append-only output")
	}
}

func TestResponsiveTableKeepsCompleteLinkAndAlignment(t *testing.T) {
	url := "https://example.com/this/is/a/long/destination/that/must/not/be/truncated"
	source := "| Key | Value |\n|:---|---:|\n| [docs](" + url + ") | 123 |"
	rendered, _, _ := RenderWithWidth(source, "", false, nil, 40)
	text := ui.CellsToString(style.ParseCells(rendered, ui.StyleClear))
	// Recover the URL across the visual line breaks without padding/gutters.
	compact := strings.NewReplacer("│", "", " ", "", "\n", "").Replace(text)
	if !strings.Contains(compact, url) {
		t.Fatalf("truncated link: %s", text)
	}
	lines := responsiveLines(t, "| A | B |\n|:---|---:|\n| text | 12345 |\n| next | 7 |", 30, false)
	if !strings.HasSuffix(lines[len(lines)-1], "    7") {
		t.Fatalf("lost right alignment: %q", lines)
	}
}
