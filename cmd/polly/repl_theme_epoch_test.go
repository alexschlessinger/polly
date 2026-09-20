package main

import (
	"bytes"
	"image"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

// The parser map and the style epoch are process-wide and shared with every
// other test in this binary, so a test that applies a theme must put the
// built-in mapping back.
func restoreStyleEpochTestTheme(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { style.Apply(style.DefaultTheme()) })
}

// epochTestThemeColors are unique RGB values, so a screen cell's color says
// which theme painted it. The roles below are the ones the epoch tests read:
// accent (accent text and agent links), muted (the frame, the code gutter, and
// the scrollbar thumb), code (fenced code bodies), and polly-green (the bird).
var epochTestThemeColors = map[string]string{
	"accent":      "#ff00aa",
	"muted":       "#00ff88",
	"code":        "#123456",
	"polly-green": "#654321",
}

func epochThemeColor(t *testing.T, role string) ui.Color {
	t.Helper()
	color, err := style.ParseColorValue(epochTestThemeColors[role])
	if err != nil {
		t.Fatalf("test theme color for %s: %v", role, err)
	}
	return color
}

// applyEpochTestTheme applies the test theme through the one entry point a
// theme change uses: style.Apply bumps the epoch and applyStyleEpoch drops the
// caches. No other invalidation happens, so these tests prove the epoch alone
// is what re-resolves the screen.
func applyEpochTestTheme(t *testing.T, r *managedREPL) {
	t.Helper()
	theme := style.Theme{Name: "epoch-test", Colors: epochTestThemeColors}
	if err := theme.Validate(); err != nil {
		t.Fatalf("test theme is invalid: %v", err)
	}
	epoch := style.Apply(theme)
	r.applyStyleEpoch(epoch)
}

// screenCellWithColor reports the first cell inside rect whose foreground is
// color, or whose background is too when includeBackground is set (the bird
// paints half-block cells with a background).
func screenCellWithColor(screen tcell.SimulationScreen, rect image.Rectangle, color ui.Color, includeBackground bool) (image.Point, bool) {
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			_, cellStyle, _ := screen.Get(x, y)
			if cellStyle.GetForeground() == color || includeBackground && cellStyle.GetBackground() == color {
				return image.Pt(x, y), true
			}
		}
	}
	return image.Point{}, false
}

func screenCellWithForeground(screen tcell.SimulationScreen, rect image.Rectangle, color ui.Color) (image.Point, bool) {
	return screenCellWithColor(screen, rect, color, false)
}

// firstCellWithForeground finds a row/column inside a cached transcript that
// carries color as its foreground.
func firstCellWithForeground(rows [][]ui.Cell, color ui.Color) (int, int, bool) {
	for y, row := range rows {
		for x, cell := range row {
			if cell.Style.Fg == color {
				return y, x, true
			}
		}
	}
	return 0, 0, false
}

// A theme change must repaint every surface that reads resolved colors, with no
// invalidation beyond style.Apply plus applyStyleEpoch: transcript rows, the
// masthead bird, a fenced code block, the inspector's orbit frame, and its
// scrollbar thumb.
func TestStyleEpochRepaintsEverySurface(t *testing.T) {
	restoreStyleEpochTestTheme(t)
	withDisplayTTY(t)
	r, screen := chromeTestREPL(t)
	screen.SetSize(140, 40)
	m := r.model
	m.masthead = mastheadState{enabled: true, sandbox: "Sandbox active"}
	m.appendLine(style.Styled("accent-colored sentence", "accent", ""))
	fenced, _, _ := markdown.RenderWithCache("```\nplain code body\n```", m.imageBaseDir, false, nil)
	m.appendLine(fenced)
	// The tools inspector supplies the frame and its scrollbar: its content is
	// long (so the thumb exists), while the transcript stays short, which keeps
	// the masthead and the new entries on screen.
	call := messages.ChatMessageToolCall{ID: "scope", Name: "read_file"}
	m.appendToolCallStart(call)
	m.inspections.setResult(call, messages.ChatMessage{Content: strings.Repeat("result\n", 80)})
	r.inspectCommand("tools")
	openToolDetails(t, r, 140)
	r.render()
	if r.chrome.frame.Empty() || r.chrome.main.Empty() {
		t.Fatalf("expected a split inspector frame=%v main=%v", r.chrome.frame, r.chrome.main)
	}

	newAccent, newMuted := epochThemeColor(t, "accent"), epochThemeColor(t, "muted")
	newBird := epochThemeColor(t, "polly-green")
	// The fence body's own markup names the role it paints with, so resolve the
	// expectation from it rather than assuming which role a code body uses: a
	// later token-role rename must not make this assertion vacuous.
	fenceColor := func() ui.Color {
		for _, cell := range style.ParseCells(fenced, ui.StyleClear) {
			if cell.Rune == 'p' {
				return cell.Style.Fg
			}
		}
		t.Fatal("the fence body is missing from its own markup")
		return ui.ColorClear
	}
	beforeFence := fenceColor()
	pane := r.transcriptW.Inner
	for _, surface := range []struct {
		what string
		want ui.Color
		bg   bool
	}{
		{"accent text", newAccent, false},
		{"masthead bird", newBird, true},
	} {
		if pt, ok := screenCellWithColor(screen, pane, surface.want, surface.bg); ok {
			t.Fatalf("the applied theme's %s color was already on screen at %v before the apply", surface.what, pt)
		}
	}

	applyEpochTestTheme(t, r)
	r.render()

	if _, ok := screenCellWithForeground(screen, pane, newAccent); !ok {
		t.Fatal("transcript text kept the previous theme")
	}
	if afterFence := fenceColor(); afterFence == beforeFence {
		t.Fatalf("the test theme did not change the fenced code body's color %v", beforeFence)
	} else if _, ok := screenCellWithForeground(screen, pane, afterFence); !ok {
		t.Fatalf("the fenced code block kept %v instead of the applied theme's %v", beforeFence, afterFence)
	}
	if _, ok := screenCellWithColor(screen, pane, newBird, true); !ok {
		t.Fatal("the masthead bird kept the previous theme")
	}
	for _, surface := range []struct {
		what string
		pt   image.Point
	}{
		{"orbit frame", r.chrome.frame.Min},
		{"scrollbar thumb", r.inspectorScrollbar.thumb.Min},
	} {
		_, cellStyle, _ := screen.Get(surface.pt.X, surface.pt.Y)
		if got := cellStyle.GetForeground(); got != newMuted {
			t.Fatalf("%s at %v: fg=%v, want the applied theme's muted %v", surface.what, surface.pt, got, newMuted)
		}
	}
}

// transcriptRows returns its cached rows when the geometry still fits and every
// block's key, text, cells, and images are unchanged, so the epoch has to be
// part of that decision: without it a theme change leaves the previous colors
// on screen even though the source never changed.
func TestTranscriptRowsReparseWhenTheStyleEpochChanges(t *testing.T) {
	restoreStyleEpochTestTheme(t)
	m := newReplModel()
	m.appendLine(style.Styled("accent-colored sentence", "accent", ""))
	rows := m.transcriptRows(80)
	if !m.visual.valid {
		t.Fatal("the first render did not cache its rows")
	}
	oldY, oldX, ok := firstCellWithForeground(rows, ui.StyleParserColorMap["accent"])
	if !ok {
		t.Fatal("the accent row is missing from the cached transcript")
	}
	width, native, cellWidth, cellHeight := m.visual.width, m.visual.nativeImages, m.visual.cellWidth, m.visual.cellHeight
	newAccent := epochThemeColor(t, "accent")
	applyEpochTestTheme(t, managedREPLForModel(t, m))

	// Same geometry, same sources, new epoch: the cached rows cannot be reused.
	// valid is restored because applyStyleEpoch's invalidate only hides the
	// cache; the reuse decision is what this test pins.
	m.visual.valid = true
	same := m.transcriptRows(80)
	if m.visual.width != width || m.visual.nativeImages != native || m.visual.cellWidth != cellWidth || m.visual.cellHeight != cellHeight {
		t.Fatal("the re-parse changed geometry")
	}
	if got := same[oldY][oldX].Style.Fg; got != newAccent {
		t.Fatalf("transcript row %d column %d kept fg=%v, want the applied theme's accent %v", oldY, oldX, got, newAccent)
	}
}

// A hidden tab's model is never written by applyStyleEpoch - a background
// turn owns it - so its next paint is what must re-resolve the same source.
func TestStyleEpochReachesBackgroundTabs(t *testing.T) {
	restoreStyleEpochTestTheme(t)
	r := managedREPLForModel(t, newReplModel())
	hidden := newReplModel()
	hidden.appendLine(style.Styled("background accent sentence", "accent", ""))
	r.tabs = append(r.tabs, &replTab{name: "background", model: hidden})
	rows := hidden.transcriptRows(80)
	oldY, oldX, ok := firstCellWithForeground(rows, ui.StyleParserColorMap["accent"])
	if !ok {
		t.Fatal("the background tab has no accent row")
	}
	newAccent := epochThemeColor(t, "accent")
	applyEpochTestTheme(t, r)
	if got := hidden.transcriptRows(80)[oldY][oldX].Style.Fg; got != newAccent {
		t.Fatalf("background tab kept fg=%v, want the applied theme's accent %v", got, newAccent)
	}
}

// The streaming prefix parses its own cells and only re-parses when the source
// changes, so a theme change mid-stream must re-parse the visible prefix
// instead of waiting for the next provider chunk.
func TestStyleEpochReparsesTheStreamingTypewriter(t *testing.T) {
	restoreStyleEpochTestTheme(t)
	withDisplayTTY(t)
	r, _ := affordanceTestREPL(t)
	m := r.model
	m.beginTurn("render")
	m.appendAssistant("`code` plus a long trailing sentence that keeps revealing past this frame")
	at := m.streamTypewriter.receivedAt
	m.renderPendingMarkdownAt(at)
	before := m.streamTypewriter.prefix()
	if before == nil {
		t.Fatal("the stream was not caught mid-reveal")
	}
	parse := func() []ui.Cell {
		return style.ParseCells(strings.TrimRight(m.transcript[m.currentAssistant].text, "\r\n"), ui.StyleClear)
	}
	underOldTheme := parse()

	applyEpochTestTheme(t, r)
	m.renderPendingMarkdownAt(at)
	after := m.streamTypewriter.prefix()
	if after == nil {
		t.Fatal("the theme change dropped the visible prefix")
	}
	underNewTheme := parse()
	if reflect.DeepEqual(underOldTheme, underNewTheme) {
		t.Fatal("the test text has no themed role to re-parse")
	}
	if len(after) != len(before) {
		t.Fatalf("the theme change moved the reveal: %d cells became %d", len(before), len(after))
	}
	if reflect.DeepEqual(before, underNewTheme[:len(before)]) {
		t.Fatal("the stale prefix already matched the new theme")
	}
	if !reflect.DeepEqual(after, underNewTheme[:len(after)]) {
		t.Fatal("the visible prefix kept the previous theme")
	}
}

// agentDetail finds clickable cells by exact style equality against the style
// style.Link renders with, so a cached copy of that resolved style would make
// agent links silently unclickable after any theme change.
func TestAgentLinksStayClickableAfterThemeChange(t *testing.T) {
	restoreStyleEpochTestTheme(t)
	withDisplayTTY(t)
	r, _ := affordanceTestREPL(t)
	m := r.model
	m.beginTurn("go")
	m.appendToolCallStart(agentCall("a", `{"label":"child label"}`))
	record, row := m.toolDisclosureRowForCall("a")
	if row == nil || row.agent == nil {
		t.Fatal("the launch call left no agent row")
	}
	row.agent.session = "child"
	r.tabs = append(r.tabs, &replTab{name: "child", model: newReplModel(), agentActivity: row.agent})
	m.toggleDisclosureGroup(activityAgents, []int64{record.id}, 0)
	rows := m.transcriptRows(80)
	if m.agentLinkPlacements = m.visibleAgentLinks(fullViewport(len(rows), 80)); len(m.agentLinkPlacements) != 1 {
		t.Fatalf("agent links before the theme change = %+v", m.agentLinkPlacements)
	}

	applyEpochTestTheme(t, r)
	rows = m.transcriptRows(80)
	m.agentLinkPlacements = m.visibleAgentLinks(fullViewport(len(rows), 80))
	if len(m.agentLinkPlacements) != 1 {
		t.Fatalf("agent links after the theme change = %+v, want the label still clickable", m.agentLinkPlacements)
	}
	link := m.agentLinkPlacements[0]
	r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: link.X, Y: link.Y}})
	if !r.workspace().inspector.open || r.workspace().inspector.target.session.Name != "child" {
		t.Fatal("clicking the re-themed agent label did not open the child view")
	}
}

// A cached child view is a display copy whose row cache is still valid (that
// clone is what repl_child_cache.go stores, and what a promoted main
// projection restores), so promoting one after a theme change must re-parse
// rather than reuse the retired rows.
func TestStyleEpochReparsesCachedChildViews(t *testing.T) {
	restoreStyleEpochTestTheme(t)
	src := newReplModel()
	src.appendLine(style.Styled("cached child accent sentence", "accent", ""))
	src.transcriptRows(80)
	if !src.visual.valid {
		t.Fatal("the source view has no cached rows to clone")
	}
	newAccent := epochThemeColor(t, "accent")

	src.mu.Lock()
	cached := childDisplayCopy(src)
	src.mu.Unlock()
	if !cached.visual.valid {
		t.Fatal("the cached child view did not clone the row cache")
	}
	applyEpochTestTheme(t, managedREPLForModel(t, src))

	rows := cached.transcriptRows(80)
	if _, _, ok := firstCellWithForeground(rows, newAccent); !ok {
		t.Fatal("the promoted child view kept the retired theme")
	}
}

// Cached image spans and the native placements derived from them describe row
// geometry, which a theme change must not touch.
func TestThemeChangeKeepsImageGeometryInPlace(t *testing.T) {
	restoreStyleEpochTestTheme(t)
	r, _ := affordanceTestREPL(t)
	m := r.model
	path := filepath.Join(t.TempDir(), "chart.png")
	writeImageFixture(t, path, 2400, 270)
	img, ok := markdown.ResolveLocalImage(path, "chart", "")
	if !ok {
		t.Fatal("the wide image did not resolve")
	}
	m.nativeImages = true
	m.imageCellWidth, m.imageCellHeight = 10, 20
	m.setTranscriptImages(m.appendTranscriptEntry(style.RenderImages([]style.Image{img}, "")), []style.Image{img})
	rows := m.transcriptRows(80)
	spans := append([]transcriptImageSpan(nil), m.visual.blocks[0].imageSpans...)
	if len(spans) != 1 {
		t.Fatalf("image spans = %#v, want one", spans)
	}
	placements := m.visibleImagePlacements(fullViewport(len(rows), 80))
	if len(placements) != 1 {
		t.Fatalf("native placements = %#v, want one", placements)
	}

	applyEpochTestTheme(t, r)
	rows = m.transcriptRows(80)
	if got := m.visual.blocks[0].imageSpans; !reflect.DeepEqual(got, spans) {
		t.Fatalf("image spans moved across the theme change: %#v became %#v", spans, got)
	}
	if got := m.visibleImagePlacements(fullViewport(len(rows), 80)); !reflect.DeepEqual(got, placements) {
		t.Fatalf("native image placements moved across the theme change: %#v became %#v", placements, got)
	}
	if len(m.streamTypewriter.cells) != 0 {
		t.Fatal("a settled model kept typewriter cells")
	}
}

// managedREPLForModel wraps a bare test model in the REPL that owns it, so
// applyStyleEpoch can be exercised without a terminal.
func managedREPLForModel(t *testing.T, m *replModel) *managedREPL {
	t.Helper()
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model = m
	r.tabs = []*replTab{{name: "ctx", model: m, workspaceRoot: true}}
	t.Cleanup(func() { _ = r.work.close() })
	return r
}

// applyStyleEpoch drops the visible model's resolved caches: the parsed rows
// (including the revision a retired projection is matched against) and the
// markdown code caches.
func TestStyleEpochDropsResolvedCachesForTheVisibleModel(t *testing.T) {
	restoreStyleEpochTestTheme(t)
	r, _ := affordanceTestREPL(t)
	m := r.model
	m.appendLine(style.Styled("code-ish line", "code", ""))
	m.transcript[0].codeCache = &markdown.CodeCache{}
	m.streamCodeCache = &markdown.CodeCache{}
	before := m.visual.revision
	m.mu.Lock()
	r.applyStyleEpoch(style.Epoch())
	m.mu.Unlock()
	if m.transcript[0].codeCache != nil || m.streamCodeCache != nil {
		t.Fatal("applyStyleEpoch left a code cache behind")
	}
	if m.visual.valid || m.visual.revision <= before {
		t.Fatalf("applyStyleEpoch left the row cache usable: valid=%v revision=%d", m.visual.valid, m.visual.revision)
	}
}

// themeEpochTestTTY is the terminal an image manager writes its graphics
// escapes to: the bytes go nowhere, and the window reports a pixel geometry so
// cell dimensions resolve.
type themeEpochTestTTY struct{ bytes.Buffer }

func (t *themeEpochTestTTY) Start() error             { return nil }
func (t *themeEpochTestTTY) Stop() error              { return nil }
func (t *themeEpochTestTTY) Drain() error             { return nil }
func (t *themeEpochTestTTY) NotifyResize(chan<- bool) {}
func (t *themeEpochTestTTY) Close() error             { return nil }
func (t *themeEpochTestTTY) WindowSize() (tcell.WindowSize, error) {
	return tcell.WindowSize{Width: 140, Height: 40, PixelWidth: 1400, PixelHeight: 800}, nil
}

// The masthead logo is a transparent PNG, and the terminal composites it over
// the cells beneath it — cells the image manager locks for as long as the
// placement stays put, so an ordinary repaint leaves them alone. A theme
// change has to release them, or the logo keeps the old background around it.
func TestStyleEpochRedrawsTerminalImages(t *testing.T) {
	restoreStyleEpochTestTheme(t)
	withDisplayTTY(t)
	r, screen := chromeTestREPL(t)
	screen.SetSize(140, 40)
	r.images = termimg.NewManagerFor(screen, &themeEpochTestTTY{}, termimg.ProtocolKitty)
	t.Cleanup(r.images.Shutdown)

	logo := termimg.LogoImage()
	placement := termimg.Placement{
		Key: "masthead:logo", Embedded: logo.Embedded,
		X: 0, Y: 0, Cols: 12, Rows: termimg.LogoArtRows,
	}
	r.images.Commit(r.images.Prepare([]termimg.Placement{placement}))
	if r.images.ActiveCount() != 1 {
		t.Fatalf("the logo was not placed: active = %d", r.images.ActiveCount())
	}
	if r.images.Prepare([]termimg.Placement{placement}) {
		t.Fatal("an unchanged frame asked to redraw the logo")
	}

	applyEpochTestTheme(t, r)
	if !r.images.Prepare([]termimg.Placement{placement}) {
		t.Fatal("a theme change left the logo's cells locked under the old background")
	}
}
