package main

import (
	"bytes"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

// themeReloadColors are the four roles the reload tests assert on, resolved.
// accent colors transcript text, muted the inspector frame and the scrollbar
// thumb, code a fenced body, and polly-green the masthead bird.
type themeReloadColors struct {
	accent, muted, code, bird ui.Color
}

// themeReloadFixture writes a user theme file carrying those four roles and
// returns its path and the colors it resolves to.
func themeReloadFixture(t *testing.T, home, name, accent, muted, code, bird string) (string, themeReloadColors) {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q,"colors":{"accent":%q,"muted":%q,"code":%q,"polly-green":%q}}`,
		name, accent, muted, code, bird)
	path := themeTestWriteFixture(t, home, name+themeFileNameSuffix, body)
	return path, themeReloadColors{
		accent: themeTestWantColor(t, accent),
		muted:  themeTestWantColor(t, muted),
		code:   themeTestWantColor(t, code),
		bird:   themeTestWantColor(t, bird),
	}
}

// themeReloadSurfaces fills the REPL with the surfaces a theme change has to
// recolor, so a reload can be observed the way a user sees it: a transcript
// line, a fenced code block, the masthead bird, and the inspector's orbit frame
// with its scrollbar thumb. It returns the fenced block's markup, which names
// its own role.
func themeReloadSurfaces(t *testing.T, r *managedREPL, screen tcell.SimulationScreen) string {
	t.Helper()
	screen.SetSize(140, 40)
	m := r.model
	m.masthead = mastheadState{enabled: true, sandbox: "Sandbox active"}
	m.appendLine(style.Styled("accent-colored sentence", "accent", ""))
	fenced, _, _ := markdown.RenderWithCache("```\nplain code body\n```", m.imageBaseDir, false, nil)
	m.appendLine(fenced)
	// The tools inspector supplies the frame and its scrollbar: its content is
	// long enough for a thumb while the transcript stays short.
	call := messages.ChatMessageToolCall{ID: "scope", Name: "read_file"}
	m.appendToolCallStart(call)
	m.inspections.setResult(call, messages.ChatMessage{Content: strings.Repeat("result\n", 80)})
	r.inspectCommand("tools")
	openToolDetails(t, r, 140)
	r.render()
	return fenced
}

// themeReloadRepaint asserts every surface carries want and none still carries
// before.
func themeReloadRepaint(t *testing.T, r *managedREPL, screen tcell.SimulationScreen, fenced string, before, want themeReloadColors) {
	t.Helper()
	pane := r.transcriptW.Inner
	if pt, ok := screenCellWithForeground(screen, pane, before.accent); ok {
		t.Fatalf("the previous accent is still on screen at %v", pt)
	}
	if _, ok := screenCellWithForeground(screen, pane, want.accent); !ok {
		t.Fatal("the transcript kept the previous theme")
	}
	if _, ok := screenCellWithColor(screen, pane, want.bird, true); !ok {
		t.Fatal("the masthead bird kept the previous theme")
	}
	// The fence body's markup names its own role, so resolve the expectation
	// from it rather than assuming which role paints a code body.
	for _, cell := range style.ParseCells(fenced, ui.StyleClear) {
		if cell.Rune != 'p' {
			continue
		}
		if cell.Style.Fg == before.code {
			t.Fatalf("the fenced code body kept %v after the reload", before.code)
		}
		if _, ok := screenCellWithForeground(screen, pane, cell.Style.Fg); !ok {
			t.Fatalf("the fenced code block is missing after the reload (want %v)", cell.Style.Fg)
		}
		break
	}
	for _, surface := range []struct {
		what string
		pt   image.Point
	}{
		{"orbit frame", r.chrome.frame.Min},
		{"scrollbar thumb", r.inspectorScrollbar.thumb.Min},
	} {
		_, cellStyle, _ := screen.Get(surface.pt.X, surface.pt.Y)
		if got := cellStyle.GetForeground(); got != want.muted {
			t.Fatalf("%s at %v: fg=%v, want the reloaded theme's muted %v", surface.what, surface.pt, got, want.muted)
		}
	}
}

// touchThemeFile forces a theme file's mtime so a reload test never depends on
// filesystem timestamp granularity.
func touchThemeFile(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// writeThemeConfig writes ~/.pollytool/config under home.
func writeThemeConfig(t *testing.T, home, line string) {
	t.Helper()
	dir := filepath.Join(home, userConfigDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, userConfigFileName), []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Acceptance 6, and the reason the tick arm renders on its own: an idle TUI
// never repaints by itself, so an edited theme file has to be picked up, applied
// and painted with no other activity, leaving no stale colors in the transcript,
// the orbit frame, the scrollbar, the bird, or a code block.
func TestThemeReloadRepaintsAnIdleREPL(t *testing.T) {
	themeTestRestoreDefault(t)
	home := themeTestHome(t)
	withDisplayTTY(t)
	r, screen := chromeTestREPL(t)

	path, before := themeReloadFixture(t, home, "solar", "#ff00aa", "#00ff88", "#123456", "#654321")
	fenced := themeReloadSurfaces(t, r, screen)
	r.config.Theme = "solar"
	if _, err := r.applyThemeByName("solar"); err != nil {
		t.Fatalf("apply solar: %v", err)
	}
	r.render()
	if source, ok := r.activeThemeSource(); !ok || source != path {
		t.Fatalf("active theme source = %q, %v, want the solar file", source, ok)
	}
	if _, ok := screenCellWithForeground(screen, r.transcriptW.Inner, before.accent); !ok {
		t.Fatal("the first theme was never painted")
	}

	// The first look only records the file it is watching, so the edit below
	// is what the next poll has to notice.
	start := time.Now()
	if r.pollTheme(start) {
		t.Fatal("the priming poll reported a change")
	}
	if r.needsTick() {
		t.Fatal("an idle REPL needs no tick; this test would not prove the poll repaints on its own")
	}
	_, want := themeReloadFixture(t, home, "solar", "#00aaff", "#ff8800", "#abcdef", "#123456")
	touchThemeFile(t, path, start.Add(2*time.Second))

	if !r.pollTheme(start.Add(2 * time.Second)) {
		t.Fatal("the edited theme file was not detected")
	}
	if got := themeTestResolvedColor("accent"); got != want.accent {
		t.Fatalf("after the edit accent=%v, want %v", got, want.accent)
	}
	r.render()
	themeReloadRepaint(t, r, screen, fenced, before, want)
}

// A reload must not thrash: the watcher has its own ~1s gate, and a look that
// finds the same stamp and the same config selection reports nothing.
func TestThemePollStaysQuietWithoutAChange(t *testing.T) {
	themeTestRestoreDefault(t)
	home := themeTestHome(t)
	withDisplayTTY(t)
	r, _ := chromeTestREPL(t)
	themeReloadFixture(t, home, "solar", "#ff00aa", "#00ff88", "#123456", "#654321")
	r.config.Theme = "solar"
	if _, err := r.applyThemeByName("solar"); err != nil {
		t.Fatalf("apply solar: %v", err)
	}
	start := time.Now()
	if r.pollTheme(start) {
		t.Fatal("the priming poll reported a change")
	}
	if r.pollTheme(start.Add(100 * time.Millisecond)) {
		t.Fatal("the poll ignored its own throttle")
	}
	if r.pollTheme(start.Add(2 * time.Second)) {
		t.Fatal("nothing changed, so the poll must report nothing")
	}
	// Another key in ~/.pollytool/config is not a theme selection.
	writeThemeConfig(t, home, "POLLYTOOL_QUIET=true")
	if r.pollTheme(start.Add(3 * time.Second)) {
		t.Fatal("a config key other than POLLYTOOL_THEME reported a theme change")
	}
}

// A half-written theme file is the case the spec calls out: the previous theme
// stays, the reason is printed once, and the finished file is applied on a later
// tick instead of needing a restart.
func TestThemeReloadRetriesAHalfWrittenFile(t *testing.T) {
	themeTestRestoreDefault(t)
	home := themeTestHome(t)
	withDisplayTTY(t)
	r, _ := chromeTestREPL(t)
	path, before := themeReloadFixture(t, home, "solar", "#ff00aa", "#00ff88", "#123456", "#654321")
	r.config.Theme = "solar"
	if _, err := r.applyThemeByName("solar"); err != nil {
		t.Fatalf("apply solar: %v", err)
	}
	start := time.Now()
	if r.pollTheme(start) {
		t.Fatal("the priming poll reported a change")
	}

	// Half of the document: an editor truncates, then writes.
	if err := os.WriteFile(path, []byte(`{"name":"solar","colors":{"accent":`), 0o644); err != nil {
		t.Fatal(err)
	}
	touchThemeFile(t, path, start.Add(2*time.Second))
	if r.pollTheme(start.Add(2 * time.Second)) {
		t.Fatal("a file that does not parse reported an applied theme")
	}
	if got := themeTestResolvedColor("accent"); got != before.accent {
		t.Fatalf("the broken file changed the theme: accent=%v, want %v", got, before.accent)
	}
	if got := strings.Join(transcriptTexts(r.model), "\n"); !strings.Contains(got, "Warning:") {
		t.Fatalf("a broken theme file printed no notice: %q", got)
	}

	// The retry is deliberate (a failed load does not advance the stamp), but
	// it must not say the same thing once a second.
	if r.pollTheme(start.Add(3 * time.Second)) {
		t.Fatal("a file that does not parse reported an applied theme")
	}
	if got := strings.Count(strings.Join(transcriptTexts(r.model), "\n"), "Warning:"); got != 1 {
		t.Fatalf("the retry printed %d warnings, want one per failure streak", got)
	}

	// The finished file has a new stamp, so the next tick applies it.
	_, want := themeReloadFixture(t, home, "solar", "#00aaff", "#ff8800", "#abcdef", "#123456")
	touchThemeFile(t, path, start.Add(4*time.Second))
	if !r.pollTheme(start.Add(4 * time.Second)) {
		t.Fatal("the finished theme file was not picked up")
	}
	if got := themeTestResolvedColor("accent"); got != want.accent {
		t.Fatalf("after the file was finished accent=%v, want %v", got, want.accent)
	}
}

// A hand edit of POLLYTOOL_THEME in ~/.pollytool/config must be followed. The
// process-wide config cache hides such an edit, so the test warms that cache
// first and the watcher has to read the file itself.
func TestThemeReloadFollowsTheConfigSelection(t *testing.T) {
	themeTestRestoreDefault(t)
	home := themeTestHome(t)
	withDisplayTTY(t)
	r, _ := chromeTestREPL(t)
	themeReloadFixture(t, home, "solar", "#ff00aa", "#00ff88", "#123456", "#654321")
	nordPath, nord := themeReloadFixture(t, home, "nord", "#00aaff", "#ff8800", "#abcdef", "#123456")
	r.config.Theme = "solar"
	if _, err := r.applyThemeByName("solar"); err != nil {
		t.Fatalf("apply solar: %v", err)
	}
	// Warm userConfigValue before the config names anything, the way a process
	// that resolved its flags at startup would have.
	if name, ok := userConfigValue("POLLYTOOL_THEME"); ok && name != "" {
		t.Fatalf("POLLYTOOL_THEME leaked into the test from the developer's config: %q", name)
	}
	start := time.Now()
	if r.pollTheme(start) {
		t.Fatal("the priming poll reported a change")
	}

	writeThemeConfig(t, home, "POLLYTOOL_THEME=nord")
	if !r.pollTheme(start.Add(2 * time.Second)) {
		t.Fatal("a POLLYTOOL_THEME edit in ~/.pollytool/config was not detected")
	}
	if got := themeTestResolvedColor("accent"); got != nord.accent {
		t.Fatalf("after the config switch accent=%v, want nord's %v", got, nord.accent)
	}
	if got := r.activeThemeName(); got != "nord" {
		t.Fatalf("active theme = %q, want nord", got)
	}
	if source, ok := r.activeThemeSource(); !ok || source != nordPath {
		t.Fatalf("active theme source = %q, %v, want the nord file", source, ok)
	}

	// Removing the selection puts the flag value back in effect.
	writeThemeConfig(t, home, "POLLYTOOL_QUIET=true")
	if !r.pollTheme(start.Add(3 * time.Second)) {
		t.Fatal("removing POLLYTOOL_THEME was not detected")
	}
	if got := r.activeThemeName(); got != "solar" {
		t.Fatalf("active theme = %q, want the flag value solar back", got)
	}
}

// /theme opens the picker, switches the session, saves the choice for later
// launches, and returns to the built-in preset.
func TestThemeCommandListsAndSwitches(t *testing.T) {
	themeTestRestoreDefault(t)
	home := themeTestHome(t)
	withDisplayTTY(t)
	r, _ := chromeTestREPL(t)
	solarPath, solar := themeReloadFixture(t, home, "solar", "#ff00aa", "#00ff88", "#123456", "#654321")

	if handled, quit := r.runCommand("/theme"); !handled || quit {
		t.Fatalf("/theme handled=%v quit=%v", handled, quit)
	}
	// With no name it opens the picker on the active theme; Escape leaves the
	// theme alone.
	if r.model.modal == nil {
		t.Fatal("/theme did not open the picker")
	}
	var labels []string
	for _, item := range r.model.modal.items {
		labels = append(labels, item.label)
	}
	listed := strings.Join(labels, "\n")
	for _, want := range []string{"default (active)", "solar", "midnight-parrot"} {
		if !strings.Contains(listed, want) {
			t.Fatalf("/theme picker = %q, want it to mention %q", listed, want)
		}
	}
	if got := r.model.modal.selectedValue(); got != "default" {
		t.Fatalf("picker selection = %q, want the active theme", got)
	}
	// Moving onto a theme previews it; Escape restores the one in effect.
	before := themeTestResolvedColor("accent")
	for r.model.modal.selectedValue() != "solar" {
		r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Down>"})
	}
	if got := themeTestResolvedColor("accent"); got != solar.accent {
		t.Fatalf("preview accent=%v, want %v", got, solar.accent)
	}
	r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Escape>"})
	if got := themeTestResolvedColor("accent"); r.model.modal != nil || got != before {
		t.Fatalf("after Escape modal=%v accent=%v, want closed and %v", r.model.modal != nil, got, before)
	}
	if got := r.activeThemeName(); got != "default" {
		t.Fatalf("preview changed the followed theme to %q", got)
	}
	// Ctrl-C dismisses the same way: the previewed theme must not stay on
	// screen.
	if handled, quit := r.runCommand("/theme"); !handled || quit {
		t.Fatalf("/theme handled=%v quit=%v", handled, quit)
	}
	for r.model.modal.selectedValue() != "solar" {
		r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Down>"})
	}
	if got := themeTestResolvedColor("accent"); got != solar.accent {
		t.Fatalf("preview accent=%v, want %v", got, solar.accent)
	}
	r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-c>"})
	if got := themeTestResolvedColor("accent"); r.model.modal != nil || got != before {
		t.Fatalf("after Ctrl-C modal=%v accent=%v, want closed and %v", r.model.modal != nil, got, before)
	}
	r.model.clearDisplay()

	if handled, quit := r.runCommand("/theme solar"); !handled || quit {
		t.Fatalf("/theme solar handled=%v quit=%v", handled, quit)
	}
	switched := strings.Join(transcriptTexts(r.model), "\n")
	if !strings.Contains(switched, solarPath) {
		t.Fatalf("/theme solar reply = %q, want the file that won %q", switched, solarPath)
	}
	if got := themeTestResolvedColor("accent"); got != solar.accent {
		t.Fatalf("after /theme solar accent=%v, want %v", got, solar.accent)
	}
	// A deliberate switch is saved as the launch default; a preview is not.
	if name, ok := themeSelectionFromConfig(); !ok || name != "solar" {
		t.Fatalf("saved theme = %q, %v, want solar", name, ok)
	}
	r.model.clearDisplay()

	if handled, quit := r.runCommand("/theme"); !handled || quit {
		t.Fatalf("/theme handled=%v quit=%v", handled, quit)
	}
	if r.model.modal == nil || r.model.modal.selectedValue() != "solar" {
		t.Fatal("/theme did not open the picker on the switched theme")
	}
	// Enter on another row switches the session exactly as "/theme name" does.
	for r.model.modal.selectedValue() != "azure-parrot" {
		r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Up>"})
	}
	r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	if got := r.activeThemeName(); r.model.modal != nil || got != "azure-parrot" {
		t.Fatalf("after Enter modal=%v active=%q, want closed and azure-parrot", r.model.modal != nil, got)
	}
	if name, _ := themeSelectionFromConfig(); name != "azure-parrot" {
		t.Fatalf("saved theme after the picker = %q, want azure-parrot", name)
	}
	r.model.clearDisplay()

	if handled, quit := r.runCommand("/theme default"); !handled || quit {
		t.Fatalf("/theme default handled=%v quit=%v", handled, quit)
	}
	restored := strings.Join(transcriptTexts(r.model), "\n")
	if !strings.Contains(restored, "default (builtin preset)") {
		t.Fatalf("/theme default reply = %q, want the builtin preset", restored)
	}
	if got, want := themeTestResolvedColor("accent"), themeTestWantColor(t, style.DefaultTheme().Colors["accent"]); got != want {
		t.Fatalf("after /theme default accent=%v, want the builtin %v", got, want)
	}
	r.model.clearDisplay()

	// An unknown name keeps the previous theme and reports the reason.
	if handled, quit := r.runCommand("/theme nope"); !handled || quit {
		t.Fatalf("/theme nope handled=%v quit=%v", handled, quit)
	}
	if got, want := themeTestResolvedColor("accent"), themeTestWantColor(t, style.DefaultTheme().Colors["accent"]); got != want {
		t.Fatalf("an unknown theme changed the table: accent=%v, want %v", got, want)
	}
	if got := strings.Join(transcriptTexts(r.model), "\n"); !strings.Contains(got, "Warning:") {
		t.Fatalf("/theme nope printed no warning: %q", got)
	}
	r.model.clearDisplay()

	r.runCommand("/theme solar extra")
	if got := strings.Join(transcriptTexts(r.model), "\n"); !strings.Contains(got, "usage: /theme") {
		t.Fatalf("/theme with two arguments = %q", got)
	}
}

// The line frontend has no color table to keep a switch in and no tick to
// reload on, so /theme must report that it is unavailable instead of applying a
// theme the process cannot hold onto.
func TestThemeCommandIsUnavailableOutsideTheTUI(t *testing.T) {
	themeTestRestoreDefault(t)
	home := themeTestHome(t)
	themeReloadFixture(t, home, "solar", "#ff00aa", "#00ff88", "#123456", "#654321")
	var out bytes.Buffer
	ctx := newWriterReplCommandContext(&Config{}, nil, &out)
	for _, line := range []string{"/theme", "/theme solar"} {
		handled, quit, err := defaultReplCommands.dispatch(line, ctx)
		if err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		if !handled || quit {
			t.Fatalf("%s handled=%v quit=%v", line, handled, quit)
		}
		if !strings.Contains(out.String(), "theme switching unavailable") {
			t.Fatalf("%s reply = %q", line, out.String())
		}
	}
	if got, want := themeTestResolvedColor("accent"), themeTestWantColor(t, style.DefaultTheme().Colors["accent"]); got != want {
		t.Fatalf("the fallback REPL applied a theme: accent=%v, want %v", got, want)
	}
}

// /theme runs while a turn is in flight (the style epoch is what makes that
// safe) and is listed by /help like every other command.
func TestThemeCommandIsRegisteredBusySafe(t *testing.T) {
	if !defaultReplCommands.busySafeCommand("/theme solar") {
		t.Fatal("/theme must run while a turn is streaming, not queue behind it")
	}
	if help := strings.Join(defaultReplCommands.helpLines(), "\n"); !strings.Contains(help, "  /theme") || !strings.Contains(help, "pick a theme, or switch this session's theme by name") {
		t.Fatalf("/help does not list /theme: %q", help)
	}
	if detail := strings.Join(defaultReplCommands.helpFor("/theme"), "\n"); !strings.Contains(detail, "usage: /theme [name]") {
		t.Fatalf("/help /theme = %q", detail)
	}
}

// Completion enumerates the names /theme accepts, built-in and user, so a
// theme can be switched without remembering its file name.
func TestThemeCommandCompletesNames(t *testing.T) {
	themeTestRestoreDefault(t)
	home := themeTestHome(t)
	themeReloadFixture(t, home, "solar", "#ff00aa", "#00ff88", "#123456", "#654321")

	completed, matches, ok := defaultReplCommands.complete("/theme s", nil)
	if !ok || completed != "/theme solar" {
		t.Fatalf("complete(/theme s) = %q, %v, %v, want /theme solar", completed, matches, ok)
	}
	for _, want := range []string{"default", "midnight-parrot", "solar"} {
		if got := completeThemeCommand(nil, []string{"/theme"}, ""); !slices.Contains(got, want) {
			t.Fatalf("theme completion = %v, want it to offer %q", got, want)
		}
	}
	if got := completeThemeCommand(nil, []string{"/theme", "solar"}, ""); len(got) != 0 {
		t.Fatalf("a second argument completed to %v, want nothing", got)
	}
}
