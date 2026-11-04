package main

import (
	"image"
	"image/color"
	"image/draw"
	"math"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	tcell "github.com/gdamore/tcell/v3"
)

// renderContextChart draws absolute token counts from zero to the local maximum,
// not fractions of a model's context window (which can change between samples).
// Call it on the UI goroutine, where the theme roles are resolved, and keep the
// returned pixels immutable while termimg prepares them. progress reveals the
// history left to right once on opening or a data change; at 1 it is static.
func renderContextChart(values []int, compacted []bool, width, height int, progress float64) image.Image {
	width, height = max(0, width), max(0, height)
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	if width == 0 || height == 0 {
		return img
	}

	// modalParagraph clears to the terminal default; themedScreen substitutes
	// the surface background only when the theme specifies one. Inherit stays
	// transparent here too, so Kitty uses the actual terminal background and
	// screenshots composite on their usual screenshotSurface fallback. Sixel
	// uses the Unicode chart instead when this background is unresolved.
	_, bg := style.Surface()
	if bg.Valid() {
		draw.Draw(img, img.Bounds(), image.NewUniform(contextChartColor(bg)), image.Point{}, draw.Src)
	}
	fg, _ := screenshotSurface()
	roleColor := func(role string) color.RGBA {
		c := chromeColor(role)
		if !c.Valid() {
			c = fg
		}
		return contextChartColor(c)
	}
	line, grid, marker := roleColor("accent"), roleColor("muted"), roleColor("active")
	// A thin raster line needs more contrast than a solid terminal glyph.
	// Tint toward the surface foreground: softer on dark themes, darker on
	// light ones, while retaining the accent hue rather than a fixed palette.
	ink := contextChartColor(fg)
	line.R = uint8((int(line.R) + int(ink.R)) / 2)
	line.G = uint8((int(line.G) + int(ink.G)) / 2)
	line.B = uint8((int(line.B) + int(ink.B)) / 2)

	padX, padY := min(8, width/10), min(6, height/8)
	left, right := padX, width-1-padX
	top, bottom := float64(padY), float64(height-1-padY)
	span := bottom - top
	for i := 0; i <= 3; i++ {
		y := int(math.Round(top + span*float64(i)/3))
		for x := left; x <= right; x++ {
			contextChartBlend(img, x, y, grid, 0.13)
		}
	}
	if len(values) == 0 {
		return img
	}
	if math.IsNaN(progress) {
		progress = 1
	}
	progress = max(0, min(1, progress))
	if progress == 0 {
		return img
	}

	// A little headroom keeps the highest sample from touching the top grid.
	peak := 1
	for _, value := range values {
		peak = max(peak, value)
	}
	ys := make([]float64, len(values))
	for i, value := range values {
		ys[i] = bottom - float64(max(0, value))/float64(peak)*span*0.9
	}
	curve := contextChartCurve(ys, right-left+1)
	reveal := float64(left) + float64(right-left)*progress
	coverage := make([]float64, width*height)
	if len(values) == 1 {
		// One observation is a point, not an invented stretch of history.
		x := float64(right)
		contextChartStroke(coverage, width, height, x, ys[0], x, ys[0], 1.5)
	} else {
		for x := left; x <= right && float64(x) <= reveal; x++ {
			y := curve[x-left]
			for py := max(0, int(math.Floor(y))); py <= int(bottom); py++ {
				depth := (float64(py) - y) / max(1, bottom-y)
				edge := max(0, min(1, float64(py)+0.5-y))
				contextChartBlend(img, x, py, line, edge*(0.10-0.07*max(0, depth)))
			}
		}
		if left == right {
			contextChartStroke(coverage, width, height, float64(left), curve[0], float64(left), curve[0], 0.9)
		}
		for x := left; x < right && float64(x) < reveal; x++ {
			x2 := min(float64(x+1), reveal)
			y1, y2 := curve[x-left], curve[x+1-left]
			y2 = y1 + (y2-y1)*(x2-float64(x))
			contextChartStroke(coverage, width, height, float64(x), y1, x2, y2, 0.9)
		}
	}
	for i, compact := range compacted {
		if !compact || i >= len(values) {
			continue
		}
		x := float64(right)
		if len(values) > 1 {
			x = float64(left) + float64(right-left)*float64(i)/float64(len(values)-1)
		}
		if x > reveal {
			continue
		}
		for y := int(top); y <= int(bottom); y++ {
			if (y-int(top))%7 < 3 {
				contextChartBlend(img, int(math.Round(x)), y, marker, 0.38)
			}
		}
	}
	for i, alpha := range coverage {
		if alpha > 0 {
			contextChartBlend(img, i%width, i/width, line, alpha*0.9)
		}
	}
	return img
}

// contextChartCurve samples a monotone cubic Hermite interpolation. The harmonic
// tangent keeps adjacent segments smooth without overshooting a peak or a
// compaction drop, and never suggests negative token counts.
func contextChartCurve(ys []float64, width int) []float64 {
	curve := make([]float64, width)
	tangents := make([]float64, len(ys))
	if len(ys) > 1 {
		tangents[0] = ys[1] - ys[0]
		tangents[len(ys)-1] = ys[len(ys)-1] - ys[len(ys)-2]
		for i := 1; i < len(ys)-1; i++ {
			a, b := ys[i]-ys[i-1], ys[i+1]-ys[i]
			if a*b > 0 {
				tangents[i] = 2 * a * b / (a + b)
			}
		}
	}
	for x := range curve {
		if len(ys) == 1 || width == 1 {
			curve[x] = ys[len(ys)-1]
			continue
		}
		pos := float64(x) / float64(width-1) * float64(len(ys)-1)
		i := min(int(pos), len(ys)-2)
		t := pos - float64(i)
		t2, t3 := t*t, t*t*t
		curve[x] = (2*t3-3*t2+1)*ys[i] + (t3-2*t2+t)*tangents[i] +
			(-2*t3+3*t2)*ys[i+1] + (t3-t2)*tangents[i+1]
	}
	return curve
}

// contextChartStroke accumulates analytic edge coverage for a round-capped
// segment. Taking the maximum at joins avoids dark knots and needs no graphics
// dependency, oversize intermediate image, or mutable drawing state.
func contextChartStroke(coverage []float64, width, height int, x1, y1, x2, y2, radius float64) {
	fringe := radius + 0.5
	dx, dy := x2-x1, y2-y1
	length2 := dx*dx + dy*dy
	for y := max(0, int(math.Floor(min(y1, y2)-fringe))); y <= min(height-1, int(math.Ceil(max(y1, y2)+fringe))); y++ {
		for x := max(0, int(math.Floor(min(x1, x2)-fringe))); x <= min(width-1, int(math.Ceil(max(x1, x2)+fringe))); x++ {
			t := 0.0
			if length2 > 0 {
				t = max(0, min(1, ((float64(x)-x1)*dx+(float64(y)-y1)*dy)/length2))
			}
			distance := math.Hypot(float64(x)-x1-t*dx, float64(y)-y1-t*dy)
			alpha := max(0, min(1, fringe-distance))
			coverage[y*width+x] = max(coverage[y*width+x], alpha)
		}
	}
}

func contextChartColor(c tcell.Color) color.RGBA {
	r, g, b := c.RGB()
	return color.RGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: 255}
}

// contextChartBlend writes premultiplied pixels, including on an inherited
// (transparent) modal surface. Both PNG and screenimg preserve that alpha.
func contextChartBlend(img *image.RGBA, x, y int, c color.RGBA, alpha float64) {
	old := img.RGBAAt(x, y)
	mix := func(dst, src uint8) uint8 {
		return uint8(math.Round(float64(dst)*(1-alpha) + float64(src)*alpha))
	}
	img.SetRGBA(x, y, color.RGBA{R: mix(old.R, c.R), G: mix(old.G, c.G), B: mix(old.B, c.B), A: mix(old.A, 255)})
}
