package main

import (
	"context"
	"fmt"
	"image"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
	"github.com/alexschlessinger/pollytool/llm"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

const contextRefreshInterval = time.Second
const contextAnimationDuration = 450 * time.Millisecond
const contextChartRows = 3

type contextSnapshotResult struct {
	snapshot contextSnapshot
	err      error
}

// contextPopover belongs to one open inspector. Only the event loop changes it;
// background reads return immutable snapshots, never mutate the conversation,
// and are canceled when the popover closes.
type contextPopover struct {
	snapshot contextSnapshot
	err      error
	cancel   context.CancelFunc
	ctx      context.Context
	pending  chan contextSnapshotResult
	nextRead time.Time

	budget       *contextBudgetDetails
	compactModel string
	used         int
	estimated    bool
	from         float64
	animatedAt   time.Time
	chartAt      time.Time
	now          time.Time
	native       bool
	chartRow     int
	chartCols    int
	chartKey     string
	chart        image.Image
}

func (r *managedREPL) openContextPopover() {
	m := &replModal{title: "Context", width: 50, details: []string{"no active session"}}
	if r.state != nil && r.state.session != nil {
		now := time.Now()
		ctx, cancel := context.WithCancel(r.state.session.Context())
		d := &contextPopover{ctx: ctx, cancel: cancel, now: now, animatedAt: now, chartAt: now, nextRead: now.Add(contextRefreshInterval)}
		m.context = d
		// The initial read retains the inspector's immediate snapshot. Subsequent
		// reads run off-thread and are throttled independently of animation.
		history, err := r.state.session.GetHistory(ctx)
		if err == nil {
			d.snapshot, err = collectContextSnapshot(ctx, r.config, r.state, history)
		}
		d.err = err
		d.updateStatus(&r.model.status, r.state, now)
		d.updateDetails(m)
	}
	r.openModal(m)
}

func (d *contextPopover) wantsTick(now time.Time) bool {
	return len(d.pending) > 0 || now.Sub(d.animatedAt) < contextAnimationDuration || now.Sub(d.chartAt) < contextAnimationDuration || d.pending == nil && !now.Before(d.nextRead)
}

func (r *managedREPL) refreshContextPopover(now time.Time) {
	m := r.model.modal
	if m == nil || m.context == nil {
		return
	}
	d := m.context
	d.now = now
	// Sixel cannot composite the chart's partial alpha onto an unknown
	// terminal background. Keep the Unicode chart until the theme supplies
	// an opaque surface; Kitty and render surfaces preserve the alpha.
	_, bg := style.Surface()
	d.native = r.images != nil && (r.images.Protocol() != termimg.ProtocolSixel || bg.Valid())
	d.updateStatus(&r.model.status, r.state, now)
	if d.pending != nil {
		select {
		case result := <-d.pending:
			d.pending = nil
			d.nextRead = now.Add(contextRefreshInterval)
			d.err = result.err
			if result.err == nil {
				if !slices.Equal(d.snapshot.trend, result.snapshot.trend) {
					d.chartAt = now
				}
				d.snapshot = result.snapshot
			}
		default:
		}
	}
	if d.pending == nil && !now.Before(d.nextRead) {
		// Copy only the read-only inputs used by collectContextSnapshot, not the
		// conversation's mutexes, atomics, or mutable settings container.
		s := r.state
		view := &conversationState{
			session: s.session, settings: s.settings, agent: s.agent,
			toolRegistry: s.toolRegistry, skillCatalog: s.skillCatalog,
			displayContract: s.displayContract, sandboxProfile: s.sandboxProfile,
		}
		config := &Config{}
		if r.config != nil {
			config.SchemaPath = r.config.SchemaPath
		}
		result := make(chan contextSnapshotResult, 1)
		d.pending = result
		ctx := d.ctx
		go func() {
			history, err := view.session.GetHistory(ctx)
			var snapshot contextSnapshot
			if err == nil {
				snapshot, err = collectContextSnapshot(ctx, config, view, history)
			}
			result <- contextSnapshotResult{snapshot: snapshot, err: err}
		}()
	}
	d.updateDetails(m)
}

func (d *contextPopover) updateStatus(status *sessionStatus, state *conversationState, now time.Time) {
	if d.used != status.contextUsed {
		d.from = d.displayedUsage(now)
		d.used = status.contextUsed
		d.animatedAt = now
	}
	d.estimated = status.contextEstimated
	d.budget = status.contextBudget
	if d.budget == nil {
		d.budget = state.currentBudget(&state.settings)
	}
	if d.budget == nil && status.contextLimit > 0 {
		d.budget = &contextBudgetDetails{input: status.contextLimit}
	}
	d.compactModel = state.settings.CompactModel
}

func contextProgress(now, started time.Time) float64 {
	x := min(1, max(0, float64(now.Sub(started))/float64(contextAnimationDuration)))
	return 1 - (1-x)*(1-x)*(1-x)
}

func (d *contextPopover) displayedUsage(now time.Time) float64 {
	return d.from + (float64(d.used)-d.from)*contextProgress(now, d.animatedAt)
}

// Keep a plain-text description alongside the visual items. It also gives the
// read-only modal its usual navigation, anchoring and dismissal semantics.
func (d *contextPopover) updateDetails(m *replModal) {
	m.details = d.budget.details(d.compactModel)
	if d.used > 0 {
		size := humanizeTokens(d.used) + " (provider count)"
		if d.estimated {
			size = "~" + humanizeTokens(d.used) + " (estimate)"
		}
		m.details = append(m.details, "request: "+size, "")
	}
	if d.err != nil {
		m.details = append(m.details, "message stats unavailable: "+d.err.Error())
	} else {
		m.details = append(m.details, d.snapshot.details()...)
	}
	m.titleNotice, m.titleNoticeRole = "", ""
	if d.err != nil {
		m.titleNotice, m.titleNoticeRole = "· unavailable", "active"
	}
}

func (d *contextPopover) items(modalWidth int) []replModalItem {
	width := max(1, modalWidth-4)
	var items []replModalItem
	add := func(text string) {
		plain := ui.CellsToString(style.ParseCells(text, ui.StyleClear))
		if rw.StringWidth(plain) <= width {
			items = append(items, replModalItem{label: " " + plain, display: " " + text})
			return
		}
		// On very narrow terminals, wrap rather than lose diagnostic values.
		for _, row := range style.VisualRows(style.Escape(plain), ui.StyleClear, width) {
			line := ui.CellsToString(row)
			items = append(items, replModalItem{label: " " + line, display: " " + style.Styled(line, "muted", "")})
		}
	}
	muted := func(text string) string { return style.Styled(text, "muted", "") }
	pair := func(left, right string) string {
		return left + strings.Repeat(" ", max(1, width-style.TextWidth(left)-style.TextWidth(right))) + right
	}
	limit := 0
	if d.budget != nil {
		limit = d.budget.input
	}
	used, source := humanizeTokens(d.used), "provider count"
	if d.estimated {
		used, source = "~"+used, "estimate"
	} else if d.used == 0 {
		used, source = "—", "not measured"
	}
	capacity, percent := "unknown budget", ""
	if d.budget != nil && limit <= 0 {
		capacity = "unlimited input"
	} else if limit > 0 {
		capacity = humanizeTokens(limit) + " input"
		if d.used > 0 {
			percent = fmt.Sprintf("%.0f%%", 100*float64(d.used)/float64(limit))
		}
	}
	add(pair(style.Styled(used, "accent", "bold")+muted(" / "+capacity), style.Styled(percent, contextUsageColor(d.used, limit), "bold")))
	if limit > 0 {
		add(contextGauge(d.displayedUsage(d.now)/float64(limit), width, contextUsageColor(d.used, limit)))
		add(muted(pair("Compact at "+humanizeTokens(llm.CompactionPoint(limit)), source)))
	} else if d.budget != nil {
		add(muted("Compacts on provider rejection · " + source))
	} else {
		add(muted("Input budget unknown · " + source))
	}
	add("")
	if d.err != nil {
		d.chartRow = -1
		add(muted("Stats unavailable · " + d.err.Error()))
		add(muted("Retrying; no snapshot data shown"))
		return items
	} else {
		total := 0
		for _, role := range d.snapshot.roles {
			total += role.tokens
		}
		for _, role := range d.snapshot.roles {
			name := map[string]string{"system": "System", "user": "You", "assistant": "Assistant", "tool": "Tools"}[role.role]
			left := fmt.Sprintf("%-9s %3d", name, role.count)
			right := fmt.Sprintf("~%s", humanizeTokens(role.tokens))
			barWidth := width - 23
			if barWidth >= 4 {
				fraction := float64(role.tokens) / float64(max(1, total))
				bar := contextBar(fraction, barWidth)
				add(muted(left) + "  " + contextBarStyle(bar, "accent") + strings.Repeat(" ", max(1, width-rw.StringWidth(left)-2-barWidth-rw.StringWidth(right))) + muted(right))
			} else {
				add(muted(pair(fmt.Sprintf("%s %d", name, role.count), right)))
			}
		}
	}
	add("")
	peak, compacted := 0, false
	for _, point := range d.snapshot.trend {
		peak = max(peak, point.tokens)
		compacted = compacted || point.compacted
	}
	if n := len(d.snapshot.trend); n > 0 {
		heading := "Last request"
		if n > 1 {
			heading = fmt.Sprintf("Last %d requests", n)
		}
		add(muted(pair(heading, "peak "+humanizeTokens(peak))))
	} else {
		add(muted("Requests"))
	}
	d.chartRow, d.chartCols = len(items), width
	if len(d.snapshot.trend) == 0 {
		add(muted("No reported requests yet"))
		d.chartRow = -1
	} else {
		rows := contextSparkline(d.snapshot.trend, width, contextChartRows, contextProgress(d.now, d.chartAt))
		for _, row := range rows {
			if d.native {
				add(strings.Repeat(" ", width))
			} else {
				add(style.Styled(row, "accent", "dim"))
			}
		}
		if compacted {
			marker := "↓"
			if d.native {
				marker = "┆"
			}
			add(muted(marker + " compaction"))
		}
	}
	add("")
	value := func(text string) string { return style.Styled(text, "", "bold") }
	cacheLabel, cacheValue := muted("Cache hit"), muted("—")
	if d.snapshot.cacheKnown {
		cache := fmt.Sprintf("%.0f%%", 100*d.snapshot.cacheFraction)
		cacheValue = value(cache)
		if barWidth := width - len("Cache hit") - len(cache) - 4; barWidth >= 4 {
			filled := int(math.Round(d.snapshot.cacheFraction * float64(barWidth)))
			cacheLabel += "  " + style.Styled(strings.Repeat("━", filled), "accent", "") + style.Styled(strings.Repeat("─", barWidth-filled), "muted", "dim")
		}
	}
	add(pair(cacheLabel, cacheValue))
	if n := d.snapshot.summaries + d.snapshot.clears; n > 0 {
		add(value(fmt.Sprint(n)) + muted(" "+pluralWord(n, "compaction", "compactions")))
	}
	if d.budget != nil {
		window, output := "—", "default"
		if d.budget.window > 0 {
			window = humanizeTokens(d.budget.window)
		}
		if d.budget.response > 0 {
			output = humanizeTokens(d.budget.response)
		}
		add(pair(muted("Window ")+value(window), muted("Output ")+value(output)))
		if d.budget.learned {
			add(muted("Provider-limited input"))
		}
	}
	if d.compactModel != "" {
		add(muted("Summary model ") + value(llm.ModelName(d.compactModel)))
	}
	add(pair(value(fmt.Sprint(d.snapshot.storedMessages))+muted(" "+pluralWord(d.snapshot.storedMessages, "message", "messages")), value("~"+humanizeTokens(d.snapshot.storedTokens))+muted(" stored")))
	return items
}

// contextBar uses eighth-cell precision, shared by the capacity and mix bars.
func contextBar(fraction float64, width int) string {
	units := int(math.Round(min(1, max(0, fraction)) * float64(width*8)))
	full, part := units/8, units%8
	bar := strings.Repeat("█", full)
	if part > 0 {
		bar += string([]rune(" ▏▎▍▌▋▊▉")[part])
	}
	return bar + strings.Repeat("·", max(0, width-rw.StringWidth(bar)))
}

func contextBarStyle(bar, role string) string {
	filled := strings.TrimRight(bar, "·")
	return style.Styled(filled, role, "") + style.Styled(bar[len(filled):], "muted", "dim")
}

func contextGauge(fraction float64, width int, role string) string {
	bar := []rune(contextBar(fraction, width))
	marker := min(width-1, max(0, llm.CompactionPoint(width)))
	return contextBarStyle(string(bar[:marker]), role) + style.Styled("│", "muted", "") + contextBarStyle(string(bar[marker+1:]), role)
}

func contextSparkline(points []contextTrendPoint, width, height int, progress float64) []string {
	rows := make([][]rune, height)
	for y := range rows {
		rows[y] = []rune(strings.Repeat(" ", width))
	}
	peak := 1
	for _, point := range points {
		peak = max(peak, point.tokens)
	}
	if len(points) > 0 {
		for x := 0; x < width && float64(x) <= float64(width-1)*progress; x++ {
			if len(points) == 1 {
				// A single reported request is a point, not a flat history.
				if progress > 0 {
					rows[0][width-1] = '•'
				}
				break
			}
			position := float64(x) * float64(len(points)-1) / float64(max(1, width-1))
			i := int(position)
			value := float64(points[i].tokens)
			if i+1 < len(points) {
				value += (float64(points[i+1].tokens) - value) * (position - float64(i))
			}
			level := min(height*8, max(1, int(math.Round(value/float64(peak)*float64(height*8)))))
			for y := height - 1; y >= 0; y-- {
				fill := min(8, max(0, level-(height-1-y)*8))
				rows[y][x] = []rune(" ▁▂▃▄▅▆▇█")[fill]
			}
		}
		for i, point := range points {
			if point.compacted {
				x := i * (width - 1) / max(1, len(points)-1)
				if len(points) == 1 {
					x = width - 1
				}
				if float64(x) <= float64(width-1)*progress {
					rows[0][x] = '↓'
				}
			}
		}
	}
	out := make([]string, height)
	for y := range rows {
		out[y] = string(rows[y])
	}
	return out
}

func (d *contextPopover) placement(m *replModal, rect image.Rectangle, cellWidth, cellHeight int) []termimg.Placement {
	if !d.native || d.chartRow < 0 || cellWidth <= 0 || cellHeight <= 0 {
		return nil
	}
	x, y := rect.Min.X+2, rect.Min.Y+1+d.chartRow-m.top
	visible := image.Rect(x, y, x+d.chartCols, y+contextChartRows).Intersect(rect.Inset(1)).Intersect(image.Rect(rect.Min.X+1, rect.Min.Y+1, rect.Max.X-1, rect.Min.Y+1+m.visible))
	if visible.Empty() {
		return nil
	}
	width, height := d.chartCols*cellWidth, contextChartRows*cellHeight
	frame := int(math.Round(contextProgress(d.now, d.chartAt) * 10))
	key := fmt.Sprintf("%d:%d:%d:%d:%d", d.chartAt.UnixNano(), width, height, style.Epoch(), frame)
	if key != d.chartKey {
		values, compacted := make([]int, len(d.snapshot.trend)), make([]bool, len(d.snapshot.trend))
		for i, point := range d.snapshot.trend {
			values[i], compacted[i] = point.tokens, point.compacted
		}
		d.chart = renderContextChart(values, compacted, width, height, float64(frame)/10)
		d.chartKey = key
	}
	return []termimg.Placement{{
		Key: "context-history", Pixels: d.chart, PixelVersion: key,
		X: x, Y: y, Cols: d.chartCols, Rows: contextChartRows,
		Clip: termimg.Clip{X: visible.Min.X - x, Y: visible.Min.Y - y, Cols: visible.Dx(), Rows: visible.Dy()},
	}}
}
