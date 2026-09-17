package main

import (
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	tcell "github.com/gdamore/tcell/v3"
)

// A cell that leaves a color at the terminal default takes the theme's surface;
// a cell that names its own keeps it, and an unthemed surface changes nothing.
func TestThemedScreenSubstitutesOnlyDefaultColors(t *testing.T) {
	t.Cleanup(func() { style.Apply(style.DefaultTheme()) })
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatal(err)
	}
	defer sim.Fini()
	screen := themedScreen{sim}

	screen.SetContent(0, 0, 'a', nil, tcell.StyleDefault.Foreground(tcell.ColorRed))
	if _, got, _ := sim.Get(0, 0); got != tcell.StyleDefault.Foreground(tcell.ColorRed) {
		t.Fatalf("default theme restyled the cell: %v", got)
	}

	theme, err := style.ParseTheme("paper", []byte(`{"colors": {"background": "#fdf6e3", "foreground": "#333333"}}`))
	if err != nil {
		t.Fatal(err)
	}
	style.Apply(theme)
	fg, bg := style.Surface()

	screen.SetContent(0, 0, 'a', nil, tcell.StyleDefault.Foreground(tcell.ColorRed))
	if _, got, _ := sim.Get(0, 0); got.GetForeground() != tcell.ColorRed || got.GetBackground() != bg {
		t.Fatalf("styled cell = %v/%v, want red on the theme background", got.GetForeground(), got.GetBackground())
	}
	screen.Put(1, 0, "b", tcell.StyleDefault.Background(tcell.ColorBlue))
	if _, got, _ := sim.Get(1, 0); got.GetForeground() != fg || got.GetBackground() != tcell.ColorBlue {
		t.Fatalf("explicit background = %v/%v, want the theme foreground on blue", got.GetForeground(), got.GetBackground())
	}
}

// The sweep keeps its original endpoints on the built-in theme and blends from
// accent to foreground once a theme pins both as RGB.
func TestSweepColorFollowsTheThemeWhenItIsRGB(t *testing.T) {
	t.Cleanup(func() { style.Apply(style.DefaultTheme()) })
	style.Apply(style.DefaultTheme())
	if got := sweepColor(0).Hex(); got != 0x5c75ab {
		t.Fatalf("default base = %#x, want 0x5c75ab", got)
	}
	if got := sweepColor(1).Hex(); got != 0xedf8ff {
		t.Fatalf("default peak = %#x, want 0xedf8ff", got)
	}
	theme, err := style.ParseTheme("paper", []byte(`{"colors": {"accent": "#268bd2", "foreground": "#333333"}}`))
	if err != nil {
		t.Fatal(err)
	}
	style.Apply(theme)
	if base, peak := sweepColor(0).Hex(), sweepColor(1).Hex(); base != 0x268bd2 || peak != 0x333333 {
		t.Fatalf("themed sweep = %#x..%#x, want 0x268bd2..0x333333", base, peak)
	}
}
