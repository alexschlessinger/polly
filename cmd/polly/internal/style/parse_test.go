package style

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"

	tcell "github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

func gotuiLiteralCells(text string, defaultStyle ui.Style) []ui.Cell {
	cells := ui.ParseStyles(text, defaultStyle)
	for i := range cells {
		cells[i].Rune, _ = styledLiteralRune(cells[i].Rune)
	}
	return cells
}

func checkParseCells(t *testing.T, text string) {
	t.Helper()
	defaults := []ui.Style{
		ui.StyleClear,
		{Fg: ui.ColorRed, Bg: ui.ColorBlue, Modifier: ui.ModifierBold | ui.ModifierDim},
	}
	for _, defaultStyle := range defaults {
		// Some malformed inputs panic in gotui. The fallback deliberately keeps
		// that behavior; differential fuzzing must compare it as well as cells.
		parse := func(fn func(string, ui.Style) []ui.Cell) (cells []ui.Cell, panicked bool) {
			defer func() { panicked = recover() != nil }()
			return fn(text, defaultStyle), false
		}
		want, wantPanic := parse(gotuiLiteralCells)
		got, gotPanic := parse(ParseCells)
		if gotPanic != wantPanic || !reflect.DeepEqual(got, want) {
			t.Fatalf("input %q, default %+v: got %v (panic %v), want %v (panic %v)", text, defaultStyle, got, gotPanic, want, wantPanic)
		}
	}
}

func TestParseCellsMatchesGotui(t *testing.T) {
	inputs := []string{
		"", "plain\ntext\twith ] and ( )", "日本語 e\u0301 🦜", "invalid \xff\xfe text",
		Escape("[literal](fg:red)"), Styled("[literal] 日本語", "accent", "bold"),
		StyledBg("background", "text", "surface"), KeyHints([2]string{"q", "quit"}, [2]string{"[", "previous"}),
		"[red](fg:red) and [blue](bg:blue,mod:bold)", "[]()", "[plain]()",
		"[unknown](fg:no-such-color,bg:no-such-color,mod:no-such-modifier)",
		"[hex](fg:#abcdef,bg:#123456)", "[order](fg:red,fg:blue,mod:bold,mod:dim)",
		"[ignored](fg:red:blue, bg:blue,fg :red,nothing,other:red,mod:bold:dim)",
		"[empty](fg:,bg:,mod:)", "[paren](fg:red(ignored) suffix)",
		"[invalid](fg:\xff)", "[invalid \xff](fg:red)",
		"[bold](mod:bold)[reverse](mod:reverse)[dim](mod:dim)[blink](mod:blink)[italic](mod:italic)[strike](mod:strike)[underline](mod:underline)",
		"[", "[unfinished", "[balanced]", "[balanced] text", "[body](", "[body](fg:red",
		"[outer [inner] text](fg:red)", "[outer [inner](fg:blue) text](fg:red)",
		"[one][two](fg:red)", "[one](fg:red)[unfinished", "[[[]]](fg:red)",
	}
	for _, input := range inputs {
		checkParseCells(t, input)
	}
}

func TestParseCellsGeneratedInputsMatchGotui(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	fragments := []string{
		"plain", " ", "\n", "日本語🦜", "\xf0\x80", "[", "]", "(", ")", ":", ",", "[]",
		Escape("[literal]"), Styled("source [i]", "accent", "bold"),
		"[a](fg:red)", "[b](bg:blue,mod:italic)", "[c](fg:#123456)",
		"[d](mod:unknown)", "[e](fg:red:blue)", "[f](fg:nonexistent)",
	}
	for range 2000 {
		var text strings.Builder
		for n := rng.Intn(20); n > 0; n-- {
			text.WriteString(fragments[rng.Intn(len(fragments))])
		}
		checkParseCells(t, text.String())
	}
}

func TestParseCellsUsesCurrentColorMap(t *testing.T) {
	const name = "parse-cells-test-role"
	previous, existed := ui.StyleParserColorMap[name]
	t.Cleanup(func() {
		if existed {
			ui.StyleParserColorMap[name] = previous
		} else {
			delete(ui.StyleParserColorMap, name)
		}
	})
	text := "[role](fg:" + name + ")"
	for _, color := range []ui.Color{ui.ColorRed, tcell.NewRGBColor(12, 34, 56)} {
		ui.StyleParserColorMap[name] = color
		checkParseCells(t, text)
		if got := ParseCells(text, ui.StyleClear)[0].Style.Fg; got != color {
			t.Fatalf("color = %v, want current theme color %v", got, color)
		}
	}
}

func FuzzParseCellsMatchesGotui(f *testing.F) {
	for _, text := range []string{"plain", "[a](fg:red)", "[a[b]c](mod:bold)", "[unterminated", "[a](fg:\xff)", Escape("[]")} {
		f.Add(text)
	}
	f.Fuzz(func(t *testing.T, text string) { checkParseCells(t, text) })
}

var parsedCells []ui.Cell

func BenchmarkParseCells(b *testing.B) {
	inputs := []struct{ name, text string }{
		{"plain", strings.Repeat("a line of output with 日本語 and 🦜\n", 100)},
		{"styled", strings.Repeat(Styled("diff --git a/example.go b/example.go", "accent", "bold")+"\n"+Styled("+ return values[index]", "green", "")+"\n", 100)},
	}
	for _, input := range inputs {
		for _, parser := range []struct {
			name string
			fn   func(string, ui.Style) []ui.Cell
		}{{"direct", ParseCells}, {"gotui", gotuiLiteralCells}} {
			b.Run(input.name+"/"+parser.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					parsedCells = parser.fn(input.text, ui.StyleClear)
				}
			})
		}
	}
}
