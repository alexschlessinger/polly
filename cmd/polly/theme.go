package main

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/urfave/cli/v3"
)

// Theme selection: the flags, environment, and file that choose a theme, and
// the one startup apply.
//
// A theme is a named set of colors for the role vocabulary in
// cmd/polly/internal/style/theme.go. This file only finds one and registers
// it; the call sites keep naming roles
// (style.Styled(text, "muted", "bold"), chromeColor("accent")), so nothing
// downstream changes when the theme does.

// The theme vocabulary of this file: the reserved default name, the file
// suffix, and the separator that makes a --theme
// value a path instead of a name.
const (
	// themeNameDefault is the built-in default preset and the reserved theme
	// name: every load failure falls back to it, so a user file of that name
	// would be unreachable behind the fallback and is ignored.
	themeNameDefault = "default"
	// themeFileNameSuffix is the extension a theme file carries. A --theme
	// value ending in it is read as a path rather than looked up by name.
	themeFileNameSuffix = ".json"

	// themePathSeparator is what makes a --theme value a path rather than a
	// name: a value containing it, or ending in themeFileNameSuffix, is read
	// as a file.
	themePathSeparator = "/"
)

// themeSelection is a resolved theme and where it came from: path is the file
// it was read from (empty for a compiled-in preset), and builtin marks the
// presets polly ships. The reload watcher stats path to notice edits.
type themeSelection struct {
	theme   style.Theme
	path    string
	builtin bool
}

// themeConfigFlags is the theme flag: --theme names a preset, a user theme, or
// a path. It reads POLLYTOOL_THEME through envDefault, so precedence is flag,
// environment, ~/.pollytool/config, built-in default, and --help lists the
// variable.
//
// The flag has no Validator: an unknown theme must be a load-time notice plus
// a fallback, never a flag-parse failure that stops polly starting.
func themeConfigFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "theme",
			Value:   themeNameDefault,
			Usage:   "Theme: a preset (default or a shipped theme), a name under ~/.pollytool/themes, or a path to a theme file",
			Sources: envDefault("POLLYTOOL_THEME"),
		},
	}
}

// themeDir is the user themes directory, ~/.pollytool/themes. Nothing here
// creates it: only a persist does (0700), so a run that never writes a theme
// leaves no directory behind.
func themeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, userConfigDirName, "themes"), nil
}

// themeDisplayPath spells the theme a notice is about the way the notices for
// ~/.pollytool/config spell theirs: a path as given, a plain name as the user
// file it was looked for.
func themeDisplayPath(name string) string {
	if isThemePath(name) {
		return name
	}
	return "~/" + userConfigDirName + "/themes/" + name + themeFileNameSuffix
}

// isThemePath reports whether a --theme value is a path rather than a name.
func isThemePath(name string) bool {
	return strings.Contains(name, themePathSeparator) || strings.HasSuffix(name, themeFileNameSuffix)
}

// resolveThemeSelection resolves a --theme value to the theme to apply. A value
// containing "/" or ending in ".json" is read as a path. Otherwise a user file
// ~/.pollytool/themes/<name>.json wins over a same-named compiled-in preset,
// and the name "default" is reserved: a user file of that name is ignored,
// because "default" is what every load failure falls back to.
//
// A returned error is a notice, never fatal: the caller falls back to the
// built-in default preset.
func resolveThemeSelection(name string) (themeSelection, error) {
	if name == "" {
		name = themeNameDefault
	}
	if isThemePath(name) {
		theme, err := loadThemeFile(name, strings.TrimSuffix(filepath.Base(name), themeFileNameSuffix))
		if err != nil {
			return themeSelection{}, err
		}
		return themeSelection{theme: theme, path: name}, nil
	}
	if name != themeNameDefault {
		path, err := themeUserPath(name)
		if err != nil {
			return themeSelection{}, err
		}
		theme, err := loadThemeFile(path, name)
		switch {
		case err == nil:
			return themeSelection{theme: theme, path: path}, nil
		case !errors.Is(err, fs.ErrNotExist):
			// A present but unreadable or invalid user file is the load
			// error the spec describes: notice plus the default preset, not
			// the same-named preset it shadows.
			return themeSelection{}, err
		}
	}
	if preset, ok := builtinThemes()[name]; ok {
		return themeSelection{theme: preset, builtin: true}, nil
	}
	return themeSelection{}, fmt.Errorf("unknown theme %q: want a preset (%s), a file in ~/.pollytool/themes, or a path",
		name, strings.Join(builtinThemeNames(), ", "))
}

// themeUserPath is the user theme file for a plain name.
func themeUserPath(name string) (string, error) {
	dir, err := themeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+themeFileNameSuffix), nil
}

// loadThemeFile reads and validates one theme file. name is the fallback for a
// document that declares none.
func loadThemeFile(path, name string) (style.Theme, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return style.Theme{}, fmt.Errorf("read theme file: %w", err)
	}
	theme, err := style.ParseTheme(name, data)
	if err != nil {
		return style.Theme{}, err
	}
	return theme, nil
}

// shippedThemeFS holds the full themes that ship in the binary: ordinary theme
// files, named by their base name, each painting its own background.
//
//go:embed themes/*.json
var shippedThemeFS embed.FS

// shippedThemes parses the embedded theme files once. A file that fails to
// parse is left out rather than fatal; TestShippedThemesParse keeps that from
// ever happening to a committed file.
var shippedThemes = sync.OnceValue(func() map[string]style.Theme {
	themes := map[string]style.Theme{}
	entries, _ := fs.ReadDir(shippedThemeFS, "themes")
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), themeFileNameSuffix)
		data, err := shippedThemeFS.ReadFile("themes/" + entry.Name())
		if err != nil {
			continue
		}
		if theme, err := style.ParseTheme(name, data); err == nil {
			themes[name] = theme
		}
	}
	return themes
})

// materializeShippedThemes writes each shipped theme that has no file yet into
// ~/.pollytool/themes, so it can be edited like a theme of the user's own: the
// file then shadows the embedded copy and the reload watcher follows it. An
// existing file is never touched — it may hold the user's edits — and deleting
// one brings the shipped version back on the next launch. Failure is silent:
// the embedded copy still resolves by name.
func materializeShippedThemes() {
	dir, err := themeDir()
	if err != nil {
		return
	}
	entries, _ := fs.ReadDir(shippedThemeFS, "themes")
	for _, entry := range entries {
		target := filepath.Join(dir, entry.Name())
		if _, err := os.Lstat(target); err == nil {
			continue
		}
		data, err := shippedThemeFS.ReadFile("themes/" + entry.Name())
		if err != nil {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return
		}
		_ = os.WriteFile(target, data, 0o644)
	}
}

// builtinThemes are the compiled-in presets. default is polly's historical
// mapping (ANSI slots the terminal remaps, so it reads on a light or a dark
// terminal alike); the rest are the shipped theme files.
func builtinThemes() map[string]style.Theme {
	themes := map[string]style.Theme{
		themeNameDefault: style.DefaultTheme(),
	}
	maps.Copy(themes, shippedThemes())
	return themes
}

// allThemeNames lists every name /theme accepts without a path: the presets
// and the user theme files, sorted, a user file that shadows a preset once.
func allThemeNames() []string {
	names := append(builtinThemeNames(), userThemeNames()...)
	sort.Strings(names)
	return slices.Compact(names)
}

// builtinThemeNames lists the presets by name, sorted so a notice or a listing
// reads the same on every run.
func builtinThemeNames() []string {
	return slices.Sorted(maps.Keys(builtinThemes()))
}

// themeSelectionFromConfig reads POLLYTOOL_THEME from ~/.pollytool/config.
//
// It reads the file directly instead of through userConfigValue, which caches
// the parsed file per path for the life of the process and is cleared only by
// writeUserConfig: a hand edit would otherwise be invisible to the reload
// watcher. Malformed lines are ignored here; the flag sources already report
// them once.
func themeSelectionFromConfig() (string, bool) {
	path, err := userConfigPath()
	if err != nil {
		return "", false
	}
	values, _, err := readUserConfig(path)
	if err != nil {
		return "", false
	}
	name, ok := values["POLLYTOOL_THEME"]
	return name, ok
}

// resolveStartupTheme resolves, applies, and reports the theme the process
// starts with, returning the selection and the notice lines to print.
//
// Every failure is a notice: an unknown name, a missing file, or malformed
// JSON leaves polly running on the built-in default preset, because a bad
// theme must never prevent polly from starting.
func resolveStartupTheme(name string) (themeSelection, []string) {
	var notices []string
	selection, err := resolveThemeSelection(name)
	if err != nil {
		notices = append(notices, fmt.Sprintf("polly: %s: %v", themeDisplayPath(name), err))
		selection = themeSelection{theme: style.DefaultTheme(), builtin: true}
	} else if notice := themeLegacyNotice(selection); notice != "" {
		notices = append(notices, notice)
	}
	// The parser map is process-global and gotui reads it lock-free on the
	// draw path, so this runs before the terminal belongs to any frontend.
	style.Apply(selection.theme)
	return selection, notices
}

// themeLegacyNotice reports a theme file that still carries retired keys. They
// are ignored rather than refused, so the file keeps loading; a theme that
// relied on a light or dark layer wants splitting into two themes.
func themeLegacyNotice(selection themeSelection) string {
	if selection.path == "" || len(selection.theme.Legacy) == 0 {
		return ""
	}
	return fmt.Sprintf("polly: %s: theme %q: ignoring retired key(s) %s: a theme is one look now, only \"colors\" applies",
		selection.path, selection.theme.Name, strings.Join(selection.theme.Legacy, ", "))
}

// applyStartupTheme resolves and applies the configured theme and keeps the
// selection, where the reload watcher can stat the same file: on the runner,
// and on the config the managed REPL receives.
//
// It runs in runConversation, before runManagedREPL hands the terminal to
// tcell and before the fallback REPL prints its first line, so it is the one
// apply both frontends see. --quiet keeps the notices off stderr; after the TUI
// owns the screen a raw stderr write would be wiped by the alternate screen
// anyway, so later failures go through the transcript notice line instead.
func (r *commandRunner) applyStartupTheme(w io.Writer) {
	// Before the resolve, so a shipped theme selected by name is followed as
	// the editable file from the first launch on.
	materializeShippedThemes()
	selection, notices := resolveStartupTheme(r.config.Theme)
	r.theme = selection
	r.config.activeTheme = selection
	if r.config.Quiet {
		return
	}
	for _, notice := range notices {
		fmt.Fprintln(w, notice)
	}
}
