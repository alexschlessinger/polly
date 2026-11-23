package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	ui "github.com/metaspartan/gotui/v5"
)

// headerRow paints a header as the transcript does and returns its one row.
func headerRow(t *testing.T, text string, width int) []ui.Cell {
	t.Helper()
	rows := style.VisualRows(text, ui.StyleClear, width)
	if len(rows) != 1 || strings.Contains(text, "\n") {
		t.Fatalf("width %d: a header is one row, got %d: %q", width, len(rows), plainStyledText(text))
	}
	return rows[0]
}

// A header is one row at every width: the triangle, as much of the label as
// fits, and the failure cue. The cue outlasts the label; nothing wraps, no
// grapheme is cut, and the hit target is exactly what was painted.
func TestChunkHeaderFitsOneRowAtEveryWidth(t *testing.T) {
	for _, c := range []activityChunk{
		{kind: activityTools, label: "read 👩‍💻 仙台 é.go and ran 2 commands"},
		{kind: activityTools, label: "read 12 files and ran 3 commands", mark: "1 failed", failed: true},
		{kind: activityTools, label: "editing 🌱👩‍💻界é.go", live: true},
		{kind: activityTools, label: "tests failed", failed: true},
		{kind: activityAgents, label: "2 agents running, 1 completed, 1 failed", live: true, failed: true},
	} {
		for _, open := range []bool{false, true} {
			for width := -1; width <= 96; width++ {
				text, cols, label := renderChunkHeader(&c, width, open)
				where := fmt.Sprintf("%q open=%v width=%d", c.label, open, width)
				if width < 1 {
					if text != "" || cols != 0 || label != (turnDockPlacement{}) {
						t.Fatalf("%s: painted %q without a row", where, text)
					}
					continue
				}
				row := headerRow(t, text, width)
				plain := ui.CellsToString(row)
				if got := style.CellsWidth(row); got > width || got != cols {
					t.Fatalf("%s: painted %d columns, hit target %d: %q", where, got, cols, plain)
				}
				glyph := "▸"
				if open {
					glyph = "▾"
				}
				if !strings.HasPrefix(plain, glyph) {
					t.Fatalf("%s: no fold triangle in %q", where, plain)
				}
				if (strings.ContainsAny(plain, "👩‍💻") && !strings.Contains(plain, "👩‍💻")) || strings.Contains(plain, " e…") || strings.Contains(plain, "e.go") {
					t.Fatalf("%s: split a grapheme: %q", where, plain)
				}
				// The cue stays whole as long as the row has room for it and
				// two columns of label.
				if cue := chunkMarkSeparator + c.mark; c.mark != "" && width >= chunkLabelLead+2+headerCols(cue) && !strings.HasSuffix(plain, cue) {
					t.Fatalf("%s: dropped the failure cue: %q", where, plain)
				}
				if c.mark != "" && width > chunkLabelLead+1 && !strings.Contains(plain, c.mark[:1]) {
					t.Fatalf("%s: no part of the failure cue survived: %q", where, plain)
				}
				// The label's sweep ink is inside the row, after the triangle,
				// and stops before an ellipsis, a cue or an agent's outcomes.
				if label.Cols == 0 {
					continue
				}
				if label.kind != c.kind || label.X != chunkLabelLead || label.X+label.Cols > cols {
					t.Fatalf("%s: label %+v outside the painted %d columns", where, label, cols)
				}
				var ink strings.Builder
				x := 0
				for _, cell := range row {
					if w := style.CellWidth(cell); x >= label.X && x+w <= label.X+label.Cols || w == 0 && x > label.X && x <= label.X+label.Cols {
						ink.WriteRune(cell.Rune)
					}
					x += style.CellWidth(cell)
				}
				if got := ink.String(); strings.Contains(got, "…") || strings.Contains(got, chunkMarkSeparator) || strings.Contains(got, ",") || !strings.HasPrefix(sentenceCase(c.label), strings.TrimRight(got, " ")) {
					t.Fatalf("%s: sweep ink %q is not the label's lead", where, got)
				}
			}
		}
	}
}

// The triangle is the accent; the label is muted once settled, bright while
// a call is in flight, and red when it is itself the failure. A failure cue
// beside a label is the red part, and the label stays muted.
func TestChunkHeaderStyles(t *testing.T) {
	for _, tc := range []struct {
		name  string
		chunk activityChunk
		want  string
	}{
		{"settled", activityChunk{label: "read 3 files"},
			style.Styled("▸", "accent", "bold") + " " + style.Styled("Read 3 files", "muted", "")},
		{"live", activityChunk{label: "reading main.go", live: true},
			style.Styled("▸", "accent", "bold") + " " + style.Styled("Reading main.go", "active", "bold")},
		{"marked", activityChunk{label: "read 3 files", mark: "1 failed", failed: true},
			style.Styled("▸", "accent", "bold") + " " + style.Styled("Read 3 files", "muted", "") + style.Styled(chunkMarkSeparator, "muted", "") + style.Styled("1 failed", "err", "bold")},
		{"the label is the failure", activityChunk{label: "tests failed", failed: true},
			style.Styled("▸", "accent", "bold") + " " + style.Styled("Tests failed", "err", "bold")},
	} {
		if got, _, _ := renderChunkHeader(&tc.chunk, 80, false); got != tc.want {
			t.Fatalf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	open, _, _ := renderChunkHeader(&activityChunk{label: "read 3 files"}, 80, true)
	if !strings.HasPrefix(open, style.Styled("▾", "accent", "bold")) {
		t.Fatalf("open header = %q", open)
	}
}

// A label is text, whatever it holds: control characters, bidi overrides and
// private image markers are dropped, and brackets stay literal rather than
// becoming style markup.
func TestChunkHeaderSanitizesItsLabel(t *testing.T) {
	unsafe := "  read\t[x](fg:err)\n" + string(style.ImageMarker(2)) + "界.go\r\a‮"
	text, cols, label := renderChunkHeader(&activityChunk{kind: activityTools, label: unsafe, mark: "[1 failed]\n", failed: true}, 60, false)
	plain := ui.CellsToString(headerRow(t, text, 60))
	if want := "▸ Read [x](fg:err) 界.go · [1 failed]"; plain != want || strings.ContainsAny(plain, "\r\t\n\a‮"+string(style.ImageMarker(2))) {
		t.Fatalf("unsafe or misparsed literal: %q, want %q", plain, want)
	}
	if cols != headerCols(plain) || label != (turnDockPlacement{kind: activityTools, X: chunkLabelLead, Cols: headerCols("Read [x](fg:err) 界.go")}) {
		t.Fatalf("sanitized geometry: cols=%d label=%+v", cols, label)
	}
}

// A run of one command names it. A row too narrow for the whole command
// shortens paths before it cuts the command off, so the file acted on stays
// in view.
func TestChunkHeaderFitsItsCommandToTheRow(t *testing.T) {
	const command = "go test ./cmd/polly/internal/markdown/render_test.go -run TestRender"
	c := &activityChunk{kind: activityTools, verb: "ran ", command: newBashSummary(command)}
	c.label = c.verb + c.command.fit(chunkLabelCols)
	wide, _, _ := renderChunkHeader(c, 120, false)
	if got := plainStyledText(wide); got != "▸ Ran "+command {
		t.Fatalf("wide header = %q", got)
	}
	for _, width := range []int{60, 48} {
		text, cols, _ := renderChunkHeader(c, width, false)
		plain := ui.CellsToString(headerRow(t, text, width))
		if cols > width || !strings.HasPrefix(plain, "▸ Ran go test ") || !strings.Contains(plain, "render_test.go") {
			t.Fatalf("width %d: the command lost its file: %q", width, plain)
		}
	}
}

// A run of one long script is named by how it begins: no shell options ahead
// of the work, whole commands while they fit, then an ellipsis. The header
// never reads as a stub of every command.
func TestChunkHeaderNamesALongScriptByItsFirstCommands(t *testing.T) {
	const script = "set -e -o pipefail; git diff --check; git ls-files -zco --exclude-standard '*.go' | xargs -0 gofmt -l; git diff -U3 -- cmd/polly/repl_transcript.go | grep -C 7 -E 'hold|until'; git diff --stat | grep -A47 handoff || test \"$?\" = 1"
	c := &activityChunk{kind: activityTools, verb: "ran ", command: newBashSummary(script)}
	c.label = c.verb + c.command.fit(chunkLabelCols)
	for width, want := range map[int]string{
		137: "▸ Ran git diff --check ; git ls-files -zco --exclude-standard '*.go' | xargs -0 gofmt -l ; git diff -U3 -- cmd/…/repl_transcript.go | …",
		80:  "▸ Ran git diff --check ; git ls-files -zco --exclude-st… | xargs -0 gofmt -l ; …",
		40:  "▸ Ran git diff --check ; git ls-fil… | …",
	} {
		text, cols, _ := renderChunkHeader(c, width, false)
		if plain := ui.CellsToString(headerRow(t, text, width)); plain != want || cols != headerCols(want) {
			t.Errorf("width %d: header %q (%d cols), want %q", width, plain, cols, want)
		}
	}
}
