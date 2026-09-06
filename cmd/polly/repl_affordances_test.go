package main

import (
	"context"
	"fmt"
	"image"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

func affordanceTestREPL(t *testing.T) (*managedREPL, tcell.SimulationScreen) {
	t.Helper()
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	screen.SetSize(100, 32)
	old := ui.DefaultBackend.Screen
	ui.DefaultBackend.Screen = screen
	t.Cleanup(func() { ui.DefaultBackend.Screen = old; screen.Fini() })
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.setupWidgets()
	r.affordanceW = &affordanceLayer{}
	r.model.affordances.enabled = true
	return r, screen
}

func TestAffordancePaintPreservesTranscriptAndClickGeometry(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	m := r.model
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	call := messages.ChatMessageToolCall{ID: "read", Name: "read_file"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "done", time.Second, nil)
	tui.AppendAssistantText("Found the relevant code.")
	r.endTurn(nil)
	m.ed.setText("draft") // Keep the normal editor cursor throughout this check.
	r.render()
	var target turnTrailerPlacement
	for _, p := range m.turnTrailerPlacements {
		if p.overlay == turnDockOverlayTools {
			target = p
			break
		}
	}
	if target.Cols == 0 {
		t.Fatal("no tool disclosure to click")
	}
	r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: target.X, Y: target.Y}})
	r.render()
	at := m.affordances.disclosures[affordanceTarget{turnDockOverlayTools, target.recordID, true}]
	if at.IsZero() {
		t.Fatal("click did not arm disclosure feedback")
	}
	for _, p := range m.turnTrailerPlacements {
		if p.recordID == target.recordID && p.overlay == target.overlay {
			target = p
		}
	}
	canonical := strings.Join(transcriptTexts(m), "\n")
	rows := make([][]ui.Cell, len(m.visual.rows))
	for i := range rows {
		rows[i] = append([]ui.Cell(nil), m.visual.rows[i]...)
	}
	placements := append([]turnTrailerPlacement(nil), m.turnTrailerPlacements...)
	_, before, _ := screen.Get(target.X, target.Y)
	r.tickAffordances(at.Add(500 * time.Millisecond))
	glyph, highlighted, _ := screen.Get(target.X, target.Y)
	if glyph != "▾" || highlighted == before {
		t.Fatalf("disclosure did not visibly react: glyph=%q before=%v after=%v", glyph, before, highlighted)
	}
	if !reflect.DeepEqual(rows, m.visual.rows) || !reflect.DeepEqual(placements, m.turnTrailerPlacements) || canonical != strings.Join(transcriptTexts(m), "\n") {
		t.Fatal("style-only tick changed transcript/cache/click geometry")
	}
	r.tickAffordances(at.Add(2 * time.Second))
	_, restored, _ := screen.Get(target.X, target.Y)
	if restored != before {
		t.Fatal("expired disclosure cue did not restore its original style")
	}
}

func TestQueuedAffordanceIsOnlyAProjection(t *testing.T) {
	for _, width := range []int{8, 40, 100} {
		m := newReplModel()
		m.affordances.enabled = true
		item := queuedREPLInput{text: "check [brackets] and 界\nthen run the tests"}
		m.appendQueuedInput(&item)
		m.transcriptRows(width)
		m.activateQueuedInput(item)
		q := m.affordances.queued[item.transcriptIndex]
		if q.fading.IsZero() {
			t.Fatal("activation did not start queue fade")
		}
		if strings.Contains(strings.Join(transcriptTexts(m), "\n"), "(queued)") {
			t.Fatal("activated queue marker remained in canonical transcript text")
		}
		rows := m.transcriptRows(width)
		l := frameLayout{width: width, height: 80, transcriptHeight: 78, inputRows: 1, statusRows: 1}
		v := l.transcriptViewport(len(rows), 0, false, 0)
		spans := m.affordanceSpans(q.fading, l, v, "", image.Point{}, false)
		var marker strings.Builder
		for i := len(spans) - 1; i >= 0; i-- {
			p := spans[i]
			if p.fade {
				for _, cx := range ui.BuildCellWithXArray(rows[p.y]) {
					if cx.X == p.x {
						marker.WriteRune(cx.Cell.Rune)
					}
				}
			}
		}
		if marker.String() != "(queued)" {
			t.Fatalf("width %d: fade targets %q instead of only the marker", width, marker.String())
		}
		if !m.expireAffordances(q.fading.Add(queueFadeDuration)) || len(m.affordances.queued) != 0 {
			t.Fatal("finished queue fade did not release its row")
		}
		shown := strings.Join(rowsText(m.transcriptRows(width)), "\n")
		if strings.Contains(shown, "queued") {
			t.Fatalf("expired marker remained visible: %q", shown)
		}
	}
}

func TestAffordanceCursorYieldsToTypingAndFocus(t *testing.T) {
	r, screen := affordanceTestREPL(t)
	m := r.model
	m.affordances.inputAt = time.Now().Add(-2 * time.Second)
	r.render()
	if !r.affordanceW.idleCursor {
		t.Fatal("idle composer did not get the breathing cursor")
	}
	if _, _, visible := screen.GetCursor(); visible {
		t.Fatal("hardware cursor and painted cursor are both visible")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "x"})
	r.render()
	if r.affordanceW.idleCursor {
		t.Fatal("typing did not return cursor control to the terminal")
	}
	if _, _, visible := screen.GetCursor(); !visible {
		t.Fatal("normal typing cursor is hidden")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: focusLostID})
	r.render()
	if len(r.affordanceW.cells) != 0 || m.affordancesVisible() {
		t.Fatal("effects remained active after focus loss")
	}
	m.focused, m.hidden = true, true
	if m.idleAffordanceCursor(time.Now().Add(time.Second)) {
		t.Fatal("hidden model animated its composer")
	}
}

func TestAgentCueSelectsCompletedCountNotFailureCount(t *testing.T) {
	for _, tc := range []struct{ label, want string }{
		{"▸ 1 agent running, 12 completed", "12"},
		{"▸ 3 agents, 1 failed", "3"},
		{"▾ 3 agents, 1 canceled", "3"},
	} {
		row := parseStyledCells("  "+turnActivityControl(string([]rune(tc.label)[0]), string([]rune(tc.label)[2:])), ui.NewStyle(ui.ColorClear))
		x, cols := agentCountCells(row, turnDockPlacement{X: 2, Cols: len([]rune(tc.label))})
		var got strings.Builder
		for _, cx := range ui.BuildCellWithXArray(row) {
			if cx.X >= x && cx.X < x+cols {
				got.WriteRune(cx.Cell.Rune)
			}
		}
		if got.String() != tc.want {
			t.Fatalf("%q highlighted %q; want %q", tc.label, got.String(), tc.want)
		}
	}
}

func TestContextCueUsesRenderedMeter(t *testing.T) {
	r, _ := affordanceTestREPL(t)
	m := r.model
	m.ed.setText("draft")
	m.status.recordContextUsage(18400, 128000, true)
	r.render()
	if !m.affordances.contextAt.IsZero() {
		t.Fatal("loading context usage should not look like new usage")
	}
	m.status.recordContextUsage(29000, 128000, true)
	r.render()
	var found bool
	for _, c := range r.affordanceW.cells {
		if c.span.duration == 1400*time.Millisecond {
			found = true
			if c.base.Rune != '█' || c.point.Y != 31 {
				t.Fatalf("context cue targets the wrong cell: %#v", c)
			}
		}
	}
	if !found {
		t.Fatal("new context cell was not highlighted")
	}
}

func TestQueueFadePreservesHeldViewport(t *testing.T) {
	m := newReplModel()
	m.affordances.enabled = true
	item := queuedREPLInput{text: "next"}
	m.appendQueuedInput(&item)
	m.activateQueuedInput(item)
	for i := 0; i < 10; i++ {
		m.appendLine("later output")
	}
	m.transcriptRows(80)
	m.followBottom = false
	m.scrollAnchor = 7
	q := m.affordances.queued[item.transcriptIndex]
	m.expireAffordances(q.fading.Add(queueFadeDuration))
	if m.scrollAnchor != 6 {
		t.Fatalf("removing a queue marker above the viewport shifted the viewed text: anchor=%d", m.scrollAnchor)
	}
}

// Queue cues are keyed by transcript index. An empty assistant block above a
// queued entry is deleted on settle, so the cue must move with its entry or
// the "(queued)" highlight lands on whatever occupies the old index.
func TestQueuedCueFollowsEntryAcrossEmptyAssistantDelete(t *testing.T) {
	m := newReplModel()
	m.affordances.enabled = true
	m.beginTurn("ask")
	m.appendAssistant("\n")
	item := queuedREPLInput{text: "next"}
	m.appendQueuedInput(&item)
	m.queue = append(m.queue, item)
	want := item.transcriptIndex - 1
	m.finishAssistantBlock("")
	if got := m.queue[0].transcriptIndex; got != want {
		t.Fatalf("queued entry index = %d, want %d", got, want)
	}
	q, ok := m.affordances.queued[want]
	if !ok || len(m.affordances.queued) != 1 || q.started.IsZero() {
		t.Fatalf("queued cue did not follow its entry to %d: %#v", want, m.affordances.queued)
	}
	rows := m.transcriptRows(80)
	l := frameLayout{width: 80, height: 40, transcriptHeight: 38, inputRows: 1, statusRows: 1}
	v := l.transcriptViewport(len(rows), 0, false, 0)
	count := 0
	for _, span := range m.affordanceSpans(q.started.Add(300*time.Millisecond), l, v, "", image.Point{}, false) {
		if span.duration == 1500*time.Millisecond {
			count++
		}
	}
	if count != len("(queued)") {
		t.Fatalf("queued cue highlighted %d cells; want %d", count, len("(queued)"))
	}

	// A cue below a later delete keeps its index, so the fading label still
	// overlays its own entry.
	m.activateQueuedInput(m.queue[0])
	m.queue = m.queue[1:]
	m.appendAssistant("\n")
	m.finishAssistantBlock("")
	if q, ok := m.affordances.queued[want]; !ok || q.fading.IsZero() {
		t.Fatalf("fading cue did not stay on entry %d: %#v", want, m.affordances.queued)
	}
	for _, block := range m.transcriptDisplayEntries(80) {
		if block.key == fmt.Sprintf("transcript:%d", want) {
			if !strings.Contains(block.text, "(queued)") {
				t.Fatalf("fading entry lost its label: %q", block.text)
			}
			return
		}
	}
	t.Fatalf("entry %d was not projected", want)
}

func TestDeliveredChildArmsCallerCueAndOnlyCurrentAgentControl(t *testing.T) {
	r, runs := newChildTestREPL(t)
	parent := r.visibleTab()
	parent.model.affordances.enabled = true
	beginParentToolCall(t, r, runs, "call-1")
	out := spawnFromTool(context.Background(), r, parent, subagent.Request{Task: "slow", Background: true}, "call-1")
	runUITask(t, r)
	if rep := awaitReport(t, out); rep.err != nil {
		t.Fatal(rep.err)
	}
	child := r.tabs[1]
	child.keepOpen = true
	// Visit the child once so its affordances are live, then leave it hidden:
	// the state a background child is in when its delivery lands.
	r.showTab(1)
	r.showTab(0)
	parent.model.mu.Lock()
	parent.model.takeActiveTool("call-1")
	parent.model.mu.Unlock()
	close(runs.release)
	settleUntil(t, r, settled(parent))
	close(runs.slow)
	settleUntil(t, r, settled(child))
	settleUntil(t, r, func() bool { return child.delivered })
	parent.model.mu.Lock()
	m := parent.model
	if len(m.affordances.agents) != 1 {
		t.Fatalf("completion did not arm exactly one group cue: %#v", m.affordances.agents)
	}
	rows := m.transcriptRows(100)
	l := frameLayout{width: 100, height: 80, transcriptHeight: 78, inputRows: 1, statusRows: 1}
	v := l.transcriptViewport(len(rows), 0, false, 0)
	spans := m.affordanceSpans(time.Now(), l, v, "", image.Point{}, false)
	count := 0
	for _, span := range spans {
		if span.duration == 1300*time.Millisecond {
			count++
		}
	}
	m.mu.Unlock()
	if count != 1 {
		t.Fatalf("arrival highlighted %d controls; want only the trailer", count)
	}
	// A hidden child does not advertise an old delivery on its next visit.
	if !child.model.affordances.enabled || !child.model.hidden {
		t.Fatal("child is not a hidden tab with live affordances")
	}
	if !child.model.affordances.caller.IsZero() {
		t.Fatal("hidden child armed a caller beacon")
	}
	r.showTab(1)
	if !child.model.affordances.caller.IsZero() {
		t.Fatal("visiting the child replayed an old caller beacon")
	}
	r.noteCallerReady(child)
	if child.model.affordances.caller.IsZero() {
		t.Fatal("visible child did not arm the caller beacon")
	}
	r.showTab(0)
	if !child.model.affordances.caller.IsZero() {
		t.Fatal("leaving the child retained a stale caller beacon")
	}
}
