package main

// set_theme: the in-process tool that restyles the running session and, after
// an explicit confirmation, persists a theme file.
//
// It is the model-facing half of the theme feature (the builtin "theme-designer" skill
// interviews the user and calls it) and it is registered only on the managed
// TUI surface: a one-shot, line, or raw run has no color table to keep and no
// event loop to apply on, so it never sees the tool.
//
// Two rules shape every line here:
//
//   - Nothing is written without confirmation. A call with persist and no
//     confirm writes nothing and answers with the exact target path and colors
//     in a confirmation_required reply, so the model can show the user what it
//     is about to create and ask again.
//   - The tool never touches ui.StyleParserColorMap. That map is a process
//     global read lock-free by gotui on the draw path while other goroutines
//     parse cells, so an apply from this tool's worker goroutine would be a
//     fatal concurrent-map-write. The theme is handed to the event loop instead
//     (gotuiTurnUI.ThemeChanged below), exactly the way set_session_title hands
//     its refresh over.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
)

const themeToolName = "set_theme"

// The replies the tool reports. Only confirmation_required is part of the
// two-call protocol the theme skill documents; the other two are ordinary
// answers.
const (
	themeStatusApplied              = "applied"
	themeStatusConfirmationRequired = "confirmation_required"
	themeStatusPersisted            = "persisted"
)

// The error codes the skill teaches the model to correct itself from. Every
// failure below is a *tools.ToolError carrying one of them.
const (
	themeCodeUnknownRole  = "UNKNOWN_ROLE"
	themeCodeInvalidColor = "INVALID_COLOR"
	themeCodeInvalidTheme = "INVALID_THEME"
	themeCodeExists       = "THEME_EXISTS"
	themeCodeWriteFailed  = "THEME_WRITE_FAILED"

	themeNoEventLoopWarning = "the running session was not restyled: this context has no managed-TUI event loop"
)

// themeToolDocument is the theme file shape the tool assembles from its
// arguments: the two keys a theme file has, so the file the tool writes is the
// file the next launch reads.
type themeToolDocument struct {
	Name   string            `json:"name"`
	Colors map[string]string `json:"colors,omitempty"`
}

// themeToolResult is the JSON the model reads back and shows the user.
type themeToolResult struct {
	Status   string             `json:"status"`
	Name     string             `json:"name"`
	Path     string             `json:"path,omitempty"`
	Theme    *themeToolDocument `json:"theme,omitempty"`
	Warnings []string           `json:"warnings,omitempty"`
}

// registerThemeTool installs set_theme for a managed-TUI session and exempts it
// from active-skill allow-list filtering, the way set_session_title is: the
// theme skill declares no allowed-tools and expects the tool it documents.
//
// The surface gate is the whole availability rule. The parser map the tool
// changes only reaches a running TUI, so the other surfaces must not advertise
// a tool that would either do nothing or mislead the model.
func registerThemeTool(state *conversationState) {
	if state == nil || state.toolRegistry == nil || state.outputCapabilities.surface != outputSurfaceManagedTUI {
		return
	}
	state.toolRegistry.Register(&tools.Func{
		Name: themeToolName,
		Desc: "Change the colors of this session's interface, and optionally save the theme under " +
			"~/.pollytool/themes so later launches use it. Without persist the change lasts only for this " +
			"session. persist without confirm writes nothing and answers with the exact file and colors: show " +
			"the user those, get their agreement, then call again with persist and confirm (add overwrite " +
			"when replacing an existing file).",
		Params: schema.Params{
			"name":      schema.S("Theme name: one file name under ~/.pollytool/themes, without a path separator and without the .json suffix."),
			"colors":    themeColorLayerParam("Colors for the 25 roles, as role: value pairs."),
			"persist":   schema.Bool("Save the theme to ~/.pollytool/themes/<name>.json and select it for later launches."),
			"confirm":   schema.Bool("Confirm the write. Required with persist: a first persist without it writes nothing."),
			"overwrite": schema.Bool("Replace an existing theme file of this name."),
		},
		Required: []string{"name"},
		Strict:   true,
		Run:      runSetTheme,
	})
	state.toolRegistry.MarkAlwaysAllowed(themeToolName)
}

// themeColorLayerParam is the role→value object. schema has no
// object helper, so the shape is written out: an object whose values are color
// strings. The role names and the value syntax are style.ParseTheme's business,
// not the schema's.
func themeColorLayerParam(description string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"description":          description,
		"additionalProperties": map[string]any{"type": "string"},
	}
}

// themeUI is the parent TurnUI capability this tool needs: a theme handed to
// the event loop that owns the parser map. The selection carries the file the
// theme was persisted to, or none for a session-only apply.
type themeUI interface{ ThemeChanged(themeSelection) }

// ThemeChanged runs on the tool goroutine that called set_theme.
//
// ui.StyleParserColorMap is process-global and read lock-free by gotui on the
// draw path, so the apply is handed to the event loop rather than performed
// here. This mirrors
// SessionTitleChanged in repl_session_title.go.
//
// The apply also records the selection as the theme the session follows, as
// /theme does: the picker's Escape restores it rather than the theme the tool
// replaced, and the reload watcher follows the file this name would load.
func (t *gotuiTurnUI) ThemeChanged(selection themeSelection) {
	if t == nil || t.repl == nil || t.repl.model == nil {
		return
	}
	r := t.repl
	// The task runs in the loop's uiTasks arm, which holds no lock, while
	// applyTheme drops the model's resolved-color caches (applyStyleEpoch), so
	// the model lock is taken here the way reloadThemeOnLoop takes it for the
	// reload watcher. A slash command reaches the same apply with the lock
	// already held, which is why the apply never takes it itself.
	r.postUI(r.work.ctx, func() {
		r.model.mu.Lock()
		defer r.model.mu.Unlock()
		r.applyTheme(selection.theme)
		r.setActiveTheme(selection.theme.Name, selection)
	})
}

// runSetTheme is the tool body. It runs on a worker goroutine, so it does no
// more than validate, write files, and post the apply.
func runSetTheme(ctx context.Context, args tools.Args) (string, error) {
	document, theme, err := themeToolTheme(args)
	if err != nil {
		return "", err
	}
	if !themeToolBool(args, "persist") {
		result := themeToolResult{Status: themeStatusApplied, Name: theme.Name, Theme: &document}
		if !applySessionTheme(ctx, themeSelection{theme: theme}) {
			result.Warnings = append(result.Warnings, themeNoEventLoopWarning)
		}
		return themeToolResultJSON(result)
	}
	path, err := themeUserPath(theme.Name)
	if err != nil {
		return "", tools.NewToolError(err.Error(), themeCodeWriteFailed)
	}
	if !themeToolBool(args, "confirm") {
		// The first half of the two-call protocol: report the exact file and
		// colors and write nothing at all.
		return themeToolResultJSON(themeToolResult{
			Status: themeStatusConfirmationRequired,
			Name:   theme.Name,
			Path:   path,
			Theme:  &document,
		})
	}
	if !themeToolBool(args, "overwrite") {
		switch _, statErr := os.Stat(path); {
		case statErr == nil:
			return "", tools.NewToolError(fmt.Sprintf("theme file %s already exists: ask the user, then call again with overwrite: true", path), themeCodeExists)
		case !errors.Is(statErr, fs.ErrNotExist):
			return "", tools.NewToolError(fmt.Sprintf("inspect %s: %v", path, statErr), themeCodeWriteFailed)
		}
	}
	if err := writeThemeFile(path, document); err != nil {
		return "", err
	}
	result := themeToolResult{Status: themeStatusPersisted, Name: theme.Name, Path: path, Theme: &document}
	if err := saveThemeSelection(theme.Name); err != nil {
		// The file is on disk but nothing would load it, so this is a failure
		// the model must report rather than a partial success.
		return "", tools.NewToolError(fmt.Sprintf("wrote %s, but %s could not be updated, so the theme will not load on the next launch: %v",
			path, userConfigDisplayPath, err), themeCodeWriteFailed)
	}
	if warning := themeShadowedWarning(theme.Name); warning != "" {
		result.Warnings = append(result.Warnings, warning)
	}
	if !applySessionTheme(ctx, themeSelection{theme: theme, path: path}) {
		result.Warnings = append(result.Warnings, themeNoEventLoopWarning)
	}
	return themeToolResultJSON(result)
}

// themeToolTheme assembles the theme document from the arguments and validates
// it with style.ParseTheme, the loader a theme file goes through. There is one
// validator on purpose: the role vocabulary, the value syntax, the reserved
// name rule, and the inherit restriction cannot drift between the tool and the
// file reader.
func themeToolTheme(args tools.Args) (themeToolDocument, style.Theme, error) {
	name, err := themeToolThemeName(args)
	if err != nil {
		return themeToolDocument{}, style.Theme{}, err
	}
	colors, err := themeToolColorLayer(args, "colors")
	if err != nil {
		return themeToolDocument{}, style.Theme{}, err
	}
	document := themeToolDocument{Name: name, Colors: colors}
	data, err := json.Marshal(document)
	if err != nil {
		return themeToolDocument{}, style.Theme{}, tools.NewToolError(fmt.Sprintf("encode theme %q: %v", name, err), themeCodeInvalidTheme)
	}
	theme, err := style.ParseTheme(name, data)
	if err != nil {
		return themeToolDocument{}, style.Theme{}, themeToolParseError(err)
	}
	return document, theme, nil
}

// themeToolThemeName reads the name argument. It must be one file name: a value
// containing a path separator or ending in ".json" is a --theme *path*, so a
// theme persisted under it would be written to a file the next launch never
// looks for, and "default" is the preset every load failure falls back to, so
// resolveThemeSelection ignores a user file of that name.
func themeToolThemeName(args tools.Args) (string, error) {
	name, _ := args["name"].(string)
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "", tools.NewToolError("a theme needs a name", themeCodeInvalidTheme)
	case isThemePath(name) || name != filepath.Base(name) || name == "." || name == "..":
		return "", tools.NewToolError(fmt.Sprintf("theme name %q: want a single file name, not a path and not a name ending in %s", name, themeFileNameSuffix), themeCodeInvalidTheme)
	case name == themeNameDefault:
		return "", tools.NewToolError(fmt.Sprintf("theme name %q is reserved for the built-in preset, whose user file is ignored: pick another name", name), themeCodeInvalidTheme)
	}
	return name, nil
}

// themeToolColorLayer reads one role→value object. A missing key means the
// layer is absent; a key that is not an object of strings is a malformed layer,
// which is INVALID_THEME rather than UNKNOWN_ROLE: the role names inside are
// ParseTheme's to judge.
func themeToolColorLayer(args tools.Args, key string) (map[string]string, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return nil, nil
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return nil, tools.NewToolError(fmt.Sprintf("%s: want an object of role: color pairs", key), themeCodeInvalidTheme)
	}
	layer := make(map[string]string, len(object))
	for role, value := range object {
		text, ok := value.(string)
		if !ok {
			return nil, tools.NewToolError(fmt.Sprintf("%s: role %q: want a color string", key, role), themeCodeInvalidTheme)
		}
		layer[role] = text
	}
	return layer, nil
}

// themeToolParseError maps a load error onto the code the skill documents.
func themeToolParseError(err error) error {
	code := themeCodeInvalidTheme
	switch {
	case errors.Is(err, style.ErrUnknownRole):
		code = themeCodeUnknownRole
	case errors.Is(err, style.ErrInvalidColor):
		code = themeCodeInvalidColor
	}
	return tools.NewToolError(err.Error(), code)
}

// themeToolBool reads a boolean argument, absent meaning false.
func themeToolBool(args tools.Args, key string) bool {
	value, _ := args[key].(bool)
	return value
}

// applySessionTheme hands the selection to the event loop and reports whether a
// UI took it. A false means this context carries no theme-capable TurnUI: the tool is
// only registered where every turn has a gotuiTurnUI, so it is unreachable in a
// real run, and the caller reports it in the reply instead of failing work that
// already succeeded.
func applySessionTheme(ctx context.Context, selection themeSelection) bool {
	ui, ok := parentTurnUIFrom(ctx).(themeUI)
	if !ok {
		return false
	}
	ui.ThemeChanged(selection)
	return true
}

// writeThemeFile creates the user themes directory on the first persist (0700,
// the directory holds nothing but the user's own themes) and writes the
// document as indented JSON so the user can read and hand-edit it, the same
// 0644 the configuration file gets.
func writeThemeFile(path string, document themeToolDocument) error {
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return tools.NewToolError(fmt.Sprintf("encode theme %q: %v", document.Name, err), themeCodeInvalidTheme)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tools.NewToolError(fmt.Sprintf("create %s: %v", dir, err), themeCodeWriteFailed)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return tools.NewToolError(fmt.Sprintf("write %s: %v", path, err), themeCodeWriteFailed)
	}
	return nil
}

// saveThemeSelection makes the persisted theme this machine's launch default:
// the same line the setup form writes, merged into ~/.pollytool/config so it
// cannot clobber the other saved defaults. writeUserConfig drops the parsed
// file's cache, which is what lets the reload watcher see the new selection on
// its next look.
func saveThemeSelection(name string) error {
	path, err := userConfigPath()
	if err != nil {
		return err
	}
	return writeUserConfig(path, map[string]string{"POLLYTOOL_THEME": name})
}

// themeShadowedWarning reports the exported variable that would keep the saved
// theme from loading on the next launch. An exported POLLYTOOL_THEME naming the
// very theme just saved is not a shadow — the environment resolves to the file
// that was written — so only a different value is reported.
func themeShadowedWarning(name string) string {
	if len(shadowedByEnvironment(map[string]string{"POLLYTOOL_THEME": name})) == 0 {
		return ""
	}
	if strings.TrimSpace(os.Getenv("POLLYTOOL_THEME")) == name {
		return ""
	}
	return "POLLYTOOL_THEME is set in your environment and overrides the saved default on the next launch: export POLLYTOOL_THEME=" + name + " to keep this theme"
}

// themeToolResultJSON renders the reply. Encoding this struct cannot fail, and
// a failure must not hide the work that already happened, so the encode error
// is reported as a plain message rather than dropped.
func themeToolResultJSON(result themeToolResult) (string, error) {
	data, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("encode set_theme reply: %w", err)
	}
	return string(data), nil
}
