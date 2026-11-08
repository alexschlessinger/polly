package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
)

const cutTestWidth = 80

// paintedLinks returns the transcript's painted rows and its visible click
// targets.
func paintedLinks(m *replModel) ([]string, []toolOutputLink) {
	rows := m.transcriptRows(cutTestWidth)
	return transcriptRowsText(rows), m.visibleToolOutputLinks(fullViewport(len(rows), cutTestWidth), 0)
}

// cutControl finds the cut control whose key matches, and reports whether
// its row is painted with the cut mark in the rail it covers.
func cutControl(t *testing.T, m *replModel, match func(key string) bool) (toolOutputLink, string, bool) {
	t.Helper()
	rows, links := paintedLinks(m)
	var found []toolOutputLink
	for _, link := range links {
		if link.action == openCutAction && match(link.key) {
			found = append(found, link)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one matching cut control, got %+v: %q", found, rows)
	}
	link := found[0]
	if link.Cols < activityRailCols || link.Y >= len(rows) {
		t.Fatalf("cut control does not cover a rail: %+v", link)
	}
	row := rows[link.Y]
	return link, row, strings.HasPrefix(row[min(len(row), link.X):], reasoningCutIndent)
}

func keyIs(want string) func(string) bool { return func(key string) bool { return key == want } }

func clickCut(t *testing.T, r *managedREPL, link toolOutputLink) {
	t.Helper()
	if !r.activateToolOutput(r.model, link, nil) {
		t.Fatalf("cut control %+v did nothing", link)
	}
}

func markedRows(rows []string) int {
	n := 0
	for _, row := range rows {
		if strings.HasPrefix(row, reasoningCutIndent) {
			n++
		}
	}
	return n
}

// callRunTurn ends a turn of calls settled calls in one run.
func callRunTurn(t *testing.T, calls int) *managedREPL {
	t.Helper()
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("read")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	for i := range calls {
		c := readCall(fmt.Sprintf("c%d", i))
		tui.AppendToolStart([]messages.ChatMessageToolCall{c})
		tui.AppendToolEnd(c, "ok", time.Millisecond, nil)
	}
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	return r
}

// A cut call run marks its first visible call; the rail there opens every
// call, the rail of the opened run's first row cuts it again, and the
// call's own row target, beside the rail, still toggles its output.
func TestCutCallRunOpensAndCloses(t *testing.T) {
	withDisplayTTY(t)
	const calls = activityRunRows + 2
	r := callRunTurn(t, calls)
	m := r.model
	runKey := func(key string) bool { return strings.HasPrefix(key, "c:") }

	confined := func(stage string) toolOutputLink {
		t.Helper()
		rows := stepRows(t, m, cutTestWidth)
		if len(rows) != activityRunRows || !strings.HasPrefix(rows[0], reasoningCutIndent+"✓ read c2.go") || markedRows(rows) != 1 {
			t.Fatalf("%s: want the newest %d calls, the first marked: %q", stage, activityRunRows, rows)
		}
		link, row, marked := cutControl(t, m, runKey)
		if !marked || !strings.Contains(row, "c2.go") || link.Cols != activityRailCols {
			t.Fatalf("%s: the control wants the marked rail of c2's row: %+v %q", stage, link, row)
		}
		return link
	}
	link := confined("abridged")
	clickCut(t, r, link)
	rows := stepRows(t, m, cutTestWidth)
	if len(rows) != calls || markedRows(rows) != 0 || !strings.Contains(rows[0], "c0.go") || m.depth != depthAbridged {
		t.Fatalf("the click wants every call, unmarked, depth untouched: %q depth=%v", rows, m.depth)
	}
	closer, row, marked := cutControl(t, m, keyIs(link.key))
	if marked || !strings.Contains(row, "c0.go") || closer.Cols != activityRailCols {
		t.Fatalf("the open run wants an unmarked control on its first row: %+v %q", closer, row)
	}
	clickCut(t, r, closer)
	link = confined("closed again")

	// The call's own target sits right after the rail on the same row.
	_, links := paintedLinks(m)
	var own toolOutputLink
	for _, l := range links {
		if l.action == "" && l.Y == link.Y {
			own = l
		}
	}
	if own.key == "" || own.X != link.X+activityRailCols {
		t.Fatalf("the marked call wants its own row target beside the rail: %+v / %+v", own, links)
	}
	for _, want := range []bool{true, false} {
		if !r.activateToolOutput(m, own, nil) {
			t.Fatal("the call's row target did nothing")
		}
		if _, callRow := m.toolOutputRow(own); callRow == nil || callRow.outputExpanded != want {
			t.Fatalf("the call's row target wants output expanded=%v", want)
		}
		if len(m.openCuts) != 0 {
			t.Fatalf("the call's row target opened a cut: %v", m.openCuts)
		}
	}
	confined("after output toggles")
}

// When the area's rows run out between runs, the top row is marked; its
// control shows every older entry and the opened area's top row closes it.
func TestCutAreaTopOpensAndCloses(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	// Three thought rows, two calls, two thought rows, one call: eleven rows
	// with the blank ones, none of the runs past activityRunRows, so the
	// area's cut falls between the first thought run and the calls.
	for step, size := range []struct{ thoughts, calls int }{{3, 2}, {2, 1}} {
		tui.ModelAttemptStarted(step, 0)
		for i := range size.thoughts {
			tui.ShowThinking(fmt.Sprintf("Plan %d-%d\n", step, i))
		}
		for i := range size.calls {
			c := readCall(fmt.Sprintf("s%d-%d", step, i))
			tui.AppendToolStart([]messages.ChatMessageToolCall{c})
			tui.AppendToolEnd(c, "ok", time.Millisecond, nil)
		}
	}
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	tailKey := func(key string) bool { return strings.HasSuffix(key, ":tail") }

	confined := func(stage string) toolOutputLink {
		t.Helper()
		rows := stepRows(t, m, cutTestWidth)
		if len(rows) != activityTailRows-1 || !strings.HasPrefix(rows[0], reasoningCutIndent+"✓ read s0-0.go") || markedRows(rows) != 1 {
			t.Fatalf("%s: want the area from s0-0, its top row marked: %q", stage, rows)
		}
		link, row, marked := cutControl(t, m, tailKey)
		if !marked || !strings.Contains(row, "s0-0.go") || link.Cols != activityRailCols {
			t.Fatalf("%s: the area's control wants the top call's rail: %+v %q", stage, link, row)
		}
		return link
	}
	link := confined("abridged")
	clickCut(t, r, link)
	rows := stepRows(t, m, cutTestWidth)
	want := []string{"Plan 0-0", "Plan 0-1", "Plan 0-2", "", "✓ read s0-0.go", "✓ read s0-1.go", "", "Plan 1-0", "Plan 1-1", "", "✓ read s1-0.go"}
	if len(rows) != len(want) || markedRows(rows) != 0 {
		t.Fatalf("the click wants every entry, unmarked: %q", rows)
	}
	for i, row := range rows {
		if row = strings.TrimSpace(row); (want[i] == "") != (row == "") || !strings.HasPrefix(row, want[i]) {
			t.Fatalf("row %d = %q, want %q: %q", i, row, want[i], rows)
		}
	}
	// The top row is now a thought: its whole row closes the area.
	closer, row, marked := cutControl(t, m, keyIs(link.key))
	if marked || !strings.Contains(row, "Plan 0-0") || closer.Cols <= activityRailCols {
		t.Fatalf("the opened area wants its top thought row to close it: %+v %q", closer, row)
	}
	clickCut(t, r, closer)
	confined("closed again")
}

// agentsTurn ends a turn of n agents, settled unless busy says otherwise.
func agentsTurn(t *testing.T, n int, busy func(int) bool) (*managedREPL, []int64) {
	t.Helper()
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("agents")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	var calls []messages.ChatMessageToolCall
	for i := range n {
		calls = append(calls, agentCall(fmt.Sprintf("agent-%d", i), fmt.Sprintf(`{"label":"worker-%d"}`, i)))
	}
	tui.AppendToolStart(calls)
	for i, call := range calls {
		tui.AppendToolEnd(call, "ok", time.Millisecond, nil)
		_, row := m.toolDisclosureRowForCall(call.ID)
		row.agent.session = call.ID
		if busy(i) {
			row.agent.setLocal("running", true)
		} else {
			row.agent.setLocal("done", false)
		}
	}
	return r, activityBlocks(m, cutTestWidth)[0].toolDisclosureIDs
}

func agentLabels(t *testing.T, m *replModel) []string {
	t.Helper()
	block := activityBlocks(m, cutTestWidth)[0]
	var labels []string
	for _, link := range block.agentLinks {
		labels = append(labels, m.toolDisclosures.get(link.recordID).rows[link.rowIndex].agent.label)
	}
	return labels
}

// Seven settled agents show the newest activityAgentWindow, the first of
// them marked; its rail opens all seven and the first agent's rail cuts
// them again.
func TestCutAgentsOpenAndClose(t *testing.T) {
	withDisplayTTY(t)
	r, ids := agentsTurn(t, activityAgentWindow+2, func(int) bool { return false })
	m := r.model
	key := agentsCutKey(ids)

	confined := func(stage string) toolOutputLink {
		t.Helper()
		if labels := agentLabels(t, m); len(labels) != activityAgentWindow || labels[0] != "worker-2" {
			t.Fatalf("%s: want the newest %d agents: %q", stage, activityAgentWindow, labels)
		}
		link, row, marked := cutControl(t, m, keyIs(key))
		if !marked || !strings.Contains(row, "worker-2 ") || link.Cols != activityRailCols {
			t.Fatalf("%s: the agents' control wants worker-2's marked rail: %+v %q", stage, link, row)
		}
		return link
	}
	clickCut(t, r, confined("abridged"))
	if labels := agentLabels(t, m); len(labels) != activityAgentWindow+2 || labels[0] != "worker-0" {
		t.Fatalf("the click wants every agent: %q", labels)
	}
	closer, row, marked := cutControl(t, m, keyIs(key))
	if marked || !strings.Contains(row, "worker-0 ") {
		t.Fatalf("the open agents want an unmarked control on the first: %+v %q", closer, row)
	}
	clickCut(t, r, closer)
	confined("closed again")
}

// A busy agent outside the window stays in view like a running call, and
// the agent after the hidden gap carries the mark.
func TestCutAgentsKeepBusyOnes(t *testing.T) {
	withDisplayTTY(t)
	r, ids := agentsTurn(t, activityAgentWindow+3, func(i int) bool { return i == 0 })
	m := r.model
	labels := agentLabels(t, m)
	want := []string{"worker-0", "worker-3", "worker-4", "worker-5", "worker-6", "worker-7"}
	if strings.Join(labels, ",") != strings.Join(want, ",") {
		t.Fatalf("agents = %q, want the busy one and the window: %q", labels, want)
	}
	link, row, marked := cutControl(t, m, keyIs(agentsCutKey(ids)))
	if !marked || !strings.Contains(row, "worker-3 ") {
		t.Fatalf("the agent after the gap wants the mark and control: %+v %q", link, row)
	}
	rows, _ := paintedLinks(m)
	for _, row := range rows {
		if strings.Contains(row, "worker-0 ") && strings.HasPrefix(row, reasoningCutIndent) {
			t.Fatalf("the busy first agent hides nothing above it: %q", row)
		}
	}
}

// Five viewed images show the newest activityImageWindow, the first image
// row marked; its rail shows all five, and the first row then cuts them.
func TestCutImagesOpenAndClose(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("look")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	call := messages.ChatMessageToolCall{ID: "view", Name: "view_image"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "ok", time.Millisecond, nil)
	path := filepath.Join(t.TempDir(), "frame.png")
	writeImageFixture(t, path, 8, 4)
	var images []style.Image
	for i := range activityImageWindow + 2 {
		images = append(images, style.Image{
			Path: path, Alt: fmt.Sprintf("image-%d.png", i), Width: 8, Height: 4, Inspection: true,
			MaxCols: style.InspectionThumbnailCols, MaxRows: style.InspectionThumbnailRows,
		})
	}
	tui.AppendToolMedia(call, images)
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	key := imagesCutKey(activityBlocks(m, cutTestWidth)[0].toolDisclosureIDs)
	shown := func() []string {
		var alts []string
		for _, img := range activityBlocks(m, cutTestWidth)[0].images {
			alts = append(alts, img.Alt)
		}
		return alts
	}

	confined := func(stage string) toolOutputLink {
		t.Helper()
		if alts := shown(); len(alts) != activityImageWindow || alts[0] != "image-2.png" {
			t.Fatalf("%s: want the newest %d images: %q", stage, activityImageWindow, alts)
		}
		link, row, marked := cutControl(t, m, keyIs(key))
		if !marked || !strings.Contains(row, "image-2.png") || link.Cols != activityRailCols {
			t.Fatalf("%s: the images' control wants the first image row's marked rail: %+v %q", stage, link, row)
		}
		return link
	}
	clickCut(t, r, confined("abridged"))
	if alts := shown(); len(alts) != activityImageWindow+2 || alts[0] != "image-0.png" {
		t.Fatalf("the click wants every image: %q", alts)
	}
	closer, row, marked := cutControl(t, m, keyIs(key))
	if marked || !strings.Contains(row, "image-0.png") {
		t.Fatalf("the open images want an unmarked control on the first row: %+v %q", closer, row)
	}
	clickCut(t, r, closer)
	confined("closed again")
}

// Ctrl-O forgets opened cuts: cycling the depth back to abridged confines
// the run again.
func TestDepthCycleClearsOpenedCuts(t *testing.T) {
	withDisplayTTY(t)
	r := callRunTurn(t, activityRunRows+2)
	m := r.model
	link, _, _ := cutControl(t, m, func(key string) bool { return strings.HasPrefix(key, "c:") })
	clickCut(t, r, link)
	if rows := stepRows(t, m, cutTestWidth); len(rows) != activityRunRows+2 || len(m.openCuts) != 1 {
		t.Fatalf("the click did not open the run: %q %v", rows, m.openCuts)
	}
	m.toggleAllDisclosures(cutTestWidth)
	if m.depth == depthAbridged || len(m.openCuts) != 0 {
		t.Fatalf("Ctrl-O kept the opened cut: depth=%v %v", m.depth, m.openCuts)
	}
	for range 4 {
		if m.depth == depthAbridged {
			break
		}
		m.toggleAllDisclosures(cutTestWidth)
	}
	if m.depth != depthAbridged {
		t.Fatalf("depth = %v, want abridged after a full cycle", m.depth)
	}
	rows := stepRows(t, m, cutTestWidth)
	if len(rows) != activityRunRows || !strings.HasPrefix(rows[0], reasoningCutIndent) || len(m.openCuts) != 0 {
		t.Fatalf("back at abridged the run wants its cut again: %q %v", rows, m.openCuts)
	}
}
