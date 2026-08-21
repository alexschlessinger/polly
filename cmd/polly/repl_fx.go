package main

import (
	"os"
	"strings"

	tcell "github.com/gdamore/tcell/v3"
)

// terminalFX emits window-level terminal effects — title, taskbar progress,
// desktop notifications — through the tcell screen so every escape shares
// tcell's output path and lifecycle (tcell pushes the title stack on entry and
// pops it on exit). Callers pass desired state each frame; only changes reach
// the terminal. All methods are called from the render/event goroutine and are
// inert on a nil controller, so unit tests and the fallback REPL need no stubs.
type terminalFX struct {
	screen tcell.Screen
	// printer is tcell's raw writer (tScreen.Print), used only for the ConEmu
	// progress OSC that tcell has no API for; nil when the screen
	// implementation doesn't expose it.
	printer      interface{ Print(string) }
	progressOK   bool
	lastTitle    string
	lastProgress string
}

func newTerminalFX(screen tcell.Screen) *terminalFX {
	fx := &terminalFX{screen: screen, progressOK: supportsProgressOSC(os.Getenv)}
	fx.printer, _ = screen.(interface{ Print(string) })
	return fx
}

// supportsProgressOSC reports whether the terminal understands the ConEmu
// OSC 9;4 progress sequence. Gated by terminal identity rather than emitted
// blind: iTerm2 parses any OSC 9 as a desktop notification, so a progress
// write there would surface as notification spam.
func supportsProgressOSC(env func(string) string) bool {
	if strings.EqualFold(env("TERM_PROGRAM"), "ghostty") {
		return true
	}
	return env("WT_SESSION") != ""
}

// Desired taskbar progress, expressed as the OSC 9;4 payload.
const (
	progressNone = "0"     // remove indicator
	progressBusy = "3;0"   // indeterminate: a turn is running
	progressFail = "2;100" // error badge; holds until the next turn starts
)

func (fx *terminalFX) setTitle(title string) {
	if fx == nil || fx.screen == nil || title == fx.lastTitle {
		return
	}
	fx.lastTitle = title
	fx.screen.SetTitle(title)
}

func (fx *terminalFX) setProgress(state string) {
	if fx == nil || fx.printer == nil || !fx.progressOK || state == fx.lastProgress {
		return
	}
	fx.lastProgress = state
	fx.printer.Print("\x1b]9;4;" + state + "\x1b\\")
}

func (fx *terminalFX) notify(title, body string) {
	if fx == nil || fx.screen == nil {
		return
	}
	fx.screen.ShowNotification(sanitizeNotice(title), sanitizeNotice(body))
}

// sanitizeNotice strips control runes so notification text can't terminate or
// corrupt the OSC sequence carrying it.
func sanitizeNotice(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// shutdown clears any lingering taskbar progress. Must run while the screen is
// still active; tcell itself restores the window title on Close.
func (fx *terminalFX) shutdown() {
	fx.setProgress(progressNone)
}
