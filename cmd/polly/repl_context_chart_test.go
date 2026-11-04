package main

import (
	"bytes"
	"image"
	"image/color"
	"math"
	"slices"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
)

func TestContextChartEdgeCasesAndDeterminism(t *testing.T) {
	style.Apply(style.DefaultTheme())
	t.Cleanup(func() { style.Apply(style.DefaultTheme()) })
	for _, tc := range []struct {
		name          string
		values        []int
		width, height int
	}{
		{name: "empty", width: 160, height: 60},
		{name: "single", values: []int{17}, width: 160, height: 60},
		{name: "flat", values: []int{17, 17, 17}, width: 160, height: 60},
		{name: "zero", values: []int{0, 0, 0}, width: 160, height: 60},
		{name: "negative", values: []int{-8, -1, -17}, width: 160, height: 60},
		{name: "mixed", values: []int{-8, 100, 60, 120}, width: 160, height: 60},
		{name: "large", values: []int{0, int(^uint(0) >> 1)}, width: 160, height: 60},
		{name: "pixel", values: []int{1, 2, 0}, width: 1, height: 1},
		{name: "column", values: []int{1, 2, 0}, width: 1, height: 16},
		{name: "row", values: []int{1, 2, 0}, width: 16, height: 1},
		{name: "tiny", values: []int{1, 2, 0}, width: 2, height: 3},
		{name: "zero width", values: []int{1}, height: 5},
		{name: "negative dimensions", values: []int{1}, width: -10, height: -3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := slices.Clone(tc.values)
			compacted := []bool{false, true, false, true, true}
			for _, progress := range []float64{0, 0.4, 1} {
				a := renderContextChart(tc.values, compacted, tc.width, tc.height, progress).(*image.RGBA)
				b := renderContextChart(tc.values, compacted, tc.width, tc.height, progress).(*image.RGBA)
				if a.Bounds() != image.Rect(0, 0, max(0, tc.width), max(0, tc.height)) {
					t.Fatalf("bounds = %v", a.Bounds())
				}
				if !bytes.Equal(a.Pix, b.Pix) {
					t.Fatal("chart is not deterministic")
				}
				for i := 0; i < len(a.Pix); i += 4 {
					if a.Pix[i] > a.Pix[i+3] || a.Pix[i+1] > a.Pix[i+3] || a.Pix[i+2] > a.Pix[i+3] {
						t.Fatal("chart contains non-premultiplied RGBA pixels")
					}
				}
			}
			if !slices.Equal(before, tc.values) || !slices.Equal(compacted, []bool{false, true, false, true, true}) {
				t.Fatal("renderer mutated its inputs")
			}
		})
	}
}

func TestContextChartScaleAndReveal(t *testing.T) {
	style.Apply(style.DefaultTheme())
	t.Cleanup(func() { style.Apply(style.DefaultTheme()) })
	render := func(values []int, progress float64) *image.RGBA {
		return renderContextChart(values, nil, 160, 60, progress).(*image.RGBA)
	}
	values := []int{20, 80, 40, 100}
	full, start, half := render(values, 1), render(values, 0), render(values, 0.5)
	if bytes.Equal(full.Pix, start.Pix) || bytes.Equal(half.Pix, start.Pix) || bytes.Equal(half.Pix, full.Pix) {
		t.Fatal("reveal did not advance through distinct frames")
	}
	for y := range 60 {
		for x := 85; x < 160; x++ {
			if half.RGBAAt(x, y) != start.RGBAAt(x, y) {
				t.Fatal("partial reveal painted the unrevealed right side")
			}
		}
	}
	if !bytes.Equal(full.Pix, render(values, 2).Pix) || !bytes.Equal(start.Pix, render(values, -1).Pix) {
		t.Fatal("progress was not clamped")
	}
	if !bytes.Equal(full.Pix, render(values, math.NaN()).Pix) {
		t.Fatal("nonfinite progress did not settle")
	}
	if !bytes.Equal(render([]int{-3, 0, -1}, 1).Pix, render([]int{0, 0, 0}, 1).Pix) {
		t.Fatal("negative token values did not clamp to zero")
	}
	if !bytes.Equal(full.Pix, render([]int{200, 800, 400, 1000}, 1).Pix) {
		t.Fatal("chart did not scale to its local token maximum")
	}
	flat, zero := render([]int{30, 30, 30}, 1), render([]int{0, 0, 0}, 1)
	if bytes.Equal(flat.Pix, zero.Pix) {
		t.Fatal("flat positive history was incorrectly normalized to zero")
	}
}

func TestContextChartCompactionMarkersAndAntialiasing(t *testing.T) {
	style.Apply(style.DefaultTheme())
	t.Cleanup(func() { style.Apply(style.DefaultTheme()) })
	values := []int{20, 100, 40, 70}
	plain := renderContextChart(values, nil, 160, 60, 1).(*image.RGBA)
	marked := renderContextChart(values, []bool{false, false, true}, 160, 60, 1).(*image.RGBA)
	// The third sample is at x=103. Both dash pixels and gaps should remain.
	changed, gaps := 0, 0
	for y := 6; y < 30; y++ {
		if plain.RGBAAt(103, y) == marked.RGBAAt(103, y) {
			gaps++
		} else {
			changed++
		}
	}
	if changed == 0 || gaps == 0 {
		t.Fatalf("compaction marker is not dashed: changed=%d gaps=%d", changed, gaps)
	}
	// Above the area fill and away from the grid, intermediate alpha comes
	// from the antialiased line edge, rather than a staircase of solid pixels.
	edges := 0
	for y := 7; y < 45; y++ {
		for x := 10; x < 150; x++ {
			a := plain.RGBAAt(x, y).A
			if a > 80 && a < 210 {
				edges++
			}
		}
	}
	if edges < 20 {
		t.Fatalf("too few antialiased edge pixels: %d", edges)
	}
}

func TestContextChartSingleRequestKeepsCompactionMarker(t *testing.T) {
	style.Apply(style.DefaultTheme())
	t.Cleanup(func() { style.Apply(style.DefaultTheme()) })
	plain := renderContextChart([]int{100}, nil, 160, 60, 1).(*image.RGBA)
	marked := renderContextChart([]int{100}, []bool{true}, 160, 60, 1).(*image.RGBA)
	if bytes.Equal(plain.Pix, marked.Pix) {
		t.Fatal("the first reported request lost its compaction marker")
	}
}

func TestContextChartBackgroundMatchesModalSurface(t *testing.T) {
	t.Cleanup(func() { style.Apply(style.DefaultTheme()) })
	style.Apply(style.DefaultTheme())
	inherited := renderContextChart(nil, nil, 100, 50, 1).(*image.RGBA)
	if inherited.RGBAAt(0, 0).A != 0 {
		t.Fatal("inherited modal background should show through the native image")
	}
	theme := style.Theme{Name: "paper", Colors: map[string]string{
		"background": "#fdf6e3", "foreground": "#333333", "accent": "#268bd2", "active": "inherit",
	}}
	style.Apply(theme)
	paper := renderContextChart([]int{10, 20, 15}, []bool{false, true}, 100, 50, 1).(*image.RGBA)
	if got := paper.RGBAAt(0, 0); got != (color.RGBA{R: 0xfd, G: 0xf6, B: 0xe3, A: 255}) {
		t.Fatalf("background = %v, want modal surface #fdf6e3", got)
	}
	for i := 3; i < len(paper.Pix); i += 4 {
		if paper.Pix[i] != 255 {
			t.Fatal("explicit modal background has transparent pixels")
		}
	}
}

func TestContextChartCurveDoesNotOvershoot(t *testing.T) {
	for _, values := range [][]float64{{0, 10, 0, 8}, {10, 10, 10}, {0, 0, 0}, {10, 1, 2, 10}} {
		curve := contextChartCurve(values, 301)
		for x, y := range curve {
			pos := float64(x) / 300 * float64(len(values)-1)
			i := min(int(pos), len(values)-2)
			if y < min(values[i], values[i+1])-1e-8 || y > max(values[i], values[i+1])+1e-8 {
				t.Fatalf("curve overshot sample range at %d: %v", x, y)
			}
		}
	}
}
