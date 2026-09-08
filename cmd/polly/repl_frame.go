package main

import (
	"image"
	"strings"
	"time"

	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
	"github.com/metaspartan/gotui/v5/widgets"
)

// Frame layout and rendering: the vertical split, widgets, and cursor placement.

// transcriptParagraph is a small, REPL-specific paragraph renderer. gotui's
// stock Paragraph clips from the top after wrapping, which hides the newest
// rows of a long assistant message exactly when follow-bottom matters most.
// This renderer wraps before clipping and can pin overflow to the bottom.
type transcriptParagraph struct {
	ui.Block
	Text      string
	TextStyle ui.Style
	PinBottom bool
	TopRow    int
	Rows      [][]ui.Cell
	UseRows   bool
	// OverlayBottom, when non-empty, replaces the pane's bottom rows with the
	// turn-detail drawer and/or scrolled-up activity ticker. Covered transcript
	// rows remain reachable by scrolling; opening the drawer never reflows them.
	OverlayBottom [][]ui.Cell
}

func newTranscriptParagraph() *transcriptParagraph {
	return &transcriptParagraph{
		Block:     *ui.NewBlock(),
		TextStyle: ui.Theme.Paragraph.Text,
		PinBottom: true,
	}
}

func (p *transcriptParagraph) Draw(buf *ui.Buffer) {
	p.Block.Draw(buf)
	if p.Inner.Dx() <= 0 || p.Inner.Dy() <= 0 {
		return
	}
	rows := p.Rows
	if !p.UseRows {
		rows = transcriptVisualRows(p.Text, p.TextStyle, p.Inner.Dx())
	}
	p.drawRows(buf, rows)
}

func (p *transcriptParagraph) drawRows(buf *ui.Buffer, rows [][]ui.Cell) {
	height := p.Inner.Dy()
	if height <= 0 || len(rows) == 0 {
		return
	}

	start := 0
	if p.PinBottom && len(rows) > height {
		start = len(rows) - height
	} else if !p.PinBottom {
		start = p.TopRow
		if start < 0 {
			start = 0
		}
		if start > len(rows)-1 {
			start = len(rows) - 1
		}
	}
	rows = rows[start:]
	if len(rows) > height {
		rows = rows[:height]
	}

	topPadding := 0
	if p.PinBottom && len(rows) < height {
		topPadding = height - len(rows)
	}
	for i, row := range rows {
		y := i + topPadding
		if y >= height {
			break
		}
		for _, cx := range ui.BuildCellWithXArray(row) {
			if rw.RuneWidth(cx.Cell.Rune) == 0 || cx.X+rw.RuneWidth(cx.Cell.Rune) > p.Inner.Dx() {
				continue
			}
			buf.SetCell(cx.Cell, image.Pt(cx.X, y).Add(p.Inner.Min))
		}
	}

	overlay := p.OverlayBottom
	if len(overlay) > height {
		overlay = overlay[len(overlay)-height:]
	}
	for i, row := range overlay {
		y := height - len(overlay) + i
		for x := 0; x < p.Inner.Dx(); x++ {
			buf.SetCell(ui.Cell{Rune: ' ', Style: ui.StyleClear}, image.Pt(x, y).Add(p.Inner.Min))
		}
		for _, cx := range ui.BuildCellWithXArray(row) {
			if cx.X >= p.Inner.Dx() || rw.RuneWidth(cx.Cell.Rune) == 0 {
				continue
			}
			buf.SetCell(cx.Cell, image.Pt(cx.X, y).Add(p.Inner.Min))
		}
	}
}

// transcriptHeight returns the current usable line count of the transcript
// pane based on the live terminal dimensions. Used by scroll handlers so
// scroll deltas match what the user actually sees.
func (r *managedREPL) transcriptHeight() int {
	return max(1, r.frameLayoutFor(ui.TerminalDimensions()).transcriptHeight)
}

// frameLayout is the vertical geometry of one frame: how the terminal height
// splits, top to bottom, between the logo band, the transcript pane, the turn
// dock, the divider, the composer, and the status bar. render and the scroll
// handlers derive it the same way, so scroll deltas match the pane the user
// actually sees.
type frameLayout struct {
	width, height    int
	logoRows         int
	transcriptHeight int
	dockRows         int
	dividerRows      int
	inputRows        int
	statusRows       int
	halo             bool
}

// frameLayoutFor splits a w×h terminal. The composer is the only region whose
// height follows its content, so it is measured first and then capped to what
// the fixed chrome leaves; the divider and logo drop out when short, and the
// transcript takes whatever remains. Caller must hold m.mu.
func (r *managedREPL) frameLayoutFor(w, h int) frameLayout {
	m := r.model
	l := frameLayout{width: w, height: h, inputRows: m.inputRows()}
	l.halo = r.haloChrome(w, h)
	if !m.quiet {
		l.statusRows = 1
	}
	l.dockRows = turnDockRowCount(h, l.inputRows, l.statusRows, !m.quiet && m.turnDock.visible)
	if room := h - l.statusRows - l.dockRows; room > 1 {
		l.inputRows = min(l.inputRows, room-1)
	} else {
		l.inputRows = 1
	}
	if m.status.parentName != "" {
		l.dividerRows = dividerRowCount(h, l.inputRows, l.statusRows, l.dockRows, m.quiet)
	}
	content := h - l.inputRows - l.statusRows - l.dockRows - l.dividerRows
	l.logoRows = startupLogoRowCount(content, r.startupLogoVisible, r.images != nil)
	l.transcriptHeight = max(0, content-l.logoRows)
	return l
}

// composerRow maps a row inside the composer to its screen row.
func (l frameLayout) composerRow(row int) int {
	return l.logoRows + l.transcriptHeight + l.dockRows + l.dividerRows + row
}

// transcriptViewport resolves which transcript display rows land on screen
// this frame. A pinned pane shows the last rows; an overlay ticker hides the
// bottom overlayRows. A pane with no room yields an empty window.
func (l frameLayout) transcriptViewport(totalRows, topRow int, pinBottom bool, overlayRows int) transcriptViewport {
	v := transcriptViewport{width: l.width, logoRows: l.logoRows}
	if l.transcriptHeight <= 0 || l.width <= 0 {
		return v
	}
	v.start = topRow
	if pinBottom {
		v.start = max(0, totalRows-l.transcriptHeight)
		if totalRows < l.transcriptHeight {
			v.topPadding = l.transcriptHeight - totalRows
		}
	}
	v.end = v.start + l.transcriptHeight - min(overlayRows, l.transcriptHeight)
	return v
}

// dividerRowCount reserves parent navigation above the composer, dropped
// entirely in quiet mode or when the terminal is too short to spare it.
func dividerRowCount(h, inputRows, statusRows, dockRows int, quiet bool) int {
	if quiet || h-inputRows-statusRows-dockRows < 2 {
		return 0
	}
	return 1
}

func turnDockRowCount(h, inputRows, statusRows int, visible bool) int {
	if !visible || h-inputRows-statusRows < 2 {
		return 0
	}
	return 1
}

func (r *managedREPL) setupWidgets() {
	r.setupInspectorWidgets()
	// gotui paragraphs default their TextStyle to ColorWhite, which forces
	// unstyled text (primary input, LLM responses) to white and ignores the
	// terminal theme. ColorClear (= tcell.ColorDefault) inherits the terminal's
	// default foreground instead, so text follows the theme like our accents do.
	r.logoW = newTranscriptParagraph()
	noBorder(&r.logoW.Block)
	r.logoW.TextStyle = ui.NewStyle(ui.ColorClear)
	r.logoW.UseRows = true
	r.logoW.PinBottom = false

	r.transcriptW = newTranscriptParagraph()
	noBorder(&r.transcriptW.Block)
	r.transcriptW.TextStyle = ui.NewStyle(ui.ColorClear)

	r.dividerW = newLiteralParagraph()
	noBorder(&r.dividerW.Block)
	r.dividerW.WrapText = false
	r.dividerW.TextStyle = ui.NewStyle(ui.ColorClear)

	r.inputW = newLiteralParagraph()
	noBorder(&r.inputW.Block)
	r.inputW.WrapText = false
	r.inputW.TextStyle = ui.NewStyle(ui.ColorClear)

	r.turnDockW = newLiteralParagraph()
	noBorder(&r.turnDockW.Block)
	r.turnDockW.WrapText = false
	r.turnDockW.TextStyle = ui.NewStyle(ui.ColorGrey)

	r.statusW = newLiteralParagraph()
	noBorder(&r.statusW.Block)
	r.statusW.WrapText = false
	r.statusW.TextStyle = ui.NewStyle(ui.ColorGrey)
	r.modalW = newModalParagraph()
}

// layout (re)builds the root flex for the current input height. The input row
// count varies with multi-line prompts, so the flex is rebuilt each render
// rather than sized once at setup.
func (r *managedREPL) layout(l frameLayout) {
	if l.halo && r.layoutHalo(l) {
		return
	}
	flex := widgets.NewFlex()
	noBorder(&flex.Block)
	flex.Direction = widgets.FlexColumn
	if l.logoRows > 0 {
		flex.AddItem(r.logoW, l.logoRows, 0, false)
	}
	if r.workspace().inspector.open {
		flex.AddItem(r.inspectorLayout(l), 0, 1, false)
	} else {
		flex.AddItem(r.transcriptW, 0, 1, false)
	}
	if l.dockRows > 0 {
		flex.AddItem(r.turnDockW, 1, 0, false)
	}
	if l.dividerRows > 0 {
		flex.AddItem(r.dividerW, 1, 0, false)
	}
	flex.AddItem(r.inputW, l.inputRows, 0, false)
	if l.statusRows > 0 {
		flex.AddItem(r.statusW, 1, 0, false)
	}
	flex.SetRect(0, 0, l.width, l.height)
	r.rootFlex = flex
}

// noBorder disables the visible border AND cancels the unconditional 1-cell
// Inner inset that Block.SetRect adds. Without negative padding, a 1-row
// borderless Paragraph collapses to zero inner rows and refuses to paint.
func noBorder(b *ui.Block) {
	b.Border = false
	b.PaddingLeft = -1
	b.PaddingRight = -1
	b.PaddingTop = -1
	b.PaddingBottom = -1
}

func (r *managedREPL) render() {
	w, h := ui.TerminalDimensions()
	if w < 1 || h < 2 {
		return
	}
	r.relayTabSignals()
	r.refreshAgentActivities()
	r.refreshInspector(w)
	mainWidth := r.inspectorTranscriptWidth(w)
	imageCellWidth, imageCellHeight := 0, 0
	if r.images != nil {
		imageCellWidth, imageCellHeight = r.images.cellDimensions()
	}

	r.model.mu.Lock()
	now := time.Now()
	r.model.expireAffordances(now)
	r.model.renderPendingMarkdownAt(now)
	if r.model.imageCellWidth != imageCellWidth || r.model.imageCellHeight != imageCellHeight {
		r.model.imageCellWidth = imageCellWidth
		r.model.imageCellHeight = imageCellHeight
		r.model.visual.invalidate()
	}
	r.model.refreshActiveTools()
	r.model.refreshStreamCursor()
	r.model.refreshReasoningRecords(mainWidth)
	r.model.refreshExpandedTurnTrailer(mainWidth)
	l := r.frameLayoutFor(w, h)
	input, curRow, curCol, editable := r.model.renderInputForTerminal(l.inputRows, w)
	transcriptRows := (conversationView{}).Rows(r.model, mainWidth)
	topRow, pinTranscriptBottom := r.model.settleScroll(len(transcriptRows), l.transcriptHeight)
	status := r.model.statusRow(w)
	divider := r.model.dividerRow(l)
	modalOpen := r.model.modal != nil
	modalText, modalTitle := "", ""
	modalWidth, modalHeight := 0, 0
	if modalOpen {
		r.model.modal.halo = r.haloEnabled()
		modalWidth = modalWidthForTerminal(w, r.model.modal.width)
		maxRows := max(1, h-8)
		modalText = r.model.modal.text(maxRows, modalWidth)
		modalTitle = r.model.modal.title
		modalHeight = min(h, max(3, strings.Count(modalText, "\n")+3))
	}
	var dock string
	if l.dockRows > 0 {
		dock, _ = r.model.turnDockRow(w)
	}
	title := r.model.frameTitle()
	progress := r.model.frameProgress()
	notices := r.model.takeNotices()
	focusKnown, focused := r.model.focusKnown, r.model.focused
	ticker := r.model.activityTicker(len(transcriptRows), topRow, l.transcriptHeight)
	var overlay [][]ui.Cell
	if ticker != "" {
		overlay = append(overlay, parseStyledCells(ticker, ui.NewStyle(ui.ColorClear)))
	}
	paneLayout := l
	paneLayout.width = mainWidth
	viewport := paneLayout.transcriptViewport(len(transcriptRows), topRow, pinTranscriptBottom, len(overlay))
	imagePlacements := r.model.visibleImagePlacements(viewport)
	r.model.imagePlacements = imagePlacements
	r.model.reasoningPlacements = r.model.visibleReasoningPlacements(viewport)
	r.model.toolDisclosurePlacements = r.model.visibleToolDisclosurePlacements(viewport)
	r.model.imageDisclosurePlacements = r.model.visibleImageDisclosurePlacements(viewport)
	r.model.agentDisclosurePlacements = r.model.visibleDisclosurePlacements(viewport, turnDockOverlayAgents)
	r.model.agentLinkPlacements = r.model.visibleAgentLinks(viewport)
	r.model.turnTrailerPlacements = r.model.visibleTurnTrailerPlacements(viewport)
	r.model.inspectionLinks = r.model.visibleInspectionLinks(viewport, 0)
	var affordanceSpans []affordanceSpan
	idleCursor := r.affordanceW != nil && editable && !r.workspace().inspector.searching && r.model.idleAffordanceCursor(now)
	if r.affordanceW != nil {
		affordanceSpans = r.model.affordanceSpans(now, l, viewport, status, image.Pt(min(curCol, w-1), l.composerRow(curRow)), idleCursor)
	}
	r.model.mu.Unlock()
	r.mainTranscriptBounds = image.Rect(0, viewport.logoRows, mainWidth, viewport.logoRows+l.transcriptHeight)
	if r.workspace().inspector.open {
		if r.workspace().inspector.maximized || w < 120 {
			r.mainTranscriptBounds = image.Rectangle{}
			imagePlacements = nil
			visible := affordanceSpans[:0]
			for _, span := range affordanceSpans {
				if span.y < l.logoRows || span.y >= l.logoRows+l.transcriptHeight {
					visible = append(visible, span)
				}
			}
			affordanceSpans = visible
		}
		imagePlacements = append(imagePlacements, r.renderInspector(l)...)
		if r.workspace().inspector.searching {
			editable = false
			idleCursor = false
		}
	} else {
		r.inspectorBounds, r.inspectorDivider = image.Rectangle{}, image.Rectangle{}
		r.inspectorButtons = nil
		r.inspectorDragging = false
	}
	notices = append(notices, r.takeHiddenNotices(focusKnown, focused)...)

	if l.logoRows == imageLogoHeight && r.images != nil {
		// The image splash rides the same placement pipeline as thumbnails:
		// its band is blank in the text layer and the manager draws, diffs,
		// and releases it like any other placement.
		if logo, ok := startupLogoPlacement(w, imageCellWidth, imageCellHeight); ok {
			imagePlacements = append([]terminalImagePlacement{logo}, imagePlacements...)
		}
	}

	imagesChanged := false
	if modalOpen {
		imagePlacements = nil
	}
	if r.images != nil {
		imagesChanged = r.images.prepare(imagePlacements)
	}

	r.transcriptW.Rows = transcriptRows
	r.transcriptW.UseRows = true
	r.transcriptW.PinBottom = pinTranscriptBottom
	r.transcriptW.TopRow = topRow
	r.transcriptW.OverlayBottom = overlay
	if l.logoRows == imageLogoHeight {
		r.logoW.Rows = make([][]ui.Cell, l.logoRows)
	} else {
		r.logoW.Rows = pollyLogoRows(w)
	}
	r.logoW.TopRow = 0
	r.logoW.OverlayBottom = nil
	r.inputW.Text = input
	r.turnDockW.Text = dock
	r.statusW.Text = status
	if modalOpen {
		x := (w - modalWidth) / 2
		y := (h - modalHeight) / 2
		r.modalW.Text = modalText
		r.modalW.Title = modalTitle
		r.modalW.PaddingRight = 0
		if r.haloEnabled() {
			r.modalW.PaddingRight = 1
		}
		r.modalW.SetRect(x, y, x+modalWidth, y+modalHeight)
		if r.haloEnabled() {
			m := r.model.modal
			m.listBounds = image.Rect(r.modalW.Inner.Min.X, r.modalW.Inner.Min.Y, r.modalW.Inner.Max.X, min(r.modalW.Inner.Max.Y, r.modalW.Inner.Min.Y+m.visible))
			track := image.Rect(x+modalWidth-2, m.listBounds.Min.Y, x+modalWidth-1, m.listBounds.Max.Y)
			if m.inputMode {
				track = image.Rectangle{}
			}
			r.modalScrollbar = newHaloScrollbar(track, len(m.filteredItems()), m.visible, m.top)
			r.modalScrollbar.hover = r.mousePositionKnown && r.mousePosition.In(track)
			r.modalScrollbar.dragging = r.scrollDrag.pane == "modal"
			r.modalW.scrollbar = r.modalScrollbar
		} else {
			r.modalW.scrollbar = haloScrollbar{}
		}
	}
	r.dividerW.Text = divider

	r.layout(l)
	if l.halo {
		r.setHaloScrollbars()
	} else {
		r.haloBounds = haloGeometry{}
		r.inspectorScrollbar = haloScrollbar{}
	}
	ui.Clear()
	r.placeCursor(editable && !modalOpen && !idleCursor, curCol, l.composerRow(curRow), w)
	var drawable ui.Drawable = r.rootFlex
	if modalOpen {
		if r.affordanceW != nil {
			r.affordanceW.cells = nil
			r.affordanceW.idleCursor = false
		}
		if !r.haloEnabled() {
			ui.Render(r.rootFlex, r.modalW)
		}
	} else if r.affordanceW != nil {
		r.affordanceW.Drawable = r.rootFlex
		r.affordanceW.spans = affordanceSpans
		r.affordanceW.now = now
		r.affordanceW.idleCursor = idleCursor
		drawable = r.affordanceW
	}
	if r.haloEnabled() {
		drawable = r.haloFrame(drawable, l, now, modalOpen)
		if r.affordanceW != nil {
			r.affordanceW.theme = r.themeW
		}
		ui.Render(drawable)
		for n := range r.haloOrbit.cells {
			cell := &r.haloOrbit.cells[n]
			cell.last = r.themeW.convert(r.haloOrbit.frame(n, now), cell.point)
		}
	} else if !modalOpen {
		ui.Render(drawable)
	}
	if r.images != nil {
		r.images.commit(imagesChanged)
	}

	// Window-level effects go out after the frame, on this same goroutine, so
	// their escapes serialize with tcell's own writes.
	if r.fx != nil {
		r.fx.setTitle(title)
		r.fx.setProgress(progress)
		for _, body := range notices {
			r.fx.notify("polly", body)
		}
	}
}

func modalWidthForTerminal(terminalWidth, preferred int) int {
	if preferred <= 0 {
		preferred = 64
	}
	width := min(preferred, terminalWidth)
	if terminalWidth > 4 {
		width = min(preferred, terminalWidth-4)
	}
	if terminalWidth >= 24 {
		width = max(24, width)
	}
	return width
}

// placeCursor positions (or hides) the hardware terminal cursor on the input
// row. gotui's render flushes the screen with Show(), which also emits the
// cursor state set here, so this must run before ui.Render.
func (r *managedREPL) placeCursor(editable bool, cursorCol, rowY, width int) {
	screen := ui.DefaultBackend.Screen
	if screen == nil {
		return
	}
	if !editable {
		screen.HideCursor()
		return
	}
	x := cursorCol
	if x > width-1 {
		x = width - 1
	}
	_, height := screen.Size()
	if rowY < 0 {
		rowY = 0
	}
	if height > 0 && rowY > height-1 {
		rowY = height - 1
	}
	screen.ShowCursor(x, rowY)
}
