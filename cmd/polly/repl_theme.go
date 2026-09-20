package main

import (
	"os"
	"sort"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
)

// Theme reload: the one apply path, the file watcher behind the event loop's
// tick, and the /theme command.
//
// style.Apply rewrites ui.StyleParserColorMap, the process-global table every
// call site resolves through, and gotui reads that map lock-free on the draw
// path, so an apply may only happen on the TUI event loop. applyTheme is the
// single place that happens after startup; the watcher, /theme, and the
// set_theme tool (through the task it posts) all reach it.
//
// A theme change is not visible until the screen is repainted, and the caches
// holding resolved cells have to be dropped by applyStyleEpoch
// (cmd/polly/repl_theme_epoch.go). applyTheme deliberately does not render
// itself: /theme is dispatched with the model lock held (a slash command comes
// from the composer's locked key handler) and render() takes that same lock, so
// a render there would self-deadlock. Every caller repaints — the ticker arm
// for the watcher, the uiTasks arm for a posted task, and the event arm that
// handled the command — which is also why the watcher reports a change instead
// of painting.

// themePollInterval is the reload watcher's own throttle. The event loop ticks
// every 50ms and a look is a couple of stats plus, at most, one small read, but
// polling the filesystem 20 times a second for an idle prompt is pointless;
// once a second never delays a save the user just made by more than a second.
// The spec's "~1s tick" is not a tick that exists (the closest thing is the
// inspector's refresh throttle), so the gate lives here.
const themePollInterval = time.Second

// themeFileStamp is one version of a theme file: the size and modification time
// a stat reports. Both are part of it, because an editor can rewrite a file
// within a single timestamp resolution tick.
type themeFileStamp struct {
	modTime time.Time
	size    int64
}

// themeWatchState is the reload watcher's memory. It belongs to the managed
// REPL rather than to the config, because a picker preview and a failed load
// change what is on screen without changing what the session follows.
type themeWatchState struct {
	// requested is the theme the session follows: the config file's
	// POLLYTOOL_THEME when it has one, otherwise the flag or environment value
	// the process started with. /theme replaces it for the session.
	requested string
	// stamp is the version of the followed file the watcher last looked at. A
	// failed load deliberately does not advance it: that is what retries a
	// half-written file on the next tick.
	stamp themeFileStamp
	// noticed is the warning already printed for the failure in progress, so a
	// retry does not repeat it once a second. A successful apply clears it.
	noticed string
	// configName and configSet are the POLLYTOOL_THEME value last seen in
	// ~/.pollytool/config, and configStamp the version of the file it came
	// from. Only a *change* is followed: that keeps a hand edit visible even
	// though userConfigValue caches the file for the life of the process,
	// without letting the file override the --theme flag at startup.
	configName  string
	configSet   bool
	configStamp themeFileStamp
	// primed records that the first look has happened. It only baselines the
	// config value, the followed name, and a selection set without a stamp.
	primed bool
	// polledAt is when the watcher last looked.
	polledAt time.Time
}

// applyTheme applies theme on the event loop and returns the new
// style epoch. It is the one apply path for the watcher, /theme, and the
// set_theme tool, and it must run there: the parser map it rewrites is read
// lock-free by gotui while other goroutines parse cells.
//
// The caller repaints; see the file comment for why rendering here would
// deadlock.
func (r *managedREPL) applyTheme(theme style.Theme) uint64 {
	epoch := style.Apply(theme)
	r.applyStyleEpoch(epoch)
	return epoch
}

// restoreActiveTheme puts the theme the session follows back after a preview:
// the selection the startup apply recorded, or the built-in default when the
// launch resolved none.
func (r *managedREPL) restoreActiveTheme() {
	theme := style.DefaultTheme()
	if r.config != nil && r.config.activeTheme.theme.Name != "" {
		theme = r.config.activeTheme.theme
	}
	r.applyTheme(theme)
}

// resolveAndApplyTheme resolves a theme by the value --theme accepts, applies
// it, and records what the session now follows.
func (r *managedREPL) resolveAndApplyTheme(name string) (themeSelection, uint64, error) {
	selection, err := resolveThemeSelection(name)
	if err != nil {
		return themeSelection{}, style.Epoch(), err
	}
	epoch := r.applyTheme(selection.theme)
	r.setActiveTheme(name, selection)
	return selection, epoch, nil
}

// applyThemeByName resolves and applies a theme by name or path. On a load
// error the previous theme stays in effect and the reason goes to the
// transcript: after runManagedREPL the TUI owns the screen, so a raw stderr
// write would be wiped by the alternate screen and stdout is the only place
// left to say what happened.
func (r *managedREPL) applyThemeByName(name string) (uint64, error) {
	_, epoch, err := r.resolveAndApplyTheme(name)
	if err != nil {
		r.model.appendNoticeLine("Warning: " + err.Error())
		return epoch, err
	}
	return epoch, nil
}

// setActiveTheme records the theme the session follows: the selection on the
// config the reload watcher reads, the name it resolves next time, and the
// version of the file to compare against.
//
// A selection that came from a built-in preset has no file of its own, but the
// watcher still follows the user file the name would shadow, so creating one is
// picked up like any other edit.
func (r *managedREPL) setActiveTheme(name string, selection themeSelection) {
	if r.config != nil {
		r.config.activeTheme = selection
	}
	w := &r.themeWatch
	w.requested = name
	w.noticed = ""
	w.stamp = themeFileStamp{}
	if path, ok := r.activeThemeSource(); ok {
		if stamp, err := statThemeFile(path); err == nil {
			w.stamp = stamp
		}
	}
}

// activeThemeSource is the theme file the reload watcher follows: the file the
// selection in effect came from, or — when a load failure left a built-in
// preset on screen — the file the followed name would be, so a half-written
// theme is still picked up once it is complete. ok is false for a built-in
// preset, which has no file to edit.
func (r *managedREPL) activeThemeSource() (string, bool) {
	if r == nil {
		return "", false
	}
	if r.config != nil && r.config.activeTheme.path != "" {
		return r.config.activeTheme.path, true
	}
	path := themeWatchPath(r.activeThemeName())
	return path, path != ""
}

// activeThemeName is the theme the session follows, as /theme names it: the
// value the watcher resolves, falling back to the --theme value for a REPL
// that has not looked yet.
func (r *managedREPL) activeThemeName() string {
	if r == nil {
		return themeNameDefault
	}
	if name := r.themeWatch.requested; name != "" {
		return name
	}
	return themeFollowFlag(r.config)
}

// themeFollowFlag is the name the watcher starts with: the --theme value, or
// the selection's own file when a REPL was handed one without a name.
func themeFollowFlag(config *Config) string {
	if config == nil {
		return themeNameDefault
	}
	if config.Theme != "" {
		return config.Theme
	}
	if config.activeTheme.path != "" {
		return config.activeTheme.path
	}
	return themeNameDefault
}

// themeWatchPath is the file to stat for a --theme value: the path itself, or
// the user theme file a plain name resolves to before it falls back to a
// compiled-in preset. Empty for the reserved default name, which has no user
// file shadow (resolveThemeSelection ignores one) and no editable preset.
func themeWatchPath(name string) string {
	if name == "" || name == themeNameDefault {
		return ""
	}
	if isThemePath(name) {
		return name
	}
	path, err := themeUserPath(name)
	if err != nil {
		return ""
	}
	return path
}

// statThemeFile stamps a theme file's current version.
func statThemeFile(path string) (themeFileStamp, error) {
	info, err := os.Stat(path)
	if err != nil {
		return themeFileStamp{}, err
	}
	return themeFileStamp{modTime: info.ModTime(), size: info.Size()}, nil
}

// configThemeStamp stamps ~/.pollytool/config, the file the watcher re-reads for
// POLLYTOOL_THEME. A path that cannot be resolved stamps zero, which reads as
// "unchanged" until it can be.
func configThemeStamp() (themeFileStamp, error) {
	path, err := userConfigPath()
	if err != nil {
		return themeFileStamp{}, err
	}
	return statThemeFile(path)
}

// pollTheme is the hot-reload watcher, called from the event loop's 50ms tick.
// It reports whether the theme changed, and the caller then repaints — the TUI
// would otherwise sit on the old colors forever, because needsTick() is true
// only for a busy model, a live picker, swarm activity, or an inspector retry,
// so an idle REPL never repaints by itself.
//
// A load error keeps the previous theme and prints one notice per failure
// streak, so a half-written file is retried on the next tick rather than
// reported once per second. The model lock is taken here, because the tick
// holds none while /theme reaches applyTheme with it already held.
func (r *managedREPL) pollTheme(now time.Time) bool {
	if r == nil || r.model == nil {
		return false
	}
	w := &r.themeWatch
	if !w.polledAt.IsZero() && now.Sub(w.polledAt) < themePollInterval {
		return false
	}
	w.polledAt = now
	if !w.primed {
		// The first look only records what is already there, so a theme can
		// never be applied twice for the same content.
		w.primed = true
		w.configName, w.configSet = themeSelectionFromConfig()
		w.configStamp, _ = configThemeStamp()
		if w.requested == "" {
			w.requested = themeFollowFlag(r.config)
		}
		if w.stamp == (themeFileStamp{}) {
			if path, ok := r.activeThemeSource(); ok {
				if stamp, err := statThemeFile(path); err == nil {
					w.stamp = stamp
				}
			}
		}
		return false
	}
	if stamp, _ := configThemeStamp(); stamp != w.configStamp {
		// ~/.pollytool/config changed under us. Reading it here instead of
		// through userConfigValue is the point: that cache is keyed on the path
		// and cleared only by writeUserConfig, so a hand edit would be
		// invisible.
		name, ok := themeSelectionFromConfig()
		if name == w.configName && ok == w.configSet {
			// The edit did not touch the theme selection.
			w.configStamp = stamp
		} else {
			effective := name
			if !ok {
				// The selection was removed, so the flag or environment value
				// the process started with is in effect again.
				effective = themeFollowFlag(r.config)
			}
			if !r.reloadThemeOnLoop(effective) {
				// Leave the baseline alone so the next tick retries: a theme
				// file that does not exist yet becomes live as soon as it does.
				return false
			}
			w.configName, w.configSet, w.configStamp = name, ok, stamp
			return true
		}
	}
	path, ok := r.activeThemeSource()
	if !ok {
		return false
	}
	stamp, err := statThemeFile(path)
	if err != nil || stamp == w.stamp {
		// A file that is gone or unreadable is not a theme change, and the
		// stamp comparison is the whole "did the user save?" test.
		return false
	}
	return r.reloadThemeOnLoop(w.requested)
}

// reloadThemeOnLoop re-resolves and applies name, taking the model lock the
// tick does not hold. It reports whether a new theme was applied.
func (r *managedREPL) reloadThemeOnLoop(name string) bool {
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	return r.reloadTheme(name)
}

// reloadTheme re-applies the followed theme after its file or the config
// selection changed. A load error leaves the previous theme in effect and
// reports the reason once per failure streak.
func (r *managedREPL) reloadTheme(name string) bool {
	if _, _, err := r.resolveAndApplyTheme(name); err != nil {
		body := err.Error()
		if body != r.themeWatch.noticed {
			r.themeWatch.noticed = body
			r.model.appendNoticeLine("Warning: " + body)
		}
		return false
	}
	return true
}

// userThemeNames lists the theme files under ~/.pollytool/themes by base name,
// sorted. A missing directory is empty rather than an error (nothing creates it
// but a persist), and the reserved default name is skipped because
// resolveThemeSelection ignores a user file of that name.
func userThemeNames() []string {
	dir, err := themeDir()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), themeFileNameSuffix) {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), themeFileNameSuffix)
		if name == "" || name == themeNameDefault {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// activeThemeLine names the theme in effect and the file it came from: the line
// /theme answers a switch with.
func (r *managedREPL) activeThemeLine() string {
	active := r.activeThemeName()
	path, ok := r.activeThemeSource()
	switch {
	case ok && path != active:
		return "active theme: " + active + " · " + path
	case ok:
		return "active theme: " + active
	default:
		return "active theme: " + active + " (builtin preset)"
	}
}

// replThemeCommand handles /theme: with no argument it opens the picker of available
// themes, with a name it switches this session's theme, and "default" returns
// to the built-in preset. The choice is saved as the launch default (switchTheme);
// set_theme is what writes a theme file.
func replThemeCommand(ctx *replCommandContext, args []string) replCommandResult {
	if len(args) > 2 {
		return replCommandResult{err: ctx.replyLine("usage: /theme [name]")}
	}
	if ctx == nil || ctx.themeCommand == nil {
		// The line frontend has no color table of its own and no tick to
		// reload on, so it must not silently apply a theme it cannot keep.
		return replCommandResult{err: ctx.replyLine("theme switching unavailable")}
	}
	name := ""
	if len(args) == 2 {
		name = args[1]
	}
	return replCommandResult{err: ctx.replyLines(ctx.themeCommand(name))}
}

// completeThemeCommand offers the names /theme accepts: the built-in presets
// and the user theme files. A path is typed by hand — completing it would mean
// walking the filesystem — and only the first argument completes.
func completeThemeCommand(_ *replCommandContext, fields []string, prefix string) []string {
	if completionArgPos(fields, prefix) != 1 {
		return nil
	}
	names := allThemeNames()
	return matchingWords(names, prefix)
}
