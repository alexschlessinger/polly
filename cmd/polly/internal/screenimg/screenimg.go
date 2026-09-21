// Package screenimg rasterizes a text screen into an image, so the TUI can
// write a PNG of the frame it painted: the cells, their colors and the theme's
// surface substitution are exactly what the screen holds, with no terminal
// capture, screen-recording permission or external converter in the way.
//
// Cells are read from the live screen rather than rebuilt from the widget tree
// because the screen holds the finished frame: the themed screen's default
// color substitution and every direct cell write (affordance overlays, idle
// cues) are already in it. Native terminal graphics are escape sequences
// rather than cells, so image placements are absent by construction.
package screenimg

import (
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"unicode/utf8"

	tcell "github.com/gdamore/tcell/v3"
	tcellcolor "github.com/gdamore/tcell/v3/color"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"github.com/alexschlessinger/pollytool/images"
)

// Source is the cell grid a capture reads. A tcell.Screen satisfies it.
type Source interface {
	Size() (width, height int)
	Get(x, y int) (str string, style tcell.Style, width int)
}

// DefaultForeground and DefaultBackground stand in for the cells a frame leaves
// at the terminal's own colors, which is what polly's built-in default theme
// asks for in both surface roles. A PNG cell cannot be transparent, so a
// capture assumes a dark terminal.
var (
	DefaultForeground = tcellcolor.NewHexColor(0xd0d4e0)
	DefaultBackground = tcellcolor.NewHexColor(0x14151a)
)

// fontFiles are tried in order for glyph coverage; embedded faces keep text
// and TUI symbols readable when no system fonts are available.
var fontFiles = []string{
	"/System/Library/Fonts/Menlo.ttc",
	"/System/Library/Fonts/Apple Symbols.ttf",
	"/System/Library/Fonts/Supplemental/Arial Unicode.ttf",
}

// DejaVu Sans Mono supplies symbols missing from Go Mono on hosts without the
// optional system faces. Its license and release source accompany the asset.
//
//go:embed assets/DejaVuSansMono.ttf
var dejavuMonoTTF []byte

// Overlay is a picture a surface placed over the cell grid: what a terminal
// would show there through its own graphics protocol. A capture fits it to the
// cells it was placed on and paints it last, so an image polly placed appears
// in the PNG over the cells it covered.
type Overlay struct {
	// Rect is the cell rectangle the image was fitted to, and Visible is the
	// part of it on screen.
	Rect    image.Rectangle
	Visible image.Rectangle
	// Image is the placement's source. Render fits it to Rect at its own cell
	// size and draws it one pixel per screen pixel from Rect's top-left
	// corner, the way a prepared placement is drawn into the terminal.
	Image image.Image
}

// Render paints one font cell per screen cell into a new image. fg and bg are
// the colors a cell that names none of its own is painted with.
func Render(src Source, fg, bg tcellcolor.Color, overlays ...Overlay) *image.RGBA {
	w, h := src.Size()
	if w < 1 || h < 1 {
		return image.NewRGBA(image.Rect(0, 0, 0, 0))
	}
	renderMu.Lock()
	defer renderMu.Unlock()
	set := faces()
	cellW, cellH := set.cellSize()
	img := image.NewRGBA(image.Rect(0, 0, w*cellW, h*cellH))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: toRGBA(bg)}, image.Point{}, draw.Src)

	type glyph struct {
		text string
		face font.Face
		x, y int
		col  color.RGBA
	}
	var glyphs []glyph
	for y := 0; y < h; y++ {
		for x := 0; x < w; {
			text, st, width := src.Get(x, y)
			if width < 1 {
				// A continuation cell of a wide rune; the leader owns both cells.
				x++
				continue
			}
			cellFg, cellBg := resolveCell(st, fg, bg)
			// A wide rune covers both of its cells, so the fill spans them and
			// the continuation cell is never filled on its own.
			box := image.Rect(x*cellW, y*cellH, (x+width)*cellW, (y+1)*cellH)
			draw.Draw(img, box, &image.Uniform{C: cellBg}, image.Point{}, draw.Src)
			if text != "" && text != " " {
				face, ascent := set.forText(text, st.HasBold())
				glyphs = append(glyphs, glyph{
					text: text,
					face: face,
					x:    x * cellW,
					y:    y*cellH + ascent,
					col:  cellFg,
				})
			}
			x += width
		}
	}
	// Every background is painted before any glyph, so a wide rune draws across
	// both of its cells instead of being clipped by its neighbour's fill.
	for _, g := range glyphs {
		d := font.Drawer{Dst: img, Src: &image.Uniform{C: g.col}, Face: g.face, Dot: fixed.P(g.x, g.y)}
		d.DrawString(g.text)
	}
	// Images go on top of the text layer, the way a terminal draws them, and
	// over the cells their placement locked: a thumbnail covers whole cells.
	for _, overlay := range overlays {
		if overlay.Image == nil {
			continue
		}
		slot := image.Rect(overlay.Rect.Min.X*cellW, overlay.Rect.Min.Y*cellH,
			overlay.Rect.Max.X*cellW, overlay.Rect.Max.Y*cellH)
		visible := image.Rect(overlay.Visible.Min.X*cellW, overlay.Visible.Min.Y*cellH,
			overlay.Visible.Max.X*cellW, overlay.Visible.Max.Y*cellH)
		box := slot.Intersect(visible).Intersect(img.Bounds())
		if box.Empty() {
			continue
		}
		// Fitted to the whole slot before clipping, so a placement scrolled
		// partly out of the pane shows the matching part of its image.
		fitted := images.Fit(overlay.Image, slot.Dx(), slot.Dy())
		offset := box.Min.Sub(slot.Min).Add(fitted.Bounds().Min)
		draw.Draw(img, box, fitted, offset, draw.Over)
	}
	return img
}

// SavePNG writes the capture of src to path, creating missing parent
// directories so a caller can name a fresh output directory.
func SavePNG(path string, src Source, fg, bg tcellcolor.Color, overlays ...Overlay) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create screenshot directory: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create screenshot: %w", err)
	}
	defer f.Close()
	if err := png.Encode(f, Render(src, fg, bg, overlays...)); err != nil {
		return fmt.Errorf("encode screenshot: %w", err)
	}
	return nil
}

// CellSize is the pixel box one screen cell occupies in a capture, measured from
// the same default face Render draws with. A surface that places images itself
// reports it so those images land on the capture's own pixels.
func CellSize() (int, int) {
	renderMu.Lock()
	defer renderMu.Unlock()
	return faces().cellSize()
}

// resolveCell resolves one cell's colors: the cell's own, the capture defaults
// for a cell that names neither, reverse video swapped, and dim mixed toward
// the background the way a terminal renders it.
func resolveCell(st tcell.Style, fg, bg tcellcolor.Color) (color.RGBA, color.RGBA) {
	cellFg, cellBg := fg, bg
	if c := st.GetForeground(); c.Valid() {
		cellFg = c
	}
	if c := st.GetBackground(); c.Valid() {
		cellBg = c
	}
	if st.HasReverse() {
		cellFg, cellBg = cellBg, cellFg
	}
	if st.HasDim() {
		cellFg = mix(cellBg, cellFg)
	}
	return toRGBA(cellFg), toRGBA(cellBg)
}

// faces is the font set every capture draws with, loaded once: the system font
// files run to megabytes. An opentype face is not safe for concurrent use, so
// renderMu serializes the callers that share the set.
var (
	faces    = sync.OnceValue(loadFaces)
	renderMu sync.Mutex
)

// faceSet is the loaded font faces plus the metrics every cell uses.
type faceSet struct {
	faces []font.Face
	bold  font.Face
}

func loadFaces() *faceSet {
	set := &faceSet{}
	for _, path := range fontFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		face, err := faceFrom(data, 0)
		if err != nil {
			continue
		}
		set.faces = append(set.faces, face)
		// The first file that loads is the default face; its second face is
		// the bold cut of the same family (Menlo.ttc ships regular then bold).
		if len(set.faces) == 1 {
			if bold, err := faceFrom(data, 1); err == nil {
				set.bold = bold
			}
		}
	}
	set.faces = append(set.faces, mustFace(gomono.TTF), mustFace(dejavuMonoTTF))
	if set.bold == nil {
		set.bold = mustFace(gomonobold.TTF)
	}
	return set
}

var faceOptions = &opentype.FaceOptions{Size: 12, DPI: 72, Hinting: font.HintingFull}

// faceFrom loads face index of a font file; a single-face file is a collection
// of one.
func faceFrom(data []byte, index int) (font.Face, error) {
	collection, err := opentype.ParseCollection(data)
	if err != nil {
		return nil, err
	}
	fnt, err := collection.Font(index)
	if err != nil {
		return nil, err
	}
	return opentype.NewFace(fnt, faceOptions)
}

func mustFace(data []byte) font.Face {
	face, err := faceFrom(data, 0)
	if err != nil {
		// The embedded faces are parsed by this package's own tests; a
		// failure here would mean the embedded asset is corrupt.
		panic(fmt.Sprintf("screenimg: load embedded font: %v", err))
	}
	return face
}

// cellSize is the pixel box one screen cell occupies, measured from the default
// face so every cell aligns.
func (s *faceSet) cellSize() (int, int) {
	face := s.faces[0]
	return font.MeasureString(face, "M").Ceil(), face.Metrics().Height.Ceil() + 2
}

// forText picks the face for a cell's text: the first face with coverage for
// its first rune, in the bold cut when the style asks for it and the family
// has one. It returns the baseline offset inside the cell with the face.
func (s *faceSet) forText(text string, bold bool) (font.Face, int) {
	first, _ := utf8.DecodeRuneInString(text)
	face := s.faces[0]
	for _, candidate := range s.faces {
		if _, _, ok := candidate.GlyphBounds(first); ok {
			face = candidate
			break
		}
	}
	if bold && face == s.faces[0] && s.bold != nil {
		face = s.bold
	}
	return face, face.Metrics().Ascent.Ceil() + 1
}

func toRGBA(c tcellcolor.Color) color.RGBA {
	r, g, b := c.RGB()
	return color.RGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: 255}
}

// mix returns the color halfway between a and b, which is how a terminal dims a
// foreground toward its background.
func mix(a, b tcellcolor.Color) tcellcolor.Color {
	ar, ag, ab := a.RGB()
	br, bg, bb := b.RGB()
	return tcellcolor.NewRGBColor((ar+br)/2, (ag+bg)/2, (ab+bb)/2)
}
