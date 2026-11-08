package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/swarm"
)

func TestAgentTimelineRecord(t *testing.T) {
	var tl agentTimeline
	tl.spans, tl.open = map[string][]agentSpan{}, map[string]bool{}
	t0 := time.Unix(1000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	call := func(id string) messages.ChatMessageToolCall {
		return messages.ChatMessageToolCall{ID: id, Name: "bash"}
	}

	tl.record("a", phaseQuery, messages.ChatMessageToolCall{}, at(0))
	tl.record("a", phaseQuery, messages.ChatMessageToolCall{}, at(3))
	tl.record("a", phaseTool, call("c1"), at(5))
	tl.record("a", phaseTool, call("c1"), at(7))
	tl.record("a", phaseTool, call("c2"), at(9))
	tl.record("a", phaseNone, messages.ChatMessageToolCall{}, at(12))
	tl.record("a", phaseNone, messages.ChatMessageToolCall{}, at(20))

	spans := tl.spans["a"]
	type want struct {
		start, end int
		phase      agentPhase
		call       string
	}
	expected := []want{{0, 5, phaseQuery, ""}, {5, 9, phaseTool, "c1"}, {9, 12, phaseTool, "c2"}}
	if len(spans) != len(expected) {
		t.Fatalf("spans: %+v", spans)
	}
	for n, w := range expected {
		s := spans[n]
		if !s.start.Equal(at(w.start)) || !s.end.Equal(at(w.end)) || s.phase != w.phase || s.call.ID != w.call {
			t.Fatalf("span %d = %+v, want %+v", n, s, w)
		}
	}
	if tl.open["a"] {
		t.Fatal("a phaseNone sample must close the span")
	}
	if tl.revision != 3 {
		t.Fatalf("revision %d counts span starts, want 3", tl.revision)
	}
}

// timelineState is two members on a 100-second axis: "old" ran 0-40s and
// finished; "live" started at 20s and is running at 100s.
func timelineState(t0 time.Time) *swarm.State {
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	return &swarm.State{
		Members: map[string]*swarm.Member{
			"old":  {ID: "old", Name: "old", Execution: "e-old"},
			"live": {ID: "live", Name: "live", Execution: "e-live"},
		},
		Executions: map[string]*swarm.Execution{
			"e-old":  {ID: "e-old", Member: "old", Status: "completed", Started: at(0), Ended: at(40)},
			"e-live": {ID: "e-live", Member: "live", Status: "running", Started: at(20)},
		},
		Tasks: map[string]*swarm.Task{},
	}
}

func TestAgentLanesCells(t *testing.T) {
	t0 := time.Unix(5000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	var tl agentTimeline
	tl.spans, tl.open = map[string][]agentSpan{}, map[string]bool{}
	tl.record("live", phaseQuery, messages.ChatMessageToolCall{}, at(50))
	bash := messages.ChatMessageToolCall{ID: "c1", Name: "bash", Arguments: `{"command":"go test ./llm/..."}`}
	tl.record("live", phaseTool, bash, at(70))

	lanes := buildAgentLanes(timelineState(t0), &tl, []string{"old", "live"}, map[string]bool{"old": true}, at(100))
	if lanes == nil {
		t.Fatal("no lanes")
	}
	if !lanes.live || !lanes.from.Equal(at(0)) || !lanes.to.Equal(at(100)) {
		t.Fatalf("axis %v..%v live=%v", lanes.from, lanes.to, lanes.live)
	}
	lanes.cols = 10
	phases := func(id string) []agentPhase {
		var out []agentPhase
		for col := range lanes.cols {
			out = append(out, lanes.cellPhase(id, col))
		}
		return out
	}
	// Each column is ten seconds: old ran in the first four; live ran
	// unobserved from 20s, queried from 50s and ran a tool from 70s.
	if got, want := phases("old"), []agentPhase{phaseRan, phaseRan, phaseRan, phaseRan, 0, 0, 0, 0, 0, 0}; !slices.Equal(got, want) {
		t.Fatalf("old lane %v, want %v", got, want)
	}
	if got, want := phases("live"), []agentPhase{0, 0, phaseRan, phaseRan, phaseRan, phaseQuery, phaseQuery, phaseTool, phaseTool, phaseTool}; !slices.Equal(got, want) {
		t.Fatalf("live lane %v, want %v", got, want)
	}
	if row := plainStyledText(lanes.laneRow("live")); row != "  ──━━━━━━━━" {
		t.Fatalf("cell lane %q", row)
	}
	if axis := plainStyledText(lanes.axisRow()); !strings.HasPrefix(axis, "  0:00") || !strings.HasSuffix(axis, "now") {
		t.Fatalf("axis %q", axis)
	}

	lanes.scrub = true
	for _, tc := range []struct {
		cursor int
		id     string
		want   string
	}{
		{10, "live", "starts at 0:20"},
		{30, "live", "running · phase not recorded"},
		{55, "live", "■ querying  0:05 in"},
		{80, "live", "■ $ go test ./llm/...  0:10 in"},
		{60, "old", "finished at 0:40"},
	} {
		lanes.cursor = at(tc.cursor)
		if got := strings.TrimSpace(plainStyledText(lanes.scrubRow(tc.id, 60))); got != tc.want {
			t.Fatalf("at %ds %s: %q, want %q", tc.cursor, tc.id, got, tc.want)
		}
	}
	lanes.cursor = at(55)
	if row := plainStyledText(lanes.laneRow("live")); row != "  ──━━━│━━━━" {
		t.Fatalf("scrubbed lane %q", row)
	}
	if axis := plainStyledText(lanes.axisRow()); !strings.Contains(axis, " 0:55 ") {
		t.Fatalf("scrubbed axis %q", axis)
	}
	if got := lanes.moveCursor(at(55), -100); !got.Equal(lanes.from) {
		t.Fatalf("cursor left of the axis: %v", got)
	}
	if got := lanes.moveCursor(at(55), 1); !got.Equal(at(65)) {
		t.Fatalf("cursor step: %v", got)
	}
	// Three agents at most share a moment; the ribbon draws one block a column.
	if ribbon := []rune(plainStyledText(lanes.ribbonRow())); len(ribbon) != agentLaneOffset+lanes.cols {
		t.Fatalf("ribbon %q", string(ribbon))
	}
}

func TestAgentLanesNativePlacements(t *testing.T) {
	t0 := time.Unix(5000, 0)
	var tl agentTimeline
	lanes := buildAgentLanes(timelineState(t0), &tl, []string{"old", "live"}, nil, t0.Add(100*time.Second))
	lanes.cols, lanes.native, lanes.scrub, lanes.cursor = 30, true, true, t0.Add(50*time.Second)
	rows := []agentLaneRow{{row: 0}, {row: 4, id: "old"}, {row: 9, id: "live"}}
	var cache agentLaneImages
	viewport := transcriptViewport{start: 0, end: 8, logoRows: 3}
	got := cache.placements(lanes, rows, viewport, 50, 8, 16)
	if len(got) != 2 {
		t.Fatalf("placements %+v", got)
	}
	ribbon, lane := got[0], got[1]
	if ribbon.Rows != agentRibbonRows || ribbon.Y != 3 || ribbon.X != 50+agentLaneOffset || ribbon.Cols != 30 {
		t.Fatalf("ribbon placement %+v", ribbon)
	}
	if lane.Rows != 1 || lane.Y != 7 || lane.Pixels.Bounds().Dx() != 240 || lane.Pixels.Bounds().Dy() != 16 {
		t.Fatalf("lane placement %+v", lane)
	}
	again := cache.placements(lanes, rows, viewport, 50, 8, 16)
	if again[1].Pixels != lane.Pixels {
		t.Fatal("an unchanged picture must reuse its image")
	}
	lanes.cursor = t0.Add(60 * time.Second)
	if moved := cache.placements(lanes, rows, viewport, 50, 8, 16); moved[1].Pixels == lane.Pixels || moved[1].PixelVersion == lane.PixelVersion {
		t.Fatal("moving the cursor must repaint")
	}
}

func TestAgentsInspectorLanesAndScrub(t *testing.T) {
	r, ids := agentsInspectorFixture(t)
	s := r.visibleTab().swarmSnapshot
	start := time.Now().Add(-90 * time.Second)
	for n, id := range ids {
		s.Executions[id].Started = start.Add(time.Duration(n*10) * time.Second)
	}
	s.Executions[ids[2]].Ended = start.Add(60 * time.Second)
	r.openAttention()
	v := waitInspector(t, r, 160)
	text := inspectorText(v)
	if !strings.Contains(text, "0:00") || !strings.Contains(text, "now") || !strings.Contains(text, "━") {
		t.Fatalf("lanes missing:\n%s", text)
	}
	if !slices.Contains(v.agentsActions, agentsTimelineAction) {
		t.Fatalf("axis row is not clickable: %v", v.agentsActions)
	}

	r.toggleAgentTimeline(keyContext{})
	r.inspectorRefreshAt = time.Time{}
	v = waitInspector(t, r, 160)
	text = inspectorText(v)
	if !strings.Contains(text, "running · phase not recorded") || !strings.Contains(text, "│") {
		t.Fatalf("scrub rows missing:\n%s", text)
	}
	list := r.agentsList()
	before := list.cursor
	if !r.navigateAgentsInspector("<Left>") || !list.cursor.Before(before) {
		t.Fatalf("Left did not scrub: %v -> %v", before, list.cursor)
	}
	// Up/Down still move by agent; the axis is never selected.
	list.selected = ids[0]
	r.navigateAgentsInspector("<Up>")
	if list.selected == agentsTimelineAction || list.selected != ids[0] {
		t.Fatalf("selection moved onto the axis: %q", list.selected)
	}
	r.inspectorAction(agentsTimelineAction)
	if list.scrub {
		t.Fatal("clicking the axis must leave scrub mode")
	}

	r.inspectorRefreshAt = time.Time{}
	v = waitInspector(t, r, 48)
	if text := inspectorText(v); strings.Contains(text, "━") || slices.Contains(v.agentsActions, agentsTimelineAction) {
		t.Fatalf("a narrow pane drops the lanes:\n%s", text)
	}
}
