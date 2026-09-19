package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	ui "github.com/metaspartan/gotui/v5"
)

// themeTestHome points HOME (and USERPROFILE, which os.UserHomeDir reads on
// Windows) at a fresh directory, so theme discovery cannot see the developer's
// own ~/.pollytool. Tests are serial, so t.Setenv is safe here.
func themeTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// themeTestRestoreDefault re-applies the built-in theme when a test applied
// another one: ui.StyleParserColorMap is global to the test binary.
func themeTestRestoreDefault(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { style.Apply(style.DefaultTheme()) })
}

// themeTestWriteFixture writes a theme file under home's themes directory.
func themeTestWriteFixture(t *testing.T, home, name, body string) string {
	t.Helper()
	dir := filepath.Join(home, userConfigDirName, "themes")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// themeTestUnsetEnv clears a variable for the rest of the test, keeping the value
// t.Setenv registered for cleanup.
func themeTestUnsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

// themeTestResolvedColor is the color a role currently resolves to.
func themeTestResolvedColor(role string) ui.Color { return ui.StyleParserColorMap[role] }

// themeTestWantColor parses a theme value into the color it resolves to.
func themeTestWantColor(t *testing.T, value string) ui.Color {
	t.Helper()
	color, err := style.ParseColorValue(value)
	if err != nil {
		t.Fatalf("ParseColorValue(%q) = %v", value, err)
	}
	return color
}

func TestThemeFlagsListVariablesInHelp(t *testing.T) {
	cmd := getCommand()
	var out bytes.Buffer
	cmd.Writer = &out
	if err := cmd.Run(context.Background(), []string{"polly", "--help"}); err != nil {
		t.Fatalf("run --help: %v", err)
	}
	help := out.String()
	for _, want := range []string{"--theme", "[$POLLYTOOL_THEME]"} {
		if !strings.Contains(help, want) {
			t.Fatalf("--help output missing %q:\n%s", want, help)
		}
	}
	if strings.Count(help, "[$POLLYTOOL_THEME]") != 1 {
		t.Fatalf("--help output lists [$POLLYTOOL_THEME] %d times, want 1:\n%s",
			strings.Count(help, "[$POLLYTOOL_THEME]"), help)
	}
}

func TestThemeSelectionPrecedence(t *testing.T) {
	home := themeTestHome(t)
	configPath := filepath.Join(home, userConfigDirName, userConfigFileName)
	if err := writeUserConfig(configPath, map[string]string{"POLLYTOOL_THEME": "from-file"}); err != nil {
		t.Fatal(err)
	}
	themeTestUnsetEnv(t, "POLLYTOOL_THEME")

	// The file is the third tier: it seeds a run that passes neither a flag
	// nor an environment value.
	config, _ := parseEnvTestConfig(t)
	if config.Theme != "from-file" {
		t.Fatalf("config from file = theme %q, want from-file", config.Theme)
	}

	// The environment beats the file.
	t.Setenv("POLLYTOOL_THEME", "from-env")
	config, _ = parseEnvTestConfig(t)
	if config.Theme != "from-env" {
		t.Fatalf("config from env = theme %q, want from-env", config.Theme)
	}

	// A flag beats both.
	config, _ = parseEnvTestConfig(t, "--theme", "from-flag")
	if config.Theme != "from-flag" {
		t.Fatalf("config from flag = theme %q, want from-flag", config.Theme)
	}

	// With no file entry either, the built-in default stands. writeUserConfig
	// clears the parsed-file cache, so removing the key is visible here the
	// way a hand edit would be to a fresh process.
	if err := writeUserConfig(configPath, map[string]string{"POLLYTOOL_THEME": ""}); err != nil {
		t.Fatal(err)
	}
	themeTestUnsetEnv(t, "POLLYTOOL_THEME")
	config, _ = parseEnvTestConfig(t)
	if config.Theme != themeNameDefault {
		t.Fatalf("config with no theme source = theme %q, want default", config.Theme)
	}
}

func TestResolveThemeSelection(t *testing.T) {
	home := themeTestHome(t)
	userPath := themeTestWriteFixture(t, home, "solar.json",
		`{"name":"solar","colors":{"accent":"#0b5cad"}}`)
	// A user file that shadows a preset name wins over the preset.
	shadowPath := themeTestWriteFixture(t, home, "midnight-parrot.json", `{"colors":{"accent":"#123456"}}`)
	// "default" is reserved: this file must be ignored.
	themeTestWriteFixture(t, home, "default.json", `{"colors":{"accent":"#ff0000"}}`)
	themeTestWriteFixture(t, home, "broken.json", `{"colors":`)
	themeTestWriteFixture(t, home, "typo.json", `{"colors":{"acent":"#ff0000"}}`)
	themeTestWriteFixture(t, home, "nocolor.json", `{"colors":{"accent":"muted"}}`)

	pathOnly := filepath.Join(home, "loose.json")
	if err := os.WriteFile(pathOnly, []byte(`{"colors":{"accent":"#abcdef"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name        string
		value       string
		wantPath    string
		wantBuiltin bool
		wantName    string
		wantAccent  string
		wantErr     error
		wantErrText string
	}{
		{name: "user file", value: "solar", wantPath: userPath, wantName: "solar", wantAccent: "#0b5cad"},
		{name: "default reserved", value: "default", wantBuiltin: true, wantName: "default"},
		{name: "shipped theme", value: "midnight-parrot", wantBuiltin: true, wantName: "midnight-parrot", wantAccent: "#7aa2f7"},
		{name: "empty value", value: "", wantBuiltin: true, wantName: "default"},
		{name: "path", value: pathOnly, wantPath: pathOnly, wantName: "loose", wantAccent: "#abcdef"},
		{name: "relative path", value: "loose.json", wantPath: "loose.json", wantName: "loose", wantAccent: "#abcdef"},
		{name: "missing path", value: filepath.Join(home, "gone.json"), wantErr: fs.ErrNotExist},
		{name: "missing name", value: "nope", wantErrText: "unknown theme"},
		{name: "broken json", value: "broken", wantErr: style.ErrInvalidTheme},
		{name: "unknown role", value: "typo", wantErr: style.ErrUnknownRole},
		{name: "role as value", value: "nocolor", wantErr: style.ErrInvalidColor},
	}

	// A user file shadows a same-named preset: this is asserted before the
	// table because the file has to be removed before the preset rows can
	// resolve to the compiled-in preset again.
	selection, err := resolveThemeSelection("midnight-parrot")
	if err != nil {
		t.Fatalf("resolve shadowed preset: %v", err)
	}
	if selection.path != shadowPath || selection.builtin || selection.theme.Colors["accent"] != "#123456" {
		t.Fatalf("shadowed preset = path %q builtin %v accent %q, want the user file",
			selection.path, selection.builtin, selection.theme.Colors["accent"])
	}
	if err := os.Remove(shadowPath); err != nil {
		t.Fatal(err)
	}

	// A relative ".json" value is a path: it resolves against the working
	// directory, not the themes directory.
	t.Chdir(home)

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			selection, err := resolveThemeSelection(tt.value)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("resolveThemeSelection(%q) error = %v, want %v", tt.value, err, tt.wantErr)
				}
				return
			}
			if tt.wantErrText != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrText) {
					t.Fatalf("resolveThemeSelection(%q) error = %v, want it to mention %q", tt.value, err, tt.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveThemeSelection(%q) = %v", tt.value, err)
			}
			if selection.path != tt.wantPath || selection.builtin != tt.wantBuiltin || selection.theme.Name != tt.wantName {
				t.Fatalf("resolveThemeSelection(%q) = path %q builtin %v name %q, want path %q builtin %v name %q",
					tt.value, selection.path, selection.builtin, selection.theme.Name, tt.wantPath, tt.wantBuiltin, tt.wantName)
			}
			if tt.wantAccent != "" && selection.theme.Colors["accent"] != tt.wantAccent {
				t.Fatalf("resolveThemeSelection(%q) accent = %q, want %q", tt.value, selection.theme.Colors["accent"], tt.wantAccent)
			}
		})
	}
}

func TestThemeSelectionFromConfigRereadsTheFile(t *testing.T) {
	home := themeTestHome(t)
	configPath := filepath.Join(home, userConfigDirName, userConfigFileName)
	if err := writeUserConfig(configPath, map[string]string{"POLLYTOOL_THEME": "first"}); err != nil {
		t.Fatal(err)
	}
	// Populate the process-lifetime cache the flag sources use, so the
	// difference between the two readers is observable.
	if value, ok := userConfigValue("POLLYTOOL_THEME"); !ok || value != "first" {
		t.Fatalf("userConfigValue = %q, %v, want first, true", value, ok)
	}
	if err := os.WriteFile(configPath, []byte(userConfigHeader+"POLLYTOOL_THEME=second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if value, _ := userConfigValue("POLLYTOOL_THEME"); value != "first" {
		t.Fatalf("userConfigValue after a hand edit = %q, want the cached first", value)
	}
	name, ok := themeSelectionFromConfig()
	if !ok || name != "second" {
		t.Fatalf("themeSelectionFromConfig = %q, %v, want second, true", name, ok)
	}
	// A missing file is simply no selection.
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	if name, ok := themeSelectionFromConfig(); ok {
		t.Fatalf("themeSelectionFromConfig with no file = %q, %v, want no selection", name, ok)
	}
}

func TestResolveStartupThemeAppliesAndNotices(t *testing.T) {
	themeTestRestoreDefault(t)
	home := themeTestHome(t)
	themeTestWriteFixture(t, home, "broken.json", `{"colors":`)
	themeTestWriteFixture(t, home, "solar.json", `{"colors":{"accent":"#0b5cad"}}`)

	t.Run("preset", func(t *testing.T) {
		selection, notices := resolveStartupTheme("midnight-parrot")
		if len(notices) != 0 {
			t.Fatalf("notices = %q, want none", notices)
		}
		if !selection.builtin || selection.theme.Name != "midnight-parrot" {
			t.Fatalf("selection = %+v, want the shipped preset", selection)
		}
		if got, want := themeTestResolvedColor("accent"), themeTestWantColor(t, "#7aa2f7"); got != want {
			t.Fatalf("accent after the preset = %v, want %v", got, want)
		}
	})

	t.Run("user file", func(t *testing.T) {
		selection, notices := resolveStartupTheme("solar")
		if len(notices) != 0 {
			t.Fatalf("notices = %q, want none", notices)
		}
		if selection.builtin || selection.path == "" {
			t.Fatalf("selection = %+v, want a user file", selection)
		}
		if got, want := themeTestResolvedColor("accent"), themeTestWantColor(t, "#0b5cad"); got != want {
			t.Fatalf("accent = %v, want %v", got, want)
		}
	})

	t.Run("unknown name", func(t *testing.T) {
		selection, notices := resolveStartupTheme("nope")
		if len(notices) != 1 || !strings.HasPrefix(notices[0], "polly: ~/.pollytool/themes/nope.json: ") {
			t.Fatalf("notices = %q, want one notice about the themes path", notices)
		}
		if !selection.builtin || selection.theme.Name != themeNameDefault {
			t.Fatalf("selection = %+v, want the built-in default", selection)
		}
	})

	t.Run("missing file path", func(t *testing.T) {
		missing := filepath.Join(home, "gone.json")
		selection, notices := resolveStartupTheme(missing)
		if len(notices) != 1 || !strings.HasPrefix(notices[0], "polly: "+missing+": ") {
			t.Fatalf("notices = %q, want one notice about %s", notices, missing)
		}
		if !selection.builtin || selection.theme.Name != themeNameDefault {
			t.Fatalf("selection = %+v, want the built-in default", selection)
		}
	})

	t.Run("broken json", func(t *testing.T) {
		selection, notices := resolveStartupTheme("broken")
		if len(notices) != 1 || !strings.HasPrefix(notices[0], "polly: ~/.pollytool/themes/broken.json: ") {
			t.Fatalf("notices = %q, want one notice about the broken file", notices)
		}
		if !selection.builtin || selection.theme.Name != themeNameDefault {
			t.Fatalf("selection = %+v, want the built-in default", selection)
		}
	})

	t.Run("retired keys", func(t *testing.T) {
		// An older file keeps loading: its layers are ignored with a notice
		// and only colors applies.
		themeTestWriteFixture(t, home, "layers.json",
			`{"variant":"light","colors":{"accent":"#000000"},"light":{"accent":"#0b5cad"}}`)
		selection, notices := resolveStartupTheme("layers")
		if len(notices) != 1 || !strings.Contains(notices[0], "retired key(s) variant, light") {
			t.Fatalf("notices = %q, want one notice naming the retired keys", notices)
		}
		if selection.builtin {
			t.Fatalf("selection = %+v, want the user file", selection)
		}
		if got, want := themeTestResolvedColor("accent"), themeTestWantColor(t, "#000000"); got != want {
			t.Fatalf("accent = %v, want the base color %v", got, want)
		}
	})
}

func TestApplyStartupThemeStoresSelectionAndHonorsQuiet(t *testing.T) {
	themeTestRestoreDefault(t)
	home := themeTestHome(t)
	themeTestWriteFixture(t, home, "solar.json", `{"colors":{"accent":"#0b5cad"}}`)

	runner := &commandRunner{conversationOpener: conversationOpener{config: &Config{Theme: "solar"}}}
	var out bytes.Buffer
	runner.applyStartupTheme(&out)
	if out.Len() != 0 {
		t.Fatalf("stderr = %q, want no notice for a valid theme", out.String())
	}
	wantPath := filepath.Join(home, userConfigDirName, "themes", "solar.json")
	if runner.config.activeTheme.builtin || runner.config.activeTheme.path != wantPath {
		t.Fatalf("runner theme = %+v, want the solar file", runner.config.activeTheme)
	}

	quiet := &commandRunner{conversationOpener: conversationOpener{config: &Config{Theme: "nope", Quiet: true}}}
	out.Reset()
	quiet.applyStartupTheme(&out)
	if out.Len() != 0 {
		t.Fatalf("stderr with --quiet = %q, want nothing", out.String())
	}
	if !quiet.config.activeTheme.builtin || quiet.config.activeTheme.theme.Name != themeNameDefault {
		t.Fatalf("quiet runner theme = %+v, want the default fallback", quiet.config.activeTheme)
	}

	loud := &commandRunner{conversationOpener: conversationOpener{config: &Config{Theme: "nope"}}}
	out.Reset()
	loud.applyStartupTheme(&out)
	if !strings.HasPrefix(out.String(), "polly: ~/.pollytool/themes/nope.json: ") {
		t.Fatalf("stderr = %q, want the startup notice", out.String())
	}
}

// Every committed theme file parses, paints its own surface, and resolves by
// its base name like any other preset.
func TestShippedThemesParse(t *testing.T) {
	entries, err := fs.ReadDir(shippedThemeFS, "themes")
	if err != nil || len(entries) == 0 {
		t.Fatalf("shipped themes: %v (%d files)", err, len(entries))
	}
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), themeFileNameSuffix)
		theme, ok := shippedThemes()[name]
		if !ok {
			t.Fatalf("%s did not parse", entry.Name())
		}
		if theme.Name != name {
			t.Fatalf("%s declares name %q", entry.Name(), theme.Name)
		}
		for _, role := range []string{"background", "foreground"} {
			if theme.Colors[role] == "" {
				t.Fatalf("%s sets no %s", entry.Name(), role)
			}
		}
	}
}

// Shipped themes land in the user themes directory once, editable, and an
// existing file is never overwritten.
func TestMaterializeShippedThemesKeepsUserEdits(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, userConfigDirName, "themes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	edited := filepath.Join(dir, "amber-parrot.json")
	if err := os.WriteFile(edited, []byte(`{"colors":{"accent":"#123456"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	materializeShippedThemes()
	if data, _ := os.ReadFile(edited); string(data) != `{"colors":{"accent":"#123456"}}` {
		t.Fatalf("user edit overwritten: %s", data)
	}
	selection, err := resolveThemeSelection("azure-parrot")
	if err != nil || selection.builtin || selection.path == "" {
		t.Fatalf("azure-parrot = %+v, %v; want the materialized file", selection, err)
	}
}
