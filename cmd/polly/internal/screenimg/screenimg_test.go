package screenimg

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	tcell "github.com/gdamore/tcell/v3"
)

type fakeCell struct {
	text  string
	style tcell.Style
	width int
}

// fakeSource is a fixed grid of cells; every cell not named is a blank one at
// the terminal's default colors.
type fakeSource struct {
	w, h  int
	cells map[image.Point]fakeCell
}

func (f fakeSource) Size() (int, int) { return f.w, f.h }

func (f fakeSource) Get(x, y int) (string, tcell.Style, int) {
	cell, ok := f.cells[image.Pt(x, y)]
	if !ok {
		return " ", tcell.StyleDefault, 1
	}
	width := cell.width
	if width < 1 {
		width = 1
	}
	return cell.text, cell.style, width
}

func TestRenderFillsCellsAndMeasuresThem(t *testing.T) {
	cellW, cellH := loadFaces().cellSize()
	img := Render(fakeSource{w: 3, h: 2}, DefaultForeground, DefaultBackground)
	if got, want := img.Bounds(), image.Rect(0, 0, 3*cellW, 2*cellH); got != want {
		t.Fatalf("bounds = %v, want %v", got, want)
	}
	// A grid of blank default cells is exactly the capture's background.
	blank := Render(fakeSource{w: 2, h: 1}, DefaultForeground, DefaultBackground)
	want := toRGBA(DefaultBackground)
	for y := 0; y < cellH; y++ {
		for x := 0; x < 2*cellW; x++ {
			if got := blank.RGBAAt(x, y); got != want {
				t.Fatalf("blank cell pixel (%d,%d) = %v, want %v", x, y, got, want)
			}
		}
	}
}

func TestResolveCellAppliesReverseAndDim(t *testing.T) {
	// Dim mixes the foreground toward the background; the background is
	// untouched, so a blank dim cell looks like any other cell.
	fg, bg := resolveCell(tcell.StyleDefault.Dim(true), DefaultForeground, DefaultBackground)
	if want := toRGBA(mix(DefaultBackground, DefaultForeground, 0.5)); fg != want {
		t.Fatalf("dim foreground = %v, want %v", fg, want)
	}
	if want := toRGBA(DefaultBackground); bg != want {
		t.Fatalf("dim background = %v, want %v", bg, want)
	}

	// Reverse swaps them, which a blank cell shows as a background.
	fg, bg = resolveCell(tcell.StyleDefault.Reverse(true), DefaultForeground, DefaultBackground)
	if want := toRGBA(DefaultBackground); fg != want {
		t.Fatalf("reverse foreground = %v, want %v", fg, want)
	}
	if want := toRGBA(DefaultForeground); bg != want {
		t.Fatalf("reverse background = %v, want %v", bg, want)
	}
	cellW, cellH := loadFaces().cellSize()
	reverse := Render(fakeSource{w: 1, h: 1, cells: map[image.Point]fakeCell{
		image.Pt(0, 0): {text: " ", style: tcell.StyleDefault.Reverse(true)},
	}}, DefaultForeground, DefaultBackground)
	if got := reverse.RGBAAt(cellW-1, cellH-1); got != toRGBA(DefaultForeground) {
		t.Fatalf("reverse cell pixel = %v, want the foreground", got)
	}

	// A cell's own colors win over the capture defaults.
	ink := tcell.NewHexColor(0x123456)
	paper := tcell.NewHexColor(0xabcdef)
	fg, bg = resolveCell(tcell.StyleDefault.Foreground(ink).Background(paper), DefaultForeground, DefaultBackground)
	if fg != toRGBA(ink) || bg != toRGBA(paper) {
		t.Fatalf("cell colors = %v/%v, want %v/%v", fg, bg, toRGBA(ink), toRGBA(paper))
	}
}

func TestRenderSpreadsWideCellsOverBothColumns(t *testing.T) {
	cellW, cellH := loadFaces().cellSize()
	red := tcell.NewHexColor(0xff0000)
	img := Render(fakeSource{w: 2, h: 1, cells: map[image.Point]fakeCell{
		image.Pt(0, 0): {text: " ", style: tcell.StyleDefault.Background(red), width: 2},
	}}, DefaultForeground, DefaultBackground)
	for _, x := range []int{0, cellW, 2*cellW - 1} {
		if got, want := img.RGBAAt(x, cellH-1), toRGBA(red); got != want {
			t.Fatalf("wide cell pixel x=%d = %v, want the cell background %v", x, got, want)
		}
	}
}

func TestRenderDrawsGlyphs(t *testing.T) {
	cellW, cellH := loadFaces().cellSize()
	ink := tcell.NewHexColor(0x00ff00)
	img := Render(fakeSource{w: 1, h: 1, cells: map[image.Point]fakeCell{
		image.Pt(0, 0): {text: "M", style: tcell.StyleDefault.Foreground(ink)},
	}}, DefaultForeground, DefaultBackground)
	background := toRGBA(DefaultBackground)
	drawn := 0
	for y := 0; y < cellH; y++ {
		for x := 0; x < cellW; x++ {
			if img.RGBAAt(x, y) != background {
				drawn++
			}
		}
	}
	if drawn == 0 {
		t.Fatalf("no pixel of the %dx%d cell carried a glyph", cellW, cellH)
	}
}

func TestSavePNGDecodesAndCreatesDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shots", "nested", "screen.png")
	cellW, cellH := loadFaces().cellSize()
	if err := SavePNG(path, fakeSource{w: 2, h: 2}, DefaultForeground, DefaultBackground); err != nil {
		t.Fatalf("SavePNG: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open screenshot: %v", err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("decode screenshot: %v", err)
	}
	if got, want := img.Bounds(), image.Rect(0, 0, 2*cellW, 2*cellH); got != want {
		t.Fatalf("decoded bounds = %v, want %v", got, want)
	}

	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}
	if err := SavePNG(filepath.Join(blocked, "screen.png"), fakeSource{w: 1, h: 1}, DefaultForeground, DefaultBackground); err == nil {
		t.Fatal("SavePNG under a regular file succeeded, want an error")
	}
}

func TestRenderRejectsEmptyGrid(t *testing.T) {
	img := Render(fakeSource{}, DefaultForeground, DefaultBackground)
	if got := img.Bounds(); got != image.Rect(0, 0, 0, 0) {
		t.Fatalf("empty grid bounds = %v, want an empty image", got)
	}
}
