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
// caller wraps the result in colors via styleStatusLine.
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
	clearLine   = esc + "[2K"
	resetRegion = esc + "[r"
)

// sizer reports the current terminal dimensions (columns, rows).
type sizer interface {
	Size() (int, int, error)
}

// statusBar keeps the bottom row reserved for a fixed status line via DECSTBM.
// User-visible content scrolls inside rows 1..h-1, while the bar is repainted
// at row h with absolute cursor addressing and the prior cursor restored.
type statusBar struct {
	out io.Writer
	sz  sizer

	mu        sync.Mutex
	installed bool
	torn      bool
	resizing  bool
	visible   bool // false when the terminal is too small for the bar
	barShown  bool // true when bar is currently painted on the terminal
	paintedW  int  // visibleWidth of the last rendered bar text
	paintedLn string
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

const minStatusBarCols = 28

func supportsStatusBar(w, h int) bool {
	return h >= 10 && w >= minStatusBarCols && os.Getenv("TERM") != "dumb"
}

func paintWidth(w int) int {
	if w <= 1 {
		return 1
	}
	// Reserve the last column so painting the bar can't trigger autowrap on
	// a freshly-shown bar at full terminal width.
	return w - 1
}

func cursorPos(row, col int) string {
	return fmt.Sprintf(esc+"[%d;%dH", row, col)
}

func scrollRegion(bottom int) string {
	return fmt.Sprintf(esc+"[1;%dr", bottom)
}

// Install paints the initial bar and starts the ticker + resize watcher.
func (b *statusBar) Install() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.installed {
		return nil
	}
	w, h, err := b.sz.Size()
	if err != nil || !supportsStatusBar(w, h) {
		return errStatusBarUnavailable
	}
	b.cachedW, b.cachedH = w, h
	b.installed = true
	b.visible = true
	b.applyRegionLocked()
	b.paintLocked()
	b.done = make(chan struct{})
	b.startTicker(b.done)
	b.watchResizes(b.done)
	return nil
}

// Uninstall clears the bar, resets the scroll region, and stops the ticker.
// Idempotent.
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
	if b.barShown {
		b.clearBarLocked(b.wrappedRowsForWidth(b.cachedW))
	}
	b.resetRegionLocked()
	b.visible = false
}

func (b *statusBar) applyRegionLocked() {
	if b.cachedH < 2 {
		return
	}
	fmt.Fprintf(b.out, "%s%s%s", saveCursor, scrollRegion(b.cachedH-1), restCursor)
}

func (b *statusBar) resetRegionLocked() {
	fmt.Fprintf(b.out, "%s%s%s", saveCursor, resetRegion, restCursor)
}

func (b *statusBar) wrappedRowsForWidth(width int) int {
	if b.paintedW <= 0 {
		return 1
	}
	pw := paintWidth(width)
	if pw <= 0 {
		pw = 1
	}
	rows := (b.paintedW + pw - 1) / pw
	if rows < 1 {
		return 1
	}
	return rows
}

// paintLocked repaints the fixed bottom-row bar and restores the content
// cursor. Caller must hold b.mu.
func (b *statusBar) paintLocked() {
	if !b.installed || b.torn || !b.visible {
		return
	}
	rendered := renderBar(b.state, paintWidth(b.cachedW))
	styled := styleStatusLine(rendered, b.state.turnState)
	fmt.Fprintf(b.out, "%s%s%s%s%s", saveCursor, cursorPos(b.cachedH, 1), clearLine, styled, restCursor)
	b.barShown = true
	b.paintedW = visibleWidth(rendered)
	b.paintedLn = rendered
}

// clearBarLocked clears the bar row and any wrapped residue the terminal may
// have introduced above it after a width shrink. Caller must hold b.mu.
func (b *statusBar) clearBarLocked(rows int) {
	if rows < 1 {
		rows = 1
	}
	if b.cachedH < 1 {
		return
	}
	start := b.cachedH - rows + 1
	if start < 1 {
		start = 1
	}
	var seq strings.Builder
	seq.WriteString(saveCursor)
	for row := start; row <= b.cachedH; row++ {
		seq.WriteString(cursorPos(row, 1))
		seq.WriteString(clearLine)
	}
	seq.WriteString(restCursor)
	fmt.Fprint(b.out, seq.String())
	b.barShown = false
	b.paintedLn = ""
}

func (b *statusBar) clearWidthForResize(minWidth, finalWidth int) int {
	clearWidth := finalWidth
	if minWidth > 0 && minWidth < clearWidth {
		clearWidth = minWidth
	}
	// If a resize burst finishes at the same width it started (or wider), we
	// may have missed transient narrower widths because SIGWINCH coalesced.
	// Clear conservatively for the narrowest visible supported width so wrapped
	// bar residue from a skipped narrow phase doesn't survive the final repaint.
	if finalWidth >= b.cachedW && clearWidth > minStatusBarCols {
		clearWidth = minStatusBarCols
	}
	if clearWidth < 1 {
		return 1
	}
	return clearWidth
}

// Write implements io.Writer. Content writes stay in the scroll region above
// the fixed bar. The mutex only serializes our own writes so ticker / resize
// repaints cannot interleave with prompt, tool, or streaming output.
func (b *statusBar) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.out.Write(p)
}

// styleStatusLine wraps the rendered line in barFieldStyle and substitutes a
// highlighted (or error-colored) version of the turnState token.
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
	if !b.resizing {
		b.paintLocked()
	}
	b.mu.Unlock()
}

// tickInterval is how often the redraw goroutine wakes up to refresh the bar.
// Fast enough that elapsed time updates feel live, slow enough that an idle bar
// barely costs anything. The line-diff cache suppresses no-op paints.
const tickInterval = 250 * time.Millisecond

// startTicker spawns the redraw goroutine. It stops when done is closed.
func (b *statusBar) startTicker(done <-chan struct{}) {
	go func() {
		t := time.NewTicker(tickInterval)
		defer t.Stop()
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
				line := renderBar(b.state, paintWidth(b.cachedW))
				if !b.resizing && b.visible && (!b.barShown || line != b.paintedLn) {
					b.paintLocked()
				}
				b.mu.Unlock()
			}
		}
	}()
}

// handleResizeBurst applies a settled resize after the watcher has observed a
// burst of SIGWINCH events. minWidth is the narrowest width seen in the burst;
// finalW/finalH are the settled dimensions.
func (b *statusBar) handleResizeBurst(minWidth, finalW, finalH int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.installed || b.torn {
		return
	}
	defer func() { b.resizing = false }()

	clearRows := 0
	if b.barShown {
		clearRows = b.wrappedRowsForWidth(b.clearWidthForResize(minWidth, finalW))
	}
	b.cachedW, b.cachedH = finalW, finalH
	supported := supportsStatusBar(finalW, finalH)

	if supported {
		if clearRows > 0 {
			b.clearBarLocked(clearRows)
		}
		b.visible = true
		b.applyRegionLocked()
		b.paintLocked()
	} else {
		if clearRows > 0 {
			b.clearBarLocked(clearRows)
		}
		b.resetRegionLocked()
		b.visible = false
	}
}

// handleResize re-measures the terminal and applies the resize immediately.
// Tests call this directly; the live watcher uses handleResizeBurst after it
// has collected a resize burst.
func (b *statusBar) handleResize() {
	w, h, err := b.sz.Size()
	if err != nil {
		return
	}
	b.handleResizeBurst(w, w, h)
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
// errors (OnError). Reset to "waiting" so the bar stops claiming an active tool.
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

// ContentWriter routes terminal output through the bar so all writes share the
// same mutex as bar repaints.
func (b *statusBar) ContentWriter() io.Writer { return b }

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

const (
	resizeSettleInterval = 16 * time.Millisecond
	resizeStableSamples  = 2
)

func (b *statusBar) sampleSize() (int, int, error) {
	return b.sz.Size()
}

// watchResizeLoop drains resize bursts, tracks the narrowest width seen during
// the burst, suppresses intermediate repaints, then redraws once at the final
// settled size.
func (b *statusBar) watchResizeLoop(done <-chan struct{}, ch <-chan os.Signal) {
	for {
		select {
		case <-done:
			return
		case <-ch:
		}

		w, h, err := b.sampleSize()
		if err != nil {
			continue
		}
		minWidth, finalW, finalH := w, w, h

		b.mu.Lock()
		if !b.installed || b.torn {
			b.mu.Unlock()
			return
		}
		b.resizing = true
		b.mu.Unlock()

		stable := 0
		for stable < resizeStableSamples {
			select {
			case <-done:
				return
			case <-ch:
				stable = 0
			case <-time.After(resizeSettleInterval):
				stable++
			}

			w, h, err = b.sampleSize()
			if err != nil {
				continue
			}
			if w < minWidth {
				minWidth = w
			}
			if w != finalW || h != finalH {
				finalW, finalH = w, h
				stable = 0
			}
		}

		b.handleResizeBurst(minWidth, finalW, finalH)
	}
}

// watchResizes spawns a goroutine that listens for SIGWINCH until done closes.
func (b *statusBar) watchResizes(done <-chan struct{}) {
	// Buffer 1 is fine here because watchResizeLoop drains bursts by polling the
	// terminal size until it stabilizes, so dropped intermediate signals do not
	// lose the narrowest width that matters for wrap-residue cleanup.
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(ch)
		b.watchResizeLoop(done, ch)
	}()
}
