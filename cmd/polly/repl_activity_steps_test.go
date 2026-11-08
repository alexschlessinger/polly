package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	rw "github.com/mattn/go-runewidth"
)

// stepRows returns the rows under the first activity line, plain, with the
// step offsets the right column carries.
func stepRows(t *testing.T, m *replModel, width int) []string {
	t.Helper()
	blocks := activityBlocks(m, width)
	if len(blocks) == 0 {
		t.Fatal("no activity line")
	}
	return strings.Split(plainStyledText(blocks[0].text), "\n")[1:]
}

func readCall(id string) messages.ChatMessageToolCall {
	return messages.ChatMessageToolCall{ID: id, Name: "read_file", Arguments: fmt.Sprintf(`{"path":"%s.go"}`, id)}
}

// A live turn lays its steps out as they ran: each step's thought above the
// calls it made, one blank row wherever thoughts and calls meet, the step's
// start since the prompt in the right column of its first row.
func TestOpenLineLaysStepsOutInOrder(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("review")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.ModelAttemptStarted(0, 0)
	tui.ShowThinking("Plan the review")
	tui.AppendToolStart([]messages.ChatMessageToolCall{readCall("first")})
	tui.AppendToolEnd(readCall("first"), "ok", 300*time.Millisecond, nil)
	// The second response begins a minute and a half in.
	m.mu.Lock()
	m.turnStarted = time.Now().Add(-90 * time.Second)
	m.mu.Unlock()
	tui.ModelAttemptStarted(1, 0)
	tui.ShowThinking("Check the callers")
	tui.AppendToolStart([]messages.ChatMessageToolCall{readCall("second")})
	tui.AppendToolEnd(readCall("second"), "ok", 2500*time.Millisecond, nil)
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)

	rows := stepRows(t, m, 80)
	if len(rows) != 7 || rows[1] != activityIndent || rows[3] != activityIndent || rows[5] != activityIndent {
		t.Fatalf("two steps want thought, call, thought, call, a blank row between each: %q", rows)
	}
	if !strings.HasPrefix(rows[0], "  Plan the review") || !strings.HasSuffix(rows[0], " 0:00") {
		t.Fatalf("first step wants its thought with the turn's start: %q", rows[0])
	}
	if !strings.Contains(rows[2], "✓ read first.go") || strings.Contains(rows[2], "0.3s") || strings.HasSuffix(strings.TrimSpace(rows[2]), "0:00") {
		t.Fatalf("a quick call wants neither a duration nor a second offset: %q", rows[2])
	}
	if !strings.HasPrefix(rows[4], "  Check the callers") || !strings.HasSuffix(rows[4], " 1:30") {
		t.Fatalf("second step wants its start in the right column: %q", rows)
	}
	if !strings.Contains(rows[6], "✓ read second.go") || !strings.Contains(rows[6], "2.5s") {
		t.Fatalf("a call from a second up keeps its duration inline: %q", rows[6])
	}
}

// An abridged line is a log of runs: a thought of six rows is one run and
// keeps its newest activityRunRows, and the area, whose blank rows between
// thoughts and calls count, keeps the newest of those; the first shown is
// marked as cut and carries the step's start. The full depth shows every
// row of the thought once its part is whole.
func TestAbridgedLineCutsALongRun(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("think")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.ModelAttemptStarted(0, 0)
	for i := range 6 {
		tui.ShowThinking(fmt.Sprintf("First %d\n", i))
	}
	tui.AppendToolStart([]messages.ChatMessageToolCall{readCall("a")})
	tui.AppendToolEnd(readCall("a"), "ok", time.Millisecond, nil)
	tui.ModelAttemptStarted(1, 0)
	tui.ShowThinking("Second step")
	tui.AppendToolStart([]messages.ChatMessageToolCall{readCall("b")})
	tui.AppendToolEnd(readCall("b"), "ok", time.Millisecond, nil)
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)

	rows := stepRows(t, m, 80)
	want := []string{"First 4", "First 5", "", "✓ read a.go", "", "Second step", "", "✓ read b.go"}
	if len(rows) != len(want) || len(rows) != activityTailRows {
		t.Fatalf("rows = %q, want the long run cut to fill the area: %q", rows, want)
	}
	for i, row := range rows {
		cut := strings.HasPrefix(row, reasoningCutIndent)
		row = strings.TrimSpace(strings.TrimPrefix(row, reasoningCutIndent))
		if (want[i] == "" && row != "") || !strings.HasPrefix(row, want[i]) || cut != (i == 0) {
			t.Fatalf("row %d = %q, want %q, the cut run marked on its first visible row alone", i, row, want[i])
		}
	}
	if !strings.HasSuffix(rows[0], " 0:00") {
		t.Fatalf("the step's start wants its first visible entry: %q", rows)
	}
	expandLines(m, activityBlocks(m, 80)[0].reasoningIDs)
	// Expanded, every row shows, one blank row between thought and calls.
	full := []string{"First 0", "First 1", "First 2", "First 3", "First 4", "First 5", "", "✓ read a.go", "", "Second step", "", "✓ read b.go"}
	rows = stepRows(t, m, 80)
	if len(rows) != len(full) || strings.HasPrefix(rows[0], reasoningCutIndent) {
		t.Fatalf("expanded line wants %q: %q", full, rows)
	}
	for i, row := range rows {
		if row = strings.TrimSpace(row); (full[i] == "" && row != "") || !strings.HasPrefix(row, full[i]) {
			t.Fatalf("expanded row %d = %q, want %q: %q", i, row, full[i], rows)
		}
	}
}

// A step's start sits on its first visible entry: a step whose thought run
// is cut carries it on the marked row, not on a hidden one.
func TestStepStartMovesToItsFirstVisibleEntry(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("think")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.ModelAttemptStarted(0, 0)
	tui.AppendToolStart([]messages.ChatMessageToolCall{readCall("a")})
	tui.AppendToolEnd(readCall("a"), "ok", time.Millisecond, nil)
	// The second response begins a minute and a half in.
	m.mu.Lock()
	m.turnStarted = time.Now().Add(-90 * time.Second)
	m.mu.Unlock()
	tui.ModelAttemptStarted(1, 0)
	for i := range activityRunRows + 2 {
		tui.ShowThinking(fmt.Sprintf("Later %d\n", i))
	}
	tui.AppendToolStart([]messages.ChatMessageToolCall{readCall("b")})
	tui.AppendToolEnd(readCall("b"), "ok", time.Millisecond, nil)
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)

	rows := stepRows(t, m, 80)
	if len(rows) != activityRunRows+4 || !strings.Contains(rows[0], "read a.go") || !strings.HasSuffix(rows[0], " 0:00") {
		t.Fatalf("the first step wants its call with its start: %q", rows)
	}
	if rows[1] != activityIndent || rows[activityRunRows+2] != activityIndent {
		t.Fatalf("the thought run wants a blank row on either side: %q", rows)
	}
	if !strings.HasPrefix(rows[2], reasoningCutIndent+"Later 2") || !strings.HasSuffix(rows[2], " 1:30") {
		t.Fatalf("the second step's start wants its first visible row, marked as cut: %q", rows)
	}
	for _, row := range rows[3:] {
		if strings.Contains(row, "1:30") {
			t.Fatalf("the second step's start shows twice: %q", rows)
		}
	}
}

// An abridged thought is rows of text: paragraph breaks do not take rows
// of the tail, and a thought that fits shows whole with no cut mark; the
// whole thought at full depth stacks its one-line paragraphs too.
func TestThoughtTailSkipsBlankRows(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("think")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.ShowThinking("Alpha.\n\nBeta.\n\nGamma.\n\nDelta.")
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	rows := stepRows(t, m, 80)
	want := []string{"Alpha.", "Beta.", "Gamma.", "Delta."}
	if len(rows) != len(want) {
		t.Fatalf("rows = %q, want %q", rows, want)
	}
	for i, row := range rows {
		if strings.TrimSpace(row) != want[i] || strings.HasPrefix(row, reasoningCutIndent) {
			t.Fatalf("row %d = %q, want %q and no cut mark", i, row, want[i])
		}
	}
	expandLines(m, activityBlocks(m, 80)[0].reasoningIDs)
	if rows := stepRows(t, m, 80); len(rows) != 4 || strings.TrimSpace(rows[0]) != "Alpha." || strings.TrimSpace(rows[1]) != "Beta." {
		t.Fatalf("the whole thought wants every one-line paragraph, stacked: %q", rows)
	}
}

// abridgedLog is the abridged layout of a log of settled entries, none
// open, by the rules: each run keeps its newest activityRunRows entries,
// the area the newest activityTailRows rows of those, counting the blank
// row ("") that stands between a thought and a call. An entry whose
// predecessor in its run is hidden is marked as cut, and so is the top row
// when anything above it is hidden: a cut run's or, between runs, the
// area's.
func abridgedLog(log []string, thought []bool) (rows []string, marked []bool) {
	visible := make([]bool, len(log))
	run := make([]int, len(log))
	for i := range log {
		if i > 0 {
			run[i] = run[i-1]
			if thought[i] != thought[i-1] {
				run[i]++
			}
		}
	}
	for i := range log {
		later := 0
		for j := i + 1; j < len(log) && run[j] == run[i]; j++ {
			later++
		}
		visible[i] = later < activityRunRows
	}
	shown, below := 0, -1
	for i := len(log) - 1; i >= 0; i-- {
		if !visible[i] {
			continue
		}
		cost := 1
		if below >= 0 && thought[below] != thought[i] {
			cost++
		}
		if shown += cost; shown > activityTailRows {
			for j := i; j >= 0; j-- {
				visible[j] = false
			}
			break
		}
		below = i
	}
	above := -1
	for i := range log {
		if !visible[i] {
			continue
		}
		if above >= 0 && thought[above] != thought[i] {
			rows, marked = append(rows, ""), append(marked, false)
		}
		rows = append(rows, log[i])
		marked = append(marked, i > 0 && (above < 0 || run[i-1] == run[i] && !visible[i-1]))
		above = i
	}
	return rows, marked
}

// A log of alternating runs fills activityTailRows rows, the blank rows
// between thoughts and calls among them: as thoughts and calls arrive, older runs leave only off the top, a run past
// activityRunRows loses its oldest entries from inside, and the cut mark
// shows exactly on an entry whose run lost the one before it and on the
// top row once anything above it left.
func TestAlternatingRunsFillTheTail(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	// The full log, oldest first, as the turn writes it.
	var log []string
	var thoughts []bool
	thought := func(text string) {
		tui.ShowThinking(text + "\n")
		log, thoughts = append(log, text), append(thoughts, true)
	}
	call := func(id string) {
		c := readCall(id)
		tui.AppendToolStart([]messages.ChatMessageToolCall{c})
		tui.AppendToolEnd(c, "ok", time.Millisecond, nil)
		log, thoughts = append(log, "✓ read "+id+".go"), append(thoughts, false)
	}
	check := func() []string {
		t.Helper()
		rows := stepRows(t, m, 80)
		want, marked := abridgedLog(log, thoughts)
		if len(rows) != len(want) || len(rows) > activityTailRows {
			t.Fatalf("log of %d entries shows %q, want %q", len(log), rows, want)
		}
		for i, row := range rows {
			cut := strings.HasPrefix(row, reasoningCutIndent)
			text := strings.TrimSpace(strings.TrimPrefix(row, reasoningCutIndent))
			if (want[i] == "" && row != activityIndent) || !strings.HasPrefix(text, want[i]) || cut != marked[i] {
				t.Fatalf("row %d = %q, want %q marked %v: %q", i, row, want[i], marked[i], rows)
			}
		}
		return rows
	}
	steps := []struct{ thoughts, calls int }{{3, 2}, {2, 3}, {1, 6}, {2, 1}}
	for step, size := range steps {
		tui.ModelAttemptStarted(step, 0)
		for i := range size.thoughts {
			thought(fmt.Sprintf("Plan %d-%d", step, i))
			check()
		}
		for i := range size.calls {
			call(fmt.Sprintf("s%d-%d", step, i))
			check()
		}
	}
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	// Spelled out: the six-call run keeps its newest four, and the two blank
	// rows below it leave room for three, the first marked; the runs above
	// it left off the top.
	rows := check()
	want := []string{"… ✓ read s2-3.go", "✓ read s2-4.go", "✓ read s2-5.go", "", "Plan 3-0", "Plan 3-1", "", "✓ read s3-0.go"}
	if len(rows) != len(want) {
		t.Fatalf("rows = %q, want %q", rows, want)
	}
	for i, row := range rows {
		if (want[i] == "" && row != activityIndent) || !strings.HasPrefix(strings.TrimLeft(row, " "), want[i]) {
			t.Fatalf("row %d = %q, want %q: %q", i, row, want[i], rows)
		}
	}
}

// A call still running stays in its run however old it is, and takes a row
// of the area: the run shows it, then its newest activityRunRows calls,
// the first of those marked for the settled calls between them.
func TestRunningCallStaysInACutRun(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	// A thought that, with the blank row, the running call and the newest
	// calls, is one row more than the area holds.
	for i := range activityTailRows - activityRunRows - 1 {
		tui.ShowThinking(fmt.Sprintf("Look %d\n", i))
	}
	tui.AppendToolStart([]messages.ChatMessageToolCall{readCall("slow")})
	for i := range activityRunRows + 2 {
		c := readCall(fmt.Sprintf("quick-%d", i))
		tui.AppendToolStart([]messages.ChatMessageToolCall{c})
		tui.AppendToolEnd(c, "ok", time.Millisecond, nil)
	}
	rows := stepRows(t, m, 80)
	// The running call takes a row of the area: the thought's oldest row
	// leaves the top. Then the blank row, the running call, a gap of two
	// settled calls, the newest four.
	if len(rows) != activityTailRows || !strings.HasPrefix(rows[0], reasoningCutIndent+"Look 1") {
		t.Fatalf("rows = %q, want %d, the thought's first row gone", rows, activityTailRows)
	}
	if rows[2] != activityIndent {
		t.Fatalf("the thought wants a blank row below it: %q", rows)
	}
	rows = rows[activityTailRows-activityRunRows-2:]
	if !strings.Contains(rows[1], "slow.go") || strings.HasPrefix(rows[1], reasoningCutIndent) || strings.Contains(rows[1], "✓") {
		t.Fatalf("the running call wants its own unmarked row: %q", rows)
	}
	if !strings.HasPrefix(rows[2], reasoningCutIndent) || !strings.Contains(rows[2], "quick-2.go") {
		t.Fatalf("the entry after the gap wants the cut mark: %q", rows)
	}
	for i, row := range rows[3:] {
		if strings.HasPrefix(row, reasoningCutIndent) || !strings.Contains(row, fmt.Sprintf("quick-%d.go", i+3)) {
			t.Fatalf("row %d = %q, want quick-%d unmarked: %q", i+3, row, i+3, rows)
		}
	}
	// Settled, it is an old call like the others and leaves the run.
	tui.AppendToolEnd(readCall("slow"), "ok", time.Millisecond, nil)
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	if rows := stepRows(t, m, 80); len(rows) != activityTailRows || strings.Contains(strings.Join(rows, "\n"), "slow.go") || !strings.Contains(rows[0], "Look 0") || !strings.HasPrefix(rows[activityTailRows-activityRunRows], reasoningCutIndent) {
		t.Fatalf("a settled old call wants to leave the run: %q", rows)
	}
}

// A reloaded turn lays its steps out the same way, with each step's start
// taken from the turn audit's attempts.
func TestHydratedTurnLaysStepsOutWithAuditTimes(t *testing.T) {
	withDisplayTTY(t)
	started := time.Date(2026, 11, 5, 0, 37, 45, 0, time.UTC)
	withID := func(msg messages.ChatMessage, id string) messages.ChatMessage {
		if msg.Metadata == nil {
			msg.Metadata = map[string]any{}
		}
		msg.Metadata[messages.MetadataKeyDisplayID] = id
		return msg
	}
	result := func(call messages.ChatMessageToolCall, d time.Duration) messages.ChatMessage {
		msg := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: "ok"}
		msg.SetToolSucceeded(true)
		msg.SetToolDuration(d)
		return msg
	}
	first, second := readCall("first"), readCall("second")
	finished := started.Add(4 * time.Minute)
	audit := messages.TurnAudit{Version: 1, ID: "turn", StartedAt: started, RunFinishedAt: finished, Status: "completed", Attempts: []messages.ModelAttemptAudit{
		{ID: "step-0", Purpose: "conversation", StartedAt: started.Add(200 * time.Millisecond)},
		{ID: "step-1", Purpose: "conversation", StartedAt: started.Add(75 * time.Second)},
		{ID: "step-2", Purpose: "conversation", StartedAt: started.Add(3*time.Minute + 59*time.Second)},
	}}
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "review"},
		withID(messages.ChatMessage{Role: messages.MessageRoleAssistant, Reasoning: "Plan the review", ToolCalls: []messages.ChatMessageToolCall{first}}, "step-0"),
		result(first, 300*time.Millisecond),
		withID(messages.ChatMessage{Role: messages.MessageRoleAssistant, Reasoning: "Check the callers", ToolCalls: []messages.ChatMessageToolCall{second}}, "step-1"),
		result(second, 130*time.Second),
		withID(messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "Done."}, "step-2"),
		{Role: messages.MessageRoleInternal, Metadata: map[string]any{messages.MetadataKeyTurnAudit: audit}},
	}
	for _, typed := range []bool{true, false} {
		t.Run(fmt.Sprintf("typed audit %v", typed), func(t *testing.T) {
			msgs := append([]messages.ChatMessage(nil), history...)
			if !typed {
				// A stored audit comes back as generic JSON.
				msgs[len(msgs)-1] = messages.ChatMessage{Role: messages.MessageRoleInternal, Metadata: map[string]any{messages.MetadataKeyTurnAudit: map[string]any{
					"version": 1, "id": "turn", "started_at": started.Format(time.RFC3339Nano), "run_finished_at": finished.Format(time.RFC3339Nano), "status": "completed",
					"attempts": []any{
						map[string]any{"id": "step-0", "started_at": started.Add(200 * time.Millisecond).Format(time.RFC3339Nano)},
						map[string]any{"id": "step-1", "started_at": started.Add(75 * time.Second).Format(time.RFC3339Nano)},
					},
				}}}
			}
			m := newReplModel()
			m.hydrateHistory(msgs, "ctx")
			rows := stepRows(t, m, 80)
			if len(rows) != 7 || rows[1] != activityIndent || rows[3] != activityIndent || rows[5] != activityIndent {
				t.Fatalf("reloaded turn wants two steps, a blank row between thought and call: %q", rows)
			}
			if !strings.HasPrefix(rows[0], "  Plan the review") || !strings.HasSuffix(rows[0], " 0:00") {
				t.Fatalf("first step wants the audit's start: %q", rows[0])
			}
			if !strings.Contains(rows[2], "✓ read first.go") || strings.Contains(rows[2], "0.3s") {
				t.Fatalf("quick reloaded call wants no duration: %q", rows[2])
			}
			if !strings.HasPrefix(rows[4], "  Check the callers") || !strings.HasSuffix(rows[4], " 1:15") {
				t.Fatalf("second step wants its start: %q", rows)
			}
			if !strings.Contains(rows[6], "✓ read second.go") || !strings.Contains(rows[6], "2m10s") {
				t.Fatalf("long reloaded call keeps its duration: %q", rows[6])
			}
		})
	}
}

// Steps run on without a blank row between them where calls follow calls;
// only a thought sets a blank row on either side. Their starts in the right
// column tell them apart.
func TestThoughtlessStepsRunOn(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("review")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	for step := range 2 {
		m.mu.Lock()
		m.turnStarted = time.Now().Add(-time.Duration(step) * 10 * time.Second)
		m.mu.Unlock()
		tui.ModelAttemptStarted(step, 0)
		call := readCall(fmt.Sprintf("s%d", step))
		tui.AppendToolStart([]messages.ChatMessageToolCall{call})
		tui.AppendToolEnd(call, "ok", time.Millisecond, nil)
	}
	tui.ModelAttemptStarted(2, 0)
	tui.ShowThinking("Now a thought")
	tui.AppendToolStart([]messages.ChatMessageToolCall{readCall("s2")})
	tui.AppendToolEnd(readCall("s2"), "ok", time.Millisecond, nil)
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)

	rows := stepRows(t, m, 80)
	if len(rows) != 6 || rows[2] != activityIndent || !strings.HasPrefix(rows[3], "  Now a thought") || rows[4] != activityIndent || !strings.Contains(rows[5], "s2.go") {
		t.Fatalf("two bare steps, then the thought step set apart by blank rows: %q", rows)
	}
	for i, want := range []string{"0:00", "0:10"} {
		if !strings.Contains(rows[i], fmt.Sprintf("s%d.go", i)) || !strings.HasSuffix(rows[i], " "+want) {
			t.Fatalf("bare step %d wants its start at the right: %q", i, rows[i])
		}
	}
}

// Without an attempt hook or an audit, a turn is one step with no offsets,
// as it was.
func TestUntaggedActivityIsOneStep(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.ShowThinking("Look")
	tui.AppendToolStart([]messages.ChatMessageToolCall{readCall("a")})
	tui.AppendToolEnd(readCall("a"), "ok", time.Millisecond, nil)
	tui.ShowThinking("Look again")
	tui.AppendToolStart([]messages.ChatMessageToolCall{readCall("b")})
	tui.AppendToolEnd(readCall("b"), "ok", time.Millisecond, nil)
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	rows := stepRows(t, m, 80)
	if len(rows) != 5 || !strings.HasPrefix(rows[0], "  Look") || !strings.HasPrefix(rows[1], "  Look again") || rows[2] != activityIndent || !strings.Contains(rows[3], "a.go") || !strings.Contains(rows[4], "b.go") {
		t.Fatalf("untagged activity wants thought, one blank row, then calls: %q", rows)
	}
	for _, row := range rows {
		if strings.Contains(row, ":0") {
			t.Fatalf("no offset without a clock: %q", row)
		}
	}
}

func TestStepOffsetLabel(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{{0, "0:00"}, {6 * time.Second, "0:06"}, {73 * time.Second, "1:13"}, {3661 * time.Second, "1:01:01"}, {499 * time.Millisecond, "0:00"}, {500 * time.Millisecond, "0:01"}} {
		if got := stepOffsetLabel(tc.d); got != tc.want {
			t.Fatalf("stepOffsetLabel(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// Compaction that drops the front of the tail moves the segments with it;
// a segment left with nothing merges into the one that owns the front.
func TestReasoningSegmentsSurviveCompaction(t *testing.T) {
	record := &reasoningRecord{}
	record.beginSegment(0, 0, false, false, false)
	appendReasoningTail(record, strings.Repeat("a", 10), false)
	record.beginSegment(1, time.Second, true, true, false)
	record.trimSegments(appendReasoningTail(record, strings.Repeat("b", 10), true))
	if len(record.segments) != 2 || record.segments[1].start != 11 {
		t.Fatalf("segments = %+v", record.segments)
	}
	record.beginSegment(2, 2*time.Second, true, true, false)
	record.trimSegments(appendReasoningTail(record, strings.Repeat("c\n", reasoningTailLimitRunes), true))
	if len(record.segments) != 1 || record.segments[0].step != 2 || record.segments[0].start != 0 || !record.tailCut {
		t.Fatalf("compaction wants the newest segment alone at the front: %+v cut=%v", record.segments, record.tailCut)
	}
	if texts := record.segmentTexts(); len(texts) != 1 || !strings.HasPrefix(strings.TrimLeft(texts[0], "\n"), "c") {
		t.Fatalf("segment texts = %q", texts)
	}
}

// thoughtPartLink finds the click target of a thought part, if any.
func thoughtPartLink(m *replModel) (toolOutputLink, bool) {
	return outputLinkWithAction(m, thoughtPartAction)
}

// outputLinkWithAction finds the first click target with the action.
func outputLinkWithAction(m *replModel, action string) (toolOutputLink, bool) {
	rows := m.transcriptRows(80)
	for _, link := range m.visibleToolOutputLinks(fullViewport(len(rows), 80), 0) {
		if link.action == action {
			return link, true
		}
	}
	return toolOutputLink{}, false
}

// An abridged thought run longer than activityRunRows is a control on its
// marked row: a click opens the run whole, its older rows taking none of
// the area so nothing else leaves, and a click on its first row confines
// it again. At full depth a thought part is the control instead: a click
// cuts it to its newest reasoningPreviewLines rows, another shows it whole.
// The depth never changes, and a short thought is no control.
func TestClickingAThoughtOpensAndConfinesIt(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("think")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	for i := range 6 {
		tui.ShowThinking(fmt.Sprintf("Line %d\n", i))
	}
	for i := range activityRunRows {
		tui.AppendToolStart([]messages.ChatMessageToolCall{readCall(fmt.Sprintf("c%d", i))})
		tui.AppendToolEnd(readCall(fmt.Sprintf("c%d", i)), "ok", time.Millisecond, nil)
	}
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	confined := func(stage string) {
		t.Helper()
		rows := stepRows(t, m, 80)
		// The blank row takes one of the area's rows from the thought run.
		if len(rows) != activityTailRows || !strings.HasPrefix(rows[0], reasoningCutIndent+"Line 3") || rows[activityRunRows-1] != activityIndent {
			t.Fatalf("%s: the thought run wants its newest %d rows, marked, then a blank row: %q", stage, activityRunRows-1, rows)
		}
		if _, ok := thoughtPartLink(m); ok {
			t.Fatalf("%s: an abridged line offered a part control", stage)
		}
	}
	confined("abridged")
	link, ok := outputLinkWithAction(m, openCutAction)
	if !ok || !r.activateToolOutput(m, link, nil) {
		t.Fatal("the cut thought run is not a control")
	}
	rows := stepRows(t, m, 80)
	if len(rows) != 6+1+activityRunRows || !strings.HasPrefix(rows[0], "  Line 0") || rows[6] != activityIndent || !strings.Contains(rows[len(rows)-1], "c3.go") || m.depth != depthAbridged {
		t.Fatalf("click did not open the whole run beside every call, depth untouched: %q depth=%v", rows, m.depth)
	}
	for _, row := range rows {
		if strings.HasPrefix(row, reasoningCutIndent) {
			t.Fatalf("an open run wants no cut mark: %q", rows)
		}
	}
	link, ok = outputLinkWithAction(m, openCutAction)
	if !ok || !r.activateToolOutput(m, link, nil) {
		t.Fatal("the open run is not a control")
	}
	confined("closed again")

	m.toggleAllDisclosures(80)
	if m.depth != depthFull {
		t.Fatalf("depth = %v, want full", m.depth)
	}
	if rows := stepRows(t, m, 80); len(rows) != 6+1+activityRunRows || !strings.Contains(rows[0], "Line 0") || strings.TrimSpace(rows[6]) != "" {
		t.Fatalf("full depth wants the whole thought: %q", rows)
	}
	if _, ok := outputLinkWithAction(m, openCutAction); ok {
		t.Fatal("the full depth offered a run control")
	}
	link, ok = thoughtPartLink(m)
	if !ok || !r.activateToolOutput(m, link, nil) {
		t.Fatal("the long thought is not a control at full depth")
	}
	if rows := stepRows(t, m, 80); len(rows) != reasoningPreviewLines+1+activityRunRows || !strings.HasPrefix(rows[0], reasoningCutIndent) || !strings.Contains(rows[0], "Line 3") || m.depth != depthFull {
		t.Fatalf("click did not abridge the thought with the depth untouched: %q depth=%v", rows, m.depth)
	}
	link, ok = thoughtPartLink(m)
	if !ok || !r.activateToolOutput(m, link, nil) {
		t.Fatal("the abridged thought is not a control")
	}
	if rows := stepRows(t, m, 80); len(rows) != 6+1+activityRunRows || !strings.Contains(rows[0], "Line 0") || strings.HasPrefix(rows[0], reasoningCutIndent) {
		t.Fatalf("second click did not show the whole thought: %q", rows)
	}

	// A short thought has nothing to show and is no control at any depth.
	short := newManagedREPL(&Config{}, "ctx", 0, 0)
	short.model.beginTurn("think")
	shortUI := &gotuiTurnUI{repl: short, model: short.model, config: short.config, turnID: short.model.turnID}
	shortUI.ShowThinking("Brief.")
	shortUI.AppendAssistantText("Done.")
	short.endTurn(nil)
	if _, ok := outputLinkWithAction(short.model, openCutAction); ok {
		t.Fatal("a short abridged thought became a control")
	}
	short.model.toggleAllDisclosures(80)
	if _, ok := thoughtPartLink(short.model); ok || short.model.depth != depthFull {
		t.Fatal("a short thought became a control")
	}
}

// Every row of an open line keeps the step column: a wrapped thought
// carries its step's start on its first row, not the first row with room,
// and a call row with no start of its own ends where the others do.
func TestStepColumnHoldsEveryRow(t *testing.T) {
	withDisplayTTY(t)
	const width = 80
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("review")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.ModelAttemptStarted(0, 0)
	tui.ShowThinking(strings.Repeat("words that wrap over rows ", 5) + "end.")
	long := messages.ChatMessageToolCall{ID: "long", Name: "bash", Arguments: `{"command":"` + strings.Repeat("grep -n pattern file.go ; ", 6) + `"}`}
	tui.AppendToolStart([]messages.ChatMessageToolCall{long, readCall("short")})
	tui.AppendToolEnd(long, "ok", 300*time.Millisecond, nil)
	tui.AppendToolEnd(readCall("short"), "ok", 300*time.Millisecond, nil)
	m.mu.Lock()
	m.turnStarted = time.Now().Add(-65 * time.Second)
	m.mu.Unlock()
	tui.ModelAttemptStarted(1, 0)
	tui.AppendToolStart([]messages.ChatMessageToolCall{readCall("later")})
	tui.AppendToolEnd(readCall("later"), "ok", 300*time.Millisecond, nil)
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)

	rows := stepRows(t, m, width)
	if len(rows) < 4 || !strings.HasPrefix(rows[0], "  words that wrap") || !strings.HasSuffix(rows[0], " 0:00") {
		t.Fatalf("a wrapped thought wants its step's start on its first row: %q", rows)
	}
	edge := width - stepColumn(width)
	for _, row := range rows[1 : len(rows)-1] {
		if w := rw.StringWidth(row); w > edge || strings.HasSuffix(row, ":00") {
			t.Fatalf("a thought or call row runs past the step column (%d > %d): %q", w, edge, row)
		}
	}
	last := rows[len(rows)-1]
	if !strings.Contains(last, "read later.go") || !strings.HasSuffix(last, " 1:05") || rw.StringWidth(last) != width {
		t.Fatalf("the next step's call wants its start at the edge: %q", last)
	}
	if !strings.Contains(strings.Join(rows, "\n"), "✓ $ grep -n") {
		t.Fatalf("the long command's row is missing: %q", rows)
	}

	// A narrow pane has no column: its rows take the whole width.
	narrow := stepRows(t, m, 40)
	widest := 0
	for _, row := range narrow {
		widest = max(widest, rw.StringWidth(row))
	}
	if stepColumn(40) != 0 || widest != 40 {
		t.Fatalf("a narrow pane wants its rows at full width: %q", narrow)
	}
}

// A whole thought keeps a paragraph break only after prose of more than one
// row; one-line summaries stack, so full depth adds text over abridged, not
// blank rows, beyond the one blank row that sets thought and calls apart.
func TestWholeThoughtStacksOneLineSummaries(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("velocity")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.ModelAttemptStarted(0, 0)
	tui.ShowThinking("Checking Git history dates\n\nChecking reachable Git history\n\nConfirming the date window")
	tui.AppendToolStart([]messages.ChatMessageToolCall{readCall("first")})
	tui.AppendToolEnd(readCall("first"), "ok", time.Millisecond, nil)
	tui.ModelAttemptStarted(1, 0)
	tui.ShowThinking(strings.Repeat("prose that runs over more than one row ", 4) + "ends.\n\nComparing main history")
	tui.AppendToolStart([]messages.ChatMessageToolCall{readCall("second")})
	tui.AppendToolEnd(readCall("second"), "ok", time.Millisecond, nil)
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	for m.depth != depthFull {
		m.toggleAllDisclosures(80)
	}
	rows := stepRows(t, m, 80)
	blank := func(row string) bool { return strings.TrimSpace(row) == "" }
	for i := 1; i < len(rows); i++ {
		if blank(rows[i]) && blank(rows[i-1]) {
			t.Fatalf("rows %d and %d are both blank: %q", i-1, i, rows)
		}
	}
	if len(rows) < 6 || blank(rows[1]) || blank(rows[2]) || !blank(rows[3]) || !strings.Contains(rows[4], "read first.go") {
		t.Fatalf("one-line summaries want stacking, one blank row above the calls: %q", rows)
	}
	if !blank(rows[5]) {
		t.Fatalf("the next step's thought wants one blank row below the calls: %q", rows)
	}
	rest := rows[6:]
	cut := -1
	for i, row := range rest {
		if blank(row) {
			cut = i
			break
		}
	}
	if cut < 2 || len(rest) < cut+4 || !strings.HasPrefix(rest[cut+1], "  Comparing main history") || !blank(rest[cut+2]) || !strings.Contains(rest[cut+3], "read second.go") {
		t.Fatalf("prose wants its paragraph break kept: %q", rest)
	}
}
