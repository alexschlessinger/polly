package style

import (
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"

	tcellcolor "github.com/gdamore/tcell/v3/color"
	ui "github.com/metaspartan/gotui/v5"
)

// restoreDefaultTheme puts the process-global parser map back the way init left
// it. The map is shared with every other test in this binary — the wrap tests
// read accent and muted out of it — so any test that applies a theme must
// restore it.
func restoreDefaultTheme(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { Apply(DefaultTheme()) })
}

func TestRoleVocabularyIsComplete(t *testing.T) {
	restoreDefaultTheme(t)
	if len(roleNames) != 25 {
		t.Fatalf("role vocabulary has %d names, want 25", len(roleNames))
	}
	Apply(DefaultTheme())
	for _, role := range roleNames {
		color, known := ui.StyleParserColorMap[role]
		if !known {
			t.Fatalf("role %q is not in the parser map", role)
		}
		// The surface roles are the terminal's own by default.
		if surface := slices.Contains(surfaceRoles, role); (color == ui.ColorClear) != surface {
			t.Fatalf("role %q resolved to %v", role, color)
		}
	}
}

// The built-in preset must stay byte-identical to the mapping polly registered
// by hand before themes existed: the semantic roles on terminal-remappable
// palette slots, not on fixed RGB values that would ignore the terminal's theme.
func TestDefaultThemeKeepsTheLegacyMapping(t *testing.T) {
	restoreDefaultTheme(t)
	want := map[string]ui.Color{
		"ok":     ui.ColorGreen,
		"err":    ui.ColorRed,
		"run":    ui.ColorTeal,
		"accent": ui.ColorBlue,
		"active": ui.ColorYellow,
		"muted":  ui.ColorGrey,
		"code":   ui.ColorWhite,

		"polly-green": pollyGreen,
		"polly-light": pollyLight,
		"polly-wing":  pollyWing,
		"polly-crown": pollyCrown,
		"polly-beak":  pollyBeak,
		"polly-mouth": pollyMouth,
		"polly-face":  pollyFace,
		"polly-eye":   pollyEye,
		"polly-foot":  pollyFoot,
	}
	if len(want) != 16 {
		t.Fatalf("legacy mapping has %d roles, want 16", len(want))
	}
	theme := DefaultTheme()
	if theme.Name != "default" {
		t.Fatalf("DefaultTheme() name = %q, want default", theme.Name)
	}
	if err := theme.Validate(); err != nil {
		t.Fatalf("DefaultTheme().Validate() = %v", err)
	}
	Apply(theme)
	for role, color := range want {
		if got := ui.StyleParserColorMap[role]; got != color {
			t.Errorf("role %q = %v, want %v", role, got, color)
		}
	}
}

func TestDefaultThemeLeavesTokenRolesOnTheirFallback(t *testing.T) {
	restoreDefaultTheme(t)
	fallback := map[string]string{
		"syn-comment": "muted",
		"syn-keyword": "accent",
		"syn-string":  "ok",
		"syn-number":  "active",
		"syn-func":    "code",
		"syn-add":     "ok",
		"syn-del":     "err",
	}
	theme := DefaultTheme()
	for token := range fallback {
		if _, set := theme.Colors[token]; set {
			t.Fatalf("DefaultTheme() sets token role %q; it must stay unset", token)
		}
	}
	Apply(theme)
	for token, role := range fallback {
		got, want := ui.StyleParserColorMap[token], ui.StyleParserColorMap[role]
		if got != want {
			t.Errorf("role %q = %v, want %s = %v", token, got, role, want)
		}
	}
}

func TestParseColorValue(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    ui.Color
		wantErr error
	}{
		{name: "truecolor", value: "#8ab4f8", want: tcellcolor.NewHexColor(0x8ab4f8)},
		{name: "truecolor uppercase", value: "#8AB4F8", want: tcellcolor.NewHexColor(0x8ab4f8)},
		{name: "shorthand expands", value: "#abc", want: tcellcolor.NewHexColor(0xaabbcc)},
		{name: "shorthand uppercase expands", value: "#AbC", want: tcellcolor.NewHexColor(0xaabbcc)},
		{name: "black", value: "#000000", want: tcellcolor.NewHexColor(0x000000)},
		{name: "palette first slot", value: "palette:0", want: tcellcolor.PaletteColor(0)},
		{name: "palette last slot", value: "palette:255", want: tcellcolor.PaletteColor(255)},
		{name: "palette XTerm slot", value: "palette:8", want: tcellcolor.PaletteColor(8)},
		{name: "inherit", value: "inherit", want: ui.ColorClear},
		{name: "parser name green", value: "green", want: ui.StyleParserColorMap["green"]},
		{name: "parser name grey", value: "grey", want: ui.StyleParserColorMap["grey"]},
		{name: "parser name darkred", value: "darkred", want: ui.StyleParserColorMap["darkred"]},
		{name: "parser name clear", value: "clear", want: ui.ColorClear},

		{name: "bad hex digit", value: "#gggggg", wantErr: ErrInvalidColor},
		{name: "short hex", value: "#abcde", wantErr: ErrInvalidColor},
		{name: "long hex", value: "#abcd", wantErr: ErrInvalidColor},
		{name: "bare hash", value: "#", wantErr: ErrInvalidColor},
		{name: "palette out of range", value: "palette:256", wantErr: ErrInvalidColor},
		{name: "palette negative", value: "palette:-1", wantErr: ErrInvalidColor},
		{name: "palette without index", value: "palette:", wantErr: ErrInvalidColor},
		{name: "palette without number", value: "palette:red", wantErr: ErrInvalidColor},
		{name: "empty", value: "", wantErr: ErrInvalidColor},
		{name: "unknown name", value: "chartreuse", wantErr: ErrInvalidColor},
		{name: "whitespace", value: " green ", wantErr: ErrInvalidColor},
		{name: "semantic role is not a value", value: "accent", wantErr: ErrInvalidColor},
		{name: "bird role is not a value", value: "polly-green", wantErr: ErrInvalidColor},
		{name: "token role is not a value", value: "syn-comment", wantErr: ErrInvalidColor},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseColorValue(tc.value)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ParseColorValue(%q) error = %v, want %v", tc.value, err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if got != tc.want {
				t.Fatalf("ParseColorValue(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// gotui's table, not tcell's W3C names, is the source of a value like "cyan"
// (gotui maps it to light cyan, tcell to the aqua slot).
func TestParseColorValueUsesTheParserTable(t *testing.T) {
	got, err := ParseColorValue("cyan")
	if err != nil {
		t.Fatalf("ParseColorValue(cyan) = %v", err)
	}
	if got != ui.ColorLightCyan || got != ui.StyleParserColorMap["cyan"] {
		t.Fatalf("ParseColorValue(cyan) = %v, want gotui's %v", got, ui.ColorLightCyan)
	}
}

func TestValidateRejectsInheritOnLinkColors(t *testing.T) {
	tests := []struct {
		role    string
		wantErr error
	}{
		{role: "accent", wantErr: ErrInvalidColor},
		{role: "muted", wantErr: ErrInvalidColor},
		{role: "ok"},
		{role: "code"},
		{role: "syn-comment"},
		{role: "polly-eye"},
	}
	for _, tc := range tests {
		t.Run(tc.role, func(t *testing.T) {
			theme := Theme{Name: "inherit-test", Colors: map[string]string{tc.role: "inherit"}}
			if err := theme.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate(%s=inherit) = %v, want %v", tc.role, err, tc.wantErr)
			}
			if !errors.Is(Theme{Name: "inherit-test"}.Validate(), nil) {
				t.Fatal("an empty theme must validate")
			}
		})
	}
}

func TestParseTheme(t *testing.T) {
	tests := []struct {
		name    string
		arg     string
		data    string
		want    Theme
		wantErr error
	}{
		{
			name: "colors only defaults the name",
			arg:  "solar-polly",
			data: `{"colors":{"accent":"#8ab4f8"}}`,
			want: Theme{Name: "solar-polly", Colors: map[string]string{"accent": "#8ab4f8"}},
		},
		{
			name: "declared name wins",
			arg:  "file-name",
			data: `{"name":"solar-polly","colors":{"muted":"palette:8"}}`,
			want: Theme{Name: "solar-polly", Colors: map[string]string{"muted": "palette:8"}},
		},
		{
			// An older file keeps loading: the retired keys are reported and
			// nothing in them applies or is even validated.
			name: "retired keys are ignored and listed",
			arg:  "solar-polly",
			data: `{"variant":"solarized","colors":{"accent":"green"},"light":{"acccent":"#gggggg"},"dark":{}}`,
			want: Theme{Name: "solar-polly", Colors: map[string]string{"accent": "green"}, Legacy: []string{"variant", "light", "dark"}},
		},
		{
			name: "bird and token roles are ordinary roles",
			arg:  "bird",
			data: `{"colors":{"polly-eye":"#31312a","syn-func":"#c58af9"}}`,
			want: Theme{Name: "bird", Colors: map[string]string{"polly-eye": "#31312a", "syn-func": "#c58af9"}},
		},
		{
			name: "empty colors",
			arg:  "empty",
			data: `{"name":"empty","colors":{}}`,
			want: Theme{Name: "empty", Colors: map[string]string{}},
		},

		{name: "unknown role", arg: "t", data: `{"colors":{"acccent":"green"}}`, wantErr: ErrUnknownRole},
		{name: "unknown top-level key", arg: "t", data: `{"colour":{},"colors":{}}`, wantErr: ErrInvalidTheme},
		{name: "unknown top-level key with layers", arg: "t", data: `{"colors":{},"variants":{}}`, wantErr: ErrInvalidTheme},
		{name: "bad value", arg: "t", data: `{"colors":{"accent":"#gggggg"}}`, wantErr: ErrInvalidColor},
		{name: "polly role as a value", arg: "t", data: `{"colors":{"accent":"muted"}}`, wantErr: ErrInvalidColor},
		{name: "inherit on accent", arg: "t", data: `{"colors":{"accent":"inherit"}}`, wantErr: ErrInvalidColor},
		{name: "malformed json", arg: "t", data: `{"colors":`, wantErr: ErrInvalidTheme},
		{name: "empty document", arg: "t", data: ``, wantErr: ErrInvalidTheme},
		{name: "trailing data", arg: "t", data: `{"colors":{}} {"colors":{}}`, wantErr: ErrInvalidTheme},
		{name: "colors of the wrong shape", arg: "t", data: `{"colors":["accent"]}`, wantErr: ErrInvalidTheme},
		{name: "value of the wrong type", arg: "t", data: `{"colors":{"accent":5}}`, wantErr: ErrInvalidTheme},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseTheme(tc.arg, []byte(tc.data))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ParseTheme(%s) error = %v, want %v", tc.data, err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseTheme(%s) = %#v, want %#v", tc.data, got, tc.want)
			}
		})
	}
}

func TestApplyResolvesSetRolesAndFallbacks(t *testing.T) {
	restoreDefaultTheme(t)
	Apply(Theme{Name: "plain", Colors: map[string]string{"accent": "#222222", "muted": "palette:8"}})
	accent := tcellcolor.NewHexColor(0x222222)
	if got := ui.StyleParserColorMap["accent"]; got != accent {
		t.Errorf("accent = %v, want %v", got, accent)
	}
	if got := ui.StyleParserColorMap["muted"]; got != tcellcolor.PaletteColor(8) {
		t.Errorf("muted = %v, want %v", got, tcellcolor.PaletteColor(8))
	}
	if got := ui.StyleParserColorMap["ok"]; got != ui.ColorGreen {
		t.Errorf("unset role ok = %v, want the built-in %v", got, ui.ColorGreen)
	}
	if got := ui.StyleParserColorMap["syn-keyword"]; got != accent {
		t.Errorf("unset token role syn-keyword = %v, want accent's %v", got, accent)
	}
}

// Apply cannot report an error, so it is total: a theme that skipped
// ParseTheme/Validate still leaves every role resolvable.
func TestApplyKeepsTheTableCompleteForUnparseableValues(t *testing.T) {
	restoreDefaultTheme(t)
	broken := Theme{
		Name:   "broken",
		Colors: map[string]string{"accent": "#gggggg", "code": "inherit", "muted": "palette:999"},
	}
	Apply(broken)
	if got := ui.StyleParserColorMap["accent"]; got != ui.ColorBlue {
		t.Errorf("invalid accent = %v, want the built-in %v", got, ui.ColorBlue)
	}
	if got := ui.StyleParserColorMap["muted"]; got != ui.ColorGrey {
		t.Errorf("invalid muted = %v, want the built-in %v", got, ui.ColorGrey)
	}
	if got := ui.StyleParserColorMap["code"]; got != ui.ColorClear {
		t.Errorf("inherit code = %v, want %v", got, ui.ColorClear)
	}
	for _, role := range roleNames {
		if _, known := ui.StyleParserColorMap[role]; !known {
			t.Fatalf("role %q is not in the parser map", role)
		}
	}
}

// Validate rejects inherit on accent and muted with ErrInvalidColor, and Apply
// refuses to clear them even when a hand-built theme skips validation: the
// agent-link hit test and the wrap gutter detector compare those resolved
// colors.
func TestApplyRefusesToClearLinkColors(t *testing.T) {
	restoreDefaultTheme(t)
	cleared := Theme{Name: "cleared", Colors: map[string]string{"accent": "inherit", "muted": "inherit"}}
	Apply(cleared)
	if got := ui.StyleParserColorMap["accent"]; got != ui.ColorBlue {
		t.Errorf("inherit accent = %v, want the built-in %v", got, ui.ColorBlue)
	}
	if got := ui.StyleParserColorMap["muted"]; got != ui.ColorGrey {
		t.Errorf("inherit muted = %v, want the built-in %v", got, ui.ColorGrey)
	}
	if got := ui.StyleParserColorMap["syn-keyword"]; got != ui.ColorBlue {
		t.Errorf("inherit accent left syn-keyword at %v, want %v", got, ui.ColorBlue)
	}
}

func TestApplyBumpsTheEpoch(t *testing.T) {
	restoreDefaultTheme(t)
	first := Apply(DefaultTheme())
	second := Apply(DefaultTheme())
	if second <= first {
		t.Fatalf("epoch did not increase: %d then %d", first, second)
	}
	if got := Epoch(); got != second {
		t.Fatalf("Epoch() = %d, want %d", got, second)
	}
}

// Rendering paths read the epoch while an apply bumps it, so the pair has to be
// race-free; run this with -race.
func TestEpochSurvivesConcurrentReads(t *testing.T) {
	restoreDefaultTheme(t)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = Epoch()
				}
			}
		}()
	}
	var last uint64
	theme := DefaultTheme()
	for range 50 {
		last = Apply(theme)
	}
	close(stop)
	wg.Wait()
	if got := Epoch(); got != last {
		t.Fatalf("Epoch() = %d, want the last applied %d", got, last)
	}
}

// The surface roles default to the terminal's own colors and are set like any
// other role.
func TestSurfaceRolesResolve(t *testing.T) {
	restoreDefaultTheme(t)
	Apply(DefaultTheme())
	if fg, bg := Surface(); fg != ui.ColorClear || bg != ui.ColorClear {
		t.Fatalf("default surface = %v/%v, want clear/clear", fg, bg)
	}
	theme, err := ParseTheme("paper", []byte(`{"colors": {"background": "#fdf6e3"}}`))
	if err != nil {
		t.Fatal(err)
	}
	Apply(theme)
	if fg, bg := Surface(); fg != ui.ColorClear || bg.Hex() != 0xfdf6e3 {
		t.Fatalf("surface = %v/%#x, want clear/0xfdf6e3", fg, bg.Hex())
	}
}
