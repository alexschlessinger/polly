package main

import (
	"image"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/headlessscreen"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
	"github.com/alexschlessinger/pollytool/messages"
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
func underlinedRun(t *testing.T, screen *headlessscreen.Screen, y int) string {
	return frameUnderlinedRun(screenSnapshot(t, screen), y)
}

func frameUnderlinedRun(frame *headlessscreen.Frame, y int) string {
	width, _ := frame.Size()
	var b strings.Builder
	for x := 0; x < width; x++ {
		str, style, _ := frame.Get(x, y)
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
	frame := screenSnapshot(t, screen)
	if got := frameUnderlinedRun(frame, target.Y); got != "▸ 1 tool" {
		t.Fatalf("hovered label underline = %q, want the triangle and label with their inner space", got)
	}
	if record.expanded {
		t.Fatal("hover expanded the disclosure")
	}
	if str, style, _ := frame.Get(target.X-1, target.Y); style.HasUnderline() {
		t.Fatalf("underline spilled onto %q before the label", str)
	}

	// Motion within the same target does not repaint; leaving it clears.
	ev := mouseEvent("<MouseRelease>", image.Pt(target.X+1, target.Y))
	r.handleEvent(ev)
	if r.wantsRenderForEvent(ev) {
		t.Fatal("motion inside one target asked for a repaint")
	}
	hoverAt(t, r, image.Pt(target.X, target.Y+1))
	if got := underlinedRun(t, screen, target.Y); got != "" {
		t.Fatalf("underline stayed after the pointer left: %q", got)
	}

	// A click expands the rows; the pinned transcript shifts, and the
	// pointer now rests on whatever moved under it. Affordance ticks repaint
	// cells between frames and must keep that target's mark.
	hoverAt(t, r, image.Pt(target.X, target.Y))
	at := time.Now()
	r.handleEvent(mouseEvent("<MouseLeft>", image.Pt(target.X, target.Y)))
	r.render()
	if !record.expanded {
		t.Fatal("click did not expand the disclosure")
	}
	// Move past the boundary spacing to the expanded tool row. It mixes a green check, a bright
	// label, and muted metadata; the mark stays one color across all three.
	target = m.disclosurePlacements[activityTools][0]
	hoverAt(t, r, image.Pt(target.X+2, target.Y+1))
	before := underlinedRun(t, screen, r.hover.rect.Min.Y)
	if !strings.HasPrefix(before, "✓ read") || !strings.HasSuffix(before, "1.0s") {
		t.Fatalf("hovered tool row underline = %q, want one line from the check to the duration", before)
	}
	r.tickAffordances(at.Add(500 * time.Millisecond))
	r.tickAffordances(at.Add(2 * time.Second))
	if got := underlinedRun(t, screen, r.hover.rect.Min.Y); got != before {
		t.Fatalf("affordance tick changed the hover underline: %q -> %q", before, got)
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
	openToolDetails(t, r, 140)
	r.render()
	statusText := func() string {
		frame := screenSnapshot(t, screen)
		var b strings.Builder
		for x := 0; x < 140; x++ {
			str, _, _ := frame.Get(x, 39)
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

	// The frame's close button has no words, so it is underlined and named.
	closeRect := r.chrome.close
	if closeRect.Empty() {
		t.Fatal("inspector frame has no close button")
	}
	hoverAt(t, r, closeRect.Min)
	if got := underlinedRun(t, screen, closeRect.Min.Y); got != "×" {
		t.Fatalf("close hover underline = %q", got)
	}
	if got := statusText(); !strings.HasPrefix(got, hoverHintClose) {
		t.Fatalf("close hover status = %q", got)
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

	// In a strip the caption row is shared, so each caption's underline
	// stops at its neighbour's; the last one runs to the pane edge.
	m.mu.Lock()
	m.imagePlacements = append(m.imagePlacements,
		termimg.Placement{Key: "b", Path: "/tmp/b.png", X: 20, Y: 10, Cols: 8, Rows: 2},
		termimg.Placement{Key: "c", Path: "/tmp/c.png", X: 40, Y: 10, Cols: 8, Rows: 3})
	first := r.hoverTargetAt(image.Pt(4, 11))
	middle := r.hoverTargetAt(image.Pt(22, 11))
	last := r.hoverTargetAt(image.Pt(42, 11))
	m.mu.Unlock()
	if first.rect.Max.X != 20 || middle.rect.Min.X != 20 || middle.rect.Max.X != 40 || last.rect.Min.X != 40 || last.rect.Max.X <= 48 {
		t.Fatalf("strip hovers = %v, %v, %v", first.rect, middle.rect, last.rect)
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
	if got := underlinedRun(t, screen, row); !strings.Contains(got, "second-work") || strings.HasPrefix(got, " ") {
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
	if got := underlinedRun(t, screen, row); got != "" {
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
	if got := underlinedRun(t, screen, height-1); got != m.status.contextName {
		t.Fatalf("session hover underline = %q, want %q", got, m.status.contextName)
	}
	if m.modal != nil {
		t.Fatal("hover opened the sessions picker")
	}
}

// An open thought is one target however many rows it wraps to: hovering any
// row underlines all of them.
func TestHoverUnderlinesEveryRowOfAnOpenThought(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	m := r.model
	m.affordances.inputAt = time.Now()
	m.beginTurn("question")
	m.appendThinking("first line of thought\nsecond line of thought\nthird line of thought")
	thought := m.currentReasoningRecord()
	thought.expanded = true
	m.refreshReasoningRecord(thought, 100)
	r.render()
	var rows []int
	for _, link := range m.inspectionLinks {
		if link.kind == thoughtViewKind {
			rows = append(rows, link.rect.Min.Y)
		}
	}
	if len(rows) < 3 {
		t.Fatalf("thought rows = %v, want at least three", rows)
	}
	hoverAt(t, r, image.Pt(activityRailCols+2, rows[1]))
	frame := screenSnapshot(t, screen)
	for i, want := range []string{"first", "second", "third"} {
		if got := frameUnderlinedRun(frame, rows[i]); !strings.Contains(got, want+" line of thought") {
			t.Fatalf("thought row %d underline = %q", i, got)
		}
	}
}
