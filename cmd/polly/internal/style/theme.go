package style

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	tcellcolor "github.com/gdamore/tcell/v3/color"
	ui "github.com/metaspartan/gotui/v5"
)

// The role vocabulary a theme names: seven semantic roles, seven syntax-token
// roles whose unset value falls back to a semantic role, the nine
// masthead-bird colors, and the two surface roles. Call sites keep using the strings (Styled(text,
// "muted", "bold"), chromeColor("accent")); a theme file uses them as keys, and
// Apply registers the resolved color of every one of them in
// ui.StyleParserColorMap under exactly these names.
const (
	roleOK     = "ok"
	roleErr    = "err"
	roleRun    = "run"
	roleAccent = "accent"
	roleActive = "active"
	roleMuted  = "muted"
	roleCode   = "code"

	roleSynComment = "syn-comment"
	roleSynKeyword = "syn-keyword"
	roleSynString  = "syn-string"
	roleSynNumber  = "syn-number"
	roleSynFunc    = "syn-func"
	roleSynAdd     = "syn-add"
	roleSynDel     = "syn-del"

	// The surface roles: the color behind every cell that names no background,
	// and the color of text that names no foreground. Both default to inherit,
	// the terminal's own, and only the managed TUI paints them (see Surface).
	roleBackground = "background"
	roleForeground = "foreground"
)

// The value syntax.
const (
	// valueInherit drops styling: the terminal's default foreground, which
	// gotui's parser already knows as ui.ColorClear.
	valueInherit = "inherit"
	// valuePalettePrefix introduces an explicit ANSI palette slot, 0-255.
	valuePalettePrefix = "palette:"
)

var (
	// The four groups of the vocabulary, in the order the documentation lists
	// them: semanticRoles, birdRoles and surfaceRoles have built-in colors
	// (builtinColors), tokenRoles fall back to a semantic role (tokenFallbacks).
	semanticRoles = []string{roleOK, roleErr, roleRun, roleAccent, roleActive, roleMuted, roleCode}
	tokenRoles    = []string{roleSynComment, roleSynKeyword, roleSynString, roleSynNumber, roleSynFunc, roleSynAdd, roleSynDel}
	birdRoles     = []string{"polly-green", "polly-light", "polly-wing", "polly-crown", "polly-beak", "polly-mouth", "polly-face", "polly-eye", "polly-foot"}
	surfaceRoles  = []string{roleBackground, roleForeground}
	// roleNames is the complete vocabulary: 7 semantic + 7 token + 9 bird + 2
	// surface roles.
	roleNames = slices.Concat(semanticRoles, tokenRoles, birdRoles, surfaceRoles)
)

// tokenFallbacks maps a token role to the semantic role its color comes from
// while the theme leaves it unset, so a token role renders exactly as the
// semantic role it borrowed before it had a name of its own.
var tokenFallbacks = map[string]string{
	roleSynComment: roleMuted,
	roleSynKeyword: roleAccent,
	roleSynString:  roleOK,
	roleSynNumber:  roleActive,
	roleSynFunc:    roleCode,
	roleSynAdd:     roleOK,
	roleSynDel:     roleErr,
}

// inheritDeniedRoles are the roles that reject "inherit". Two heuristics
// compare these roles' resolved colors: repl_agents.go's link hit test matches
// cells by exact style equality, and wrap.go's gutter detector compares the
// first cell's foreground to accent and muted. A clear color would break both.
var inheritDeniedRoles = map[string]bool{roleAccent: true, roleMuted: true}

// builtinColors is polly's own mapping, the color behind every role a theme
// leaves unset: the seven semantic roles on ANSI palette slots (XTerm 0-15)
// that the terminal remaps to its active theme — unlike gotui's dark*/cyan
// names, which resolve to fixed RGB and ignore it — and the masthead bird in
// true color. Quiet variants are produced with the "dim" modifier at the call
// site, not by a darker fixed color here.
var builtinColors = map[string]ui.Color{
	roleOK:         ui.ColorGreen,  // success ✓
	roleErr:        ui.ColorRed,    // failure ✗ / errors
	roleRun:        ui.ColorTeal,   // running-tool arrow (ANSI cyan, XTerm6)
	roleAccent:     ui.ColorBlue,   // prompts & interactive markers
	roleActive:     ui.ColorYellow, // status-bar active turn
	roleMuted:      ui.ColorGrey,   // metadata (ANSI bright-black, XTerm8)
	roleCode:       ui.ColorWhite,  // fenced code block contents
	"polly-green":  pollyGreen,
	"polly-light":  pollyLight,
	"polly-wing":   pollyWing,
	"polly-crown":  pollyCrown,
	"polly-beak":   pollyBeak,
	"polly-mouth":  pollyMouth,
	"polly-face":   pollyFace,
	"polly-eye":    pollyEye,
	"polly-foot":   pollyFoot,
	roleBackground: ui.ColorClear, // the terminal's own background
	roleForeground: ui.ColorClear, // the terminal's own text color
}

// parserColorNames is gotui's own name table, frozen before polly registers a
// single role into the live map. Theme values resolve against it, so a value
// naming one of polly's own roles ("accent": "muted") is a deterministic
// INVALID_COLOR instead of a self-reference. Package-level variables are
// initialized before any init function, which is what makes this snapshot
// ordering safe.
var parserColorNames = func() map[string]ui.Color {
	names := make(map[string]ui.Color, len(ui.StyleParserColorMap))
	for name, color := range ui.StyleParserColorMap {
		names[name] = color
	}
	return names
}()

// Sentinel load errors. Every theme load failure wraps exactly one of these, so
// a caller can classify it with errors.Is instead of matching messages — the
// set_theme tool maps them onto UNKNOWN_ROLE / INVALID_COLOR / INVALID_THEME.
var (
	// ErrUnknownRole reports a role name outside the 25-role vocabulary.
	ErrUnknownRole = errors.New("unknown role")
	// ErrInvalidColor reports a value that is none of the accepted forms, or
	// "inherit" on a role that cannot be cleared.
	ErrInvalidColor = errors.New("invalid color")
	// ErrInvalidTheme reports a document that is not a theme at all: malformed
	// JSON, an unknown top-level key, or a layer of the wrong shape.
	ErrInvalidTheme = errors.New("invalid theme")
)

// Theme is a named set of colors for the role vocabulary. A theme is one look:
// one that wants a light and a dark version is two themes. Legacy lists the
// retired top-level keys ("variant", "light", "dark") a parsed document still
// carried; they are accepted and ignored so an older file keeps loading, and
// the caller reports them.
type Theme struct {
	Name   string
	Colors map[string]string
	Legacy []string
}

// DefaultTheme returns polly's built-in preset: exactly the mapping polly
// registered by hand before themes existed, with the token roles deliberately
// unset so they follow their semantic fallback's resolved color.
func DefaultTheme() Theme {
	colors := make(map[string]string, len(builtinColors))
	for role, color := range builtinColors {
		colors[role] = colorValue(color)
	}
	return Theme{Name: "default", Colors: colors}
}

// themeFile is the on-disk shape. Only name and colors mean anything; the
// three retired keys are decoded solely so they are not unknown. Any other
// top-level key is a load error, never a silently ignored setting.
type themeFile struct {
	Name    string            `json:"name"`
	Colors  map[string]string `json:"colors"`
	Variant json.RawMessage   `json:"variant"`
	Light   json.RawMessage   `json:"light"`
	Dark    json.RawMessage   `json:"dark"`
}

// ParseTheme reads a theme document. name is the theme's name (the file's base
// name or the --theme value) and is used when the document declares none. An
// unknown role name, an unknown top-level key, or an unparseable value is a
// wrapped error; nothing here is fatal to the process, so a caller prints a
// notice and falls back to DefaultTheme.
func ParseTheme(name string, data []byte) (Theme, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var file themeFile
	if err := dec.Decode(&file); err != nil {
		return Theme{}, fmt.Errorf("theme %q: %w: %v", name, ErrInvalidTheme, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return Theme{}, fmt.Errorf("theme %q: %w: unexpected trailing data", name, ErrInvalidTheme)
	}
	if file.Name == "" {
		file.Name = name
	}
	theme := Theme{Name: file.Name, Colors: file.Colors}
	for _, legacy := range []struct {
		key   string
		value json.RawMessage
	}{{"variant", file.Variant}, {"light", file.Light}, {"dark", file.Dark}} {
		if legacy.value != nil {
			theme.Legacy = append(theme.Legacy, legacy.key)
		}
	}
	if err := theme.Validate(); err != nil {
		return Theme{}, err
	}
	return theme, nil
}

// ParseColorValue resolves one role value:
//
//	#rrggbb / #rgb      true color ("#rgb" is expanded to six digits here;
//	                    tcell would not)
//	palette:0..255      an explicit ANSI slot, 0-15 still terminal-remappable
//	inherit             ui.ColorClear, the terminal's default foreground
//	a name gotui knows  the parser color of that name ("green", "darkred", …)
//
// Names resolve against the parser's table as it was before polly registered
// its roles, so naming a polly role here is ErrInvalidColor rather than a
// cycle.
func ParseColorValue(v string) (ui.Color, error) {
	switch {
	case v == valueInherit:
		return ui.ColorClear, nil
	case v == "":
		return ui.ColorClear, fmt.Errorf("%w: empty value", ErrInvalidColor)
	case strings.HasPrefix(v, "#"):
		return parseHexColor(v)
	case strings.HasPrefix(v, valuePalettePrefix):
		index, err := strconv.Atoi(strings.TrimPrefix(v, valuePalettePrefix))
		if err != nil || index < 0 || index > 255 {
			return ui.ColorClear, fmt.Errorf("%w %q: want palette:0 through palette:255", ErrInvalidColor, v)
		}
		return tcellcolor.PaletteColor(index), nil
	}
	if color, ok := parserColorNames[v]; ok {
		return color, nil
	}
	return ui.ColorClear, fmt.Errorf("%w %q: want #rrggbb, palette:N with N 0-255, inherit, or a name gotui knows", ErrInvalidColor, v)
}

// parseHexColor parses #rgb or #rrggbb. Anything else, including a
// non-hexadecimal digit anywhere, is ErrInvalidColor.
func parseHexColor(v string) (ui.Color, error) {
	digits := v[1:]
	switch len(digits) {
	case 3:
		digits = string([]byte{digits[0], digits[0], digits[1], digits[1], digits[2], digits[2]})
	case 6:
	default:
		return ui.ColorClear, fmt.Errorf("%w %q: want #rgb or #rrggbb", ErrInvalidColor, v)
	}
	rgb, err := strconv.ParseUint(digits, 16, 32)
	if err != nil {
		return ui.ColorClear, fmt.Errorf("%w %q: want #rgb or #rrggbb", ErrInvalidColor, v)
	}
	return tcellcolor.NewHexColor(int32(rgb)), nil
}

// Validate reports whether every entry names a role from the vocabulary and
// holds a value ParseColorValue accepts. Roles are walked by sorted name so
// that a theme with several problems always reports the same first one.
func (t Theme) Validate() error {
	roles := make([]string, 0, len(t.Colors))
	for role := range t.Colors {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		value := t.Colors[role]
		if !slices.Contains(roleNames, role) {
			return fmt.Errorf("theme %q: colors: %w %q", t.Name, ErrUnknownRole, role)
		}
		if value == valueInherit && inheritDeniedRoles[role] {
			return fmt.Errorf("theme %q: colors: role %q: %w %q: the agent-link hit test and the wrap gutter detect this role by its resolved color", t.Name, role, ErrInvalidColor, value)
		}
		if _, err := ParseColorValue(value); err != nil {
			return fmt.Errorf("theme %q: colors: role %q: %w", t.Name, role, err)
		}
	}
	return nil
}

// Apply registers every role of t in ui.StyleParserColorMap and returns the
// new process-wide style epoch.
//
// gotui reads that map lock-free on the draw path while other goroutines parse
// cells, so Apply must run on the TUI event loop; calling it from a tool or
// worker goroutine is a fatal concurrent-map-write, not a lost update.
//
// Resolution is total, so the table is always complete and no call site can
// resolve to an unknown name: a role the theme does not set — or one whose
// value is not parseable — falls back to its token fallback role, then to
// polly's built-in mapping.
func Apply(t Theme) uint64 {
	for _, role := range roleNames {
		ui.StyleParserColorMap[role] = resolveRole(t, role)
	}
	epochMu.Lock()
	defer epochMu.Unlock()
	styleEpoch++
	return styleEpoch
}

// resolveRole resolves one role, following the token → semantic fallback
// exactly one step (no token role falls back to another token role, so this
// terminates).
func resolveRole(t Theme, role string) ui.Color {
	// Validate rejects inherit on these at load time; Apply cannot report an
	// error, so this is the last line of defense for the two roles whose
	// heuristics break on a clear color.
	if value, ok := t.Colors[role]; ok && !(value == valueInherit && inheritDeniedRoles[role]) {
		if color, err := ParseColorValue(value); err == nil {
			return color
		}
	}
	if fallback, ok := tokenFallbacks[role]; ok {
		return resolveRole(t, fallback)
	}
	return builtinColors[role]
}

// Surface reports the resolved foreground and background roles: what a cell
// that names no color of its own is painted with. ui.ColorClear means the
// terminal's default. Like every read of the parser map it belongs on the TUI
// event loop.
func Surface() (fg, bg ui.Color) {
	return ui.StyleParserColorMap[roleForeground], ui.StyleParserColorMap[roleBackground]
}

var (
	epochMu    sync.Mutex
	styleEpoch uint64
)

// Epoch reports the process-wide style epoch, which every Apply bumps. Anything
// caching a resolved ui.Cell, ui.Style or ui.Color — not a role name — must
// discard it when the epoch changes.
func Epoch() uint64 {
	epochMu.Lock()
	defer epochMu.Unlock()
	return styleEpoch
}

// colorValue renders a resolved color in the theme value syntax: palette slots
// 0-255 stay palette references, so a preset built from them still lets the
// terminal's own theme choose the RGB; fixed colors become #rrggbb; the clear
// color becomes inherit.
func colorValue(c ui.Color) string {
	if c == ui.ColorClear {
		return valueInherit
	}
	if index, ok := paletteIndex(c); ok {
		return valuePalettePrefix + strconv.Itoa(index)
	}
	return fmt.Sprintf("#%06x", c.Hex())
}

// paletteIndex reports whether c is a plain ANSI palette slot (XTerm 0-255)
// rather than a fixed RGB color, and returns the slot.
func paletteIndex(c ui.Color) (int, bool) {
	v := uint32(c)
	if v&uint32(tcellcolor.IsValid) == 0 || v&uint32(tcellcolor.IsRGB) != 0 || v&uint32(tcellcolor.IsSpecial) != 0 {
		return 0, false
	}
	index := int(v &^ uint32(tcellcolor.IsValid))
	if index > 255 {
		return 0, false
	}
	return index, true
}
