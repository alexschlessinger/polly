package main

import (
	"encoding/base64"
	"maps"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	tcell "github.com/gdamore/tcell/v3"
)

// terminalFX emits window-level terminal effects — title, taskbar progress,
// OSC 7501 program status, desktop notifications — through the tcell screen so
// every escape shares tcell's output path and lifecycle (tcell pushes the title
// stack on entry and pops it on exit). Callers pass desired state each frame;
// only changes reach the terminal. All methods are called from the render/event
// goroutine and are inert on a nil controller, so unit tests and the fallback
// REPL need no stubs.
type terminalFX struct {
	screen tcell.Screen
	// printer is tcell's raw writer (tScreen.Print), used for OSC 9;4 progress
	// and OSC 7501 program status sequences that tcell has no API for; nil when
	// the screen implementation doesn't expose it.
	printer         interface{ Print(string) }
	progressOK      bool
	programStatusOK bool
	lastTitle       string
	lastProgress    string
	lastRootStatus  string
	lastChildStatus map[string]string
}

func newTerminalFX(screen tcell.Screen) *terminalFX {
	fx := &terminalFX{
		screen:          screen,
		printer:         screenPrinter(screen),
		progressOK:      supportsProgressOSC(os.Getenv),
		programStatusOK: supportsProgramStatusOSC(os.Getenv),
	}
	return fx
}

type ttyStringPrinter struct {
	tty tcell.Tty
}

func (p ttyStringPrinter) Print(s string) {
	_, _ = p.tty.Write([]byte(s))
}

// screenPrinter unwraps Polly's screen decorators (themedScreen and
// trackedPaintScreen) so raw OSC escapes still reach tcell's tScreen.Print or
// its underlying Tty.
func screenPrinter(screen tcell.Screen) interface{ Print(string) } {
	for screen != nil {
		if p, ok := screen.(interface{ Print(string) }); ok {
			return p
		}
		switch wrapped := screen.(type) {
		case themedScreen:
			screen = wrapped.Screen
		case *trackedPaintScreen:
			screen = wrapped.Screen
		default:
			if tty, ok := screen.Tty(); ok && tty != nil {
				return ttyStringPrinter{tty: tty}
			}
			return nil
		}
	}
	return nil
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

// supportsProgramStatusOSC reports whether the terminal understands the
// Program Status Protocol (OSC 7501), such as Rex.
func supportsProgramStatusOSC(env func(string) string) bool {
	return env("REX_SESSION") != "" || env("REX_BLOCK") != ""
}

// Desired taskbar progress, expressed as the OSC 9;4 payload.
const (
	progressNone = "0"     // remove indicator
	progressBusy = "3;0"   // indeterminate: a turn is running
	progressFail = "2;100" // error badge; holds until the next turn starts
)

// programStatusRecord describes one OSC 7501 status report.
type programStatusRecord struct {
	ID    string // Empty for the root record; "worker/<id>" for child workers.
	State string // idle, working, done, blocked, error, clear
	Kind  string // permission, question, auth (only with blocked)
	App   string // Stable app identifier (root carries "polly")
	Title string // Short human-readable label (encoded as base64)
	Msg   string // Single-line human-readable status message (encoded as base64)
}

// programStatusSnapshot is the complete desired OSC 7501 state for one frame.
type programStatusSnapshot struct {
	Root     programStatusRecord
	Children []programStatusRecord
}

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

func (fx *terminalFX) setProgramStatus(snap programStatusSnapshot) {
	if fx == nil || fx.printer == nil || !fx.programStatusOK {
		return
	}
	if rootSeq := formatProgramStatusOSC(snap.Root); rootSeq != "" && rootSeq != fx.lastRootStatus {
		fx.lastRootStatus = rootSeq
		fx.printer.Print(rootSeq)
	}

	nextChildren := make(map[string]string, len(snap.Children))
	for _, child := range snap.Children {
		if child.ID == "" {
			continue
		}
		seq := formatProgramStatusOSC(child)
		if seq == "" {
			continue
		}
		nextChildren[child.ID] = seq
		if fx.lastChildStatus[child.ID] != seq {
			fx.printer.Print(seq)
		}
	}

	// Clear any previously reported child record that has settled or vanished.
	if len(fx.lastChildStatus) > 0 {
		stale := make([]string, 0, len(fx.lastChildStatus))
		for id := range fx.lastChildStatus {
			if _, stillPresent := nextChildren[id]; !stillPresent {
				stale = append(stale, id)
			}
		}
		slices.Sort(stale)
		for _, id := range stale {
			if clearSeq := formatProgramStatusOSC(programStatusRecord{ID: id, State: "clear"}); clearSeq != "" {
				fx.printer.Print(clearSeq)
			}
		}
	}
	if len(nextChildren) == 0 {
		fx.lastChildStatus = nil
	} else {
		fx.lastChildStatus = maps.Clone(nextChildren)
	}
}

// formatProgramStatusOSC encodes one OSC 7501 escape sequence according to the
// Program Status Protocol specification. Returns "" when rec.State is empty.
func formatProgramStatusOSC(rec programStatusRecord) string {
	if rec.State == "" {
		return ""
	}
	var pairs []string
	pairs = append(pairs, "state="+rec.State)
	if id := sanitizeProgramStatusID(rec.ID); id != "" {
		pairs = append(pairs, "id="+id)
	} else if rec.ID != "" {
		// Malformed non-empty ID must not fall back to the root record.
		return ""
	}
	if rec.State == "blocked" && rec.Kind != "" {
		pairs = append(pairs, "kind="+rec.Kind)
	}
	if rec.App != "" {
		pairs = append(pairs, "app="+rec.App)
	}
	if title := sanitizeProgramStatusText(rec.Title, 192); title != "" {
		pairs = append(pairs, "title="+base64.StdEncoding.EncodeToString([]byte(title)))
	}
	if msg := sanitizeProgramStatusText(rec.Msg, 512); msg != "" {
		pairs = append(pairs, "msg="+base64.StdEncoding.EncodeToString([]byte(msg)))
	}
	return "\x1b]7501;" + strings.Join(pairs, ":") + "\x1b\\"
}

// sanitizeProgramStatusText removes C0/DEL/C1 control characters (required by
// OSC 7501), collapses whitespace to single spaces, and truncates to maxBytes
// on a UTF-8 rune boundary.
func sanitizeProgramStatusText(s string, maxBytes int) string {
	cleaned := strings.Map(func(r rune) rune {
		if r <= 0x1f || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return ' '
		}
		return r
	}, s)
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	if maxBytes > 0 && len(cleaned) > maxBytes {
		cleaned = truncateUTF8Bytes(cleaned, maxBytes)
	}
	return cleaned
}

func truncateUTF8Bytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	i := limit
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

// sanitizeProgramStatusID normalizes a slash-delimited OSC 7501 record id so
// every segment matches [A-Za-z0-9_.+-]{1,32}.
func sanitizeProgramStatusID(id string) string {
	if id == "" {
		return ""
	}
	rawParts := strings.Split(id, "/")
	parts := make([]string, 0, len(rawParts))
	for _, seg := range rawParts {
		var b strings.Builder
		for _, r := range seg {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
				r == '_' || r == '.' || r == '+' || r == '-' {
				b.WriteRune(r)
			} else {
				b.WriteByte('-')
			}
			if b.Len() == 32 {
				break
			}
		}
		clean := strings.Trim(b.String(), "-")
		if clean == "" {
			continue
		}
		parts = append(parts, clean)
		if len(parts) == 8 {
			break
		}
	}
	return strings.Join(parts, "/")
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

// shutdown clears any lingering taskbar progress and OSC 7501 status records.
// Must run while the screen is still active; tcell itself restores the window
// title on Close.
func (fx *terminalFX) shutdown() {
	fx.setProgress(progressNone)
	if fx != nil && fx.printer != nil && fx.programStatusOK && (fx.lastRootStatus != "" || len(fx.lastChildStatus) > 0) {
		fx.printer.Print("\x1b]7501;state=clear\x1b\\")
		fx.lastRootStatus = ""
		fx.lastChildStatus = nil
	}
}
