package main

import (
	"errors"
	"image"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
	"github.com/alexschlessinger/pollytool/messages"
	tcell "github.com/gdamore/tcell/v3"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

func contextDashboardREPL(t *testing.T, native bool) *managedREPL {
	t.Helper()
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	screen.SetSize(100, 40)
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "dashboard")
	first, second := cacheMessage(60_000, 30_000, true), cacheMessage(24_000, 20_000, true)
	first.Content, second.Content = "first answer", "second answer"
	testAddMessages(t, session, []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "question"}, first,
		(messages.Compaction{Summary: "summary"}).Message(), second,
	})
	r.state = &conversationState{session: session, settings: Settings{SystemPrompt: "test persona"}}
	r.model.status.recordContextUsage(24_000, 100_000)
	r.model.status.contextBudget = &contextBudgetDetails{window: 110_000, input: 100_000, response: 10_000}
	if native {
		r.images = termimg.NewRenderManager(screen, 100, 40, 8, 16)
		t.Cleanup(r.images.Shutdown)
	}
	r.openContextPopover()
	t.Cleanup(func() {
		if r.model.modal != nil {
			r.closeModal()
		}
	})
	r.render()
	return r
}

func TestContextDashboardPresentationAndFallback(t *testing.T) {
	r := contextDashboardREPL(t, false)
	d := r.model.modal.context
	d.animatedAt, d.chartAt = time.Now().Add(-time.Second), time.Now().Add(-time.Second)
	r.render()
	got := plainStyledText(r.modalW.Text)
	for _, want := range []string{"24.0k / 100k input", "24%", "Compact at 90.0k", "Assistant", "Last 2 requests", "peak 60.0k", "↓ compaction", "Session usage", "↑ input", "↓ output", "Parent", "Subagents", "Total", "84.0k", "Cache hit", "60%", "1 compaction", "Window 110k", "Output 10.0k", "4 messages", "stored"} {
		if !strings.Contains(got, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, got)
		}
	}
	if r.model.modal.titleNotice != "" {
		t.Fatalf("dashboard retained title notice: %q", r.model.modal.titleNotice)
	}
	for _, unwanted := range []string{"MESSAGE MIX", "estimated tokens", "provider count", "Cumulative tokens", "recorded requests"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("dashboard retained redundant label %q:\n%s", unwanted, got)
		}
	}
	for _, line := range strings.Split(got, "\n") {
		for _, role := range []string{"You", "Assistant", "Tools", "System"} {
			if strings.Contains(line, role) && !strings.Contains(line, "~") {
				t.Fatalf("role estimate lost its marker: %q", line)
			}
		}
	}
	if !strings.ContainsAny(got, "▁▂▃▄▅▆▇█") || !strings.Contains(got, "│") {
		t.Fatalf("missing fallback chart or compaction gauge marker:\n%s", got)
	}
	if r.model.modal.bounds.Max != image.Pt(100, 39) || r.model.modal.bounds.Dx() != 50 {
		t.Fatalf("dashboard no longer compact and anchored: %v", r.model.modal.bounds)
	}
	if d.wantsTick(time.Now()) {
		t.Fatal("settled dashboard requested continuous animation")
	}
}

func TestContextDashboardRefreshAndAnimation(t *testing.T) {
	r := contextDashboardREPL(t, false)
	m, now := r.model.modal, time.Now()
	d := m.context
	d.animatedAt = now.Add(-time.Second)
	d.chartAt = d.animatedAt
	r.model.status.recordContextUsage(50_000, 100_000)
	r.refreshContextPopover(now)
	if d.displayedUsage(now) != 24_000 || d.displayedUsage(now.Add(contextAnimationDuration)) != 50_000 {
		t.Fatalf("usage transition = %v -> %v", d.displayedUsage(now), d.displayedUsage(now.Add(contextAnimationDuration)))
	}
	if !d.wantsTick(now) {
		t.Fatal("changed data did not request animation")
	}
	testAddMessages(t, r.state.session, []messages.ChatMessage{cacheMessage(50_000, 10_000, true)})
	d.nextRead = now.Add(-time.Second)
	r.refreshContextPopover(now)
	if d.pending == nil {
		t.Fatal("live refresh did not start")
	}
	select {
	case result := <-d.pending:
		d.pending <- result
	case <-time.After(5 * time.Second):
		t.Fatal("live snapshot did not finish")
	}
	r.refreshContextPopover(now.Add(10 * time.Millisecond))
	if len(d.snapshot.trend) != 3 || d.snapshot.trend[2].tokens != 50_000 || d.snapshot.storedMessages != 5 {
		t.Fatalf("live snapshot = %+v", d.snapshot)
	}
	if !strings.Contains(strings.Join(m.details, "\n"), "request: 50.0k") {
		t.Fatalf("request readout did not refresh: %v", m.details)
	}
	r.closeModal()
	select {
	case <-d.ctx.Done():
	default:
		t.Fatal("closing dashboard did not cancel its reads")
	}
}

func TestContextDashboardReplacementCancelsReads(t *testing.T) {
	r := contextDashboardREPL(t, false)
	d := r.model.modal.context
	r.openModal(&replModal{title: "Approval"})
	select {
	case <-d.ctx.Done():
	default:
		t.Fatal("replacing dashboard left its reader alive")
	}
	if r.model.modal.title != "Approval" || r.model.pendingModal != nil {
		t.Fatal("read-only dashboard displaced an approval")
	}
}

func TestContextDashboardNativeChartClipsAndClears(t *testing.T) {
	r := contextDashboardREPL(t, true)
	m := r.model.modal
	d := m.context
	if overlays := r.images.Placements(); len(overlays) != 1 || !overlays[0].Visible.In(m.bounds) {
		t.Fatalf("native chart missing or escaped modal: %+v", overlays)
	}
	d.chartAt = time.Now().Add(-time.Second)
	r.render()
	first := d.chart
	r.render()
	if d.chart != first {
		t.Fatal("unchanged frame regenerated chart pixels")
	}
	// Clip the first chart row by scrolling it above the content viewport.
	m.top, m.visible = d.chartRow+1, 2
	placements := d.placement(m, image.Rect(10, 10, 60, 16), 8, 16)
	if len(placements) != 1 || placements[0].Clip.Y != 1 || placements[0].Clip.Rows != 2 {
		t.Fatalf("partially scrolled chart = %+v", placements)
	}
	m.top, m.visible = d.chartRow, 1
	if got := d.placement(m, image.Rect(0, 0, 50, 2), 8, 16); len(got) != 0 {
		t.Fatalf("chart painted over a border on a two-row terminal: %+v", got)
	}
	m.top = d.chartRow + contextChartRows
	if got := d.placement(m, image.Rect(10, 10, 60, 16), 8, 16); len(got) != 0 {
		t.Fatalf("offscreen chart retained a placement: %+v", got)
	}
	r.closeModal()
	r.render()
	if got := r.images.Placements(); len(got) != 0 {
		t.Fatalf("closed dashboard retained native images: %+v", got)
	}
}

func TestContextDashboardChartProtocolAndBackground(t *testing.T) {
	for _, protocol := range []termimg.Protocol{termimg.ProtocolSixel, termimg.ProtocolKitty, termimg.ProtocolRender} {
		t.Run(protocol.String(), func(t *testing.T) {
			restoreStyleEpochTestTheme(t)
			r := contextDashboardREPL(t, false)
			tty := &imageTestTTY{window: tcell.WindowSize{Width: 100, Height: 40, PixelWidth: 800, PixelHeight: 640}}
			r.images = termimg.NewManagerFor(ui.DefaultBackend.Screen, tty, protocol)
			t.Cleanup(r.images.Shutdown)
			m := r.model.modal
			d := m.context
			d.animatedAt, d.chartAt = time.Now().Add(-time.Second), time.Now().Add(-time.Second)
			d.nextRead = time.Now().Add(time.Hour)

			// Exercise the real popup across theme changes, including removing an
			// already placed Sixel chart when its background becomes unresolved.
			for _, background := range []string{"inherit", "#fdf6e3", "inherit"} {
				r.applyStyleEpoch(style.Apply(style.Theme{Name: "light", Colors: map[string]string{
					"foreground": "#303030", "background": background, "muted": "#808080", "accent": "#268bd2",
				}}))
				tty.Reset()
				r.render()
				wantNative := protocol != termimg.ProtocolSixel || background != "inherit"
				if d.native != wantNative {
					t.Fatalf("background %s: native = %t, want %t", background, d.native, wantNative)
				}
				rows := strings.Split(plainStyledText(r.modalW.Text), "\n")
				chartStart := d.chartRow - m.top
				if chartStart < 0 || chartStart+contextChartRows > len(rows) {
					t.Fatalf("chart rows outside popup: start=%d rows=%d", chartStart, len(rows))
				}
				chartRows := rows[chartStart : chartStart+contextChartRows]
				overlays := r.images.Placements()
				if !wantNative {
					if len(overlays) != 0 || r.images.ActiveCount() != 0 || tty.Len() != 0 {
						t.Fatalf("inherited Sixel chart reached the image backend: overlays=%d active=%d bytes=%d", len(overlays), r.images.ActiveCount(), tty.Len())
					}
					for y, want := range contextSparkline(d.snapshot.trend, d.chartCols, contextChartRows, 1) {
						if strings.TrimSpace(chartRows[y]) != strings.TrimSpace(want) {
							t.Fatalf("fallback row %d = %q, want %q", y, chartRows[y], want)
						}
					}
					if !strings.Contains(plainStyledText(r.modalW.Text), "↓ compaction") {
						t.Fatal("Unicode fallback lost its compaction caption")
					}
					continue
				}
				if len(overlays) != 1 || !overlays[0].Visible.In(m.bounds) {
					t.Fatalf("background %s: native chart missing or outside popup: %+v", background, overlays)
				}
				if strings.TrimSpace(strings.Join(chartRows, "")) != "" {
					t.Fatalf("native chart retained Unicode rows: %q", chartRows)
				}
				if opaque := overlays[0].Image.(*image.RGBA).Opaque(); opaque != (background != "inherit") {
					t.Fatalf("background %s: chart opacity = %t; must preserve the theme's background choice", background, opaque)
				}
				if protocol == termimg.ProtocolSixel && !strings.Contains(tty.String(), "\x1bP") || protocol == termimg.ProtocolKitty && !strings.Contains(tty.String(), "\x1b_G") {
					t.Fatalf("native chart did not emit %s graphics", protocol)
				}
			}
		})
	}
}

func TestContextDashboardReadFailureDoesNotInventEmptyStatistics(t *testing.T) {
	for _, snapshot := range []contextSnapshot{{}, {storedMessages: 12, summaries: 3, trend: []contextTrendPoint{{tokens: 100}}}} {
		d := &contextPopover{snapshot: snapshot, err: errors.New("session read failed")}
		var lines []string
		for _, item := range d.items(50) {
			lines = append(lines, item.label)
		}
		text := strings.Join(lines, "\n")
		if !strings.Contains(text, "Stats unavailable") || d.chartRow != -1 {
			t.Fatalf("failure not exposed: %q", text)
		}
		for _, invalid := range []string{"No reported requests", "compactions", "stored", "Last request", "Cache hit"} {
			if strings.Contains(text, invalid) {
				t.Fatalf("failed read displayed invented or stale %q: %s", invalid, text)
			}
		}
	}
}

func TestContextDashboardCompactFooter(t *testing.T) {
	for _, tc := range []struct {
		name          string
		known         bool
		fraction      float64
		want          string
		filled, empty int
	}{
		{"unknown", false, 0, "—", 0, 0},
		{"zero", true, 0, "0%", 0, 31},
		{"partial", true, 0.75, "75%", 23, 7},
		{"full", true, 1, "100%", 29, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &contextPopover{
				budget: &contextBudgetDetails{window: 110_000, input: 100_000, response: 10_000},
				snapshot: contextSnapshot{cacheKnown: tc.known, cacheFraction: tc.fraction,
					storedMessages: 1, storedTokens: 2_000},
			}
			var footer []string
			for _, item := range d.items(50) {
				if strings.Contains(item.label, "Cache hit") || len(footer) > 0 {
					footer = append(footer, item.label)
				}
			}
			if len(footer) != 3 {
				t.Fatalf("default footer should have three rows: %q", footer)
			}
			if !strings.HasSuffix(footer[0], tc.want) || strings.Count(footer[0], "━") != tc.filled || strings.Count(footer[0], "─") != tc.empty {
				t.Fatalf("cache meter = %q, want %q with %d filled / %d empty cells", footer[0], tc.want, tc.filled, tc.empty)
			}
			if !strings.Contains(footer[1], "Window 110k") || !strings.Contains(footer[1], "Output 10.0k") || !strings.Contains(footer[2], "1 message") || !strings.Contains(footer[2], "~2.0k stored") {
				t.Fatalf("footer lost its limits or transcript estimate: %q", footer)
			}
		})
	}
}

func TestContextDashboardNondefaultDetails(t *testing.T) {
	d := &contextPopover{
		budget: &contextBudgetDetails{input: 100_000, learned: true}, compactModel: "openai/summarizer",
		snapshot: contextSnapshot{summaries: 2, clears: 1},
	}
	var lines []string
	for _, item := range d.items(50) {
		lines = append(lines, item.label)
	}
	text := strings.Join(lines, "\n")
	for _, want := range []string{"3 compactions", "Provider-limited input", "Summary model summarizer"} {
		if !strings.Contains(text, want) {
			t.Fatalf("nondefault detail %q missing:\n%s", want, text)
		}
	}
	m := &replModal{}
	d.err = errors.New("read failed")
	d.updateDetails(m)
	if !strings.Contains(m.titleNotice, "unavailable") {
		t.Fatalf("missing failure notice: %q", m.titleNotice)
	}
	d.err = nil
	d.updateDetails(m)
	if m.titleNotice != "" {
		t.Fatalf("recovered popup should have a plain title: %q", m.titleNotice)
	}
}

func TestContextDashboardHistoryCaption(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, compacted := range []bool{false, true} {
			d := &contextPopover{native: native, snapshot: contextSnapshot{trend: []contextTrendPoint{{tokens: 100, compacted: compacted}}}}
			var lines []string
			for _, item := range d.items(50) {
				lines = append(lines, item.label)
			}
			text := strings.Join(lines, "\n")
			marker := "↓"
			if native {
				marker = "┆"
			}
			if !strings.Contains(text, "Last request") || !strings.Contains(text, "peak 100") || strings.Contains(text, marker+" compaction") != compacted {
				t.Fatalf("native=%t compacted=%t: misleading history caption:\n%s", native, compacted, text)
			}
		}
	}
}

func TestContextSparklineSingleRequest(t *testing.T) {
	rows := contextSparkline([]contextTrendPoint{{tokens: 100}}, 20, 3, 1)
	if got := strings.TrimSpace(strings.Join(rows, "")); got != "•" {
		t.Fatalf("single request invented a stretch of history: %q", got)
	}
	rows = contextSparkline([]contextTrendPoint{{tokens: 100, compacted: true}}, 20, 3, 1)
	if rows[0] != strings.Repeat(" ", 19)+"↓" {
		t.Fatalf("single request lost compaction marker: %q", rows[0])
	}
}

func TestContextDashboardUnknownAndUnlimitedBudgets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget *contextBudgetDetails
		want   string
	}{
		{name: "unknown", want: "unknown budget"},
		{name: "unlimited", budget: &contextBudgetDetails{}, want: "unlimited input"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &contextPopover{budget: tc.budget, snapshot: contextSnapshot{cache: "unknown"}}
			var lines []string
			for _, item := range d.items(50) {
				lines = append(lines, item.label)
			}
			text := strings.Join(lines, "\n")
			if !strings.Contains(text, tc.want) || strings.Contains(text, "0%") || !strings.Contains(text, "No reported requests yet") {
				t.Fatalf("unknown data misrepresented:\n%s", text)
			}
		})
	}
}

func TestContextDashboardCountCaptions(t *testing.T) {
	for _, budget := range []*contextBudgetDetails{nil, {}, {input: 100_000}} {
		for _, tc := range []struct {
			used      int
			estimated bool
			want      string
		}{
			{used: 1000},
			{used: 1000, estimated: true, want: "estimate"},
			{want: "not measured"},
		} {
			d := &contextPopover{budget: budget, used: tc.used, estimated: tc.estimated}
			var lines []string
			for _, item := range d.items(50) {
				line := strings.TrimSpace(item.label)
				if strings.HasSuffix(line, " ·") || strings.Contains(line, "provider count") {
					t.Fatalf("redundant count caption: %q", line)
				}
				lines = append(lines, line)
			}
			if tc.want != "" && !strings.Contains(strings.Join(lines, "\n"), tc.want) {
				t.Fatalf("missing count qualification %q: %v", tc.want, lines)
			}
		}
	}
}

func TestContextDashboardNarrowRowsAndThemeInvalidation(t *testing.T) {
	r := contextDashboardREPL(t, true)
	d := r.model.modal.context
	for _, width := range []int{10, 24, 28, 50} {
		for _, item := range d.items(width) {
			if got := rw.StringWidth(item.label); got > width-2 {
				t.Fatalf("width %d: %d-column row %q", width, got, item.label)
			}
		}
	}
	restoreStyleEpochTestTheme(t)
	d.chartAt = time.Now().Add(-time.Second)
	r.render()
	before := d.chartKey
	theme := style.DefaultTheme()
	theme.Colors["accent"] = "#aa6688"
	style.Apply(theme)
	r.render()
	if d.chartKey == before {
		t.Fatal("chart did not invalidate after theme change")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	if r.model.modal != nil {
		t.Fatal("Enter did not close the read-only dashboard")
	}
}
