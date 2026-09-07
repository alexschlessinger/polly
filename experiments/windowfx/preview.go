package main

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/png"
	"math"
	"os"

	ui "github.com/metaspartan/gotui/v5"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Like textfx, rasterize the actual gotui buffer with local glyph fallback.
// A bounded glyph cache makes repeated animation frames cheap to export.
type previewRenderer struct {
	faces []font.Face
	w, h  int
	tiles map[ui.Cell]*image.RGBA
}

func newPreviewRenderer() *previewRenderer {
	r := &previewRenderer{tiles: make(map[ui.Cell]*image.RGBA)}
	for _, path := range []string{"/System/Library/Fonts/Menlo.ttc", "/System/Library/Fonts/Apple Symbols.ttf"} {
		if data, err := os.ReadFile(path); err == nil {
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
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: 13, DPI: 72, Hinting: font.HintingFull})
	if err == nil {
		r.faces = append(r.faces, face)
	}
}

func (r *previewRenderer) close() {
	for _, f := range r.faces {
		f.Close()
	}
}

func rgba(c ui.Color) color.RGBA {
	r, g, b := c.RGB()
	return color.RGBA{uint8(r), uint8(g), uint8(b), 255}
}

func (r *previewRenderer) tile(cell ui.Cell) *image.RGBA {
	if tile := r.tiles[cell]; tile != nil {
		return tile
	}
	img := image.NewRGBA(image.Rect(0, 0, r.w, r.h))
	fg, bg := cell.Style.Fg, cell.Style.Bg
	draw.Draw(img, img.Bounds(), &image.Uniform{C: rgba(bg)}, image.Point{}, draw.Src)
	if cell.Rune != 0 && cell.Rune != ' ' {
		face := r.faces[0]
		for _, candidate := range r.faces {
			if _, _, ok := candidate.GlyphBounds(cell.Rune); ok {
				face = candidate
				break
			}
		}
		d := font.Drawer{Dst: img, Src: &image.Uniform{C: rgba(fg)}, Face: face,
			Dot: fixed.P(0, face.Metrics().Ascent.Ceil()+1)}
		d.DrawString(string(cell.Rune))
		if cell.Style.Modifier&ui.ModifierBold != 0 {
			d.Dot = fixed.P(0, face.Metrics().Ascent.Ceil()+1)
			d.Dot.X += 32
			d.DrawString(string(cell.Rune))
		}
	}
	if len(r.tiles) >= 16000 {
		clear(r.tiles)
	}
	r.tiles[cell] = img
	return img
}

func (r *previewRenderer) capture(p *playground) *image.RGBA {
	buf := ui.NewBuffer(p.GetRect())
	p.Draw(buf)
	img := image.NewRGBA(image.Rect(0, 0, buf.Dx()*r.w, buf.Dy()*r.h))
	for y := 0; y < buf.Dy(); y++ {
		for x := 0; x < buf.Dx(); x++ {
			tile := r.tile(buf.GetCell(image.Pt(x, y)))
			box := image.Rect(x*r.w, y*r.h, (x+1)*r.w, (y+1)*r.h)
			draw.Draw(img, box, tile, image.Point{}, draw.Src)
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
	if err := png.Encode(f, r.capture(p)); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func previewPalette() color.Palette {
	var result color.Palette
	seen := make(map[color.RGBA]bool)
	add := func(c ui.Color) {
		rgb := rgba(c)
		if !seen[rgb] && len(result) < 256 {
			result = append(result, rgb)
			seen[rgb] = true
		}
	}
	add(desk)
	add(ink)
	add(subtle)
	add(sea)
	for _, a := range ansiSwatches {
		add(a.rgb)
	}
	for _, style := range styles {
		s := style.colors
		for _, c := range []ui.Color{s.bg, s.fg, s.muted, s.edge, s.accent, s.second, s.title} {
			add(c)
		}
	}
	for _, style := range styles {
		s := style.colors
		for i := 1; i <= 5; i++ {
			add(mix(s.edge, s.accent, float64(i)/6))
			add(mix(s.accent, s.second, float64(i)/6))
		}
	}
	return result
}

func saveGIF(path string, p *playground, seconds float64) error {
	frames := int(math.Round(seconds * 12))
	if p.GetRect().Dx()*p.GetRect().Dy()*frames > 1800000 {
		return fmt.Errorf("GIF is too large; reduce --width, --height, or --seconds")
	}
	r := newPreviewRenderer()
	defer r.close()
	palette := previewPalette()
	index := make(map[uint32]uint8)
	out := &gif.GIF{LoopCount: 0}
	for n := 0; n < frames; n++ {
		if p.moving() {
			p.t = float64(n) / 12
		}
		img := r.capture(p)
		frame := image.NewPaletted(img.Bounds(), palette)
		// Cache antialiased colors across frames instead of performing a
		// 256-color search independently for every pixel of every frame.
		for i := 0; i < len(frame.Pix); i++ {
			k := i * 4
			key := uint32(img.Pix[k])<<16 | uint32(img.Pix[k+1])<<8 | uint32(img.Pix[k+2])
			v, ok := index[key]
			if !ok {
				v = uint8(palette.Index(color.RGBA{img.Pix[k], img.Pix[k+1], img.Pix[k+2], 255}))
				index[key] = v
			}
			frame.Pix[i] = v
		}
		out.Image = append(out.Image, frame)
		out.Delay = append(out.Delay, 8+n%3/2)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := gif.EncodeAll(f, out); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
