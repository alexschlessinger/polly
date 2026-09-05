package main

import (
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"

	ui "github.com/metaspartan/gotui/v5"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Capture the same Draw buffer, choosing fonts by actual glyph coverage.
// gotui's stock exporter forces all symbols through Apple Symbols, which
// drops some checkmarks/arrows even when the primary font supports them.
type previewRenderer struct {
	faces []font.Face
	w, h  int
}

func newPreviewRenderer() *previewRenderer {
	r := &previewRenderer{}
	for _, path := range []string{"/System/Library/Fonts/Menlo.ttc", "/System/Library/Fonts/Apple Symbols.ttf"} {
		data, err := os.ReadFile(path)
		if err == nil {
			r.addFont(data)
		}
	}
	r.addFont(gomono.TTF)
	r.w = font.MeasureString(r.faces[0], "M").Ceil()
	r.h = r.faces[0].Metrics().Height.Ceil() + 2
	return r
}

func (r *previewRenderer) addFont(data []byte) {
	coll, err := opentype.ParseCollection(data)
	if err != nil {
		return
	}
	f, err := coll.Font(0)
	if err != nil {
		return
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: 12, DPI: 72, Hinting: font.HintingFull})
	if err == nil {
		r.faces = append(r.faces, face)
	}
}

func (r *previewRenderer) close() {
	for _, f := range r.faces {
		f.Close()
	}
}

func toRGBA(c ui.Color) color.RGBA {
	r, g, b := c.RGB()
	return color.RGBA{uint8(r), uint8(g), uint8(b), 255}
}

func (r *previewRenderer) capture(p *playground) *image.RGBA {
	buf := ui.NewBuffer(p.GetRect())
	p.Draw(buf)
	img := image.NewRGBA(image.Rect(0, 0, buf.Dx()*r.w, buf.Dy()*r.h))
	for y := 0; y < buf.Dy(); y++ {
		for x := 0; x < buf.Dx(); x++ {
			cell := buf.GetCell(image.Pt(x, y))
			fg, bg := cell.Style.Fg, cell.Style.Bg
			if bg == ui.ColorClear {
				bg = rgb(25, 27, 38)
			}
			if fg == ui.ColorClear {
				fg = rgb(204, 211, 234)
			}
			if cell.Style.Modifier&ui.ModifierReverse != 0 {
				fg, bg = bg, fg
			}
			if cell.Style.Modifier&ui.ModifierDim != 0 {
				fg = mix(bg, fg, .5)
			}
			box := image.Rect(x*r.w, y*r.h, (x+1)*r.w, (y+1)*r.h)
			draw.Draw(img, box, &image.Uniform{C: toRGBA(bg)}, image.Point{}, draw.Src)
			if cell.Rune == 0 || cell.Rune == ' ' {
				continue
			}
			face := r.faces[0]
			for _, candidate := range r.faces {
				if _, _, ok := candidate.GlyphBounds(cell.Rune); ok {
					face = candidate
					break
				}
			}
			d := font.Drawer{Dst: img, Src: &image.Uniform{C: toRGBA(fg)}, Face: face,
				Dot: fixed.P(x*r.w, y*r.h+face.Metrics().Ascent.Ceil()+1)}
			d.DrawString(string(cell.Rune))
		}
	}
	return img
}

func savePreview(path string, p *playground) error {
	r := newPreviewRenderer()
	defer r.close()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, r.capture(p))
}

// Use ramps from the lab's inks instead of a general purpose GIF cube:
// the background stays exact and dim text retains its color.
func previewPalette() color.Palette {
	bg := rgb(25, 27, 38)
	inks := []ui.Color{
		rgb(204, 211, 234), rgb(113, 159, 255), rgb(106, 229, 201),
		rgb(193, 141, 255), rgb(246, 191, 105), rgb(246, 123, 149),
		rgb(118, 129, 156), rgb(237, 248, 255), rgb(42, 38, 36),
		rgb(152, 188, 228), rgb(150, 185, 246), rgb(181, 160, 248),
	}
	colors := color.Palette{toRGBA(bg)}
	for _, ink := range inks {
		for i := 1; i <= 21; i++ {
			colors = append(colors, toRGBA(mix(bg, ink, float64(i)/21)))
		}
	}
	return colors
}
