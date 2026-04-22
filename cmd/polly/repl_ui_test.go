package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

func renderManagedScreen(t *testing.T, ui *managedREPL, screen *replScreen, term *termEmulator, full bool) {
	t.Helper()
	var buf bytes.Buffer
	ui.out = &buf
	ui.render(screen, full)
	if err := term.writeString(buf.String()); err != nil {
		t.Fatalf("emulate managed render: %v", err)
	}
}

func TestManagedREPLKeepsInputAndStatusPinnedAcrossResize(t *testing.T) {
	forceStatusBarTERM(t)
	screen := newReplScreen(80, 10, false, "claude-sonnet-4-6", "itest", 1, 0)
	screen.appendUserPrompt("explain the resize issue")
	screen.appendAssistantText(strings.Repeat("wrapped output ", 8))
	for _, r := range "draft input" {
		screen.handleKey(replKey{kind: keyRune, r: r})
	}

	ui := &managedREPL{}
	term := newTermEmulator(80, 10)
	renderManagedScreen(t, ui, screen, term, true)

	if got := term.rowText(9); !strings.HasPrefix(got, "> draft input") {
		t.Fatalf("row 9 = %q, want pinned input row", got)
	}
	if got := term.rowText(10); !strings.Contains(got, "itest") || !strings.Contains(got, "idle") {
		t.Fatalf("row 10 = %q, want status row", got)
	}

	term.resize(34, 10)
	term.setRowText(7, "STALE RESIDUE")
	screen.setSize(34, 10)
	renderManagedScreen(t, ui, screen, term, true)

	if got := term.rowText(9); !strings.HasPrefix(got, "> draft input") {
		t.Fatalf("narrow row 9 = %q, want input row", got)
	}
	if got := term.rowText(10); !strings.Contains(got, "itest") || !strings.Contains(got, "idle") {
		t.Fatalf("narrow row 10 = %q, want status row", got)
	}
	if strings.Contains(term.rowText(7), "STALE RESIDUE") {
		t.Fatalf("row 7 still contains residue after narrow render: %q", term.rowText(7))
	}

	term.resize(80, 10)
	term.setRowText(6, "WIDE RESIDUE")
	screen.setSize(80, 10)
	renderManagedScreen(t, ui, screen, term, true)

	if got := term.rowText(9); !strings.HasPrefix(got, "> draft input") {
		t.Fatalf("wide row 9 = %q, want input row", got)
	}
	if got := term.rowText(10); !strings.Contains(got, "itest") || !strings.Contains(got, "idle") {
		t.Fatalf("wide row 10 = %q, want status row", got)
	}
	if strings.Contains(term.rowText(6), "WIDE RESIDUE") {
		t.Fatalf("row 6 still contains residue after wide render: %q", term.rowText(6))
	}
}

func TestReplScreenScrollbackFollowsBottomOnlyWhenRequested(t *testing.T) {
	screen := newReplScreen(32, 6, false, "model", "ctx", 0, 0)
	for i := 0; i < 12; i++ {
		screen.appendNotice(strings.Repeat("line ", 2) + string(rune('a'+i)))
	}

	bottom := strings.Join(screen.visibleTranscriptRows(), "\n")
	screen.pageUp()
	if screen.followBottom {
		t.Fatalf("pageUp should disable follow-bottom")
	}
	scrolled := strings.Join(screen.visibleTranscriptRows(), "\n")
	if scrolled == bottom {
		t.Fatalf("pageUp should change the visible viewport")
	}

	screen.appendAssistantText("new tail content that should not yank the viewport")
	afterAppend := strings.Join(screen.visibleTranscriptRows(), "\n")
	if afterAppend != scrolled {
		t.Fatalf("new content changed viewport while scrolled back")
	}

	screen.pageDown()
	screen.pageDown()
	if !screen.followBottom {
		t.Fatalf("pageDown to bottom should restore follow-bottom")
	}
	if got := strings.Join(screen.visibleTranscriptRows(), "\n"); !strings.Contains(got, "new tail content") {
		t.Fatalf("bottom viewport missing latest content: %q", got)
	}
}

func TestReplScreenInputEditingAndHorizontalScroll(t *testing.T) {
	screen := newReplScreen(16, 6, false, "model", "ctx", 0, 0)
	for _, r := range "abcdefghijklmnop" {
		screen.handleKey(replKey{kind: keyRune, r: r})
	}
	if screen.inputScroll == 0 {
		t.Fatalf("expected horizontal scroll for long input")
	}

	screen.handleKey(replKey{kind: keyLeft})
	screen.handleKey(replKey{kind: keyLeft})
	screen.handleKey(replKey{kind: keyBackspace})
	if got := screen.inputString(); got != "abcdefghijklmop" {
		t.Fatalf("input after edit = %q", got)
	}

	screen.handleKey(replKey{kind: keyCtrlA})
	screen.handleKey(replKey{kind: keyRune, r: '>'})
	if got := screen.inputString(); got != ">abcdefghijklmop" {
		t.Fatalf("input after ctrl-a insert = %q", got)
	}

	screen.handleKey(replKey{kind: keyCtrlE})
	screen.handleKey(replKey{kind: keyCtrlW})
	if got := screen.inputString(); got != "" {
		t.Fatalf("input after ctrl-w = %q", got)
	}
}

func TestReplScreenApprovalFlow(t *testing.T) {
	screen := newReplScreen(40, 8, false, "model", "ctx", 0, 0)
	reply := make(chan []bool, 1)
	calls := []messages.ChatMessageToolCall{
		{Name: "grep", Arguments: `{"pattern":"foo"}`},
		{Name: "bash", Arguments: `{"command":"echo hi"}`},
	}
	screen.approval = newApprovalState(calls, reply)

	screen.handleKey(replKey{kind: keyRune, r: 'n'})
	if screen.approval == nil || screen.approval.index != 1 {
		t.Fatalf("first denial should advance to second approval")
	}

	screen.handleKey(replKey{kind: keyRune, r: 'a'})
	results := <-reply
	if screen.approval != nil {
		t.Fatalf("approval should finish after approve-all")
	}
	if len(results) != 2 || results[0] || !results[1] {
		t.Fatalf("approval results = %v, want [false true]", results)
	}
}

func TestParseReplKeysRecognizesNavigation(t *testing.T) {
	keys, pending := parseReplKeys(nil, []byte{0x1b, '[', '5', '~', 0x1b, '[', '6', '~', 0x1b, '[', 'A'})
	if len(pending) != 0 {
		t.Fatalf("pending = %v, want empty", pending)
	}
	want := []replKeyKind{keyPageUp, keyPageDown, keyUp}
	if len(keys) != len(want) {
		t.Fatalf("len(keys) = %d, want %d", len(keys), len(want))
	}
	for i, key := range keys {
		if key.kind != want[i] {
			t.Fatalf("key %d = %v, want %v", i, key.kind, want[i])
		}
	}
}
