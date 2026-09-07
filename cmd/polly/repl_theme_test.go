package main

import (
	"context"
	"fmt"
	"image"
	"io"
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
	"github.com/urfave/cli/v3"
)

func TestThemeFlag(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		args      []string
		want      string
		invalid   bool
	}{
		{"default", "", nil, "default", false},
		{"halo flag", "", []string{"--theme=halo"}, "halo", false},
		{"env", "halo", nil, "halo", false},
		{"override", "halo", []string{"--theme=default"}, "default", false},
		{"invalid flag", "", []string{"--theme=neon"}, "", true},
		{"invalid env", "neon", nil, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("POLLYTOOL_THEME", tc.env)
			if tc.env == "" {
				_ = os.Unsetenv("POLLYTOOL_THEME")
			}
			var config *Config
			cmd := &cli.Command{Writer: io.Discard, ErrWriter: io.Discard, Flags: outputConfigFlags(), Action: func(_ context.Context, c *cli.Command) error { config = parseConfig(c); return nil }}
			err := cmd.Run(context.Background(), append([]string{"polly"}, tc.args...))
			if tc.invalid {
				if err == nil || config != nil {
					t.Fatalf("invalid theme reached action: %v", err)
				}
				return
			}
			if err != nil || config.Theme != tc.want {
				t.Fatalf("config=%+v err=%v", config, err)
			}
			config.PromptSet = true
			if mode, err := selectConversationMode(config, false); err != nil || mode != conversationModeOneShot {
				t.Fatalf("theme affected one-shot selection: %v %v", mode, err)
			}
		})
	}
}

func haloTestREPL(t *testing.T) (*managedREPL, tcell.SimulationScreen) {
	t.Helper()
	r, screen := affordanceTestREPL(t)
	r.config.Theme = "halo"
	t.Cleanup(func() { _ = r.work.close() })
	return r, screen
}

func TestHaloFrameGeometryAndEditor(t *testing.T) {
	withDisplayTTY(t)
	r, screen := haloTestREPL(t)
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
		if l.halo || !r.haloBounds.outer.Empty() || r.inputW.Inner.Min.X != 0 || r.inputW.Inner.Dx() != size.X {
			t.Fatal("Halo changed unframed conversation/composer geometry")
		}
		for _, pt := range []image.Point{{0, 0}, {2, r.inputW.Inner.Min.Y}} {
			_, style, _ := screen.Get(pt.X, pt.Y)
			if style.GetBackground() != ui.ColorClear {
				t.Fatal("Halo colored the main canvas")
			}
		}
	}
	if m.ed.text() != "first\n"+strings.Repeat("界", 80) {
		t.Fatal("render changed editor text")
	}
}

func TestHaloSplitResizeMaximizeAndControls(t *testing.T) {
	withDisplayTTY(t)
	r, screen := haloTestREPL(t)
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
		if r.inspectorW.Inner.Dx() != r.inspectorGeometry(width).width {
			t.Fatal("inspector wrapper differs from body")
		}
		if width == 240 && (r.mainTranscriptBounds.Dx() != 167 || r.inspectorW.Inner.Dx() != 70) {
			t.Fatal("default split is not 70 percent conversation and 30 percent inspector")
		}
		if r.haloBounds.outer.Intersect(r.mainTranscriptBounds).Dx() > 0 || r.inputW.Inner.Min.X != 0 || r.inputW.Inner.Dx() != width {
			t.Fatal("inspector chrome reached the conversation or composer")
		}
		checkInspectorHeaderGeometry(t, inspectorHeaderLayout{text: r.inspectorHeaderW.Text, buttons: r.inspectorButtons, rows: r.inspectorHeaderRows}, r.inspectorHeaderW.Inner)
		if width >= 120 {
			for _, ratio := range []float64{.01, .99, .5} {
				r.inspectorRatio = ratio
				r.render()
				if r.mainTranscriptBounds.Dx() < 50 || r.inspectorW.Inner.Dx() < 50 {
					t.Fatal("resize violated readable minimum")
				}
			}
		} else if !r.mainTranscriptBounds.Empty() {
			t.Fatal("narrow inspector overlaps root")
		}
		parent := headerButton(r.inspectorButtons, "parent")
		if !headerButton(r.inspectorButtons, "maximize").Empty() || !headerButton(r.inspectorButtons, "close").Empty() || parent.Dx() != 1 || parent.Min != r.inspectorHeaderW.Inner.Min {
			t.Fatal("expected a single-cell parent arrow at the title's left edge")
		}
		if title := plainStyledText(r.inspectorHeaderW.Text); !strings.HasPrefix(title, "< ") || strings.Contains(title, "─") || r.inspectorHeaderRows != 1 {
			t.Fatalf("expected a single title row with a leading arrow: %q", title)
		}
		if r.inspectorW.Inner.Min.Y != r.inspectorHeaderW.Inner.Max.Y {
			t.Fatal("body does not immediately follow the title")
		}
	}
	r.inspectCommand("maximize")
	r.render()
	if !r.mainTranscriptBounds.Empty() || r.inspectorW.Inner.Min.X != 1 {
		t.Fatal("maximize failed")
	}
	r.handleEvent(mouseEvent("<MouseLeft>", headerButton(r.inspectorButtons, "parent").Min))
	r.render()
	if r.workspace().inspector.open || r.transcriptW.Inner.Min.X != 0 {
		t.Fatal("close failed")
	}
	if m.ed.text() != "draft stays here" {
		t.Fatal("inspection changed composer")
	}
}

func TestHaloScrollbarsFollowAndModalMapping(t *testing.T) {
	withDisplayTTY(t)
	r, screen := haloTestREPL(t)
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

func TestHaloOrbitUsesDisplayCellsOnly(t *testing.T) {
	withDisplayTTY(t)
	r, screen := haloTestREPL(t)
	m := r.model
	m.beginTurn("go")
	r.render()
	if r.haloOrbit.active {
		t.Fatal("main conversation animated a Halo frame")
	}
	r.inspect(tabViewTarget(r.visibleTab()))
	waitInspector(t, r, 100)
	r.render()
	if !r.haloOrbit.active {
		t.Fatal("busy frame did not animate")
	}
	epoch := r.haloOrbit.epoch
	rows := append([][]ui.Cell(nil), m.visual.rows...)
	placements := append([]inspectionLink(nil), m.inspectionLinks...)
	canonical := strings.Join(transcriptTexts(m), "\n")
	geometry := r.mainTranscriptBounds
	edgeBefore := r.haloOrbit.cells[3].last
	r.tickAffordances(epoch.Add(700 * time.Millisecond))
	if r.haloOrbit.cells[3].last == edgeBefore {
		t.Fatal("orbit did not move")
	}
	if !reflect.DeepEqual(rows, m.visual.rows) || !reflect.DeepEqual(placements, m.inspectionLinks) || canonical != strings.Join(transcriptTexts(m), "\n") || geometry != r.mainTranscriptBounds {
		t.Fatal("border tick mutated content or hitboxes")
	}
	r.render()
	if !r.haloOrbit.epoch.Equal(epoch) {
		t.Fatal("redraw restarted orbit")
	}
	screen.SetSize(140, 40)
	r.render()
	if !r.haloOrbit.epoch.Equal(epoch) || len(r.haloOrbit.cells) != 2*(r.haloBounds.outer.Dx()+r.haloBounds.outer.Dy())-4 {
		t.Fatal("resize lost phase or perimeter")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: focusLostID})
	r.render()
	if r.haloOrbit.active {
		t.Fatal("unfocused motion")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: focusGainedID})
	r.openModal(&replModal{title: "Dialog", items: []replModalItem{{label: "one"}}})
	r.render()
	if r.haloOrbit.active {
		t.Fatal("modal did not pause motion")
	}
	r.closeModal()
	r.endTurn(nil)
	r.render()
	if r.haloOrbit.active {
		t.Fatal("idle Halo should be static")
	}
}

func TestHaloHistoricalToolDoesNotAnimateWithLiveSource(t *testing.T) {
	withDisplayTTY(t)
	r, screen := haloTestREPL(t)
	screen.SetSize(140, 30)
	m := r.model
	m.beginTurn("work")
	call := messages.ChatMessageToolCall{ID: "done", Name: "read_file"}
	m.appendToolCallStart(call)
	m.inspections.setResult(call, messages.ChatMessage{Content: "complete"})
	r.inspectCommand("tools")
	waitInspector(t, r, 140)
	r.render()
	if r.haloOrbit.active {
		t.Fatal("historical tool inherited busy source")
	}
}

func TestHaloPaletteKeepsCanonicalRolesAndLimitedColor(t *testing.T) {
	cell := ui.Cell{Rune: '>', Style: ui.NewStyle(ui.ColorBlue, ui.ColorClear, ui.ModifierBold)}
	parser := ui.StyleParserColorMap["accent"]
	for _, colors := range []int{1 << 24, 256, 16, 0} {
		theme := themeLayer{halo: true, colors: colors, panes: []image.Rectangle{image.Rect(0, 0, 10, 10)}}
		if theme.convert(cell, image.Pt(11, 1)) != cell {
			t.Fatal("palette leaked outside Halo surface")
		}
		next := theme.convert(cell, image.Pt(1, 1))
		if next.Rune != cell.Rune || next.Style.Modifier&ui.ModifierBold == 0 {
			t.Fatal("theme changed glyph/emphasis")
		}
		if colors == 1<<24 && (next.Style.Fg != haloPalette.accent || next.Style.Bg != cell.Style.Bg) {
			t.Fatal("Halo palette mismatch")
		}
		if colors == 16 || colors == 256 {
			if next.Style.Fg.IsRGB() || next.Style.Bg.IsRGB() {
				t.Fatal("limited color did not quantize")
			}
		}
		if colors == 0 && (next.Style.Fg != ui.ColorClear || next.Style.Bg != ui.ColorClear) {
			t.Fatal("monochrome contains colors")
		}
	}
	if cell.Style.Fg != ui.ColorBlue || ui.StyleParserColorMap["accent"] != parser {
		t.Fatal("theme changed canonical roles")
	}
	theme := themeLayer{}
	if theme.convert(cell, image.Pt(1, 1)) != cell {
		t.Fatal("default theme changed")
	}
}

func TestHaloScopesPaletteToInspectorAndDialogs(t *testing.T) {
	withDisplayTTY(t)
	r, screen := haloTestREPL(t)
	screen.SetSize(140, 40)
	m := r.model
	m.ed.setText("draft")
	r.render()
	_, promptStyle, _ := screen.Get(0, r.inputW.Inner.Min.Y)
	call := messages.ChatMessageToolCall{ID: "scope", Name: "read_file"}
	m.appendToolCallStart(call)
	m.inspections.setResult(call, messages.ChatMessage{Content: "result"})
	r.inspectCommand("tools")
	waitInspector(t, r, 140)
	r.render()
	_, after, _ := screen.Get(0, r.inputW.Inner.Min.Y)
	if after != promptStyle {
		t.Fatal("inspector recolored composer")
	}
	_, mainStyle, _ := screen.Get(0, r.mainTranscriptBounds.Min.Y)
	if mainStyle.GetBackground() != ui.ColorClear {
		t.Fatal("inspector recolored main canvas")
	}
	for _, pt := range []image.Point{r.inspectorW.Inner.Min, r.inspectorHeaderW.Inner.Min, r.haloBounds.outer.Min, r.haloBounds.inspector.track.Min} {
		_, inspectorStyle, _ := screen.Get(pt.X, pt.Y)
		if inspectorStyle.GetBackground() != mainStyle.GetBackground() {
			t.Fatalf("inspector background differs from main at %v", pt)
		}
	}
	r.closeInspector()
	r.openModal(&replModal{title: "Scoped dialog", items: []replModalItem{{label: "one"}}})
	r.render()
	_, dialogStyle, _ := screen.Get(r.modalW.Inner.Min.X, r.modalW.Inner.Min.Y)
	if dialogStyle.GetBackground() != r.themeW.color(haloPalette.pane) {
		t.Fatal("standalone dialog lost Halo background")
	}
	_, outsideStyle, _ := screen.Get(0, 0)
	if outsideStyle.GetBackground() != ui.ColorClear {
		t.Fatal("dialog recolored main canvas")
	}
	r.closeModal()
	r.render()
	_, after, _ = screen.Get(0, r.inputW.Inner.Min.Y)
	if after != promptStyle || r.haloOrbit.active || len(r.haloOrbit.cells) != 0 {
		t.Fatal("closing Halo surfaces left chrome or colors behind")
	}
}

func TestHaloFullscreenInspectorKeepsComposerCursor(t *testing.T) {
	withDisplayTTY(t)
	r, screen := haloTestREPL(t)
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

func TestHaloNativeMediaAndDisclosureOrigins(t *testing.T) {
	withDisplayTTY(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	r, screen := haloTestREPL(t)
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
			if !link.rect.In(r.mainTranscriptBounds) {
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
				t.Fatalf("image crosses frame/rail: %+v inner=%v", p, r.inspectorW.Inner)
			}
		}
		before := append([]terminalImagePlacement(nil), placements...)
		r.haloOrbit.tick(screen, r.themeW, time.Now().Add(time.Second))
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
	if !image.Rect(p.X, p.Y, p.X+p.Cols, p.Y+p.Rows).In(r.mainTranscriptBounds) {
		t.Fatalf("root image missed content origin: %+v", p)
	}
}

func TestHaloPasteSearchApprovalAndDefaultPaint(t *testing.T) {
	withDisplayTTY(t)
	r, screen := haloTestREPL(t)
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
	if r.haloOrbit.active {
		t.Fatal("approval animated")
	}
	for _, c := range r.haloOrbit.cells {
		if c.base.Style.Fg != haloPalette.attention {
			t.Fatal("approval frame is not amber")
		}
	}
	m.approval = nil
	m.busy = false
	m.quiet = true
	r.render()
	if r.haloOrbit.active {
		t.Fatal("quiet motion")
	}
	m.quiet = false
	m.ed.setText("draft")
	r.closeInspector()
	r.config.Theme = "default"
	r.render()
	defaultLayout := r.frameLayoutFor(80, 24)
	if defaultLayout.halo || r.inputW.Inner.Min.X != 0 {
		t.Fatal("default theme gained insets")
	}
	_, style, _ := screen.Get(2, r.inputW.Inner.Min.Y)
	fg, bg := style.GetForeground(), style.GetBackground()
	if fg != ui.ColorClear || bg != ui.ColorClear {
		t.Fatalf("default text lost terminal colors: %v", style)
	}
}
