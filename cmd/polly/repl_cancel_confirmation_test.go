package main

import (
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	ui "github.com/metaspartan/gotui/v5"
)

func TestCancellationWarningPreservesDraftAndStreaming(t *testing.T) {
	for _, key := range []string{"<Escape>", "<C-c>"} {
		t.Run(key, func(t *testing.T) {
			r := newManagedREPL(&Config{}, "ctx", 0, 0)
			m := r.model
			m.beginTurn("question")
			m.ed.setText("unfinished\ndraft")
			before := m.fullTranscript()
			canceled := false
			r.visibleTab().turnCancel = func() { canceled = true }
			press := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}) }
			press(key)
			if canceled || m.canceling || m.cancelKey != key || m.fullTranscript() != before || m.ed.text() != "unfinished\ndraft" {
				t.Fatal("warning changed turn, transcript, or draft")
			}
			for _, width := range []int{20, 80} {
				text, _, _, editable := m.renderInputForTerminal(1, width)
				if editable || !strings.Contains(plainStyledText(text), "again") || style.TextWidth(text) > width || m.inputRows() != 1 {
					t.Fatalf("warning at width %d: %q", width, text)
				}
			}
			tui := &gotuiTurnUI{repl: r, model: m, config: r.config}
			tui.AppendAssistantText("still running")
			if !strings.Contains(m.fullTranscript(), "still running") {
				t.Fatal("first press interrupted streaming")
			}
			press("x")
			if m.cancelKey != "" || canceled || m.ed.text() != "unfinished\ndraftx" {
				t.Fatal("typing did not dismiss warning and preserve input")
			}
			press(key)
			if canceled {
				t.Fatal("an intervening key did not reset confirmation")
			}
			press(key)
			if !canceled || !m.canceling || m.cancelKey != "" {
				t.Fatal("second consecutive press did not cancel")
			}
		})
	}
}

func TestCancellationConfirmationRequiresSameKeyAndCurrentTurn(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.beginTurn("question")
	press := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}) }
	press("<Escape>")
	press("<C-c>")
	if r.model.canceling || r.model.cancelKey != "<C-c>" {
		t.Fatal("switching keys confirmed cancellation")
	}
	r.endTurn(nil)
	if r.model.cancelKey != "" {
		t.Fatal("completed turn kept its warning")
	}
	r.model.beginTurn("next")
	press("<C-c>")
	if r.model.canceling {
		t.Fatal("previous turn's warning canceled a new turn")
	}
}

func TestCancellationConfirmationDoesNotFollowTabSwitch(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "first", "second")
	r.showTab(0)
	r.model.busy = true
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-c>"})
	r.showTab(1)
	r.model.busy = true
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-c>"})
	if r.model.canceling {
		t.Fatal("confirmation crossed tabs")
	}
	r.showTab(0)
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-c>"})
	if r.model.canceling {
		t.Fatal("returning to a tab reused old confirmation")
	}
}

func TestEscapeDismissalDoesNotArmCancellation(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.beginTurn("question")
	r.inspect(tabViewTarget(r.visibleTab()))
	r.workspace().inspector.focused = true
	press := func() { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Escape>"}) }
	press() // return focus
	press() // close inspector
	if r.model.cancelKey != "" || r.model.canceling {
		t.Fatal("dismissing inspector armed cancellation")
	}
	press()
	if r.model.canceling || r.model.cancelKey != "<Escape>" {
		t.Fatal("first cancellation key did not warn after dismissal")
	}
	press()
	if !r.model.canceling {
		t.Fatal("second cancellation key did not cancel")
	}
}
