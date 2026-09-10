package main

import (
	"image"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

func TestContextStatusPopoverShowsMessageCountsWithoutChangingConversation(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "ctx")
	testAddMessages(t, session, []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "question"},
		{Role: messages.MessageRoleAssistant, Content: "answer"},
	})
	r.state = &conversationState{session: session, settings: Settings{Model: "openai/test", MaxHistoryTokens: 256_000}}
	m := r.model
	m.status.recordContextUsage(12_300, 256_000, true)
	m.appendLine("existing transcript")
	m.ed.setText("unfinished draft")
	m.busy = true // Inspecting usage must also work during a turn.
	before := m.fullTranscript()
	r.render()
	f := m.status.contextField
	_, height := screen.Size()
	point := image.Pt(f.X, height-1) // Include the fixed-width padding in the target.
	hoverAt(t, r, point)
	if m.modal != nil || strings.TrimSpace(underlinedRun(screen, height-1)) != "~12.3k/256k" {
		t.Fatal("context hover did not expose the readout as a click target")
	}
	click := mouseEvent("<MouseLeft>", point)
	r.handleEvent(click)
	r.render()
	modal := m.modal
	if modal == nil || modal.title != "Messages" {
		t.Fatal("context click did not open its popover")
	}
	if got := strings.Join(strings.Fields(strings.Join(modal.details, "\n")), " "); got != "user 1 assistant 1 tool 0 system 0" {
		t.Fatalf("popover message counts = %q", got)
	}
	if modal.bounds.Max != image.Pt(100, height-1) {
		t.Fatalf("popover is not anchored above the status row: %v", modal.bounds)
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "x"})
	r.handleEvent(mouseEvent("<MouseLeft>", modal.listBounds.Min))
	if m.modal != modal {
		t.Fatal("clicking detail text dismissed the popover")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Escape>"})
	if m.modal != nil {
		t.Fatal("Escape did not close the popover")
	}
	r.handleEvent(click)
	r.render()
	r.handleEvent(mouseEvent("<MouseLeft>", image.Pt(m.status.sessionField.X, height-1)))
	if m.modal != nil {
		t.Fatal("outside click did not close the popover or leaked into the session picker")
	}
	if m.ed.text() != "unfinished draft" || m.fullTranscript() != before || !m.busy || len(testSessionHistory(t, session)) != 2 {
		t.Fatal("popover changed the draft, transcript, active turn, or durable conversation")
	}
	m.statusRow(8)
	if m.status.contextField.Cols != 0 {
		t.Fatal("hidden context readout retained a click target")
	}
}

func TestContextPopoverScrollsOnShortTerminal(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "ctx")
	r.state = &conversationState{session: session, settings: Settings{MaxHistoryTokens: 256_000}}
	r.openContextPopover()
	r.render()
	screen.SetSize(28, 10)
	r.render()
	m := r.model.modal
	if !m.bounds.In(image.Rect(0, 0, 28, 9)) || r.modalScrollbar.thumb.Empty() {
		t.Fatalf("resized popover bounds=%v scrollbar=%v", m.bounds, r.modalScrollbar)
	}
	for _, item := range m.items {
		if rw.StringWidth(item.label) > m.bounds.Dx()-2 {
			t.Fatalf("detail exceeded the popover width: %q", item.label)
		}
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<End>"})
	r.render()
	if m.top == 0 || !strings.Contains(plainStyledText(r.modalW.Text), "system     0") {
		t.Fatalf("cannot scroll to the final detail: %q", r.modalW.Text)
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	if r.model.modal != nil {
		t.Fatal("Enter did not dismiss the popover")
	}
}
