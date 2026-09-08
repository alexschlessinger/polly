package main

import (
	"context"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

func chromeTestREPL(t *testing.T) (*managedREPL, tcell.SimulationScreen) {
	t.Helper()
	r, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = r.work.close() })
	return r, screen
}

func screenGlyph(screen tcell.SimulationScreen, pt image.Point) string {
	glyph, _, _ := screen.Get(pt.X, pt.Y)
	return glyph
}

func TestChromeFrameGeometryAndEditor(t *testing.T) {
	withDisplayTTY(t)
	r, screen := chromeTestREPL(t)
	m := r.model
	m.appendNoticeLine(strings.Repeat("wrapped words ", 60))
	m.ed.setText("first\n" + strings.Repeat("界", 80))
	for _, size := range []image.Point{{140, 40}, {120, 24}, {80, 24}, {24, 12}, {20, 8}, {80, 6}} {
		screen.SetSize(size.X, size.Y)
		r.render()
		l := r.frameLayoutFor(size.X, size.Y)
		if r.transcriptW.Inner.Dx() != m.visual.width || r.transcriptW.Inner.Dy() != l.transcriptHeight {
			t.Fatalf("%v: rendered content and wrapper disagree: %v width=%d layout=%+v", size, r.transcriptW.Inner, m.visual.width, l)
		}
		x, y, visible := screen.GetCursor()
		if !visible || !image.Pt(x, y).In(r.inputW.Inner) {
			t.Fatalf("%v cursor=(%d,%d,%v), input=%v", size, x, y, visible, r.inputW.Inner)
		}
		if !l.chrome.frame.Empty() || !r.chrome.frame.Empty() || r.inputW.Inner.Min.X != 0 || r.inputW.Inner.Dx() != size.X {
			t.Fatal("a closed inspector changed conversation/composer geometry")
		}
		for _, pt := range []image.Point{{0, 0}, {2, r.inputW.Inner.Min.Y}} {
			_, style, _ := screen.Get(pt.X, pt.Y)
			if style.GetBackground() != ui.ColorClear {
				t.Fatal("chrome colored the main canvas")
			}
		}
	}
	if m.ed.text() != "first\n"+strings.Repeat("界", 80) {
		t.Fatal("render changed editor text")
	}
}

func TestChromeSplitResizeMaximizeAndControls(t *testing.T) {
	withDisplayTTY(t)
	r, screen := chromeTestREPL(t)
	screen.SetSize(140, 40)
	m := r.model
	m.ed.setText("draft stays here")
	m.status.description = strings.Repeat("界 long title ", 30)
	call := messages.ChatMessageToolCall{ID: "one", Name: "read_file"}
	m.appendToolCallStart(call)
	m.inspections.setResult(call, messages.ChatMessage{Content: strings.Repeat("result line\n", 80)})
	r.inspectCommand("tools")
	for _, width := range []int{240, 140, 120, 80, 180} {
		screen.SetSize(width, 40)
		waitInspector(t, r, width)
		r.render()
		g := r.chrome
		if r.inspectorW.Inner.Dx() != r.inspectorGeometry(width).width {
			t.Fatal("inspector wrapper differs from body")
		}
		if width == 240 && (g.main.Dx() != 167 || r.inspectorW.Inner.Dx() != 71) {
			t.Fatalf("default split is not 70 percent conversation and 30 percent inspector: %v %v", g.main, r.inspectorW.Inner)
		}
		if g.frame.Empty() {
			t.Fatalf("no frame at width %d", width)
		}
		if g.frame.Intersect(g.main).Dx() > 0 || r.inputW.Inner.Min.X != 0 || r.inputW.Inner.Dx() != width {
			t.Fatal("inspector chrome reached the conversation or composer")
		}
		if screenGlyph(screen, g.frame.Min) != "╭" || screenGlyph(screen, g.frame.Max.Sub(image.Pt(1, 1))) != "╯" {
			t.Fatalf("frame corners missing at width %d", width)
		}
		checkInspectorHeaderGeometry(t, inspectorHeaderLayout{text: r.inspectorHeaderW.Text, buttons: r.inspectorButtons, rows: r.inspectorHeaderRows}, r.inspectorHeaderW.Inner)
		if width >= splitThreshold {
			for _, ratio := range []float64{.01, .99, .5} {
				r.inspectorRatio = ratio
				r.render()
				if r.chrome.main.Dx() < inspectorMinWidth || r.inspectorW.Inner.Dx() < inspectorMinWidth {
					t.Fatal("resize violated readable minimum")
				}
			}
		} else if !g.main.Empty() {
			t.Fatal("narrow inspector overlaps root")
		}
		parent := headerButton(r.inspectorButtons, "parent")
		if !headerButton(r.inspectorButtons, "maximize").Empty() || !headerButton(r.inspectorButtons, "close").Empty() || parent.Dx() <= 2 || parent.Min != r.inspectorHeaderW.Inner.Min {
			t.Fatal("expected the parent control to span the arrow and title")
		}
		if title := plainStyledText(r.inspectorHeaderW.Text); !strings.HasPrefix(title, "‹ ") || strings.Contains(title, "─") || strings.Contains(title, "[") || r.inspectorHeaderRows != 2 {
			t.Fatalf("expected a title row and a meta row with no rule: %q", title)
		}
		if r.inspectorW.Inner.Min.Y != r.inspectorHeaderW.Inner.Max.Y {
			t.Fatal("body does not immediately follow the title")
		}
	}
	r.inspectCommand("maximize")
	r.render()
	if !r.chrome.main.Empty() || r.inspectorW.Inner.Min.X != 1 || r.chrome.frame.Min.X != 0 {
		t.Fatal("maximize failed")
	}
	r.handleEvent(mouseEvent("<MouseLeft>", headerButton(r.inspectorButtons, "parent").Min))
	r.render()
	if r.workspace().inspector.open || r.transcriptW.Inner.Min.X != 0 || !r.chrome.frame.Empty() {
		t.Fatal("close failed")
	}
	if m.ed.text() != "draft stays here" {
		t.Fatal("inspection changed composer")
	}
}

func TestChromeScrollbarsFollowAndModalMapping(t *testing.T) {
	withDisplayTTY(t)
	r, screen := chromeTestREPL(t)
	screen.SetSize(100, 32)
	m := r.model
	m.appendNoticeLine(strings.Repeat("line\n", 120))
	r.inspect(tabViewTarget(r.visibleTab()))
	waitInspector(t, r, 100)
	r.render()
	state := r.workspace().viewState(r.workspace().inspector.target)
	b := r.inspectorScrollbar
	if b.thumb.Empty() {
		t.Fatal("missing scrollbar")
	}
	// The thumb rides the frame's right edge; content owns every column before it.
	if b.track.Min.X != 99 || r.inspectorW.Inner.Max.X != 99 {
		t.Fatalf("scrollbar is not on the frame edge: track=%v inner=%v", b.track, r.inspectorW.Inner)
	}
	if screenGlyph(screen, b.thumb.Min) != "┃" {
		t.Fatalf("thumb glyph = %q", screenGlyph(screen, b.thumb.Min))
	}
	if screenGlyph(screen, image.Pt(b.track.Min.X, b.track.Min.Y)) != "│" && b.thumb.Min.Y != b.track.Min.Y {
		t.Fatal("edge outside the thumb lost its border")
	}
	r.handleEvent(mouseEvent("<MouseLeft>", b.thumb.Min))
	r.handleEvent(mouseEvent("<MouseLeft>", b.track.Min))
	r.handleEvent(mouseEvent("<MouseRelease>", b.track.Min))
	r.render()
	if state.follow || state.top != 0 {
		t.Fatal("drag did not disengage follow")
	}
	m.appendNoticeLine("new streamed output")
	waitInspector(t, r, 100)
	r.render()
	if state.top != 0 || state.follow {
		t.Fatal("new content stole scroll anchor")
	}
	b = r.inspectorScrollbar
	r.handleEvent(mouseEvent("<MouseLeft>", b.thumb.Min))
	r.handleEvent(mouseEvent("<MouseLeft>", image.Pt(b.track.Min.X, b.track.Max.Y-1)))
	r.handleEvent(mouseEvent("<MouseRelease>", b.track.Min))
	r.render()
	if !state.follow {
		t.Fatal("drag to bottom did not restore follow")
	}
	items := make([]replModalItem, 100)
	for n := range items {
		items[n] = replModalItem{label: fmt.Sprintf("item %03d", n), value: fmt.Sprint(n)}
	}
	chosen := ""
	r.openModal(&replModal{title: "Choose", items: items, selected: 55, onSubmit: func(v string) { chosen = v }})
	r.render()
	modal := m.modal
	if modal.top == 0 || !image.Pt(modal.listBounds.Min.X, modal.listBounds.Min.Y+modal.selected-modal.top).In(modal.listBounds) {
		t.Fatal("selection is not visible")
	}
	if r.modalScrollbar.thumb.Empty() || r.modalScrollbar.thumb.Min.X != r.modalW.Max.X-1 {
		t.Fatalf("dialog scrollbar is not on its border: %v dialog=%v", r.modalScrollbar.thumb, r.modalW.Rectangle)
	}
	want := fmt.Sprint(modal.top + 2)
	r.handleEvent(mouseEvent("<MouseLeft>", modal.listBounds.Min.Add(image.Pt(1, 2))))
	if chosen != want {
		t.Fatalf("clicked filtered row = %q, want %q", chosen, want)
	}
	r.openModal(&replModal{title: "Choose", items: items, selected: 99})
	r.render()
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "0"})
	r.render()
	if m.modal.top != 0 || m.modal.selected != 0 {
		t.Fatal("filter did not reset/clamp viewport")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<End>"})
	r.render()
	if m.modal.selected != len(m.modal.filteredItems())-1 {
		t.Fatal("modal End failed")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "z"})
	r.render()
	if m.modal.top != 0 || m.modal.visible != 0 || !r.modalScrollbar.thumb.Empty() {
		t.Fatal("empty filter retained list geometry")
	}
}

func TestOrbitUsesDisplayCellsOnly(t *testing.T) {
	withDisplayTTY(t)
	r, screen := chromeTestREPL(t)
	m := r.model
	m.beginTurn("go")
	r.render()
	if r.orbit.active {
		t.Fatal("main conversation animated a frame")
	}
	r.inspect(tabViewTarget(r.visibleTab()))
	waitInspector(t, r, 100)
	r.render()
	if !r.orbit.active {
		t.Fatal("busy frame did not animate")
	}
	epoch := r.orbit.epoch
	rows := append([][]ui.Cell(nil), m.visual.rows...)
	placements := append([]inspectionLink(nil), m.inspectionLinks...)
	canonical := strings.Join(transcriptTexts(m), "\n")
	geometry := r.chrome
	edgeBefore := r.orbit.cells[3].last
	r.tickAffordances(epoch.Add(700 * time.Millisecond))
	if r.orbit.cells[3].last == edgeBefore {
		t.Fatal("orbit did not move")
	}
	if !reflect.DeepEqual(rows, m.visual.rows) || !reflect.DeepEqual(placements, m.inspectionLinks) || canonical != strings.Join(transcriptTexts(m), "\n") || geometry != r.chrome {
		t.Fatal("border tick mutated content or hitboxes")
	}
	r.render()
	if !r.orbit.epoch.Equal(epoch) {
		t.Fatal("redraw restarted orbit")
	}
	screen.SetSize(140, 40)
	r.render()
	if !r.orbit.epoch.Equal(epoch) || len(r.orbit.cells) != 2*(r.chrome.frame.Dx()+r.chrome.frame.Dy())-4 {
		t.Fatal("resize lost phase or perimeter")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: focusLostID})
	r.render()
	if r.orbit.active {
		t.Fatal("unfocused motion")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: focusGainedID})
	r.openModal(&replModal{title: "Dialog", items: []replModalItem{{label: "one"}}})
	r.render()
	if r.orbit.active {
		t.Fatal("modal did not pause motion")
	}
	r.closeModal()
	r.endTurn(nil)
	r.render()
	if r.orbit.active {
		t.Fatal("idle frame should be static")
	}
}

func TestOrbitGlintUsesPaletteSlotsAndSkipsThumb(t *testing.T) {
	withDisplayTTY(t)
	r, screen := chromeTestREPL(t)
	screen.SetSize(140, 40)
	m := r.model
	m.appendNoticeLine(strings.Repeat("line\n", 120))
	m.beginTurn("go")
	r.inspect(tabViewTarget(r.visibleTab()))
	waitInspector(t, r, 140)
	r.render()
	if !r.orbit.active || r.inspectorScrollbar.thumb.Empty() {
		t.Fatalf("fixture lacks a live frame with a thumb: active=%v thumb=%v", r.orbit.active, r.inspectorScrollbar.thumb)
	}
	accent, seen := chromeColor("accent"), false
	for tick := 0; tick < 40; tick++ {
		now := r.orbit.epoch.Add(time.Duration(tick) * 50 * time.Millisecond)
		r.orbit.tick(screen, now)
		for n := range r.orbit.cells {
			cell := r.orbit.frame(n, now)
			if cell.Style.Fg.IsRGB() || cell.Style.Bg.IsRGB() {
				t.Fatal("glint used a color outside the terminal palette")
			}
			if cell.Style.Fg == accent {
				seen = true
			}
		}
		if screenGlyph(screen, r.inspectorScrollbar.thumb.Min) != "┃" {
			t.Fatalf("tick %d painted over the scrollbar thumb", tick)
		}
	}
	if !seen {
		t.Fatal("glint never lit an accent cell")
	}
	// The drag grip appears only while the pointer is over the divider.
	grip := image.Pt(r.chrome.divider.Min.X, r.chrome.divider.Min.Y+r.chrome.divider.Dy()/2)
	if screenGlyph(screen, grip) != "│" {
		t.Fatalf("grip shown without hover: %q", screenGlyph(screen, grip))
	}
	r.handleEvent(mouseEvent("<MouseRelease>", grip))
	r.render()
	if screenGlyph(screen, grip) != "⋮" {
		t.Fatalf("hovering the divider did not show the grip: %q", screenGlyph(screen, grip))
	}
}

func TestHistoricalToolDoesNotAnimateWithLiveSource(t *testing.T) {
	withDisplayTTY(t)
	r, screen := chromeTestREPL(t)
	screen.SetSize(140, 30)
	m := r.model
	m.beginTurn("work")
	call := messages.ChatMessageToolCall{ID: "done", Name: "read_file"}
	m.appendToolCallStart(call)
	m.inspections.setResult(call, messages.ChatMessage{Content: "complete"})
	r.inspectCommand("tools")
	waitInspector(t, r, 140)
	r.render()
	if r.orbit.active {
		t.Fatal("historical tool inherited busy source")
	}
}

func TestChromeKeepsTerminalColors(t *testing.T) {
	withDisplayTTY(t)
	r, screen := chromeTestREPL(t)
	screen.SetSize(140, 40)
	m := r.model
	m.ed.setText("draft")
	call := messages.ChatMessageToolCall{ID: "scope", Name: "read_file"}
	m.appendToolCallStart(call)
	m.inspections.setResult(call, messages.ChatMessage{Content: strings.Repeat("result\n", 80)})
	r.inspectCommand("tools")
	waitInspector(t, r, 140)
	r.render()
	check := func(pt image.Point, want ui.Color, what string) {
		t.Helper()
		_, style, _ := screen.Get(pt.X, pt.Y)
		fg, bg := style.GetForeground(), style.GetBackground()
		if fg != want || fg.IsRGB() || bg != ui.ColorClear {
			t.Fatalf("%s at %v: fg=%v bg=%v, want palette %v on the terminal background", what, pt, fg, bg, want)
		}
	}
	check(r.chrome.frame.Min, ui.ColorGrey, "frame")
	check(r.inspectorScrollbar.thumb.Min, ui.ColorGrey, "thumb")
	check(r.inspectorHeaderW.Inner.Min, ui.ColorBlue, "title arrow")
	_, prompt, _ := screen.Get(2, r.inputW.Inner.Min.Y)
	if prompt.GetForeground() != ui.ColorClear || prompt.GetBackground() != ui.ColorClear {
		t.Fatalf("composer text lost terminal colors: %v", prompt)
	}
	r.openModal(&replModal{title: "Scoped dialog", items: []replModalItem{{label: "one"}}})
	r.render()
	check(r.modalW.Min, ui.ColorGrey, "dialog border")
	_, inside, _ := screen.Get(r.modalW.Inner.Min.X, r.modalW.Inner.Min.Y)
	if inside.GetBackground() != ui.ColorClear {
		t.Fatal("dialog painted its own background")
	}
	r.closeModal()
	r.closeInspector()
	r.render()
	if r.orbit.active || len(r.orbit.cells) != 0 || !r.chrome.frame.Empty() {
		t.Fatal("closing the inspector left chrome behind")
	}
}

func TestFullscreenInspectorKeepsComposerCursor(t *testing.T) {
	withDisplayTTY(t)
	r, screen := chromeTestREPL(t)
	screen.SetSize(80, 24)
	r.model.affordances.inputAt = time.Now().Add(-time.Second)
	r.inspect(tabViewTarget(r.visibleTab()))
	waitInspector(t, r, 80)
	r.render()
	found := false
	for _, cell := range r.affordanceW.cells {
		if cell.span.cursor && cell.point.In(r.inputW.Inner) {
			found = true
		}
	}
	if !found {
		t.Fatal("fullscreen inspector hid the composer's idle cursor")
	}
	r.inspectorAction("find")
	r.render()
	for _, cell := range r.affordanceW.cells {
		if cell.span.cursor {
			t.Fatal("inspector search left a composer cursor")
		}
	}
}

func TestChromeNativeMediaAndDisclosureOrigins(t *testing.T) {
	withDisplayTTY(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	r, screen := chromeTestREPL(t)
	screen.SetSize(140, 40)
	tty := &imageTestTTY{window: tcell.WindowSize{Width: 140, Height: 40, PixelWidth: 1400, PixelHeight: 800}}
	r.images = &terminalImageManager{screen: screen, tty: tty, protocol: terminalImageKitty}
	t.Cleanup(func() { r.images.shutdown() })
	m := r.model
	m.nativeImages = true
	m.artifactStore = testArtifactStore(t)
	path := filepath.Join(t.TempDir(), "image.png")
	writeImageFixture(t, path, 24, 12)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := m.artifactStore.Put(context.Background(), artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Name: "image.png", Data: data})
	if err != nil {
		t.Fatal(err)
	}
	m.beginTurn("inspect image")
	call := messages.ChatMessageToolCall{ID: "image", Name: "view_image"}
	tui := &gotuiTurnUI{model: m, config: r.config, repl: r}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "image ready", time.Second, nil)
	m.inspections.setResult(call, messages.ChatMessage{Role: messages.MessageRoleTool, Parts: []messages.ContentPart{{Type: "image_artifact", Artifact: &ref}}})
	r.endTurn(nil)
	m.toggleLatestTurnTrailerOverlay(turnDockOverlayTools)
	r.render()
	clicked := false
	for _, link := range m.inspectionLinks {
		if link.kind == toolViewKind {
			if !link.rect.In(r.chrome.main) {
				t.Fatal("detail click escapes framed content")
			}
			r.handleEvent(mouseEvent("<MouseLeft>", link.rect.Min))
			clicked = true
			break
		}
	}
	if !clicked || !r.workspace().inspector.open {
		t.Fatal("framed tool detail did not open inspector")
	}
	for _, width := range []int{140, 80, 120, 180} {
		screen.SetSize(width, 40)
		waitInspector(t, r, width)
		r.render()
		placements := r.workspace().inspector.current.model.imagePlacements
		if len(placements) != 1 || len(r.images.active) != 1 {
			t.Fatalf("width %d lost native image: %v", width, placements)
		}
		for _, p := range placements {
			if !image.Rect(p.X, p.Y, p.X+p.Cols, p.Y+p.Rows).In(r.inspectorW.Inner) {
				t.Fatalf("image crosses frame: %+v inner=%v", p, r.inspectorW.Inner)
			}
		}
		before := append([]terminalImagePlacement(nil), placements...)
		r.orbit.tick(screen, time.Now().Add(time.Second))
		if !reflect.DeepEqual(before, r.workspace().inspector.current.model.imagePlacements) {
			t.Fatal("edge tick re-placed media")
		}
	}
	r.closeInspector()
	r.render()
	if len(r.images.active) != 0 {
		t.Fatal("closing inspector left native image displayed")
	}
	images := []transcriptImage{{Path: path, Width: 24, Height: 12, Inspection: true}}
	idx := m.appendTranscriptEntry(renderInspectionTranscriptImages(images))
	m.setTranscriptImages(idx, images)
	r.render()
	if len(m.imagePlacements) != 1 {
		t.Fatal("missing image in root conversation")
	}
	p := m.imagePlacements[0]
	if !image.Rect(p.X, p.Y, p.X+p.Cols, p.Y+p.Rows).In(r.chrome.main) {
		t.Fatalf("root image missed content origin: %+v", p)
	}
}

func TestPasteSearchAndApprovalFrame(t *testing.T) {
	withDisplayTTY(t)
	r, screen := chromeTestREPL(t)
	m := r.model
	screen.SetSize(80, 24)
	paste := "界界\nsecond line\nthird"
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: pasteStartID})
	for _, ch := range paste {
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: string(ch)})
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: pasteEndID})
	r.render()
	if !strings.Contains(m.ed.text(), "界界") {
		t.Fatalf("paste lost unicode: %q", m.ed.text())
	}
	glyph, _, _ := screen.Get(r.inputW.Inner.Min.X+2, r.inputW.Inner.Min.Y)
	if glyph != "界" {
		t.Fatalf("wide character was overwritten: %q", glyph)
	}
	m.hist.searching = true
	r.render()
	if _, _, visible := screen.GetCursor(); visible {
		t.Fatal("history search exposed composer cursor")
	}
	m.hist.searching = false
	m.busy = true
	r.inspect(tabViewTarget(r.visibleTab()))
	waitInspector(t, r, 80)
	m.approval = &approvalState{calls: []messages.ChatMessageToolCall{{Name: "read_file"}}}
	r.render()
	if r.orbit.active {
		t.Fatal("approval animated")
	}
	for _, c := range r.orbit.cells {
		if c.base.Style.Fg != chromeColor("active") {
			t.Fatal("approval frame is not the attention color")
		}
	}
	m.approval = nil
	m.busy = false
	m.quiet = true
	r.render()
	if r.orbit.active {
		t.Fatal("quiet motion")
	}
	m.quiet = false
	m.ed.setText("draft")
	r.closeInspector()
	r.render()
	if !r.frameLayoutFor(80, 24).chrome.frame.Empty() || r.inputW.Inner.Min.X != 0 {
		t.Fatal("closed inspector kept insets")
	}
	_, style, _ := screen.Get(2, r.inputW.Inner.Min.Y)
	if fg, bg := style.GetForeground(), style.GetBackground(); fg != ui.ColorClear || bg != ui.ColorClear {
		t.Fatalf("text lost terminal colors: %v", style)
	}
}

func TestScrollbarPagingKeepsUnseenOutputBaseline(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	r.inspect(tabViewTarget(r.visibleTab()))
	waitInspector(t, r, 140)
	s := r.workspace().viewState(r.workspace().inspector.target)
	r.inspectorScrollbar = scrollbar{total: 100, visible: 20}
	s.follow, s.lastRows = false, 50
	r.setScrollTop("inspector", 10)
	if s.top != 10 || s.follow || s.lastRows != 50 {
		t.Fatalf("paging while scrolled up marked unseen output as seen: %+v", s)
	}
	r.setScrollTop("inspector", 90)
	if s.top != 80 || !s.follow || s.lastRows != 100 {
		t.Fatalf("paging to the end did not resume following: %+v", s)
	}
}

func TestModalWheelScrollsOutsideListRows(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	m := &replModal{title: "pick", listBounds: image.Rect(10, 10, 40, 15)}
	for n := 0; n < 10; n++ {
		m.items = append(m.items, replModalItem{label: fmt.Sprintf("item %d", n), value: fmt.Sprint(n)})
	}
	r.model.modal = m
	r.handleModalEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseWheelDown>", Payload: ui.Mouse{X: 0, Y: 0}})
	if m.selected != 3 {
		t.Fatalf("wheel over the dialog frame did not move the selection: %d", m.selected)
	}
	r.handleModalEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: 0, Y: 0}})
	if r.model.modal != m || m.selected != 3 {
		t.Fatal("a click outside the rows selected or activated an item")
	}
}

func TestShortTranscriptUsesPlainInspector(t *testing.T) {
	withDisplayTTY(t)
	r, screen := chromeTestREPL(t)
	screen.SetSize(80, 12)
	call := messages.ChatMessageToolCall{ID: "a", Name: "a"}
	r.model.appendToolCallStart(call)
	r.model.inspections.setResult(call, messages.ChatMessage{Content: strings.Repeat("row\n", 40)})
	r.inspectCommand("tools")
	waitInspector(t, r, 80)
	r.model.turnDock.visible = true
	r.model.ed.setText(strings.Repeat("line\n", maxInputRows-1) + "line")
	l := r.frameLayoutFor(80, 12)
	if !l.chrome.plain || l.transcriptHeight >= 3 {
		t.Fatalf("fixture did not squeeze the transcript under the frame minimum: %+v", l)
	}
	r.render()
	bottom := l.logoRows + l.transcriptHeight
	if !r.chrome.frame.Empty() || !r.inspectorScrollbar.track.Empty() || !r.inspectorScrollbar.thumb.Empty() {
		t.Fatalf("a frame was laid out with no room for it: chrome=%+v scrollbar=%+v", r.chrome, r.inspectorScrollbar)
	}
	if r.chrome.inner.Max.Y > bottom || r.chrome.inner.Min.X != 0 || r.chrome.inner.Dx() != 80 {
		t.Fatalf("plain inspector does not span the transcript region (%d): %v", bottom, r.chrome.inner)
	}
	for y := 0; y < 12; y++ {
		for x := 0; x < 80; x++ {
			if glyph := screenGlyph(screen, image.Pt(x, y)); glyph == "╭" || glyph == "╯" {
				t.Fatalf("frame glyph %q painted at (%d,%d) without a frame", glyph, x, y)
			}
		}
	}
}
