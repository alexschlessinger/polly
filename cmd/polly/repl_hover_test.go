package main

import (
	"image"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
	"github.com/alexschlessinger/pollytool/messages"
	tcell "github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

// hoverAt moves the pointer with a button-free motion event and repaints
// when the REPL says a hover changed, as the event loop does.
func hoverAt(t *testing.T, r *managedREPL, p image.Point) {
	t.Helper()
	ev := mouseEvent("<MouseRelease>", p)
	r.handleEvent(ev)
	if r.wantsRenderForEvent(ev) {
		r.render()
	}
}

// underlinedRun collects the underlined cells on a row. Every underlined
// cell must carry the hover color, whatever its text's own style, so the mark
// reads as one line across a check, a label, and muted metadata.
func underlinedRun(screen tcell.SimulationScreen, y int) string {
	width, _ := screen.Size()
	var b strings.Builder
	for x := 0; x < width; x++ {
		str, style, _ := screen.Get(x, y)
		if style.HasUnderline() {
			if style.GetUnderlineColor() != hoverUnderlineColor() {
				return "underline color follows the text at " + str
			}
			b.WriteString(str)
		}
	}
	return b.String()
}

func TestHoverUnderlinesTheTargetUnderThePointer(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	m := r.model
	m.affordances.inputAt = time.Now()
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	call := messages.ChatMessageToolCall{ID: "read", Name: "read_file"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "done", time.Second, nil)
	tui.AppendAssistantText("Found the relevant code.")
	r.endTurn(nil)
	r.render()
	if len(m.disclosurePlacements[activityTools]) != 1 {
		t.Fatalf("tool hitboxes = %#v, want one", m.disclosurePlacements[activityTools])
	}
	target := m.disclosurePlacements[activityTools][0]
	record := m.currentToolDisclosure()

	hoverAt(t, r, image.Pt(target.X+target.Cols-1, target.Y))
	if got := underlinedRun(screen, target.Y); got != "▸ 1 tool" {
		t.Fatalf("hovered label underline = %q, want the triangle and label with their inner space", got)
	}
	if record.expanded {
		t.Fatal("hover expanded the disclosure")
	}
	if str, style, _ := screen.Get(target.X-1, target.Y); style.HasUnderline() {
		t.Fatalf("underline spilled onto %q before the label", str)
	}

	// Motion within the same target does not repaint; leaving it clears.
	ev := mouseEvent("<MouseRelease>", image.Pt(target.X+1, target.Y))
	r.handleEvent(ev)
	if r.wantsRenderForEvent(ev) {
		t.Fatal("motion inside one target asked for a repaint")
	}
	hoverAt(t, r, image.Pt(target.X, target.Y+1))
	if got := underlinedRun(screen, target.Y); got != "" {
		t.Fatalf("underline stayed after the pointer left: %q", got)
	}

	// A click expands the rows; the pinned transcript shifts, and the
	// pointer now rests on whatever moved under it. The glint repaints
	// cells between frames and must keep that target's mark.
	hoverAt(t, r, image.Pt(target.X, target.Y))
	r.handleEvent(mouseEvent("<MouseLeft>", image.Pt(target.X, target.Y)))
	r.render()
	at := m.affordances.disclosures[affordanceTarget{activityTools, target.recordID}]
	if at.IsZero() || !record.expanded {
		t.Fatal("click did not expand and arm the glint")
	}
	// The expanded tool row under the pointer mixes a green check, a bright
	// label, and muted metadata; the mark stays one color across all three.
	before := underlinedRun(screen, r.hover.rect.Min.Y)
	if !strings.HasPrefix(before, "✓ read") || !strings.HasSuffix(before, "1.0s") {
		t.Fatalf("hovered tool row underline = %q, want one line from the check to the duration", before)
	}
	r.tickAffordances(at.Add(500 * time.Millisecond))
	r.tickAffordances(at.Add(2 * time.Second))
	if got := underlinedRun(screen, r.hover.rect.Min.Y); got != before {
		t.Fatalf("glint tick changed the hover underline: %q -> %q", before, got)
	}
}

func TestHoverNamesWordlessTargetsInTheStatusRow(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	screen.SetSize(140, 40)
	m := r.model
	m.affordances.inputAt = time.Now()
	call := messages.ChatMessageToolCall{ID: "scope", Name: "read_file"}
	m.appendToolCallStart(call)
	m.inspections.setResult(call, messages.ChatMessage{Content: strings.Repeat("result\n", 80)})
	r.inspectCommand("tools")
	waitInspector(t, r, 140)
	r.render()
	statusText := func() string {
		var b strings.Builder
		for x := 0; x < 140; x++ {
			str, _, _ := screen.Get(x, 39)
			b.WriteString(str)
		}
		return strings.TrimSpace(b.String())
	}
	if !strings.HasPrefix(statusText(), "read_file") && strings.Contains(statusText(), "Drag") {
		t.Fatalf("idle status already carried a hint: %q", statusText())
	}
	hoverAt(t, r, image.Pt(r.chrome.divider.Min.X, r.chrome.divider.Min.Y+3))
	if got := statusText(); !strings.HasPrefix(got, hoverHintResize) {
		t.Fatalf("divider hover status = %q", got)
	}
	if len(r.hoverCells) != 0 {
		t.Fatalf("divider hover underlined cells: %v", r.hoverCells)
	}
	hoverAt(t, r, r.inspectorScrollbar.thumb.Min)
	if got := statusText(); !strings.HasPrefix(got, hoverHintScroll) {
		t.Fatalf("thumb hover status = %q", got)
	}
	hoverAt(t, r, image.Pt(2, 2))
	if got := statusText(); strings.HasPrefix(got, "Drag") {
		t.Fatalf("hint stayed after the pointer left the chrome: %q", got)
	}

	// The header's back control has words, so it is underlined, not named.
	if len(r.inspectorButtons) == 0 {
		t.Fatal("inspector header has no buttons")
	}
	parent := r.inspectorButtons[0]
	hoverAt(t, r, parent.rect.Min)
	if got := underlinedRun(screen, parent.rect.Min.Y); !strings.HasPrefix(got, "‹ read_file") {
		t.Fatalf("header hover underline = %q", got)
	}
	if got := statusText(); strings.HasPrefix(got, "Drag") || strings.HasPrefix(got, "Open") {
		t.Fatalf("worded target carried a hint: %q", got)
	}

	// A thumbnail names its action and underlines its caption row.
	m.mu.Lock()
	m.imagePlacements = []termimg.Placement{{Key: "shot", Path: "/tmp/shot.png", X: 2, Y: 10, Cols: 8, Rows: 3}}
	m.mu.Unlock()
	m.mu.Lock()
	got := r.hoverTargetAt(image.Pt(4, 11))
	m.mu.Unlock()
	if got.hint != hoverHintImage || got.rect.Min.Y != 9 || got.rect.Max.Y != 10 || got.rect.Min.X != 2 {
		t.Fatalf("thumbnail hover = %+v", got)
	}
}

func TestHoverHighlightsPickerRowsWithoutSelecting(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	screen.SetSize(100, 30)
	r.model.affordances.inputAt = time.Now()
	r.openModal(&replModal{title: "Sessions", items: []replModalItem{{label: "first-work"}, {label: "second-work"}, {label: "third-work"}}})
	r.render()
	modal := r.model.modal
	row := modal.listBounds.Min.Y + 1
	hoverAt(t, r, image.Pt(modal.listBounds.Min.X+3, row))
	if got := underlinedRun(screen, row); !strings.Contains(got, "second-work") || strings.HasPrefix(got, " ") {
		t.Fatalf("picker row hover underline = %q, want the row text without its padding", got)
	}
	if modal.selected != 0 {
		t.Fatalf("hover moved the keyboard selection to %d", modal.selected)
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Down>"})
	r.render()
	if modal.selected != 1 {
		t.Fatalf("arrow selection = %d after hover", modal.selected)
	}
	hoverAt(t, r, image.Pt(modal.listBounds.Min.X+3, modal.listBounds.Max.Y+2))
	if got := underlinedRun(screen, row); got != "" {
		t.Fatalf("underline stayed on the picker row: %q", got)
	}
}

func TestHoverUnderlinesTheSessionNameInTheStatusRow(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	m := r.model
	m.affordances.inputAt = time.Now()
	m.appendLine("hello")
	r.render()
	f := m.status.sessionField
	if f.Cols == 0 {
		t.Fatal("status row has no session field")
	}
	_, height := screen.Size()
	hoverAt(t, r, image.Pt(f.X+1, height-1))
	if got := underlinedRun(screen, height-1); got != m.status.contextName {
		t.Fatalf("session hover underline = %q, want %q", got, m.status.contextName)
	}
	if m.modal != nil {
		t.Fatal("hover opened the sessions picker")
	}
}
