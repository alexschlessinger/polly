package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

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
		{"a·b", 3},                 // U+00B7 is width 1
		{"\x1b[31mred\x1b[0m", 3},  // ANSI SGR stripped
		{"\x1b[1;3H", 0},           // Cursor move stripped
		{"hi\x1b[2Kthere", 7},      // Embedded erase
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

// fakeSizer returns a fixed terminal size.
type fakeSizer struct {
	w, h int
	err  error
}

func (f *fakeSizer) Size() (int, int, error) { return f.w, f.h, f.err }

func TestInstallEmitsScrollRegion(t *testing.T) {
	var buf bytes.Buffer
	bar := newStatusBar(&buf, &fakeSizer{w: 80, h: 24})
	bar.SetModel("claude-sonnet-4-6")
	bar.SetContext("myproject")

	if err := bar.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	got := buf.String()

	if !strings.Contains(got, "\x1b[22;0t") {
		t.Errorf("expected title push (CSI 22;0t), got %q", got)
	}
	if !strings.Contains(got, "\x1b[1;23r") {
		t.Errorf("expected DECSTBM to 1;23, got %q", got)
	}
	if !strings.Contains(got, "\x1b[1;1H") {
		t.Errorf("expected cursor move to 1;1, got %q", got)
	}
	if !strings.Contains(got, "\x1b[24;1H") {
		t.Errorf("expected paint at row 24, got %q", got)
	}
	if !strings.Contains(got, "myproject") {
		t.Errorf("expected context name in status, got %q", got)
	}

	bar.Uninstall()
	after := buf.String()[len(got):]

	if !strings.Contains(after, "\x1b[r") {
		t.Errorf("expected DECSTBM reset, got %q", after)
	}
	if !strings.Contains(after, "\x1b[24;1H") {
		t.Errorf("expected cursor move to row 24 during teardown, got %q", after)
	}
	if !strings.Contains(after, "\x1b[2K") {
		t.Errorf("expected line clear during teardown, got %q", after)
	}
}

func TestInstallRejectsTinyTerminal(t *testing.T) {
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

func TestPaintNowAfterStateChange(t *testing.T) {
	var buf bytes.Buffer
	bar := newStatusBar(&buf, &fakeSizer{w: 80, h: 24})
	if err := bar.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	installBytes := buf.Len()

	bar.setState(func(s *barState) { s.turnState = "thinking" })

	after := buf.String()[installBytes:]
	if !strings.Contains(after, "thinking") {
		t.Errorf("expected state change to trigger paint, got %q", after)
	}
	bar.Uninstall()
}

// mutableSizer lets tests change the reported size between calls.
type mutableSizer struct {
	w, h int
}

func (m *mutableSizer) Size() (int, int, error) { return m.w, m.h, nil }

func TestResizeReappliesScrollRegion(t *testing.T) {
	var buf bytes.Buffer
	sz := &mutableSizer{w: 80, h: 24}
	bar := newStatusBar(&buf, sz)
	if err := bar.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	n := buf.Len()

	sz.w, sz.h = 100, 40
	bar.handleResize()

	after := buf.String()[n:]
	if !strings.Contains(after, "\x1b[1;39r") {
		t.Errorf("expected DECSTBM to 1;39 after resize, got %q", after)
	}
	if !strings.Contains(after, "\x1b[40;1H") {
		t.Errorf("expected paint at row 40 after resize, got %q", after)
	}
	bar.Uninstall()
}

func TestResizeUninstallsWhenTooSmall(t *testing.T) {
	var buf bytes.Buffer
	sz := &mutableSizer{w: 80, h: 24}
	bar := newStatusBar(&buf, sz)
	if err := bar.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	sz.h = 2
	bar.handleResize()
	if !bar.torn {
		t.Errorf("expected bar to tear down when H=2")
	}
}

func TestStatusHandlerTransitions(t *testing.T) {
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
