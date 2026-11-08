package main

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"slices"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/swarm"
	rw "github.com/mattn/go-runewidth"
)

// Agent timeline: each swarm member's phases drawn on one shared time axis
// as a lane under every agent row. Executions supply the outline (when each
// ran); saved replies and tool results fill it in (repl_agents_timeline_read.go),
// and phases the root tab's poller observed add approval waits, parking and
// the tail not saved yet. Time with neither, such as runs from before the
// saved stamps, stays a plain "ran" bar. Ctrl-T scrubs the axis.

type agentPhase uint8

const (
	phaseNone agentPhase = iota
	// phaseRan is time inside an execution whose phase was not observed.
	phaseRan
	phaseQuery
	phaseTool
	phaseParked
	phaseWaiting
)

func (p agentPhase) role() string {
	switch p {
	case phaseTool:
		return "run"
	case phaseParked:
		return "muted"
	case phaseWaiting:
		return "active"
	}
	return "accent"
}

func (p agentPhase) word() string {
	switch p {
	case phaseQuery:
		return "querying"
	case phaseTool:
		return "toolcall"
	case phaseParked:
		return "parked"
	case phaseWaiting:
		return "approval"
	case phaseRan:
		return "running"
	}
	return ""
}

// agentSpan is one observed phase. A tool span keeps the first call that
// was live in it.
type agentSpan struct {
	start, end time.Time
	phase      agentPhase
	call       messages.ChatMessageToolCall
}

// agentTimelineSpans bounds one member's history; the oldest spans go first.
const agentTimelineSpans = 2048

// agentTimeline is owned by the event loop through the root tab.
type agentTimeline struct {
	spans map[string][]agentSpan
	open  map[string]bool
	// saved are the spans read from member transcripts, by start.
	saved   map[string][]agentSpan
	cursors map[string]agentTranscriptCursor
	reading bool
	readAt  time.Time
	// revision changes when a span starts or saved spans land; a growing
	// span only moves its end, which painters read against the clock.
	revision uint64
}

// observe samples every member's phase. The classification is the row's own
// live word, so a lane never disagrees with the status beside it.
func (t *agentTimeline) observe(view swarm.DisplayView, activities map[string]swarm.LiveActivity, approvals map[string]bool, now time.Time) {
	if view == nil {
		return
	}
	s := view.DisplaySnapshot("")
	if s == nil {
		return
	}
	if t.spans == nil {
		t.spans, t.open = map[string][]agentSpan{}, map[string]bool{}
	}
	for id, m := range s.Members {
		phase, call := agentObservedPhase(view, m, activities[id], approvals[id])
		t.record(id, phase, call, now)
	}
	for id := range t.open {
		if s.Members[id] == nil {
			t.record(id, phaseNone, messages.ChatMessageToolCall{}, now)
		}
	}
}

func agentObservedPhase(view swarm.DisplayView, m *swarm.Member, a swarm.LiveActivity, approval bool) (agentPhase, messages.ChatMessageToolCall) {
	if approval {
		return phaseWaiting, messages.ChatMessageToolCall{}
	}
	switch agentLiveWord(view, m, a) {
	case "querying", "thinking", "retrying":
		return phaseQuery, messages.ChatMessageToolCall{}
	case "toolcall":
		var call messages.ChatMessageToolCall
		if len(a.Tools) > 0 {
			call = a.Tools[0].Call
		}
		return phaseTool, call
	case "parked":
		return phaseParked, messages.ChatMessageToolCall{}
	}
	return phaseNone, messages.ChatMessageToolCall{}
}

func (t *agentTimeline) record(id string, phase agentPhase, call messages.ChatMessageToolCall, now time.Time) {
	spans := t.spans[id]
	if t.open[id] && len(spans) > 0 {
		last := &spans[len(spans)-1]
		last.end = now
		if last.phase == phase && (phase != phaseTool || last.call.ID == call.ID) {
			return
		}
		t.open[id] = false
	}
	if phase == phaseNone {
		return
	}
	spans = append(spans, agentSpan{start: now, end: now, phase: phase, call: call})
	if len(spans) > agentTimelineSpans {
		spans = slices.Delete(spans, 0, len(spans)-agentTimelineSpans)
	}
	t.spans[id] = spans
	t.open[id] = true
	t.revision++
}

// agentSegment is a stretch of one lane: executions first, observed phases
// painted over them.
type agentSegment struct {
	from, to time.Time
	phase    agentPhase
}

// agentLanes is an immutable picture of every listed agent's lane at one
// paint, shared by the cell rows and the native placements.
type agentLanes struct {
	from, to time.Time
	// live reports the axis ends at the clock rather than the last finish.
	live     bool
	segments map[string][]agentSegment
	spans    map[string][]agentSpan
	done     map[string]bool
	cols     int
	native   bool
	scrub    bool
	cursor   time.Time
	baseDir  string
	revision uint64
}

// agentLaneOffset is where lanes start: under the name, after the caret.
const agentLaneOffset = 2

// agentLanesMinWidth is the narrowest list that draws lanes; narrower ones
// drop them, as they drop the run time.
const agentLanesMinWidth = 52

// agentRibbonRows is the scrub header's concurrency chart height in cells.
const agentRibbonRows = 2

// buildAgentLanes reads the executions of the listed members and the
// observed spans. It reports nil when nothing has started yet.
func buildAgentLanes(view swarm.DisplayView, t *agentTimeline, ids []string, done map[string]bool, now time.Time) *agentLanes {
	if view == nil {
		return nil
	}
	s := view.DisplaySnapshot("")
	if s == nil {
		return nil
	}
	lanes := &agentLanes{segments: map[string][]agentSegment{}, spans: map[string][]agentSpan{}, done: done, revision: t.revision}
	listed := map[string]bool{}
	for _, id := range ids {
		listed[id] = true
	}
	widen := func(from, to time.Time) {
		if lanes.from.IsZero() || from.Before(lanes.from) {
			lanes.from = from
		}
		if to.After(lanes.to) {
			lanes.to = to
		}
	}
	for _, e := range s.Executions {
		if e == nil || !listed[e.Member] || e.Started.IsZero() {
			continue
		}
		end := e.Ended
		if end.IsZero() {
			if !executionBusy(e) {
				continue
			}
			end, lanes.live = now, true
		}
		lanes.segments[e.Member] = append(lanes.segments[e.Member], agentSegment{e.Started, end, phaseRan})
		widen(e.Started, end)
	}
	for _, id := range ids {
		slices.SortFunc(lanes.segments[id], func(a, b agentSegment) int { return a.from.Compare(b.from) })
		spans, live := t.laneSpans(id, now)
		lanes.live = lanes.live || live
		for _, span := range spans {
			lanes.segments[id] = append(lanes.segments[id], agentSegment{span.start, span.end, span.phase})
			widen(span.start, span.end)
		}
		lanes.spans[id] = spans
	}
	if lanes.live {
		lanes.to = now
	}
	if !lanes.to.After(lanes.from) {
		return nil
	}
	return lanes
}

func (l *agentLanes) span() time.Duration { return l.to.Sub(l.from) }

// timeAt is the moment under a lane column's center.
func (l *agentLanes) timeAt(col int) time.Time {
	return l.from.Add(time.Duration((float64(col) + 0.5) / float64(max(1, l.cols)) * float64(l.span())))
}

func (l *agentLanes) colAt(t time.Time) int {
	col := int(float64(t.Sub(l.from)) / float64(max(1, l.span())) * float64(l.cols))
	return max(0, min(l.cols-1, col))
}

// cellPhase is the phase a lane cell shows: an approval wait or parking
// covering half of it, since those happen inside the tool and run spans
// around them, else the phase covering most of it, else "ran" when an
// execution covers it.
func (l *agentLanes) cellPhase(id string, col int) agentPhase {
	step := l.span() / time.Duration(max(1, l.cols))
	lo := l.from.Add(step * time.Duration(col))
	hi := lo.Add(step)
	var best agentPhase
	var bestOverlap time.Duration
	overlay := map[agentPhase]time.Duration{}
	ran := false
	for _, seg := range l.segments[id] {
		end, start := seg.to, seg.from
		if hi.Before(end) {
			end = hi
		}
		if lo.After(start) {
			start = lo
		}
		overlap := end.Sub(start)
		if overlap <= 0 {
			continue
		}
		if seg.phase == phaseRan {
			ran = true
			continue
		}
		if seg.phase == phaseWaiting || seg.phase == phaseParked {
			overlay[seg.phase] += overlap
		}
		if best == phaseNone || overlap > bestOverlap {
			best, bestOverlap = seg.phase, overlap
		}
	}
	for _, phase := range []agentPhase{phaseWaiting, phaseParked} {
		if overlay[phase]*2 >= step {
			return phase
		}
	}
	if best == phaseNone && ran {
		return phaseRan
	}
	return best
}

// laneRow is the cell lane: a heavy rule in each phase's color, a faint
// track where the agent was not running, and the scrub cursor.
func (l *agentLanes) laneRow(id string) string {
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", agentLaneOffset))
	cursor := -1
	if l.scrub {
		cursor = l.colAt(l.cursor)
	}
	modifier := ""
	if l.done[id] {
		modifier = "dim"
	}
	for col := 0; col < l.cols; col++ {
		if col == cursor {
			b.WriteString(style.Styled("│", "", ""))
			continue
		}
		switch phase := l.cellPhase(id, col); phase {
		case phaseNone:
			b.WriteString(style.Styled("─", "muted", "dim"))
		case phaseRan:
			b.WriteString(style.Styled("━", phase.role(), "dim"))
		default:
			b.WriteString(style.Styled("━", phase.role(), modifier))
		}
	}
	return b.String()
}

// axisLabel is an offset from the axis start, as m:ss or h:mm:ss.
func axisLabel(d time.Duration) string {
	d = max(0, d).Truncate(time.Second)
	if d >= time.Hour {
		return fmt.Sprintf("%d:%02d:%02d", int(d/time.Hour), int(d/time.Minute)%60, int(d/time.Second)%60)
	}
	return fmt.Sprintf("%d:%02d", int(d/time.Minute), int(d/time.Second)%60)
}

var axisSteps = []time.Duration{
	5 * time.Second, 10 * time.Second, 15 * time.Second, 30 * time.Second,
	time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 30 * time.Minute,
	time.Hour, 2 * time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour,
}

// axisRow labels the lanes at the first round step that leaves room
// between labels, and the axis end on the right: "now" while it is live,
// else its length. Scrubbing writes the cursor's time over the labels.
func (l *agentLanes) axisRow() string {
	cells := make([]rune, l.cols)
	roles := make([]string, l.cols)
	for i := range cells {
		cells[i] = ' '
	}
	put := func(col int, text, role string) {
		for i, r := range text {
			if c := col + i; c >= 0 && c < l.cols {
				cells[c], roles[c] = r, role
			}
		}
	}
	end := axisLabel(l.span())
	if l.live {
		end = "now"
	}
	endCol := l.cols - len(end)
	step := axisSteps[len(axisSteps)-1]
	for _, s := range axisSteps {
		if float64(s)/float64(l.span())*float64(l.cols) >= 9 {
			step = s
			break
		}
	}
	for d := time.Duration(0); d < l.span(); d += step {
		col := l.colAt(l.from.Add(d))
		label := axisLabel(d)
		if col+len(label)+1 > endCol {
			break
		}
		put(col, label, "muted")
	}
	put(endCol, end, "")
	if l.scrub {
		label := axisLabel(l.cursor.Sub(l.from))
		col := max(0, min(l.cols-len(label), l.colAt(l.cursor)-len(label)/2))
		put(col-1, " "+label+" ", "accent")
	}
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", agentLaneOffset))
	for i := 0; i < l.cols; {
		j := i
		for j < l.cols && roles[j] == roles[i] {
			j++
		}
		b.WriteString(style.Styled(style.Escape(string(cells[i:j])), roles[i], ""))
		i = j
	}
	return b.String()
}

// concurrency counts the agents in each phase at a moment.
func (l *agentLanes) concurrency(t time.Time) map[agentPhase]int {
	counts := map[agentPhase]int{}
	for _, segments := range l.segments {
		var phase agentPhase
		for _, seg := range segments {
			if t.Before(seg.from) || !t.Before(seg.to) {
				continue
			}
			if seg.phase != phaseRan || phase == phaseNone {
				phase = seg.phase
			}
		}
		if phase != phaseNone {
			counts[phase]++
		}
	}
	return counts
}

var ribbonOrder = []agentPhase{phaseTool, phaseQuery, phaseRan, phaseParked, phaseWaiting}

// ribbonRow is the cell fallback for the concurrency chart: one block per
// column, as tall as the agents at work, in the color of most of them.
func (l *agentLanes) ribbonRow() string {
	const blocks = "▁▂▃▄▅▆▇█"
	totals := make([]int, l.cols)
	dominant := make([]agentPhase, l.cols)
	peak := 1
	for col := range l.cols {
		counts := l.concurrency(l.timeAt(col))
		most := 0
		for _, phase := range ribbonOrder {
			totals[col] += counts[phase]
			if counts[phase] > most {
				most, dominant[col] = counts[phase], phase
			}
		}
		peak = max(peak, totals[col])
	}
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", agentLaneOffset))
	cursor := -1
	if l.scrub {
		cursor = l.colAt(l.cursor)
	}
	for col, total := range totals {
		switch {
		case col == cursor:
			b.WriteString(style.Styled("│", "", ""))
		case total == 0:
			b.WriteString(" ")
		default:
			level := min(7, (total*8-1)/peak)
			modifier := ""
			if dominant[col] == phaseRan {
				modifier = "dim"
			}
			b.WriteString(style.Styled(string([]rune(blocks)[level]), dominant[col].role(), modifier))
		}
	}
	return b.String()
}

// scrubRow says what an agent was doing under the cursor: its observed
// phase and call, or where the cursor sits relative to its executions.
func (l *agentLanes) scrubRow(id string, width int) string {
	room := width - len(agentDetailIndent)
	if room < 2 {
		return ""
	}
	at := l.cursor
	for _, span := range slices.Backward(l.spans[id]) {
		if at.Before(span.start) || at.After(span.end) {
			continue
		}
		label := span.phase.word()
		if span.phase == phaseTool && span.call.Name != "" {
			var row toolDisclosureRow
			row.setCall(span.call)
			label = (&replModel{toolBaseDir: l.baseDir}).latestToolLabel(row, max(8, room-12))
		}
		label = style.StripImageMarkers(style.SanitizeImageText(label))
		meta := "  " + axisLabel(at.Sub(span.start)) + " in"
		label = rw.Truncate(label, max(1, room-2-rw.StringWidth(meta)), "…")
		return agentDetailIndent + style.Styled("■", span.phase.role(), "") + " " + style.Styled(style.Escape(label), "", "") + style.Styled(meta, "muted", "")
	}
	var first, last time.Time
	running := false
	for _, seg := range l.segments[id] {
		if seg.phase != phaseRan {
			continue
		}
		if first.IsZero() || seg.from.Before(first) {
			first = seg.from
		}
		if seg.to.After(last) {
			last = seg.to
		}
		running = running || !at.Before(seg.from) && !at.After(seg.to)
	}
	text := ""
	switch {
	case first.IsZero():
		text = "not started"
	case at.Before(first):
		text = "starts at " + axisLabel(first.Sub(l.from))
	case running:
		text = "running · phase not recorded"
	case at.After(last) && l.done[id]:
		text = "finished at " + axisLabel(last.Sub(l.from))
	default:
		text = "idle"
	}
	return agentDetailIndent + style.Styled(style.Escape(rw.Truncate(text, room, "…")), "muted", "")
}

// moveCursor steps the scrub cursor by whole lane columns, clamped to the
// axis.
func (l *agentLanes) moveCursor(cursor time.Time, cols int) time.Time {
	step := l.span() / time.Duration(max(1, l.cols))
	next := cursor.Add(step * time.Duration(cols))
	if next.Before(l.from) {
		return l.from
	}
	if next.After(l.to) {
		return l.to
	}
	return next
}

// Native lanes.

type agentLaneRow struct {
	row int
	id  string // "" is the scrub ribbon
}

// timelineColor resolves a theme role the way the context chart does.
func timelineColor(role string) color.RGBA {
	fg, _ := screenshotSurface()
	c := chromeColor(role)
	if role == "" || !c.Valid() {
		c = fg
	}
	return contextChartColor(c)
}

func newTimelineCanvas(width, height int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, max(0, width), max(0, height)))
	// Kitty composites alpha onto the real background; Sixel lanes are only
	// drawn when the theme supplies an opaque surface (agentLanesNative).
	if _, bg := style.Surface(); bg.Valid() {
		draw.Draw(img, img.Bounds(), image.NewUniform(contextChartColor(bg)), image.Point{}, draw.Src)
	}
	return img
}

func fillTimelineRect(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA, alpha float64) {
	for y := max(0, y0); y < min(img.Bounds().Dy(), y1); y++ {
		for x := max(0, x0); x < min(img.Bounds().Dx(), x1); x++ {
			contextChartBlend(img, x, y, c, alpha)
		}
	}
}

func (l *agentLanes) pixelX(t time.Time, width int) int {
	return int(float64(t.Sub(l.from)) / float64(max(1, l.span())) * float64(width))
}

// renderLane draws one agent's lane: a thin track, executions as a faint bar
// and observed phases over them, with a hairline gap where phases change.
func (l *agentLanes) renderLane(id string, width, height int) image.Image {
	img := newTimelineCanvas(width, height)
	if width <= 0 || height <= 0 {
		return img
	}
	bar := max(3, height/4)
	top := (height - bar) / 2
	fillTimelineRect(img, 0, top+bar/2, width, top+bar/2+1, timelineColor("muted"), 0.14)
	strength := 1.0
	if l.done[id] {
		strength = 0.5
	}
	for _, seg := range l.segments[id] {
		x0, x1 := l.pixelX(seg.from, width), l.pixelX(seg.to, width)
		x1 = max(x1, x0+1)
		alpha := strength
		if seg.phase == phaseRan {
			alpha *= 0.35
		} else if x1-x0 > 3 {
			x1--
		}
		fillTimelineRect(img, x0, top, x1, top+bar, timelineColor(seg.phase.role()), alpha)
	}
	if l.scrub {
		x := l.pixelX(l.cursor, width)
		fillTimelineRect(img, x-1, 0, x+1, height, timelineColor(""), 0.9)
	}
	return img
}

// renderRibbon stacks the agents at work in each phase, one sample per pixel
// column.
func (l *agentLanes) renderRibbon(width, height int) image.Image {
	img := newTimelineCanvas(width, height)
	if width <= 0 || height <= 0 {
		return img
	}
	samples := make([]map[agentPhase]int, width)
	peak := 1
	for x := range width {
		samples[x] = l.concurrency(l.from.Add(time.Duration((float64(x) + 0.5) / float64(width) * float64(l.span()))))
		total := 0
		for _, n := range samples[x] {
			total += n
		}
		peak = max(peak, total)
	}
	unit := float64(height-2) / float64(peak)
	for i := 0; i <= peak; i++ {
		y := height - 1 - int(float64(i)*unit)
		fillTimelineRect(img, 0, y, width, y+1, timelineColor("muted"), 0.14)
	}
	for x, counts := range samples {
		level := 0
		for _, phase := range ribbonOrder {
			n := counts[phase]
			if n == 0 {
				continue
			}
			y1 := height - 1 - int(float64(level)*unit)
			y0 := height - 1 - int(float64(level+n)*unit)
			alpha := 0.85
			if phase == phaseRan {
				alpha = 0.35
			}
			fillTimelineRect(img, x, y0, x+1, y1, timelineColor(phase.role()), alpha)
			level += n
		}
	}
	if l.scrub {
		x := l.pixelX(l.cursor, width)
		fillTimelineRect(img, x-1, 0, x+1, height, timelineColor(""), 0.9)
	}
	return img
}

// key identifies a picture of the lanes; any change repaints every image.
func (l *agentLanes) key(cellWidth, cellHeight int) string {
	cursor := int64(0)
	if l.scrub {
		cursor = l.cursor.UnixMilli()
	}
	return fmt.Sprintf("%d:%d:%d:%d:%d:%d:%d:%d", l.from.UnixMilli(), l.to.Unix(), cursor, l.cols, cellWidth, cellHeight, l.revision, style.Epoch())
}

// agentLaneImages caches the rendered lanes of one agents list.
type agentLaneImages struct {
	key    string
	images map[string]image.Image
}

// placements projects the visible lane rows into native images at the
// pane's screen position.
func (c *agentLaneImages) placements(l *agentLanes, rows []agentLaneRow, viewport transcriptViewport, x, cellWidth, cellHeight int) []termimg.Placement {
	if l == nil || !l.native || cellWidth <= 0 || cellHeight <= 0 {
		return nil
	}
	key := l.key(cellWidth, cellHeight)
	if c.key != key {
		c.key, c.images = key, map[string]image.Image{}
	}
	var out []termimg.Placement
	for _, row := range rows {
		height := 1
		if row.id == "" {
			height = agentRibbonRows
		}
		if !viewport.contains(row.row) || !viewport.contains(row.row+height-1) {
			continue
		}
		img := c.images[row.id]
		if img == nil {
			if row.id == "" {
				img = l.renderRibbon(l.cols*cellWidth, height*cellHeight)
			} else {
				img = l.renderLane(row.id, l.cols*cellWidth, cellHeight)
			}
			c.images[row.id] = img
		}
		out = append(out, termimg.Placement{
			Key: "agent-lane:" + row.id, Pixels: img, PixelVersion: key,
			X: x + agentLaneOffset, Y: viewport.screenY(row.row), Cols: l.cols, Rows: height,
		})
	}
	return out
}

// agentLanesNative reports whether lanes draw as images. Kitty composites
// their alpha; Sixel needs the theme's opaque surface, as the context chart
// does.
func (r *managedREPL) agentLanesNative(g viewGeometry) bool {
	if r.images == nil || !g.nativeImages {
		return false
	}
	_, bg := style.Surface()
	return r.images.Protocol() != termimg.ProtocolSixel || bg.Valid()
}

// agentsList is the focused-or-not agents list on screen, if any.
func (r *managedREPL) agentsList() *agentsInspectorState {
	i := &r.workspace().inspector
	if !i.open || i.target.kind != agentsViewKind {
		return nil
	}
	return r.workspace().viewState(i.target).agents
}

// toggleAgentTimeline is Ctrl-T on the agents list: scrubbing starts at the
// axis end and stops where it is. Caller holds r.model.mu. Never quits.
func (r *managedREPL) toggleAgentTimeline(keyContext) bool {
	r.toggleAgentScrub()
	return false
}

func (r *managedREPL) toggleAgentScrub() {
	list := r.agentsList()
	if list == nil || list.lanes == nil {
		return
	}
	list.scrub = !list.scrub
	list.cursor = list.lanes.to
	r.inspectorRefreshAt = time.Time{}
}

// scrubAgents moves the cursor a lane column per Left or Right.
func (r *managedREPL) scrubAgents(key string) bool {
	list := r.agentsList()
	if list == nil || !list.scrub || list.lanes == nil {
		return false
	}
	switch key {
	case "<Left>":
		list.cursor = list.lanes.moveCursor(list.cursor, -1)
	case "<Right>":
		list.cursor = list.lanes.moveCursor(list.cursor, 1)
	default:
		return false
	}
	r.inspectorRefreshAt = time.Time{}
	return true
}
