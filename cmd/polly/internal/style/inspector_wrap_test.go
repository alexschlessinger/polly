package style

import (
	"slices"
	"strings"
	"testing"

	ui "github.com/metaspartan/gotui/v5"
)

func TestInspectorWrapPreservesContentAndStyles(t *testing.T) {
	// Include aligned output, Unicode, a long path, and literal shell syntax.
	for _, text := range []string{
		"alpha    beta gamma delta epsilon",
		"  ls -l \"$f\" | awk '{print $5}'",
		"cmd/polly/internal/termimg/assets/logo.png: PNG image data, 418 x 418",
		"cd /Users/alex/.pollytool/worktrees/56fb08bea51eafe6fc1f07e792ca978a/slot-0005/tree &&",
		"echo abcdefghijklmnopqrstuvwxyz0123456789",
		"界界界界界界界界界界界界界界界界界界界",
	} {
		for _, width := range []int{12, 20, 40, 80} {
			source := ParseCells(Styled("│ ", "muted", "")+Styled(text, "ok", ""), ui.StyleClear)
			rows := ui.SplitCells(WrapInspectorCells(source, width), '\n')
			var recovered []ui.Cell
			indent := min(len(text)-len(strings.TrimLeft(text, " ")), width/4)
			for i, row := range rows {
				if CellsWidth(row) > width {
					t.Fatalf("width %d overflow: %q", width, ui.CellsToString(row))
				}
				prefix := 2
				if i > 0 {
					prefix += 2 + indent
					if !strings.HasPrefix(ui.CellsToString(row), "│   ") {
						t.Fatalf("continuation has no indent: %q", ui.CellsToString(row))
					}
				}
				recovered = append(recovered, row[prefix:]...)
			}
			if !slices.Equal(recovered, source[2:]) {
				t.Fatalf("width %d changed content/style: %q => %q", width, text, ui.CellsToString(recovered))
			}
		}
	}
}

func TestInspectorWrapPrefersWordAndPathBoundaries(t *testing.T) {
	for _, tc := range []struct {
		text  string
		width int
		want  []string
	}{
		{"alpha beta gamma delta", 16, []string{"│ alpha beta ", "│   gamma delta"}},
		{"cmd/polly/internal/termimg/logo.png", 20, []string{"│ cmd/polly/", "│   internal/", "│   termimg/logo.png"}},
		{"cd /Users/alex/.pollytool/worktrees/slot-0005/tree &&", 32, []string{"│ cd /Users/alex/.pollytool/", "│   worktrees/slot-0005/tree &&"}},
		{"cd /abcdefghijklmnopqrstuvwxyz", 20, []string{"│ cd /abcdefghijklmn", "│   opqrstuvwxyz"}},
		{"echo abcdefghijklmnopqrstuvwxyz", 20, []string{"│ echo abcdefghijklm", "│   nopqrstuvwxyz"}},
	} {
		cells := ParseCells(Styled("│ ", "muted", "")+tc.text, ui.StyleClear)
		got := transcriptRowsText(ui.SplitCells(WrapInspectorCells(cells, tc.width), '\n'))
		if !slices.Equal(got, tc.want) {
			t.Fatalf("wrapped = %#v, want %#v", got, tc.want)
		}
	}
}
