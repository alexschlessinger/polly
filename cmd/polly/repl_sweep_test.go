package main

import (
	"image"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
)

func TestInlineSweepTracksMembers(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	m := r.model
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.ShowThinking("consider this")
	calls := []messages.ChatMessageToolCall{{ID: "one", Name: "read_file"}, {ID: "two", Name: "read_file"}, agentCall("child", `{}`)}
	tui.AppendToolStart(calls)
	// Exercise simultaneous categories independently of the provider's sequential callbacks.
	m.reasoningRecords.get(m.turnReasoningID).active = true
	r.render()
	check := func(thought, tools, agents bool) {
		t.Helper()
		r.render()
		got := map[activityKind]bool{}
		for _, block := range m.visual.blocks {
			for _, label := range block.activityLabels {
				got[label.kind] = m.inlineActivityRunning(label.kind, block.reasoningIDs, block.toolDisclosureIDs)
			}
		}
		for kind, want := range map[activityKind]bool{activityThought: thought, activityTools: tools, activityAgents: agents, activityImages: false} {
			if got[kind] != want {
				t.Fatalf("kind %v running=%v want %v", kind, got[kind], want)
			}
		}
		count := 0
		for _, span := range r.affordanceW.spans {
			if span.sweep {
				count++
			}
		}
		want := 0
		for _, active := range []bool{thought, tools, agents} {
			if active {
				want++
			}
		}
		if count != want {
			t.Fatalf("sweeps=%d want %d", count, want)
		}
	}
	check(true, true, true)
	for _, kind := range []activityKind{activityThought, activityTools, activityAgents} {
		p := m.disclosurePlacements[kind][0]
		m.toggleDisclosureGroup(kind, []int64{p.recordID}, 100)
	}
	check(true, true, true)
	rows := make([][]ui.Cell, len(m.visual.rows))
	for i := range rows {
		rows[i] = slices.Clone(m.visual.rows[i])
	}
	placements := append([]disclosurePlacement(nil), m.disclosurePlacements[activityTools]...)
	at := m.affordances.sweepAt
	r.affordanceW.tick(screen, at.Add(time.Second))
	r.affordanceW.tick(screen, at.Add(1200*time.Millisecond))
	if !reflect.DeepEqual(rows, m.visual.rows) || !reflect.DeepEqual(placements, m.disclosurePlacements[activityTools]) {
		t.Fatal("sweep changed cached layout")
	}
	tui.AppendToolEnd(calls[0], "done", time.Second, nil)
	check(true, true, true)
	tui.AppendToolEnd(calls[1], "done", time.Second, nil)
	check(true, false, true)
	m.reasoningRecords.get(m.turnReasoningID).active = false
	m.busy = false // Background children keep sweeping after the parent becomes idle.
	check(false, false, true)
	r.affordanceW.tick(screen, at.Add(3*time.Second))
	tui.AppendToolEnd(calls[2], "done", time.Second, nil)
	check(false, false, false)
	for _, hidden := range []func(){func() { m.hidden = true }, func() { m.quiet = true }, func() { m.affordances.enabled = false }, func() { m.focusKnown = true; m.focused = false }} {
		m.hidden = false
		m.quiet = false
		m.affordances.enabled = true
		m.focusKnown = false
		hidden()
		if spans := m.affordanceSpans(at, transcriptViewport{}, image.Point{}, false); len(spans) != 0 {
			t.Fatal("suppressed animation emitted spans")
		}
	}
}

func TestSweepFrameMatchesTextfx(t *testing.T) {
	at := time.Unix(100, 0)
	base := ui.Cell{Rune: 'x', Style: ui.NewStyle(ui.ColorGreen, ui.ColorBlack, ui.ModifierBold)}
	c := affordanceCell{point: image.Pt(7, 0), base: base, span: affordanceSpan{x: 5, cols: 10, at: at, sweep: true}}
	peak := c.frame(at.Add(time.Second)) // center=2
	if peak.Style.Fg != ui.NewColorRGB(237, 248, 255) {
		t.Fatalf("peak=%v", peak.Style.Fg)
	}
	if peak.Rune != base.Rune || peak.Style.Bg != base.Style.Bg || peak.Style.Modifier != base.Style.Modifier {
		t.Fatal("sweep changed more than foreground")
	}
	if c.frame(at.Add(2*time.Second)).Style.Fg == peak.Style.Fg {
		t.Fatal("highlight did not move")
	}
	if c.frame(at.Add(3500*time.Millisecond)) != peak {
		t.Fatal("sweep did not loop")
	}
}

func TestSweepLabelClipping(t *testing.T) {
	for _, width := range []int{3, 5, 8, 14, 80} {
		m := newReplModel()
		m.beginTurn("tools")
		m.appendToolCallStart(messages.ChatMessageToolCall{ID: "one", Name: "read_file"})
		blocks := activityBlocks(m, width)
		for _, block := range blocks {
			for _, label := range block.activityLabels {
				if label.X < 4 || label.X+label.Cols > width {
					t.Fatalf("width %d invalid paint bounds %+v", width, label)
				}
				if width < 11 && label.X+label.Cols >= width {
					t.Fatal("sweep includes ellipsis")
				}
			}
		}
	}
}

func TestSweepComposesCompletionAndPreservesHover(t *testing.T) {
	_, screen := affordanceTestREPL(t)
	at := time.Unix(100, 0)
	point := image.Pt(5, 2)
	base := ui.Cell{Rune: '2', Style: ui.NewStyle(ui.ColorGreen, ui.ColorBlack)}
	sweep := affordanceCell{point: point, base: base, span: affordanceSpan{x: 4, y: 2, cols: 10, at: at, sweep: true}}
	flash := affordanceCell{point: point, base: base, span: affordanceSpan{x: 5, y: 2, cols: 1, at: at, duration: time.Second, color: ui.ColorGreen}}
	layer := affordanceLayer{cells: []affordanceCell{sweep, flash}, painted: map[image.Point]ui.Cell{}, underlined: func(p image.Point) bool { return p == point }}
	layer.tick(screen, at.Add(300*time.Millisecond))
	if layer.painted[point].Style.Fg != ui.ColorGreen {
		t.Fatal("completion flash did not compose over sweep")
	}
	now := at.Add(1100 * time.Millisecond)
	layer.tick(screen, now)
	if got, want := layer.painted[point], sweep.frame(now); got != want {
		t.Fatalf("expired flash froze sweep: got %+v want %+v", got, want)
	}
	_, st, _ := screen.Get(point.X, point.Y)
	if st.GetUnderlineStyle() == 0 {
		t.Fatal("sweep removed hover underline")
	}
	layer.tick(screen, now.Add(50*time.Millisecond))
	if layer.painted[point] != sweep.frame(now.Add(50*time.Millisecond)) {
		t.Fatal("sweep stopped after flash expiration")
	}
}

func TestAgentSweepExcludesOutcomeTails(t *testing.T) {
	for _, width := range []int{20, 48, 120} {
		for _, expanded := range []bool{false, true} {
			m := newReplModel()
			m.beginTurn("delegate")
			for _, id := range []string{"running", "done", "failed", "canceled", "paused"} {
				m.appendToolCallStart(agentCall(id, `{}`))
				_, row := m.toolDisclosureRowForCall(id)
				row.agent.setLocal(id, id == "running")
			}
			for _, record := range m.toolDisclosures.all() {
				record.agentsExpanded = expanded
			}
			blocks := activityBlocks(m, width)
			found := false
			for _, block := range blocks {
				for _, label := range block.activityLabels {
					if label.kind != activityAgents {
						continue
					}
					found = true
					header, _, _ := strings.Cut(plainStyledText(block.text), "\n")
					painted := []rune(header)[label.X : label.X+label.Cols]
					if string(painted) != "1 agent running" {
						t.Fatalf("width %d expanded %v: sweep covers %q", width, expanded, string(painted))
					}
					if width == 120 && !strings.Contains(header, ", 1 completed, 1 failed, 1 canceled, 1 paused") {
						t.Fatalf("outcome tails missing: %q", header)
					}
				}
			}
			if !found {
				t.Fatal("missing agent sweep bounds")
			}
		}
	}
}
