package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

// ansiSeq matches CSI and OSC escape sequences so visibleWidth can ignore them.
var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07]*\x07|\x1b[78]`)

// visibleWidth returns the number of terminal columns the string occupies,
// ignoring ANSI control sequences. ONLY CORRECT FOR NARROW RUNES — callers
// must not pass CJK, wide emoji, or combining marks; this function will
// undercount and break renderBar's narrowing loop. If the bar ever needs to
// display user-supplied strings, swap this for go-runewidth.
func visibleWidth(s string) int {
	stripped := ansiSeq.ReplaceAllString(s, "")
	return utf8.RuneCountInString(stripped)
}

// humanizeTokens formats a token count as a compact string: "812", "1.2k",
// "45.7k", "1.2M". Exactly one fractional digit for k/M below 100, zero above.
// Fractional values are truncated (not rounded) so e.g. 99999 renders as
// "99.9k" rather than rolling over to "100.0k".
func humanizeTokens(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 100_000:
		whole := n / 1000
		frac := (n % 1000) / 100
		return fmt.Sprintf("%d.%dk", whole, frac)
	case n < 1_000_000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		whole := n / 1_000_000
		frac := (n % 1_000_000) / 100_000
		return fmt.Sprintf("%d.%dM", whole, frac)
	}
}

type barState struct {
	model       string
	contextName string
	turnState   string
	turnStarted time.Time
	lastIn      int
	lastOut     int
	tools       int
	skills      int
	utf8        bool
}

// formatElapsed produces "0.1s".."59.9s", then "1m02s".
func formatElapsed(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	m := int(d / time.Minute)
	s := int((d % time.Minute) / time.Second)
	return fmt.Sprintf("%dm%02ds", m, s)
}

// renderBar turns a state snapshot into a single styled line of at most `width`
// visible columns. Fields are dropped in this order when the line doesn't fit:
// model, elapsed, tools, skills. Returns plain text (no ANSI styling). The
// caller (paintLocked) wraps the result in colors.
// Drop priorities for renderBar fields. Higher value = dropped sooner when
// the line doesn't fit. Named constants prevent re-introducing the bug where
// these were inverted relative to the documented drop order.
const (
	keepAlways  = 0
	dropSkills  = 1
	dropTools   = 2
	dropElapsed = 3
	dropModel   = 4
)

func renderBar(s barState, width int) string {
	sep := " · "
	if !s.utf8 {
		sep = " | "
	}

	type field struct {
		drop int
		text string
	}

	fields := []field{}
	if s.model != "" {
		fields = append(fields, field{drop: dropModel, text: s.model})
	}
	fields = append(fields, field{drop: keepAlways, text: s.contextName})
	fields = append(fields, field{drop: keepAlways, text: s.turnState})
	if !s.turnStarted.IsZero() {
		fields = append(fields, field{drop: dropElapsed, text: formatElapsed(time.Since(s.turnStarted))})
	}
	tokens := fmt.Sprintf("%s→%s", humanizeTokens(s.lastIn), humanizeTokens(s.lastOut))
	fields = append(fields, field{drop: keepAlways, text: tokens})
	if s.tools > 0 {
		fields = append(fields, field{drop: dropTools, text: fmt.Sprintf("tools:%d", s.tools)})
	}
	if s.skills > 0 {
		fields = append(fields, field{drop: dropSkills, text: fmt.Sprintf("skills:%d", s.skills)})
	}

	join := func(fs []field) string {
		parts := make([]string, len(fs))
		for i, f := range fs {
			parts[i] = f.text
		}
		return " " + strings.Join(parts, sep) + " "
	}

	line := join(fields)
	for visibleWidth(line) > width {
		idx := -1
		best := 0
		for i, f := range fields {
			if f.drop > best {
				best = f.drop
				idx = i
			}
		}
		if idx < 0 {
			return truncateEllipsis(line, width)
		}
		fields = append(fields[:idx], fields[idx+1:]...)
		line = join(fields)
	}
	return line
}

// truncateEllipsis cuts s so its visibleWidth does not exceed width, appending
// "…". Plain-text only: callers must not pass strings containing ANSI escape
// sequences, since this function counts bytes-as-runes-as-columns and will
// happily slice through the middle of a CSI parameter, leaving an unterminated
// escape that poisons downstream styling.
func truncateEllipsis(s string, width int) string {
	if visibleWidth(s) <= width {
		return s
	}
	if width <= 0 {
		return ""
	}
	if width == 1 {
		return "…"
	}
	runes := []rune(s)
	out := make([]rune, 0, width)
	w := 0
	for _, r := range runes {
		if w >= width-1 {
			break
		}
		out = append(out, r)
		w++
	}
	return string(out) + "…"
}

const (
	esc         = "\x1b"
	saveCursor  = esc + "7"
	restCursor  = esc + "8"
	showCursor  = esc + "[?25h"
	clearLine   = esc + "[2K"
	resetRegion = esc + "[r"
	titlePush   = esc + "[22;0t"
	titlePop    = esc + "[23;0t"
)

// sizer reports the current terminal dimensions (columns, rows).
type sizer interface {
	Size() (int, int, error)
}

// statusBar pins a one-line status display at the bottom of the terminal using
// DECSTBM. The single mutex serializes the bar's own state mutations and its
// own paint bursts; it does NOT synchronize against external goroutines that
// write directly to the same Writer. Callers that share the writer with the
// streaming agent output must accept occasional visual interleaving, or route
// all stdio through a bar-aware writer (out of scope for this type).
type statusBar struct {
	out io.Writer
	sz  sizer

	mu        sync.Mutex
	installed bool
	torn      bool
	state     barState
	cachedW   int
	cachedH   int
	done      chan struct{}
}

// termSizer reads the current terminal size from an FD via x/term.
type termSizer struct{ fd int }

func (t termSizer) Size() (int, int, error) { return term.GetSize(t.fd) }

func newStatusBar(out io.Writer, sz sizer) *statusBar {
	return &statusBar{
		out: out,
		sz:  sz,
		state: barState{
			contextName: "-",
			turnState:   "idle",
			utf8:        utf8Locale(os.Getenv("LANG") + "," + os.Getenv("LC_ALL")),
		},
	}
}

// utf8Locale returns true if the LANG/LC_ALL string contains "utf" (case-insensitive).
func utf8Locale(env string) bool {
	return strings.Contains(strings.ToLower(env), "utf")
}

func (b *statusBar) SetModel(m string) {
	b.mu.Lock()
	b.state.model = m
	b.mu.Unlock()
}

func (b *statusBar) SetContext(name string) {
	b.mu.Lock()
	if name == "" {
		b.state.contextName = "-"
	} else {
		b.state.contextName = name
	}
	b.mu.Unlock()
}

func (b *statusBar) SetCounts(tools, skills int) {
	b.mu.Lock()
	b.state.tools = tools
	b.state.skills = skills
	b.mu.Unlock()
}

// errStatusBarUnavailable is returned from Install when the terminal does not
// meet the minimum requirements. Callers fall back to the window-title Status.
var errStatusBarUnavailable = errors.New("terminal does not support status bar")

// Install reserves the bottom row and paints the initial status line.
func (b *statusBar) Install() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.installed {
		return nil
	}
	w, h, err := b.sz.Size()
	if err != nil || h < 10 || w < 20 || os.Getenv("TERM") == "dumb" {
		return errStatusBarUnavailable
	}
	b.cachedW, b.cachedH = w, h
	b.installed = true

	fmt.Fprint(b.out, titlePush)
	fmt.Fprintf(b.out, esc+"]0;polly · %s\x07", b.state.contextName)
	// Erase the visible screen and home the cursor so the user enters the
	// REPL on a clean slate instead of layering on top of the previous
	// terminal contents. \x1b[2J leaves scrollback intact on modern terms.
	fmt.Fprint(b.out, esc+"[2J", esc+"[H")
	fmt.Fprintf(b.out, esc+"[1;%dr", h-1)
	fmt.Fprint(b.out, esc+"[1;1H")
	b.paintLocked()
	b.done = make(chan struct{})
	b.startTicker(b.done)
	b.watchResizes(b.done)
	return nil
}

// Uninstall resets the scroll region and restores the window title. Idempotent.
func (b *statusBar) Uninstall() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.installed || b.torn {
		return
	}
	b.torn = true
	if b.done != nil {
		close(b.done)
		b.done = nil
	}
	fmt.Fprint(b.out, saveCursor)
	fmt.Fprintf(b.out, esc+"[%d;1H", b.cachedH)
	fmt.Fprint(b.out, clearLine, resetRegion, restCursor, showCursor)
	fmt.Fprint(b.out, titlePop)
	fmt.Fprintf(b.out, esc+"]0;\x07")
}

// paintLocked writes a single status-line repaint. Caller must hold b.mu.
// The full save+move+clear+content+restore sequence is built in memory and
// emitted in one Write so it can't interleave with concurrent stderr writes
// from printToolStart/printToolEnd or other goroutines sharing the writer.
func (b *statusBar) paintLocked() {
	if !b.installed || b.torn {
		return
	}
	styled := styleStatusLine(renderBar(b.state, b.cachedW), b.state.turnState)
	fmt.Fprint(b.out, saveCursor+fmt.Sprintf(esc+"[%d;1H", b.cachedH)+clearLine+styled+restCursor)
}

// styleStatusLine wraps the rendered line in barFieldStyle and substitutes a
// highlighted (or error-colored) version of the turnState token. The
// substitution preserves the field-style wrapping; we just swap one
// occurrence of the state text for the active style.
func styleStatusLine(line, turnState string) string {
	base := barFieldStyle.Styled(line)
	if turnState == "" || turnState == "idle" {
		return base
	}
	if turnState == "error" {
		return strings.Replace(base, turnState, barErrorStyle.Styled(turnState), 1)
	}
	return strings.Replace(base, turnState, barActiveStyle.Styled(turnState), 1)
}

// setState atomically mutates state and forces an immediate repaint.
func (b *statusBar) setState(mut func(*barState)) {
	b.mu.Lock()
	mut(&b.state)
	b.paintLocked()
	b.mu.Unlock()
}

// tickInterval is how often the redraw goroutine wakes up to refresh the bar.
// Fast enough that elapsed time updates feel live, slow enough that an idle bar
// barely costs anything. The line-diff cache suppresses no-op paints.
const tickInterval = 250 * time.Millisecond

// startTicker spawns the redraw goroutine. It stops when done is closed, or
// when it observes the bar has been torn down (so a forgotten close still
// lets the goroutine exit within one tick).
func (b *statusBar) startTicker(done <-chan struct{}) {
	go func() {
		t := time.NewTicker(tickInterval)
		defer t.Stop()
		var last string
		for {
			select {
			case <-done:
				return
			case <-t.C:
				b.mu.Lock()
				if !b.installed || b.torn {
					b.mu.Unlock()
					return
				}
				line := renderBar(b.state, b.cachedW)
				if line != last {
					b.paintLocked()
					last = line
				}
				b.mu.Unlock()
			}
		}
	}()
}

// handleResize re-measures the terminal and either reapplies the scroll
// region + repaints, or uninstalls the bar if the new height is too small.
// Safe to call from a signal goroutine. Do not hold b.mu when entering this
// function: the too-small branch calls Uninstall, which acquires b.mu itself.
func (b *statusBar) handleResize() {
	w, h, err := b.sz.Size()
	if err != nil {
		return
	}
	if h < 3 {
		// Uninstall acquires b.mu — must be called BEFORE locking below.
		b.Uninstall()
		return
	}
	b.mu.Lock()
	if !b.installed || b.torn {
		b.mu.Unlock()
		return
	}
	b.cachedW, b.cachedH = w, h
	fmt.Fprintf(b.out, esc+"[1;%dr", h-1)
	b.paintLocked()
	b.mu.Unlock()
}

// --- StatusHandler implementation ---

func (b *statusBar) Start() {
	b.setState(func(s *barState) {
		s.turnStarted = time.Now()
		s.turnState = "waiting"
	})
}

func (b *statusBar) Stop() {
	b.setState(func(s *barState) {
		s.turnState = "idle"
		s.turnStarted = time.Time{}
	})
}

// Clear is called by the agent loop after a tool finishes (OnToolEnd) or
// errors (OnError). The previous turnState was "tool: <name>", which would
// otherwise linger in the bar for seconds while the model decides what to do
// next. Reset to "waiting" so the bar stops claiming an active tool.
func (b *statusBar) Clear() {
	b.setState(func(s *barState) { s.turnState = "waiting" })
}

func (b *statusBar) ShowSpinner(text string) {
	b.setState(func(s *barState) {
		if text == "" {
			text = "waiting"
		}
		s.turnState = text
	})
}

func (b *statusBar) ShowThinking(tokens int) {
	b.setState(func(s *barState) { s.turnState = "thinking" })
}

func (b *statusBar) ShowToolCall(name string) {
	b.setState(func(s *barState) { s.turnState = "tool: " + name })
}

func (b *statusBar) ClearForContent() {
	b.setState(func(s *barState) { s.turnState = "streaming" })
}

func (b *statusBar) UpdateStreamingProgress(bytes int) {}

func (b *statusBar) UpdateThinkingProgress(tokens int) {
	b.setState(func(s *barState) { s.turnState = "thinking" })
}

func (b *statusBar) RecordTurnTokens(in, out int) {
	b.setState(func(s *barState) { s.lastIn = in; s.lastOut = out })
}

// snapshot returns a copy of the current state under the mutex. Test-only
// helper that lets state assertions stay race-free even when the ticker and
// SIGWINCH goroutines are live.
func (b *statusBar) snapshot() barState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Compile-time interface assertion.
var _ StatusHandler = (*statusBar)(nil)

// watchResizes spawns a goroutine that listens for SIGWINCH until done closes.
func (b *statusBar) watchResizes(done <-chan struct{}) {
	// Buffer 1 coalesces signal bursts: each handleResize re-measures from
	// scratch, so dropped signals never lose size info.
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-done:
				return
			case <-ch:
				b.handleResize()
			}
		}
	}()
}
