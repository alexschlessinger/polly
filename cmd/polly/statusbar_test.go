package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

func forceStatusBarTERM(t *testing.T) {
	t.Helper()
	t.Setenv("TERM", "xterm-256color")
}

func TestHumanizeTokens(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{999, "999"},
		{1000, "1.0k"},
		{1200, "1.2k"},
		{12345, "12.3k"},
		{99999, "99.9k"},
		{100000, "100k"},
		{999999, "999k"},
		{1_000_000, "1.0M"},
		{1_234_567, "1.2M"},
	}
	for _, c := range cases {
		if got := humanizeTokens(c.in); got != c.want {
			t.Errorf("humanizeTokens(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestVisibleWidth(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"a·b", 3},                // U+00B7 is width 1
		{"\x1b[31mred\x1b[0m", 3}, // ANSI SGR stripped
		{"\x1b[1;3H", 0},          // Cursor move stripped
		{"hi\x1b[2Kthere", 7},     // Embedded erase
	}
	for _, c := range cases {
		if got := visibleWidth(c.in); got != c.want {
			t.Errorf("visibleWidth(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func stateFull() barState {
	return barState{
		model:       "claude-sonnet-4-6",
		contextName: "myproject",
		turnState:   "streaming",
		turnStarted: time.Now().Add(-12400 * time.Millisecond),
		lastIn:      1234,
		lastOut:     380,
		tools:       4,
		skills:      2,
		utf8:        true,
	}
}

func TestRenderBarWide(t *testing.T) {
	got := renderBar(stateFull(), 120)
	for _, want := range []string{"claude-sonnet-4-6", "myproject", "streaming", "1.2k", "380", "tools:4", "skills:2"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Index(got, "myproject") > strings.Index(got, "streaming") {
		t.Errorf("context must appear before state: %q", got)
	}
}

func TestRenderBarDropsModelFirst(t *testing.T) {
	got := renderBar(stateFull(), 60)
	if strings.Contains(got, "claude-sonnet-4-6") {
		t.Errorf("model should be dropped at W=60: %q", got)
	}
	for _, want := range []string{"myproject", "streaming"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q at W=60: %q", want, got)
		}
	}
}

func TestRenderBarDropsModelAndElapsed(t *testing.T) {
	got := renderBar(stateFull(), 40)
	if strings.Contains(got, "claude-sonnet-4-6") {
		t.Errorf("model should be dropped: %q", got)
	}
	if strings.Contains(got, "12.4s") {
		t.Errorf("elapsed should be dropped: %q", got)
	}
	if !strings.Contains(got, "1.2k") {
		t.Errorf("tokens field should still be present: %q", got)
	}
}

func TestRenderBarDropsModelElapsedTools(t *testing.T) {
	got := renderBar(stateFull(), 30)
	for _, forbidden := range []string{"claude-sonnet-4-6", "12.4s", "tools:4"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("%q should be dropped at W=30: %q", forbidden, got)
		}
	}
	if !strings.Contains(got, "myproject") {
		t.Errorf("context must remain at W=30: %q", got)
	}
}

func TestRenderBarIdleHidesElapsed(t *testing.T) {
	s := stateFull()
	s.turnState = "idle"
	s.turnStarted = time.Time{}
	got := renderBar(s, 120)
	if strings.Contains(got, "s ·") || strings.Contains(got, "0.0s") {
		t.Errorf("idle state should not render elapsed: %q", got)
	}
}

func TestRenderBarLongToolName(t *testing.T) {
	s := stateFull()
	s.turnState = "tool: long_tool_name_foo"
	got := renderBar(s, 120)
	if !strings.Contains(got, "tool: long_tool_name_foo") {
		t.Errorf("tool name should be preserved at wide W: %q", got)
	}
}

func TestRenderBarUTF8Off(t *testing.T) {
	s := stateFull()
	s.utf8 = false
	got := renderBar(s, 120)
	if strings.Contains(got, "·") {
		t.Errorf("should use | separator when utf8 disabled: %q", got)
	}
	if !strings.Contains(got, " | ") {
		t.Errorf("should contain ' | ' separator: %q", got)
	}
}

func TestRenderBarVisibleWidthFits(t *testing.T) {
	for _, w := range []int{30, 40, 60, 80, 120} {
		s := stateFull()
		got := renderBar(s, w)
		if visibleWidth(got) > w {
			t.Errorf("W=%d visible width = %d, line = %q", w, visibleWidth(got), got)
		}
	}
}

func TestPaintWidthReservesLastColumn(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{1, 1},
		{2, 1},
		{20, 19},
		{80, 79},
	}
	for _, c := range cases {
		if got := paintWidth(c.in); got != c.want {
			t.Fatalf("paintWidth(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestSupportsStatusBarRequiresPracticalMinimumWidth(t *testing.T) {
	forceStatusBarTERM(t)
	if supportsStatusBar(minStatusBarCols-1, 24) {
		t.Fatalf("expected width %d to be unsupported", minStatusBarCols-1)
	}
	if !supportsStatusBar(minStatusBarCols, 24) {
		t.Fatalf("expected width %d to be supported", minStatusBarCols)
	}
}

// fakeSizer returns a fixed terminal size.
type fakeSizer struct {
	w, h int
	err  error
}

func (f *fakeSizer) Size() (int, int, error) { return f.w, f.h, f.err }

// mutableSizer lets tests change the reported size between calls.
type mutableSizer struct {
	w, h int
}

func (m *mutableSizer) Size() (int, int, error) { return m.w, m.h, nil }

type termEmulator struct {
	w, h         int
	rows         [][]rune
	row, col     int
	saveRow      int
	saveCol      int
	scrollTop    int
	scrollBottom int
}

func newTermEmulator(w, h int) *termEmulator {
	rows := make([][]rune, h)
	for i := range rows {
		rows[i] = blankRow(w)
	}
	return &termEmulator{
		w:            w,
		h:            h,
		rows:         rows,
		row:          1,
		col:          1,
		saveRow:      1,
		saveCol:      1,
		scrollTop:    1,
		scrollBottom: h,
	}
}

func blankRow(w int) []rune {
	row := make([]rune, w)
	for i := range row {
		row[i] = ' '
	}
	return row
}

func (t *termEmulator) setCursor(row, col int) {
	t.row = clampInt(row, 1, t.h)
	t.col = clampInt(col, 1, t.w)
}

func (t *termEmulator) resize(w, h int) {
	newRows := make([][]rune, h)
	for i := range newRows {
		newRows[i] = blankRow(w)
	}
	copyRows := minInt(h, t.h)
	copyCols := minInt(w, t.w)
	for row := 0; row < copyRows; row++ {
		copy(newRows[row][:copyCols], t.rows[row][:copyCols])
	}
	t.w = w
	t.h = h
	t.rows = newRows
	t.row = clampInt(t.row, 1, h)
	t.col = clampInt(t.col, 1, w)
	t.saveRow = clampInt(t.saveRow, 1, h)
	t.saveCol = clampInt(t.saveCol, 1, w)
	t.scrollTop = clampInt(t.scrollTop, 1, h)
	t.scrollBottom = clampInt(t.scrollBottom, t.scrollTop, h)
}

func (t *termEmulator) rowText(row int) string {
	return strings.TrimRight(string(t.rows[row-1]), " ")
}

func (t *termEmulator) setRowText(row int, text string) {
	t.rows[row-1] = blankRow(t.w)
	col := 0
	for _, r := range text {
		if col >= t.w {
			break
		}
		t.rows[row-1][col] = r
		col++
	}
}

func (t *termEmulator) blankLine(row int) bool {
	return strings.TrimSpace(t.rowText(row)) == ""
}

func (t *termEmulator) writeString(s string) error {
	for i := 0; i < len(s); {
		switch s[i] {
		case '\r':
			t.col = 1
			i++
		case '\n':
			t.lineFeed()
			i++
		case '\x1b':
			if i+1 >= len(s) {
				return fmt.Errorf("dangling escape")
			}
			switch s[i+1] {
			case '[':
				end := i + 2
				for end < len(s) && (s[end] < 0x40 || s[end] > 0x7e) {
					end++
				}
				if end >= len(s) {
					return fmt.Errorf("unterminated CSI")
				}
				if err := t.handleCSI(s[i+2:end], s[end]); err != nil {
					return err
				}
				i = end + 1
			case ']':
				end := i + 2
				for end < len(s) {
					if s[end] == '\a' {
						end++
						break
					}
					if s[end] == '\x1b' && end+1 < len(s) && s[end+1] == '\\' {
						end += 2
						break
					}
					end++
				}
				if end > len(s) {
					return fmt.Errorf("unterminated OSC")
				}
				i = end
			case '7':
				t.saveRow = t.row
				t.saveCol = t.col
				i += 2
			case '8':
				t.row = clampInt(t.saveRow, 1, t.h)
				t.col = clampInt(t.saveCol, 1, t.w)
				i += 2
			default:
				return fmt.Errorf("unsupported escape %q", s[i:i+2])
			}
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				return fmt.Errorf("invalid utf-8 at byte %d", i)
			}
			t.putRune(r)
			i += size
		}
	}
	return nil
}

func (t *termEmulator) handleCSI(params string, final byte) error {
	switch final {
	case 'm':
		return nil
	case 'H', 'f':
		row, col := 1, 1
		if params != "" {
			parts := strings.Split(params, ";")
			if len(parts) > 0 && parts[0] != "" {
				fmt.Sscanf(parts[0], "%d", &row)
			}
			if len(parts) > 1 && parts[1] != "" {
				fmt.Sscanf(parts[1], "%d", &col)
			}
		}
		t.row = clampInt(row, 1, t.h)
		t.col = clampInt(col, 1, t.w)
	case 'B':
		n := 1
		if params != "" {
			fmt.Sscanf(params, "%d", &n)
		}
		t.row = clampInt(t.row+n, 1, t.h)
	case 'K':
		mode := 0
		if params != "" {
			fmt.Sscanf(params, "%d", &mode)
		}
		switch mode {
		case 2:
			t.rows[t.row-1] = blankRow(t.w)
		default:
			for col := t.col - 1; col < t.w; col++ {
				t.rows[t.row-1][col] = ' '
			}
		}
	case 'r':
		top, bottom := 1, t.h
		if params != "" {
			parts := strings.Split(params, ";")
			if len(parts) > 0 && parts[0] != "" {
				fmt.Sscanf(parts[0], "%d", &top)
			}
			if len(parts) > 1 && parts[1] != "" {
				fmt.Sscanf(parts[1], "%d", &bottom)
			}
		}
		t.scrollTop = clampInt(top, 1, t.h)
		t.scrollBottom = clampInt(bottom, t.scrollTop, t.h)
		t.row = 1
		t.col = 1
	case 'h', 'l':
		return nil
	default:
		return fmt.Errorf("unsupported CSI %q%c", params, final)
	}
	return nil
}

func (t *termEmulator) putRune(r rune) {
	if r < 0x20 {
		return
	}
	if t.col > t.w {
		t.col = 1
		t.lineFeed()
	}
	t.rows[t.row-1][t.col-1] = r
	t.col++
}

func (t *termEmulator) lineFeed() {
	if t.row >= t.scrollTop && t.row <= t.scrollBottom {
		if t.row == t.scrollBottom {
			t.scrollUp()
			return
		}
		t.row++
		return
	}
	if t.row < t.h {
		t.row++
	}
}

func (t *termEmulator) scrollUp() {
	for row := t.scrollTop - 1; row < t.scrollBottom-1; row++ {
		copy(t.rows[row], t.rows[row+1])
	}
	t.rows[t.scrollBottom-1] = blankRow(t.w)
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestInstallPaintsFixedBottomBar(t *testing.T) {
	forceStatusBarTERM(t)
	var buf bytes.Buffer
	bar := newStatusBar(&buf, &fakeSizer{w: 80, h: 24})
	bar.SetModel("claude-sonnet-4-6")
	bar.SetContext("myproject")

	term := newTermEmulator(80, 24)
	term.setCursor(18, 7)

	if err := bar.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := term.writeString(buf.String()); err != nil {
		t.Fatalf("emulate install: %v", err)
	}

	if term.scrollTop != 1 || term.scrollBottom != 23 {
		t.Fatalf("scroll region = %d..%d, want 1..23", term.scrollTop, term.scrollBottom)
	}
	if term.row != 18 || term.col != 7 {
		t.Fatalf("cursor moved to %d,%d; want 18,7", term.row, term.col)
	}
	if got := term.rowText(24); !strings.Contains(got, "myproject") || !strings.Contains(got, "idle") {
		t.Fatalf("bottom row = %q, want context + idle", got)
	}
}

func TestInstallRejectsTinyTerminal(t *testing.T) {
	forceStatusBarTERM(t)
	var buf bytes.Buffer
	bar := newStatusBar(&buf, &fakeSizer{w: 80, h: 8})
	if err := bar.Install(); err == nil {
		t.Fatalf("expected Install to fail on H=8")
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no writes on fallback, got %q", buf.String())
	}
}

func TestUninstallIdempotent(t *testing.T) {
	forceStatusBarTERM(t)
	var buf bytes.Buffer
	bar := newStatusBar(&buf, &fakeSizer{w: 80, h: 24})
	if err := bar.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	bar.Uninstall()
	n := buf.Len()
	bar.Uninstall()
	if buf.Len() != n {
		t.Fatalf("second Uninstall wrote extra bytes")
	}
}

func TestPaintOnStateChangeUpdatesBottomRow(t *testing.T) {
	forceStatusBarTERM(t)
	var buf bytes.Buffer
	bar := newStatusBar(&buf, &fakeSizer{w: 80, h: 24})
	term := newTermEmulator(80, 24)
	term.setCursor(12, 3)

	if err := bar.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := term.writeString(buf.String()); err != nil {
		t.Fatalf("emulate install: %v", err)
	}

	buf.Reset()
	bar.setState(func(s *barState) { s.turnState = "thinking" })
	if err := term.writeString(buf.String()); err != nil {
		t.Fatalf("emulate repaint: %v", err)
	}

	if got := term.rowText(24); !strings.Contains(got, "thinking") {
		t.Fatalf("bottom row = %q, want thinking", got)
	}
	if term.row != 12 || term.col != 3 {
		t.Fatalf("cursor moved to %d,%d; want 12,3", term.row, term.col)
	}
}

func TestContentWritesStayInScrollRegion(t *testing.T) {
	forceStatusBarTERM(t)
	var buf bytes.Buffer
	bar := newStatusBar(&buf, &fakeSizer{w: 80, h: 10})
	term := newTermEmulator(80, 10)

	if err := bar.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := term.writeString(buf.String()); err != nil {
		t.Fatalf("emulate install: %v", err)
	}

	buf.Reset()
	for i := 1; i <= 11; i++ {
		if _, err := bar.Write([]byte(fmt.Sprintf("line %02d\r\n", i))); err != nil {
			t.Fatalf("Write line %d: %v", i, err)
		}
	}
	if _, err := bar.Write([]byte("line 12")); err != nil {
		t.Fatalf("Write final line: %v", err)
	}
	if err := term.writeString(buf.String()); err != nil {
		t.Fatalf("emulate content: %v", err)
	}

	if got := term.rowText(10); !strings.Contains(got, "idle") {
		t.Fatalf("bottom row = %q, want idle bar", got)
	}
	if got := term.rowText(9); got != "line 12" {
		t.Fatalf("row 9 = %q, want line 12", got)
	}
	if got := term.rowText(8); got != "line 11" {
		t.Fatalf("row 8 = %q, want line 11", got)
	}
}

func TestRepeatedEmptyEnterAtBottomDoesNotAccumulateGap(t *testing.T) {
	forceStatusBarTERM(t)
	var buf bytes.Buffer
	bar := newStatusBar(&buf, &fakeSizer{w: 80, h: 10})
	term := newTermEmulator(80, 10)
	term.setCursor(9, 1)

	if err := bar.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := term.writeString(buf.String()); err != nil {
		t.Fatalf("emulate install: %v", err)
	}

	buf.Reset()
	for i := 0; i < 4; i++ {
		if _, err := bar.Write([]byte("> ")); err != nil {
			t.Fatalf("Write prompt %d: %v", i, err)
		}
		if i < 3 {
			if _, err := buf.WriteString("\r\n"); err != nil {
				t.Fatalf("append enter %d: %v", i, err)
			}
		}
	}
	if err := term.writeString(buf.String()); err != nil {
		t.Fatalf("emulate prompts: %v", err)
	}

	for _, row := range []int{7, 8, 9} {
		if got := term.rowText(row); !strings.HasPrefix(got, ">") {
			t.Fatalf("row %d = %q, want prompt", row, got)
		}
	}
	if got := term.rowText(10); !strings.Contains(got, "idle") {
		t.Fatalf("bottom row = %q, want idle bar", got)
	}
}

func TestResizeClearsPotentialWrapResidueAndRepaintsBottomRow(t *testing.T) {
	forceStatusBarTERM(t)
	var buf bytes.Buffer
	sz := &mutableSizer{w: 120, h: 10}
	bar := newStatusBar(&buf, sz)
	bar.SetModel("claude-sonnet-4-6")
	bar.SetContext("myproject")
	bar.SetCounts(3, 1)

	term := newTermEmulator(120, 10)
	term.setCursor(9, 1)

	if err := bar.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	bar.Start()
	bar.ClearForContent()
	bar.RecordTurnTokens(12345, 678)
	if err := term.writeString(buf.String()); err != nil {
		t.Fatalf("emulate initial state: %v", err)
	}

	buf.Reset()
	sz.w = 40
	term.resize(40, 10)
	term.setRowText(9, "WRAPPED RESIDUE")
	bar.handleResize()
	if err := term.writeString(buf.String()); err != nil {
		t.Fatalf("emulate shrink resize: %v", err)
	}

	if !term.blankLine(9) {
		t.Fatalf("row 9 = %q, want cleared residue", term.rowText(9))
	}
	if got := term.rowText(10); !strings.Contains(got, "myproject") || !strings.Contains(got, "streaming") {
		t.Fatalf("bottom row = %q, want repainted streaming bar", got)
	}
	if term.scrollTop != 1 || term.scrollBottom != 9 {
		t.Fatalf("scroll region = %d..%d, want 1..9", term.scrollTop, term.scrollBottom)
	}
}

func TestResizeBurstClearsSkippedNarrowResidueOnWideRestore(t *testing.T) {
	forceStatusBarTERM(t)
	var buf bytes.Buffer
	sz := &mutableSizer{w: 120, h: 10}
	bar := newStatusBar(&buf, sz)
	bar.SetModel("claude-sonnet-4-6")
	bar.SetContext("myproject")
	bar.SetCounts(3, 1)

	term := newTermEmulator(120, 10)
	term.setCursor(9, 1)

	if err := bar.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	bar.Start()
	bar.ClearForContent()
	bar.RecordTurnTokens(12345, 678)
	if err := term.writeString(buf.String()); err != nil {
		t.Fatalf("emulate initial state: %v", err)
	}

	buf.Reset()
	// Simulate a fast narrow->wide burst where the terminal briefly reflowed the
	// old bar at a narrow width, but Polly only gets to repaint once at the end.
	term.setRowText(8, "SKIPPED NARROW RESIDUE A")
	term.setRowText(9, "SKIPPED NARROW RESIDUE B")
	bar.handleResizeBurst(minStatusBarCols, 120, 10)
	if err := term.writeString(buf.String()); err != nil {
		t.Fatalf("emulate burst resize: %v", err)
	}

	if !term.blankLine(8) {
		t.Fatalf("row 8 = %q, want cleared residue", term.rowText(8))
	}
	if !term.blankLine(9) {
		t.Fatalf("row 9 = %q, want cleared residue", term.rowText(9))
	}
	if got := term.rowText(10); !strings.Contains(got, "myproject") || !strings.Contains(got, "streaming") {
		t.Fatalf("bottom row = %q, want repainted streaming bar", got)
	}
}

func TestResizeHidesBarWhenTerminalTooSmallAndRestores(t *testing.T) {
	forceStatusBarTERM(t)
	var buf bytes.Buffer
	sz := &mutableSizer{w: 80, h: 10}
	bar := newStatusBar(&buf, sz)
	term := newTermEmulator(80, 10)

	if err := bar.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	bar.ShowToolCall("grep")
	if err := term.writeString(buf.String()); err != nil {
		t.Fatalf("emulate initial state: %v", err)
	}

	buf.Reset()
	sz.w = minStatusBarCols - 1
	term.resize(sz.w, sz.h)
	bar.handleResize()
	if err := term.writeString(buf.String()); err != nil {
		t.Fatalf("emulate hide resize: %v", err)
	}

	if bar.visible {
		t.Fatalf("expected bar to hide on unsupported width")
	}
	if !term.blankLine(10) {
		t.Fatalf("bottom row = %q, want cleared bar row", term.rowText(10))
	}
	if term.scrollTop != 1 || term.scrollBottom != 10 {
		t.Fatalf("scroll region = %d..%d, want reset full screen", term.scrollTop, term.scrollBottom)
	}

	buf.Reset()
	bar.ShowToolCall("sed")
	if buf.Len() != 0 {
		t.Fatalf("hidden bar wrote %q on state change", buf.String())
	}

	sz.w = 80
	term.resize(sz.w, sz.h)
	bar.handleResize()
	if err := term.writeString(buf.String()); err != nil {
		t.Fatalf("emulate restore resize: %v", err)
	}

	if !bar.visible || !bar.barShown {
		t.Fatalf("bar should be visible and painted after restore")
	}
	if got := term.rowText(10); !strings.Contains(got, "tool: sed") {
		t.Fatalf("bottom row = %q, want restored sed state", got)
	}
	if term.scrollTop != 1 || term.scrollBottom != 9 {
		t.Fatalf("scroll region = %d..%d, want 1..9", term.scrollTop, term.scrollBottom)
	}
}

func TestResizeWatcherSettlesBurstAndAppliesFinalGeometry(t *testing.T) {
	forceStatusBarTERM(t)
	var buf bytes.Buffer
	sz := &mutableSizer{w: 80, h: 24}
	bar := newStatusBar(&buf, sz)
	if err := bar.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}

	term := newTermEmulator(80, 24)
	if err := term.writeString(buf.String()); err != nil {
		t.Fatalf("emulate install: %v", err)
	}
	buf.Reset()

	done := make(chan struct{})
	defer close(done)
	ch := make(chan os.Signal, 1)
	go bar.watchResizeLoop(done, ch)

	sz.w, sz.h = 120, 40
	ch <- syscall.SIGWINCH

	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		bar.mu.Lock()
		updated := bar.cachedW == 120 && bar.cachedH == 40 && buf.Len() > 0
		bar.mu.Unlock()
		if updated {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for handleResize after signal")
		}
		time.Sleep(2 * time.Millisecond)
	}

	term.resize(120, 40)
	if err := term.writeString(buf.String()); err != nil {
		t.Fatalf("emulate signal resize: %v", err)
	}
	if term.scrollTop != 1 || term.scrollBottom != 39 {
		t.Fatalf("scroll region = %d..%d, want 1..39", term.scrollTop, term.scrollBottom)
	}
}

func TestStatusHandlerTransitions(t *testing.T) {
	forceStatusBarTERM(t)
	var buf bytes.Buffer
	bar := newStatusBar(&buf, &fakeSizer{w: 80, h: 24})
	if err := bar.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	defer bar.Uninstall()

	bar.Start()
	if s := bar.snapshot(); s.turnState != "waiting" || s.turnStarted.IsZero() {
		t.Errorf("Start should set waiting + turnStarted; got %+v", s)
	}

	bar.ShowThinking(100)
	if s := bar.snapshot(); s.turnState != "thinking" {
		t.Errorf("ShowThinking: got %q", s.turnState)
	}

	bar.ShowToolCall("grep")
	if s := bar.snapshot(); s.turnState != "tool: grep" {
		t.Errorf("ShowToolCall: got %q", s.turnState)
	}

	bar.Clear()
	if s := bar.snapshot(); s.turnState != "waiting" {
		t.Errorf("Clear should reset to waiting; got %q", s.turnState)
	}

	bar.ClearForContent()
	if s := bar.snapshot(); s.turnState != "streaming" {
		t.Errorf("ClearForContent: got %q", s.turnState)
	}

	bar.RecordTurnTokens(1000, 250)
	if s := bar.snapshot(); s.lastIn != 1000 || s.lastOut != 250 {
		t.Errorf("RecordTurnTokens: got %d/%d", s.lastIn, s.lastOut)
	}

	bar.Stop()
	if s := bar.snapshot(); s.turnState != "idle" || !s.turnStarted.IsZero() {
		t.Errorf("Stop should reset to idle; got %+v", s)
	}
}
