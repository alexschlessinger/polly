package main

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// budgetTurn is a turn that looks, edits and looks again: every step is a
// run of its own, so n steps make n header rows.
func budgetTurn(n int) chunkTurn {
	var turn chunkTurn
	for i := range n {
		id := string(rune('a' + i))
		if i%2 == 0 {
			turn.steps = append(turn.steps, []messages.ChatMessageToolCall{readCall(id)})
		} else {
			turn.steps = append(turn.steps, []messages.ChatMessageToolCall{editCall(id)})
		}
	}
	return turn
}

// history is the turn as a saved session holds it, answered with answer.
func (turn chunkTurn) history(answer string) []messages.ChatMessage {
	out := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "work"}}
	for _, calls := range turn.steps {
		out = append(out, messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: calls})
		for _, c := range calls {
			result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: c.ID, ToolName: c.Name, Content: "ok"}
			result.SetToolSucceeded(!slices.Contains(turn.fail, c.ID))
			out = append(out, result)
		}
	}
	return append(out, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: answer})
}

// activityRows are the rows of the model's one activity line, as a paint
// lays it out: an answer that has begun to show counts as following it.
func activityRows(t *testing.T, m *replModel, width int) []string {
	t.Helper()
	m.renderPendingMarkdown()
	blocks := activityBlocks(m, width)
	if len(blocks) != 1 {
		t.Fatalf("got %d activity lines, want one", len(blocks))
	}
	return strings.Split(plainStyledText(blocks[0].text), "\n")
}

// A collapsed line keeps to five rows. While the turn adds to it, the newest
// four runs keep their headers and one row above them stands for everything
// earlier, counted as a header counts. Once the answer follows, the whole
// line is that one row. A line of five runs or fewer is left alone.
func TestCollapsedActivityKeepsToItsRowBudget(t *testing.T) {
	const width = 100
	turn := budgetTurn(7)
	r, tui := turn.start(t)
	m := r.model
	running := []string{
		"▸ 3 earlier runs · edited b.go and read 2 files",
		"▸ Edited d.go",
		"▸ Read e.go",
		"▸ Edited f.go",
		"▸ Read g.go",
	}
	if got := activityRows(t, m, width); !slices.Equal(got, running) {
		t.Fatalf("the running turn shows %q, want %q", got, running)
	}
	// The summary is the line's first chunk and the newest run its last, so
	// the live label still hands on from the run that is working.
	block := activityBlocks(m, width)[0]
	if !block.currentActivity || len(block.chunks) != 8 || !block.chunks[0].summary || block.chunks[7].label != "read g.go" {
		t.Fatalf("the budgeted line has %d chunks, summary first = %v", len(block.chunks), block.chunks[0].summary)
	}

	// The answer starts: nothing more joins the line, and it folds whole.
	tui.AppendAssistantText("Done.")
	const settled = "▸ Edited 3 files and read 4 files"
	if got := activityRows(t, m, width); !slices.Equal(got, []string{settled}) {
		t.Fatalf("the line under its answer shows %q, want %q", got, settled)
	}
	tui.FinishTextTurn()
	r.endTurn(nil)
	if got := activityRows(t, m, width); !slices.Equal(got, []string{settled}) {
		t.Fatalf("the settled turn shows %q, want %q", got, settled)
	}

	// Reloaded, the turn reads the same.
	reloaded := newReplModel()
	reloaded.hydrateHistory(turn.history("Done."), "ctx")
	if got := activityRows(t, reloaded, width); !slices.Equal(got, []string{settled}) {
		t.Fatalf("the reloaded turn shows %q, want %q", got, settled)
	}

	// Five runs are within the budget, running and settled.
	fiveREPL, _ := budgetTurn(5).start(t)
	small := fiveREPL.model
	headers := []string{"▸ Read a.go", "▸ Edited b.go", "▸ Read c.go", "▸ Edited d.go", "▸ Read e.go"}
	if got := activityRows(t, small, width); !slices.Equal(got, headers) {
		t.Fatalf("five running runs show %q", got)
	}
	fiveREPL.endTurn(nil)
	if got := activityRows(t, small, width); !slices.Equal(got, headers) {
		t.Fatalf("five settled runs show %q", got)
	}
}

// The summary is a fold: a click opens it onto the headers it stands for,
// which read and open as they would without it, and another click folds
// them away again with their own folds as they were left.
func TestRunsSummaryOpensOntoItsRuns(t *testing.T) {
	const width = 100
	r, tui := budgetTurn(7).start(t)
	m := r.model
	tui.AppendAssistantText("Done.")
	tui.FinishTextTurn()
	r.endTurn(nil)
	block := activityBlocks(m, width)[0]
	summary, first, second := block.chunks[0].key, block.chunks[1].key, block.chunks[2].key
	if summary != runsSummaryKey(first) {
		t.Fatalf("the summary's fold = %q, want it named by the first run %q", summary, first)
	}

	// Open, the summary says how many runs it heads, so it reads as their
	// heading and not as one more of them.
	clickHeader(t, m, width, summary)
	open := []string{
		"▾ 7 runs · edited 3 files and read 4 files",
		"▸ Read a.go", "▸ Edited b.go", "▸ Read c.go", "▸ Edited d.go", "▸ Read e.go", "▸ Edited f.go", "▸ Read g.go",
	}
	if got := activityRows(t, m, width); !slices.Equal(got, open) {
		t.Fatalf("the open summary shows %q, want %q", got, open)
	}

	// A run under it opens onto its calls like any other.
	clickHeader(t, m, width, second)
	rows := activityRows(t, m, width)
	if len(rows) != 9 || rows[2] != "▾ Edited b.go" || !strings.Contains(rows[3], "edit b.go") || rows[4] != "▸ Read c.go" {
		t.Fatalf("a run opened under the summary shows %q", rows)
	}

	// Folding the summary hides the runs and keeps their folds.
	clickHeader(t, m, width, summary)
	if got := activityRows(t, m, width); !slices.Equal(got, []string{"▸ Edited 3 files and read 4 files"}) {
		t.Fatalf("the folded summary shows %q", got)
	}
	clickHeader(t, m, width, summary)
	if rows := activityRows(t, m, width); len(rows) != 9 || rows[2] != "▾ Edited b.go" {
		t.Fatalf("reopened, the summary lost a fold made under it: %q", rows)
	}
}

// Ctrl-O keeps its two states. Expanded has no budget: every run is open
// and there is no summary. Collapsed again, the folds made by hand are gone
// and the line is its one row.
func TestCtrlOExpandsPastTheRowBudget(t *testing.T) {
	const width = 100
	r, tui := budgetTurn(7).start(t)
	m := r.model
	tui.AppendAssistantText("Done.")
	tui.FinishTextTurn()
	r.endTurn(nil)
	paintChunks(m, width)
	if !m.toggleAllDisclosures(width) || m.depth != depthExpanded {
		t.Fatalf("Ctrl-O over a folded summary did not expand: depth = %v", m.depth.label())
	}
	rows := activityRows(t, m, width)
	if len(rows) != 14 || rows[0] != "▾ Read a.go" || !strings.Contains(rows[1], "read a.go") || rows[12] != "▾ Read g.go" {
		t.Fatalf("expanded shows %q, want every run open and no summary", rows)
	}
	for _, c := range activityBlocks(m, width)[0].chunks {
		if c.summary {
			t.Fatal("an expanded line has a summary")
		}
	}
	paintChunks(m, width)
	if !m.toggleAllDisclosures(width) || m.depth != depthCollapsed {
		t.Fatalf("the second Ctrl-O did not collapse: depth = %v", m.depth.label())
	}
	if got := activityRows(t, m, width); !slices.Equal(got, []string{"▸ Edited 3 files and read 4 files"}) {
		t.Fatalf("collapsed again shows %q", got)
	}

	// With the summary opened by hand, a press still expands: the runs under
	// it are closed, so the press changes what is on screen.
	clickHeader(t, m, width, activityBlocks(m, width)[0].chunks[0].key)
	paintChunks(m, width)
	if !m.toggleAllDisclosures(width) || m.depth != depthExpanded || len(m.chunkFolds) != 0 {
		t.Fatalf("Ctrl-O over an open summary: depth = %v, hand folds = %v", m.depth.label(), m.chunkFolds)
	}
}

// A call that failed in a run the summary stands for keeps its cue on the
// summary, and a row too narrow for everything gives up the count of earlier
// runs before the cue.
func TestRunsSummaryKeepsTheFailureCue(t *testing.T) {
	turn := chunkTurn{steps: [][]messages.ChatMessageToolCall{
		{bashCall("l", "make lint")}, {editCall("a")}, {bashCall("b", "make build")}, {editCall("c")},
		{readCall("d")}, {editCall("e")}, {readCall("f")},
	}, fail: []string{"l"}}
	m := turn.run(t)
	if got := activityRows(t, m, 100)[0]; got != "▸ 3 earlier runs · edited a.go and ran 2 commands · 1 failed" {
		t.Fatalf("the earlier runs' header = %q", got)
	}
	if got := activityRows(t, m, 44)[0]; got != "▸ Edited a.go and ran 2 commands · 1 failed" {
		t.Fatalf("at 44 columns the header = %q, want the lead dropped and the cue kept", got)
	}
	block := activityBlocks(m, 100)[0]
	if !block.chunks[0].failed || block.chunks[0].live {
		t.Fatalf("the summary's outcome: failed=%v live=%v", block.chunks[0].failed, block.chunks[0].live)
	}
	// The label is lit from after the lead, so a sweep never runs over the
	// count of runs.
	if len(block.activityLabels) == 0 || block.activityLabels[0].X != chunkLabelLead+len("3 earlier runs")+headerCols(chunkMarkSeparator) {
		t.Fatalf("the summary's label starts at %+v", block.activityLabels)
	}
}

// Agents launched in the earlier runs lead the summary, which stays lit
// while one of them works.
func TestRunsSummaryCountsItsAgents(t *testing.T) {
	const width = 100
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("delegate")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	child := agentCall("child", `{}`)
	tui.ModelAttemptStarted(0, 0)
	tui.AppendToolStart([]messages.ChatMessageToolCall{child})
	for i, c := range []messages.ChatMessageToolCall{readCall("a"), editCall("b"), readCall("c"), editCall("d"), readCall("e"), editCall("f")} {
		tui.ModelAttemptStarted(i+1, 0)
		tui.AppendToolStart([]messages.ChatMessageToolCall{c})
		tui.AppendToolEnd(c, "ok", time.Millisecond, nil)
	}
	rows := activityRows(t, m, width)
	if len(rows) != 5 || rows[0] != "▸ 3 earlier runs · 1 agent running, edited b.go and read a.go" {
		t.Fatalf("a working agent in the earlier runs shows %q", rows)
	}
	if summary := activityBlocks(m, width)[0].chunks[0]; !summary.live || summary.kind != activityAgents {
		t.Fatalf("the summary over a working agent: live=%v kind=%v", summary.live, summary.kind)
	}
	tui.AppendToolEnd(child, "ok", time.Millisecond, nil)
	if got := activityRows(t, m, width)[0]; got != "▸ 3 earlier runs · 1 agent, edited b.go and read a.go" {
		t.Fatalf("the agent done, the earlier runs' header = %q", got)
	}
}

// The budget counts runs. A thought with a row of its own is not one: at the
// budget's edge it adds its row, and no summary comes and goes with it.
func TestThoughtRowDoesNotCountAgainstTheRowBudget(t *testing.T) {
	const width = 100
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("delegate")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	for i, c := range []messages.ChatMessageToolCall{readCall("a"), editCall("b"), readCall("c"), editCall("d")} {
		tui.ModelAttemptStarted(i, 0)
		tui.AppendToolStart([]messages.ChatMessageToolCall{c})
		tui.AppendToolEnd(c, "ok", time.Millisecond, nil)
	}
	// The fifth run is an agent still at work, so a thought after it has the
	// row below.
	tui.ModelAttemptStarted(4, 0)
	tui.AppendToolStart([]messages.ChatMessageToolCall{agentCall("child", `{}`)})
	tui.ModelAttemptStarted(5, 0)
	tui.ShowThinking("while it works")
	thinkFor(m, 2*time.Second)
	want := []string{"▸ Read a.go", "▸ Edited b.go", "▸ Read c.go", "▸ Edited d.go", "▸ 1 agent running", thoughtGlyph + " Thinking… 2s"}
	if got := activityRows(t, m, width); !slices.Equal(got, want) {
		t.Fatalf("five runs and a thought show %q, want %q", got, want)
	}
}

// A notice ends the line as an answer does: what the turn does next opens a
// line of its own, so the one above folds whole while the turn runs on.
func TestNoticeEndsTheLineForTheRowBudget(t *testing.T) {
	const width = 100
	r, tui := budgetTurn(6).start(t)
	m := r.model
	if got := activityRows(t, m, width); len(got) != 5 || got[0] != "▸ 2 earlier runs · edited b.go and read a.go" {
		t.Fatalf("six running runs show %q", got)
	}
	tui.AppendWarning("A notice between batches")
	next := readCall("z")
	tui.ModelAttemptStarted(6, 0)
	tui.AppendToolStart([]messages.ChatMessageToolCall{next})
	blocks := activityBlocks(m, width)
	if len(blocks) != 2 {
		t.Fatalf("a notice wants two lines, got %d", len(blocks))
	}
	if got := plainStyledText(blocks[0].text); got != "▸ Edited 3 files and read 3 files" {
		t.Fatalf("the line above the notice shows %q", got)
	}
	if got := plainStyledText(blocks[1].text); got != "▸ Reading z.go" {
		t.Fatalf("the line below the notice shows %q", got)
	}
}

// A call still running when its line is over is what the summary says, as a
// run's own header would.
func TestRunsSummaryNamesACallStillRunning(t *testing.T) {
	const width = 100
	r, tui := budgetTurn(6).start(t)
	m := r.model
	slow := bashCall("s", "make all")
	tui.ModelAttemptStarted(6, 0)
	tui.AppendToolStart([]messages.ChatMessageToolCall{slow})
	tui.AppendWarning("A notice during the call")
	if got := plainStyledText(activityBlocks(m, width)[0].text); got != "▸ Running make all" {
		t.Fatalf("the line over a running call shows %q", got)
	}
	tui.AppendToolEnd(slow, "boom", time.Millisecond, errors.New("exit 2"))
	if got := plainStyledText(activityBlocks(m, width)[0].text); got != "▸ Edited 3 files, read 3 files and ran a command · 1 failed" {
		t.Fatalf("the line once that call failed shows %q", got)
	}
}
