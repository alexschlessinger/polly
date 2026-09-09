package main

import (
	"fmt"
	"image"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
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
	if len(m.disclosurePlacements[activityTools]) != 1 {
		t.Fatalf("tool disclosure hitboxes = %#v, want one", m.disclosurePlacements[activityTools])
	}
	target := m.disclosurePlacements[activityTools][0]
	r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: target.X, Y: target.Y}})
	r.render()
	at := m.affordances.disclosures[affordanceTarget{activityTools, target.recordID}]
	if at.IsZero() {
		t.Fatal("click did not arm disclosure feedback")
	}
	for _, p := range m.disclosurePlacements[activityTools] {
		if p.recordID == target.recordID {
			target = p
		}
	}
	canonical := strings.Join(transcriptTexts(m), "\n")
	rows := make([][]ui.Cell, len(m.visual.rows))
	for i := range rows {
		rows[i] = append([]ui.Cell(nil), m.visual.rows[i]...)
	}
	placements := append([]disclosurePlacement(nil), m.disclosurePlacements[activityTools]...)
	_, before, _ := screen.Get(target.X, target.Y)
	r.tickAffordances(at.Add(500 * time.Millisecond))
	glyph, highlighted, _ := screen.Get(target.X, target.Y)
	if glyph != "▾" || highlighted == before {
		t.Fatalf("disclosure did not visibly react: glyph=%q before=%v after=%v", glyph, before, highlighted)
	}
	if !reflect.DeepEqual(rows, m.visual.rows) || !reflect.DeepEqual(placements, m.disclosurePlacements[activityTools]) || canonical != strings.Join(transcriptTexts(m), "\n") {
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
	// The first hitbox on a row includes the triangle; a later one starts at
	// its label.
	for _, tc := range []struct {
		label, want string
		first       bool
	}{
		{"1 agent running, 12 completed", "12", true},
		{"3 agents, 1 failed", "3", true},
		{"3 agents, 1 canceled", "3", false},
	} {
		row := parseStyledCells(activityRowHeader("▾", tc.label), ui.NewStyle(ui.ColorClear))
		placement := turnDockPlacement{X: 4, Cols: len([]rune(tc.label))}
		if tc.first {
			placement = turnDockPlacement{X: 2, Cols: len([]rune(tc.label)) + 2}
		}
		x, cols := agentCountCells(row, placement)
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

// Growth in context usage lights the used count in the status row, not the
// window it is measured against.
func TestContextCueHighlightsTheUsedCount(t *testing.T) {
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
	var lit strings.Builder
	for _, c := range r.affordanceW.cells {
		if c.span.duration == 1400*time.Millisecond {
			if c.point.Y != 31 {
				t.Fatalf("context cue left the status row: %#v", c)
			}
			lit.WriteRune(c.base.Rune)
		}
	}
	if lit.String() != "~29.0k" {
		t.Fatalf("context cue lit %q, want the used count", lit.String())
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
	r := savedAgentREPL(t, true)
	parent, child := r.tabs[0], r.tabs[1]
	child.keepOpen = true
	parent.model.affordances.enabled = true
	child.model.affordances.enabled = true
	parent.model.beginTurn("delegate")
	parent.model.appendToolCallStart(agentCall("call-1", `{}`))
	record, row := parent.model.toolDisclosureRowForCall("call-1")
	row.agent.session = child.name
	row.agent.active, row.agent.status = false, "awaiting review"
	r.showTab(0)
	parent.model.takeActiveTool("call-1")
	r.endTurn(nil)
	parent.model.noteAgentCompletion(record.id)
	parent.model.mu.Lock()
	m := parent.model
	if len(m.affordances.agents) != 1 {
		m.mu.Unlock()
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

// A refreshed child view swaps in records numbered from scratch; a cue noted
// against the old numbering must not survive onto an unrelated row.
func TestReplacedChildDisplayDropsStaleDisclosureCues(t *testing.T) {
	m := newReplModel()
	m.affordances.enabled = true
	m.noteDisclosure(activityTools, 4)
	if len(m.affordances.disclosures) != 1 {
		t.Fatalf("cue not recorded: %#v", m.affordances.disclosures)
	}
	(&managedREPL{}).replaceChildDisplay(&replTab{model: m}, newReplModel())
	if len(m.affordances.disclosures) != 0 {
		t.Fatalf("stale disclosure cue survived the display swap: %#v", m.affordances.disclosures)
	}
	if !m.affordances.enabled {
		t.Fatal("reset disabled affordances for the view")
	}
}
