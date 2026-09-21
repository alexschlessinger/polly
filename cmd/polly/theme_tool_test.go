package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools"
)

// themeToolTestState is the cheapest conversation state registerThemeTool
// needs: a registry and the resolved surface.
func themeToolTestState(t *testing.T, surface outputSurface) *conversationState {
	t.Helper()
	registry := tools.NewToolRegistry(nil, tools.WithNativeTools(), tools.WithUnsafeNoSandbox())
	t.Cleanup(func() { _ = registry.Close() })
	return &conversationState{toolRegistry: registry, outputCapabilities: outputCapabilities{surface: surface}}
}

// themeToolTestREPL is the cheapest REPL that can take a posted theme apply:
// the tool never renders, so it needs only the event-loop queue, the work
// context, and the model whose resolved caches an apply drops.
func themeToolTestREPL(t *testing.T) *managedREPL {
	t.Helper()
	r := newManagedREPL(&Config{}, "theme", 0, 0)
	t.Cleanup(func() { _ = r.work.close() })
	return r
}

// themeToolContext is the context a turn hands its tools: the parent's UI, which
// is how set_theme reaches the event loop.
func themeToolContext(r *managedREPL) context.Context {
	return withParentTurnUI(context.Background(), &gotuiTurnUI{repl: r})
}

// drainThemeTasks runs the tasks the tool posted to the event loop the way the
// loop's uiTasks arm does, and reports how many ran.
func drainThemeTasks(r *managedREPL) int {
	ran := 0
	for {
		select {
		case task := <-r.uiTasks:
			task()
			ran++
		default:
			return ran
		}
	}
}

func decodeThemeReply(t *testing.T, reply string) themeToolResult {
	t.Helper()
	var result themeToolResult
	if err := json.Unmarshal([]byte(reply), &result); err != nil {
		t.Fatalf("decode set_theme reply %s: %v", reply, err)
	}
	return result
}

func assertThemeToolError(t *testing.T, err error, code string) {
	t.Helper()
	var toolErr *tools.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("error %v (%T) is not a *tools.ToolError, want code %s", err, err, code)
	}
	if toolErr.Code != code {
		t.Fatalf("error code = %q, want %q (%v)", toolErr.Code, code, err)
	}
	if toolErr.Message == "" {
		t.Fatalf("error %v carries code %s but no message", err, code)
	}
}

// activateThemeTestSkill activates a skill whose allow-list names one tool, so
// the registry is narrowed and an always-allowed tool can be told apart from a
// merely registered one.
func activateThemeTestSkill(t *testing.T, registry *tools.ToolRegistry) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "restrict")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: restrict\ndescription: Narrow the registry for a theme test\nallowed-tools: read_file\n---\n\nRestrict the tools.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := skills.LoadCatalog([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := tools.NewSkillRuntime(catalog, registry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Activate("restrict"); err != nil {
		t.Fatal(err)
	}
}

func TestThemeToolRegistrationBySurface(t *testing.T) {
	t.Run("managed tui", func(t *testing.T) {
		state := themeToolTestState(t, outputSurfaceManagedTUI)
		registerThemeTool(state)
		tool, ok := state.toolRegistry.Get(themeToolName)
		if !ok {
			t.Fatal("set_theme is not registered on the managed TUI surface")
		}
		schema := tool.GetSchema()
		if !schema.Strict {
			t.Fatal("set_theme schema is not strict")
		}
		if !slices.Contains(schema.Required(), "name") {
			t.Fatalf("set_theme required = %v, want name", schema.Required())
		}
		for _, key := range []string{"name", "colors", "persist", "confirm", "overwrite"} {
			if _, ok := schema.Properties()[key]; !ok {
				t.Fatalf("set_theme has no %q property: %v", key, schema.Properties())
			}
		}
		// The three color layers are hand-written object schemas: schema has no
		// object helper, so their shape is part of the contract.
		for _, key := range []string{"colors"} {
			property, _ := schema.Properties()[key].(map[string]any)
			if property["type"] != "object" {
				t.Fatalf("set_theme %s is %v, want an object", key, property["type"])
			}
			values, _ := property["additionalProperties"].(map[string]any)
			if values["type"] != "string" {
				t.Fatalf("set_theme %s values are %v, want strings", key, property["additionalProperties"])
			}
		}
		// Always allowed: a skill allow-list narrows the registry and the theme
		// skill declares none, so set_theme must survive activation while its
		// sibling does not.
		state.toolRegistry.Register(&tools.Func{Name: "theme_test_sibling", Run: func(context.Context, tools.Args) (string, error) { return "", nil }})
		activateThemeTestSkill(t, state.toolRegistry)
		if _, exists, allowed := state.toolRegistry.GetIfAllowed(themeToolName); !exists || !allowed {
			t.Fatal("set_theme is not always allowed under an active skill allow-list")
		}
		if _, _, allowed := state.toolRegistry.GetIfAllowed("theme_test_sibling"); allowed {
			t.Fatal("the test skill did not narrow the registry")
		}
	})
	for _, surface := range []outputSurface{outputSurfaceLineRaw, outputSurfaceLineANSI} {
		t.Run(fmt.Sprintf("surface %d", surface), func(t *testing.T) {
			state := themeToolTestState(t, surface)
			registerThemeTool(state)
			if _, ok := state.toolRegistry.Get(themeToolName); ok {
				t.Fatalf("set_theme is registered on surface %d", surface)
			}
		})
	}
}

func TestThemeToolAppliesWithoutPersisting(t *testing.T) {
	home := themeTestHome(t)
	r := themeToolTestREPL(t)
	state := themeToolTestState(t, outputSurfaceManagedTUI)
	registerThemeTool(state)
	themeTestRestoreDefault(t)
	tool, ok := state.toolRegistry.Get(themeToolName)
	if !ok {
		t.Fatal("set_theme is not registered on the managed TUI surface")
	}
	want := themeTestWantColor(t, "#123456")
	beforeEpoch, before := style.Epoch(), themeTestResolvedColor("accent")

	reply, err := tool.Execute(themeToolContext(r), map[string]any{
		"name":   "session",
		"colors": map[string]any{"accent": "#123456"},
	})
	if err != nil {
		t.Fatalf("set_theme: %v", err)
	}
	if result := decodeThemeReply(t, reply); result.Status != themeStatusApplied {
		t.Fatalf("status = %q, want %q", result.Status, themeStatusApplied)
	}
	if _, err := os.Stat(filepath.Join(home, userConfigDirName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a session-only set_theme created %s: %v", filepath.Join(home, userConfigDirName), err)
	}
	// The tool goroutine must not have written the parser map: the apply is only
	// visible once the event-loop task it posted runs.
	if themeTestResolvedColor("accent") != before || style.Epoch() != beforeEpoch {
		t.Fatal("set_theme applied the theme on its own goroutine instead of posting it")
	}
	if ran := drainThemeTasks(r); ran != 1 {
		t.Fatalf("set_theme posted %d event-loop tasks, want 1", ran)
	}
	if got := themeTestResolvedColor("accent"); got != want {
		t.Fatalf("accent after the apply = %v, want %v", got, want)
	}
	if style.Epoch() <= beforeEpoch {
		t.Fatalf("epoch after the apply = %d, want > %d", style.Epoch(), beforeEpoch)
	}
	// The applied theme is the one the session now follows, as it would be
	// after "/theme session": the picker's Escape restores it, not the theme
	// the tool replaced, and it has no file for the watcher to stat.
	if got := r.activeThemeName(); got != "session" {
		t.Fatalf("followed theme after set_theme = %q, want session", got)
	}
	if active := r.config.activeTheme; active.theme.Name != "session" || active.path != "" {
		t.Fatalf("active selection after set_theme = %+v, want the session-only theme", active)
	}
}

func TestThemeToolPersistTwoCallProtocol(t *testing.T) {
	home := themeTestHome(t)
	r := themeToolTestREPL(t)
	state := themeToolTestState(t, outputSurfaceManagedTUI)
	registerThemeTool(state)
	themeTestRestoreDefault(t)
	tool, _ := state.toolRegistry.Get(themeToolName)
	ctx := themeToolContext(r)
	args := map[string]any{
		"name":    "custom",
		"colors":  map[string]any{"accent": "#123456", "syn-comment": "grey"},
		"persist": true,
	}

	path := filepath.Join(home, userConfigDirName, "themes", "custom.json")
	reply, err := tool.Execute(ctx, args)
	if err != nil {
		t.Fatalf("persist without confirm: %v", err)
	}
	confirmation := decodeThemeReply(t, reply)
	if confirmation.Status != themeStatusConfirmationRequired {
		t.Fatalf("status = %q, want %q", confirmation.Status, themeStatusConfirmationRequired)
	}
	if confirmation.Path != path || !filepath.IsAbs(confirmation.Path) {
		t.Fatalf("path = %q, want the absolute %q", confirmation.Path, path)
	}
	if confirmation.Theme == nil || confirmation.Theme.Colors["accent"] != "#123456" {
		t.Fatalf("confirmation payload theme = %+v, want the colors sent", confirmation.Theme)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("persist without confirm wrote %s", path)
	}
	if values, _, err := readUserConfig(filepath.Join(home, userConfigDirName, userConfigFileName)); err == nil && values["POLLYTOOL_THEME"] != "" {
		t.Fatalf("persist without confirm set POLLYTOOL_THEME=%q", values["POLLYTOOL_THEME"])
	}
	if ran := drainThemeTasks(r); ran != 0 {
		t.Fatalf("persist without confirm applied %d themes, want 0", ran)
	}

	args["confirm"] = true
	reply, err = tool.Execute(ctx, args)
	if err != nil {
		t.Fatalf("persist with confirm: %v", err)
	}
	if result := decodeThemeReply(t, reply); result.Status != themeStatusPersisted || result.Path != path {
		t.Fatalf("persist reply = %+v, want status %q and path %q", result, themeStatusPersisted, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the persisted theme: %v", err)
	}
	var written themeToolDocument
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatalf("the persisted theme is not JSON (%s): %v", data, err)
	}
	if written.Name != "custom" || written.Colors["accent"] != "#123456" || written.Colors["syn-comment"] != "grey" {
		t.Fatalf("persisted theme = %+v, want the colors sent", written)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Fatalf("theme file mode = %#o, want 0644", perm)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("themes directory mode = %#o, want 0700", perm)
	}
	values, _, err := readUserConfig(filepath.Join(home, userConfigDirName, userConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	if values["POLLYTOOL_THEME"] != "custom" {
		t.Fatalf("POLLYTOOL_THEME = %q after persist, want custom", values["POLLYTOOL_THEME"])
	}
	// The restart: the next launch resolves the name through the file it saved.
	selection, err := resolveThemeSelection("custom")
	if err != nil {
		t.Fatalf("the persisted theme does not load: %v", err)
	}
	if selection.path != path || selection.builtin || selection.theme.Colors["accent"] != "#123456" {
		t.Fatalf("resolved %+v, want the persisted %s", selection, path)
	}
	// And the session is restyled through the event loop, not here.
	if ran := drainThemeTasks(r); ran != 1 {
		t.Fatalf("persist with confirm posted %d event-loop tasks, want 1", ran)
	}
	want := themeTestWantColor(t, "#123456")
	if got := themeTestResolvedColor("accent"); got != want {
		t.Fatalf("accent after the persist = %v, want %v", got, want)
	}
}

func TestThemeToolPersistRequiresOverwrite(t *testing.T) {
	themeTestHome(t)
	r := themeToolTestREPL(t)
	state := themeToolTestState(t, outputSurfaceManagedTUI)
	registerThemeTool(state)
	themeTestRestoreDefault(t)
	tool, _ := state.toolRegistry.Get(themeToolName)
	ctx := themeToolContext(r)
	args := map[string]any{
		"name":    "custom",
		"colors":  map[string]any{"accent": "#123456"},
		"persist": true,
		"confirm": true,
	}
	first, err := tool.Execute(ctx, args)
	if err != nil {
		t.Fatalf("first persist: %v", err)
	}
	path := decodeThemeReply(t, first).Path

	reply, err := tool.Execute(ctx, args)
	if reply != "" {
		t.Fatalf("a refused persist returned %q", reply)
	}
	assertThemeToolError(t, err, themeCodeExists)

	args["colors"] = map[string]any{"accent": "#654321"}
	args["overwrite"] = true
	if _, err := tool.Execute(ctx, args); err != nil {
		t.Fatalf("persist with overwrite: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var written themeToolDocument
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatal(err)
	}
	if written.Colors["accent"] != "#654321" {
		t.Fatalf("overwrite left %+v, want the new accent", written)
	}
}

func TestThemeToolErrorCodes(t *testing.T) {
	layers := func(colors map[string]any) map[string]any {
		return map[string]any{"name": "custom", "colors": colors, "persist": true, "confirm": true}
	}
	cases := []struct {
		name    string
		prepare func(t *testing.T, home string)
		args    map[string]any
		code    string
	}{
		{
			name: "unknown role",
			args: layers(map[string]any{"nope": "#123456"}),
			code: themeCodeUnknownRole,
		},
		{
			name: "invalid color",
			args: layers(map[string]any{"accent": "#gggggg"}),
			code: themeCodeInvalidColor,
		},
		{
			name: "role used as a value",
			args: layers(map[string]any{"accent": "muted"}),
			code: themeCodeInvalidColor,
		},
		{
			name: "inherit on accent",
			args: layers(map[string]any{"accent": "inherit"}),
			code: themeCodeInvalidColor,
		},
		{
			name: "palette out of range",
			args: layers(map[string]any{"accent": "palette:256"}),
			code: themeCodeInvalidColor,
		},
		{
			name: "reserved name",
			args: map[string]any{"name": themeNameDefault, "colors": map[string]any{"accent": "#123456"}},
			code: themeCodeInvalidTheme,
		},
		{
			name: "name is a path",
			args: map[string]any{"name": "sub/custom", "colors": map[string]any{"accent": "#123456"}},
			code: themeCodeInvalidTheme,
		},
		{
			name: "name carries the file suffix",
			args: map[string]any{"name": "custom.json", "colors": map[string]any{"accent": "#123456"}},
			code: themeCodeInvalidTheme,
		},
		{
			name: "layer is not an object",
			args: map[string]any{"name": "custom", "colors": "green"},
			code: themeCodeInvalidTheme,
		},
		{
			name: "layer value is not a string",
			args: layers(map[string]any{"accent": 5}),
			code: themeCodeInvalidTheme,
		},
		{
			name: "theme file exists",
			prepare: func(t *testing.T, home string) {
				themeTestWriteFixture(t, home, "custom.json", "{\"name\":\"custom\"}\n")
			},
			args: layers(map[string]any{"accent": "#123456"}),
			code: themeCodeExists,
		},
		{
			name: "write fails",
			prepare: func(t *testing.T, home string) {
				if err := os.WriteFile(filepath.Join(home, userConfigDirName), []byte("not a directory\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			args: layers(map[string]any{"accent": "#123456"}),
			code: themeCodeWriteFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := themeTestHome(t)
			if tc.prepare != nil {
				tc.prepare(t, home)
			}
			r := themeToolTestREPL(t)
			state := themeToolTestState(t, outputSurfaceManagedTUI)
			registerThemeTool(state)
			tool, _ := state.toolRegistry.Get(themeToolName)

			reply, err := tool.Execute(themeToolContext(r), tc.args)
			if reply != "" {
				t.Fatalf("a failed set_theme returned %q", reply)
			}
			assertThemeToolError(t, err, tc.code)
		})
	}
}

// The tool is registered only where a managed TUI owns the parser map, so a
// context without one cannot happen in a run; when it does (a bare context) the
// reply says the live apply was skipped rather than claiming a restyle that
// never happened.
func TestThemeToolWithoutTurnUIReportsTheSkippedApply(t *testing.T) {
	themeTestHome(t)
	state := themeToolTestState(t, outputSurfaceManagedTUI)
	registerThemeTool(state)
	themeTestRestoreDefault(t)
	tool, _ := state.toolRegistry.Get(themeToolName)
	before := style.Epoch()

	reply, err := tool.Execute(context.Background(), map[string]any{
		"name":   "session",
		"colors": map[string]any{"accent": "#123456"},
	})
	if err != nil {
		t.Fatalf("set_theme: %v", err)
	}
	result := decodeThemeReply(t, reply)
	if result.Status != themeStatusApplied {
		t.Fatalf("status = %q, want %q", result.Status, themeStatusApplied)
	}
	if len(result.Warnings) != 1 || result.Warnings[0] != themeNoEventLoopWarning {
		t.Fatalf("warnings = %v, want the skipped-apply note", result.Warnings)
	}
	if style.Epoch() != before {
		t.Fatalf("epoch = %d, want the unchanged %d", style.Epoch(), before)
	}
}
