package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	ui "github.com/metaspartan/gotui/v5"
)

func TestToolOutputLinkLayoutMatchesWrappedPrefixes(t *testing.T) {
	above := style.Styled("thought · tools", "muted", "") + "\n" + style.Rail + style.Styled("reasoning wraps too 界 é", "muted", "italic") + "\n" + style.RailBar
	content, _ := railLines(strings.Join([]string{
		"  ✓ first",
		"      " + style.Styled(strings.Repeat("wide 界 é [literal] ", 8), "accent", ""),
		"",
		"  ✓ second",
		"      " + style.Styled(strings.Repeat("unbroken", 15), "muted", ""),
		"    Next output page",
		"  ✓ last",
		"      " + style.Styled("last line", "muted", ""),
		"",
	}, "\n"))
	targets := []toolOutputTarget{
		{recordID: 1, key: "first", line: 0, cols: 7},
		{recordID: 1, key: "second", line: 3, cols: 8},
		{recordID: 1, key: "second", action: "next", line: 5, cols: 16},
		{recordID: 2, key: "last", line: 6, cols: 6},
	}
	for _, width := range []int{1, 4, 11, 40, 140} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			text := above + "\n" + content + "\n" + style.Rail + "Agent section is outside the last output span"
			first := strings.Count(above, "\n") + 1
			end := first + strings.Count(content, "\n") + 1
			wrapped, starts := style.WrapCellsWithLineStarts(style.ParseCells(text, ui.StyleClear), width)
			links := layoutToolOutputLinks(starts, first, end, targets, width)
			if !slices.Equal(wrapped, style.WrapCells(style.ParseCells(text, ui.StyleClear), width)) {
				t.Fatal("link measurement changed the wrapped cells")
			}
			if len(links) != len(targets) {
				t.Fatalf("links = %d, want %d", len(links), len(targets))
			}
			lines := strings.Split(content, "\n")
			for i, target := range targets {
				// The old prefix measurement is deliberately retained here as
				// an independent reference, not in the production hot path.
				prefix := above
				if target.line > 0 {
					prefix += "\n" + strings.Join(lines[:target.line], "\n")
				}
				rows, _ := transcriptBlockRowsWithImages(prefix, false, width, nil, false, 0, 0)
				end := above + "\n" + content
				if i+1 < len(targets) {
					end = above + "\n" + strings.Join(lines[:targets[i+1].line], "\n")
				}
				endRows, _ := transcriptBlockRowsWithImages(end, false, width, nil, false, 0, 0)
				link := links[i]
				if link.Y != len(rows) || link.Rows != len(endRows)-len(rows) {
					t.Fatalf("target %d span = [%d,%d), want [%d,%d)", i, link.Y, link.Y+link.Rows, len(rows), len(endRows))
				}
				if link.recordID != target.recordID || link.key != target.key || link.action != target.action || link.X != activityRailCols || link.Cols != min(target.cols, width-activityRailCols) {
					t.Fatalf("target identity or hitbox changed: %+v for %+v", link, target)
				}
				if link.rect().Dy() != 1 {
					t.Fatalf("body became clickable: %+v", link)
				}
			}
		})
	}
}
