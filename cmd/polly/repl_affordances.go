package main

import (
	"fmt"
	"image"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

// Screen-local feedback. None of this state belongs to stored messages or
// provider replay. Cue maps contain only recent events, not transcript history.
type affordanceTarget struct {
	kind    turnDockOverlay
	id      int64
	trailer bool
}

type queuedAffordance struct {
	started, fading time.Time
	prefix, text    string
}

type affordanceState struct {
	enabled       bool
	disclosures   map[affordanceTarget]time.Time
	agents        map[int64]time.Time
	queued        map[int]queuedAffordance
	caller        time.Time
	inputAt       time.Time
	contextKnown  bool
	contextFilled int
	contextFrom   int
	contextAt     time.Time
}

const queueFadeDuration = 400 * time.Millisecond

func (m *replModel) affordancesVisible() bool {
	return m.affordances.enabled && !m.quiet && !m.hidden && m.modal == nil && (!m.focusKnown || m.focused)
}

func (m *replModel) noteDisclosure(kind turnDockOverlay, id int64, trailer bool) {
	if !m.affordancesVisible() {
		return
	}
	if m.affordances.disclosures == nil {
		m.affordances.disclosures = make(map[affordanceTarget]time.Time)
	}
	m.affordances.disclosures[affordanceTarget{kind, id, trailer}] = time.Now()
}

func (m *replModel) noteAgentCompletion(id int64) {
	if !m.affordancesVisible() {
		return
	}
	if m.affordances.agents == nil {
		m.affordances.agents = make(map[int64]time.Time)
	}
	m.affordances.agents[id] = time.Now()
}

func (r *managedREPL) noteCallerReady(tab *replTab) {
	tab.model.mu.Lock()
	defer tab.model.mu.Unlock()
	if tab.model.affordancesVisible() {
		tab.model.affordances.caller = time.Now()
	}
}

func (m *replModel) noteQueuedInput(index int, prefix string) {
	if !m.affordancesVisible() {
		return
	}
	if m.affordances.queued == nil {
		m.affordances.queued = make(map[int]queuedAffordance)
	}
	m.affordances.queued[index] = queuedAffordance{started: time.Now(), prefix: prefix}
}

func (m *replModel) fadeQueuedInput(index int, prefix string) {
	if !m.affordancesVisible() {
		delete(m.affordances.queued, index)
		return
	}
	if m.affordances.queued == nil {
		m.affordances.queued = make(map[int]queuedAffordance)
	}
	m.affordances.queued[index] = queuedAffordance{fading: time.Now(), prefix: prefix, text: m.transcript[index].text}
}

// A tab/focus change must not replay old highlights or resurrect queue labels.
func (m *replModel) resetAffordances() {
	m.streamTypewriter.instant = true
	for index, q := range m.affordances.queued {
		if !q.fading.IsZero() {
			m.endQueueFade(index)
		}
	}
	m.affordances = affordanceState{enabled: m.affordances.enabled, inputAt: time.Now()}
}

func (m *replModel) endQueueFade(index int) {
	width := m.visual.width
	if width < 1 {
		width = 80
	}
	key := fmt.Sprintf("transcript:%d", index)
	m.mutateAnchored(width, func(block *transcriptVisualBlock) bool { return block.key == key }, func(bool) {
		delete(m.affordances.queued, index)
		m.visual.invalidate()
	})
}

func (m *replModel) expireAffordances(now time.Time) bool {
	reflow := false
	for key, at := range m.affordances.disclosures {
		if now.Sub(at) >= 1500*time.Millisecond {
			delete(m.affordances.disclosures, key)
		}
	}
	for id, at := range m.affordances.agents {
		if now.Sub(at) >= 1300*time.Millisecond {
			delete(m.affordances.agents, id)
		}
	}
	for index, q := range m.affordances.queued {
		if !q.fading.IsZero() {
			if now.Sub(q.fading) >= queueFadeDuration {
				m.endQueueFade(index)
				reflow = true
			}
		} else if now.Sub(q.started) >= 1500*time.Millisecond {
			delete(m.affordances.queued, index)
		}
	}
	return reflow
}

func (m *replModel) idleAffordanceCursor(now time.Time) bool {
	return m.affordancesVisible() && !m.busy && m.approval == nil && !m.hist.searching &&
		!m.pasting && !m.clipboardCapture && m.ed.text() == "" && now.Sub(m.affordances.inputAt) >= 700*time.Millisecond
}

type affordanceSpan struct {
	x, y, cols int
	at         time.Time
	duration   time.Duration
	color      ui.Color
	fade       bool
	cursor     bool
}

type affordanceCell struct {
	point image.Point
	base  ui.Cell
	last  ui.Cell
	span  affordanceSpan
}

// Draw decorates the real widget buffer after layout. Idle ticks reuse only
// these cells, leaving the transcript cache, image placements, and geometry alone.
type affordanceLayer struct {
	ui.Drawable
	spans      []affordanceSpan
	cells      []affordanceCell
	now        time.Time
	idleCursor bool
}

func (a *affordanceLayer) Draw(buf *ui.Buffer) {
	a.Drawable.Draw(buf)
	a.cells = a.cells[:0]
	for _, span := range a.spans {
		for x := span.x; x < span.x+span.cols; x++ {
			pt := image.Pt(x, span.y)
			if !pt.In(buf.Rectangle) {
				continue
			}
			base := buf.GetCell(pt)
			if span.cursor {
				if base.Rune != 0 && base.Rune != ' ' {
					continue
				}
				base.Rune = ' '
			}
			cell := affordanceCell{point: pt, base: base, span: span}
			cell.last = cell.frame(a.now)
			buf.SetCell(cell.last, pt)
			a.cells = append(a.cells, cell)
		}
	}
}

func smoothAffordance(v float64) float64 {
	v = math.Max(0, math.Min(1, v))
	return v * v * (3 - 2*v)
}

func affordanceStrength(now time.Time, span affordanceSpan) float64 {
	if span.at.IsZero() || span.duration <= 0 {
		return 0
	}
	phase := float64(now.Sub(span.at)) / float64(span.duration)
	if phase <= 0 || phase >= 1 {
		return 0
	}
	if phase < .2 {
		return smoothAffordance(phase / .2)
	}
	return 1 - smoothAffordance((phase-.2)/.8)
}

func (c affordanceCell) frame(now time.Time) ui.Cell {
	out := c.base
	if c.span.cursor {
		out.Style = ui.NewStyle(ui.ColorBlue, ui.ColorClear, ui.ModifierReverse)
		breath := (1 - math.Cos(now.Sub(c.span.at).Seconds()*math.Pi/3)) / 2
		if breath < .35 {
			out.Style.Modifier |= ui.ModifierDim
		}
		return out
	}
	if c.span.fade {
		if now.Sub(c.span.at) < c.span.duration {
			out.Style.Modifier |= ui.ModifierDim
		} else {
			out.Rune = ' '
		}
		return out
	}
	// Native ANSI slots follow the user's theme. Do not interpolate their
	// nominal RGB values: those are not the colors the terminal actually uses.
	if affordanceStrength(now, c.span) > .4 {
		out.Style.Fg = c.span.color
		out.Style.Modifier = (out.Style.Modifier &^ ui.ModifierDim) | ui.ModifierBold
	}
	return out
}

func (a *affordanceLayer) tick(screen tcell.Screen, now time.Time) {
	if a == nil || screen == nil {
		return
	}
	changed := false
	active := a.cells[:0]
	for _, cell := range a.cells {
		next := cell.frame(now)
		if next != cell.last {
			style := tcell.StyleDefault.Foreground(next.Style.Fg).Background(next.Style.Bg).
				Bold(next.Style.Modifier&ui.ModifierBold != 0).
				Dim(next.Style.Modifier&ui.ModifierDim != 0).
				Reverse(next.Style.Modifier&ui.ModifierReverse != 0).
				Italic(next.Style.Modifier&ui.ModifierItalic != 0).
				StrikeThrough(next.Style.Modifier&tcell.AttrStrikeThrough != 0).
				Blink(next.Style.Modifier&ui.ModifierBlink != 0)
			screen.SetContent(cell.point.X, cell.point.Y, next.Rune, nil, style)
			cell.last = next
			changed = true
		}
		if cell.span.cursor || now.Before(cell.span.at.Add(cell.span.duration)) {
			active = append(active, cell)
		}
	}
	a.cells = active
	if changed {
		screen.Show()
	}
}

func (r *managedREPL) tickAffordances(now time.Time) {
	if r.affordanceW == nil {
		return
	}
	r.model.mu.Lock()
	repaint := r.model.expireAffordances(now)
	repaint = repaint || r.model.idleAffordanceCursor(now) != r.affordanceW.idleCursor
	r.model.mu.Unlock()
	if repaint {
		r.render()
		return
	}
	r.affordanceW.tick(ui.DefaultBackend.Screen, now)
}

func (m *replModel) affordanceSpans(now time.Time, l frameLayout, v transcriptViewport, status string, cursor image.Point, idle bool) []affordanceSpan {
	if !m.affordancesVisible() {
		return nil
	}
	var spans []affordanceSpan
	add := func(x, y, cols int, at time.Time, duration time.Duration, color ui.Color) {
		if !at.IsZero() && now.Before(at.Add(duration)) {
			spans = append(spans, affordanceSpan{x: x, y: y, cols: cols, at: at, duration: duration, color: color})
		}
	}
	for kind, placements := range map[turnDockOverlay][]disclosurePlacement{
		turnDockOverlayThought: m.reasoningPlacements, turnDockOverlayTools: m.toolDisclosurePlacements,
		turnDockOverlayAgents: m.agentDisclosurePlacements, turnDockOverlayImages: m.imageDisclosurePlacements,
	} {
		for _, p := range placements {
			add(p.X, p.Y, 1, m.affordances.disclosures[affordanceTarget{kind, p.recordID, false}], 1500*time.Millisecond, ui.ColorWhite)
		}
	}
	for _, p := range m.turnTrailerPlacements {
		add(p.X, p.Y, 1, m.affordances.disclosures[affordanceTarget{p.overlay, p.recordID, true}], 1500*time.Millisecond, ui.ColorWhite)
	}
	// Work from semantic activity blocks, not arbitrary answer text. A count
	// may move from an inline group to a trailer while a child is still running.
	agentOwners := make(map[int64]int64, len(m.affordances.agents))
	for id := range m.affordances.agents {
		for trailerID, trailer := range m.turnTrailers {
			if slices.Contains(trailer.dock.toolIDs, id) {
				agentOwners[id] = trailerID
			}
		}
	}
	offset := 0
	for _, block := range m.visual.blocks {
		ids, fields := block.toolDisclosureIDs, block.activityFields
		if trailer := m.turnTrailers[block.turnTrailerID]; trailer != nil {
			ids, fields = trailer.dock.toolIDs, trailer.fields
		}
		if v.contains(offset) && len(block.rows) > 0 {
			var at time.Time
			for _, id := range ids {
				if owner := agentOwners[id]; owner != 0 && owner != block.turnTrailerID {
					continue
				}
				if m.affordances.agents[id].After(at) {
					at = m.affordances.agents[id]
				}
			}
			for _, field := range fields {
				if field.overlay == turnDockOverlayAgents {
					x, cols := agentCountCells(block.rows[0], field)
					add(x, v.screenY(offset), cols, at, 1300*time.Millisecond, ui.ColorGreen)
				}
			}
		}
		for index, q := range m.affordances.queued {
			if block.key != fmt.Sprintf("transcript:%d", index) {
				continue
			}
			rows := transcriptVisualRows(q.prefix+styled("(queued)", "muted", ""), ui.NewStyle(ui.ColorClear), l.width)
			remaining := len("(queued)")
			for y := len(rows) - 1; y >= 0 && remaining > 0; y-- {
				cells := ui.BuildCellWithXArray(rows[y])
				for i := len(cells) - 1; i >= 0 && remaining > 0; i-- {
					if cells[i].Cell.Rune == ' ' {
						continue
					}
					remaining--
					if !v.contains(offset + y) {
						continue
					}
					if q.fading.IsZero() {
						add(cells[i].X, v.screenY(offset+y), 1, q.started, 1500*time.Millisecond, ui.ColorYellow)
					} else {
						spans = append(spans, affordanceSpan{x: cells[i].X, y: v.screenY(offset + y), cols: 1, at: q.fading, duration: queueFadeDuration, fade: true})
					}
				}
			}
		}
		offset += len(block.rows)
	}
	if !m.parentLink.Empty() {
		at := m.affordances.caller
		add(m.parentLink.Min.X, m.parentLink.Min.Y, 1, at, 1600*time.Millisecond, ui.ColorWhite)
		for i := 0; i < 10; i++ {
			add(m.parentLink.Max.X+1+i, m.parentLink.Min.Y, 1, at.Add(time.Duration(9-i)*60*time.Millisecond), 500*time.Millisecond, ui.ColorWhite)
		}
	}
	filled := 0
	if m.status.contextLimit > 0 {
		filled = max(0, min(10, m.status.contextUsed*10/m.status.contextLimit))
	}
	if m.affordances.contextKnown && filled > m.affordances.contextFilled {
		m.affordances.contextAt, m.affordances.contextFrom = now, m.affordances.contextFilled
	}
	m.affordances.contextKnown, m.affordances.contextFilled = true, filled
	bar := parseStyledCells(contextMeterBar(m.status.contextUsed, m.status.contextLimit, 10), ui.NewStyle(ui.ColorClear))
	cells := parseStyledCells(status, ui.NewStyle(ui.ColorClear))
	for i, cx := range ui.BuildCellWithXArray(cells) {
		if len(bar) > 0 && i+len(bar) <= len(cells) && slices.Equal(cells[i:i+len(bar)], bar) {
			add(cx.X+m.affordances.contextFrom, l.height-1, max(0, filled-m.affordances.contextFrom), m.affordances.contextAt, 1400*time.Millisecond, ui.ColorWhite)
			break
		}
	}
	if idle {
		spans = append(spans, affordanceSpan{x: cursor.X, y: cursor.Y, cols: 1, at: m.affordances.inputAt, cursor: true})
	}
	return spans
}

// Select just the completed number, or the total when no agents remain running.
func agentCountCells(row []ui.Cell, field turnDockPlacement) (int, int) {
	var text strings.Builder
	for _, cx := range ui.BuildCellWithXArray(row) {
		if cx.X >= field.X && cx.X < field.X+field.Cols {
			text.WriteRune(cx.Cell.Rune)
		}
	}
	s := []rune(text.String())
	start := 2
	if i := strings.Index(string(s), " completed"); i >= 0 {
		start = len([]rune(string(s)[:i]))
		for start > 0 && s[start-1] >= '0' && s[start-1] <= '9' {
			start--
		}
	}
	end := start
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	return field.X + start, end - start
}
