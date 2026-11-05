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
// calls it made, a blank row between steps, the step's start since the
// prompt in the right column of its first row.
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
	if len(rows) != 5 {
		t.Fatalf("two steps want thought, call, gap, thought, call: %q", rows)
	}
	if !strings.HasPrefix(rows[0], "  Plan the review") || !strings.HasSuffix(rows[0], " 0:00") {
		t.Fatalf("first step wants its thought with the turn's start: %q", rows[0])
	}
	if !strings.Contains(rows[1], "✓ read first.go") || strings.Contains(rows[1], "0.3s") || strings.HasSuffix(strings.TrimSpace(rows[1]), "0:00") {
		t.Fatalf("a quick call wants neither a duration nor a second offset: %q", rows[1])
	}
	if strings.TrimSpace(rows[2]) != "" {
		t.Fatalf("steps want a blank row between them: %q", rows)
	}
	if !strings.HasPrefix(rows[3], "  Check the callers") || !strings.HasSuffix(rows[3], " 1:30") {
		t.Fatalf("second step wants its start in the right column: %q", rows[3])
	}
	if !strings.Contains(rows[4], "✓ read second.go") || !strings.Contains(rows[4], "2.5s") {
		t.Fatalf("a call from a second up keeps its duration inline: %q", rows[4])
	}
}

// A step whose thought is long shows its newest reasoningPreviewLines rows;
// the rows of other steps keep their own count, and the expanded state
// shows every retained row of the record both steps share.
func TestEachStepBoundsItsThought(t *testing.T) {
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
	want := []string{"First 3", "First 4", "First 5", "✓ read a.go", "", "Second step", "✓ read b.go"}
	if len(rows) != len(want) {
		t.Fatalf("rows = %q, want %d: %q", rows, len(want), want)
	}
	for i, row := range rows {
		cut := strings.HasPrefix(row, reasoningCutIndent)
		row = strings.TrimSpace(strings.TrimPrefix(row, reasoningCutIndent))
		if (row != want[i] && !strings.HasPrefix(row, want[i])) || cut != (i == 0) {
			t.Fatalf("row %d = %q, want %q, the cut part marked on its first row alone", i, row, want[i])
		}
	}
	expandLines(m, activityBlocks(m, 80)[0].reasoningIDs)
	if rows := stepRows(t, m, 80); len(rows) != 10 || !strings.Contains(rows[0], "First 0") || strings.HasPrefix(rows[0], reasoningCutIndent) {
		t.Fatalf("expanded thought wants every row: %q", rows)
	}
}

// An abridged thought is rows of text: paragraph breaks do not use it up,
// and the whole thought stacks its one-line paragraphs too.
func TestThoughtWindowSkipsBlankRows(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("think")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.ShowThinking("Alpha.\n\nBeta.\n\nGamma.\n\nDelta.")
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	rows := stepRows(t, m, 80)
	want := []string{"Beta.", "Gamma.", "Delta."}
	if len(rows) != len(want) {
		t.Fatalf("rows = %q, want %q", rows, want)
	}
	for i, row := range rows {
		if strings.TrimSpace(strings.TrimPrefix(row, reasoningCutIndent)) != want[i] || strings.HasPrefix(row, reasoningCutIndent) != (i == 0) {
			t.Fatalf("row %d = %q, want %q, the cut marked on the first row alone", i, row, want[i])
		}
	}
	expandLines(m, activityBlocks(m, 80)[0].reasoningIDs)
	if rows := stepRows(t, m, 80); len(rows) != 4 || strings.TrimSpace(rows[0]) != "Alpha." || strings.TrimSpace(rows[1]) != "Beta." {
		t.Fatalf("the whole thought wants every one-line paragraph, stacked: %q", rows)
	}
}

// The window keeps the newest calls; a step cut at its first call keeps the
// thought that led to it, and one cut among its calls loses it.
func TestRowWindowKeepsACutStepsThought(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second int
		keepsThought  bool
	}{
		{"cut at the step's first call", 3, activityRowWindow, true},
		{"cut among the step's calls", 2, activityRowWindow + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withDisplayTTY(t)
			r := newManagedREPL(&Config{}, "ctx", 0, 0)
			m := r.model
			m.beginTurn("inspect")
			tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
			for step, count := range []int{tc.first, tc.second} {
				tui.ModelAttemptStarted(step, 0)
				tui.ShowThinking(fmt.Sprintf("Step %d", step))
				for i := range count {
					call := readCall(fmt.Sprintf("s%d-%d", step, i))
					tui.AppendToolStart([]messages.ChatMessageToolCall{call})
					tui.AppendToolEnd(call, "ok", time.Millisecond, nil)
				}
			}
			tui.AppendAssistantText("Done.")
			r.endTurn(nil)

			rows := stepRows(t, m, 80)
			if got := strings.Contains(strings.TrimSpace(rows[0]), "Step 1"); got != tc.keepsThought {
				t.Fatalf("thought kept = %v, want %v: %q", got, tc.keepsThought, rows)
			}
			if strings.Contains(strings.Join(rows, "\n"), "Step 0") {
				t.Fatalf("the hidden step's thought leaked: %q", rows)
			}
			calls := 0
			for _, row := range rows {
				if strings.Contains(row, "✓ read") {
					calls++
				}
			}
			if calls != activityRowWindow {
				t.Fatalf("window wants %d calls, got %d: %q", activityRowWindow, calls, rows)
			}
		})
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
			if len(rows) != 5 {
				t.Fatalf("reloaded turn wants two steps: %q", rows)
			}
			if !strings.HasPrefix(rows[0], "  Plan the review") || !strings.HasSuffix(rows[0], " 0:00") {
				t.Fatalf("first step wants the audit's start: %q", rows[0])
			}
			if !strings.Contains(rows[1], "✓ read first.go") || strings.Contains(rows[1], "0.3s") {
				t.Fatalf("quick reloaded call wants no duration: %q", rows[1])
			}
			if strings.TrimSpace(rows[2]) != "" || !strings.HasPrefix(rows[3], "  Check the callers") || !strings.HasSuffix(rows[3], " 1:15") {
				t.Fatalf("second step wants a gap and its start: %q", rows)
			}
			if !strings.Contains(rows[4], "✓ read second.go") || !strings.Contains(rows[4], "2m10s") {
				t.Fatalf("long reloaded call keeps its duration: %q", rows[4])
			}
		})
	}
}

// Steps of bare calls run on without a blank row between them; their
// starts in the right column tell them apart.
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
	if len(rows) != 5 || strings.TrimSpace(rows[2]) != "" || !strings.HasPrefix(rows[3], "  Now a thought") {
		t.Fatalf("two bare steps, a blank row, then the thought step: %q", rows)
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
	if len(rows) != 4 || !strings.HasPrefix(rows[0], "  Look") || !strings.HasPrefix(rows[1], "  Look again") || !strings.Contains(rows[2], "a.go") || !strings.Contains(rows[3], "b.go") {
		t.Fatalf("untagged activity wants thought then calls with no gap: %q", rows)
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
	rows := m.transcriptRows(80)
	for _, link := range m.visibleToolOutputLinks(fullViewport(len(rows), 80), 0) {
		if link.action == thoughtPartAction {
			return link, true
		}
	}
	return toolOutputLink{}, false
}

// A thought longer than its abridged rows is a control: a click shows the
// whole of it and another abridges it again, with the depth untouched; a
// short thought is not.
func TestClickingAThoughtShowsAndAbridgesIt(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("think")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	for i := range 6 {
		tui.ShowThinking(fmt.Sprintf("Line %d\n", i))
	}
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	if rows := stepRows(t, m, 80); len(rows) != reasoningPreviewLines {
		t.Fatalf("abridged thought wants %d rows: %q", reasoningPreviewLines, rows)
	}
	link, ok := thoughtPartLink(m)
	if !ok || !r.activateToolOutput(m, link, nil) {
		t.Fatal("the long thought is not a control")
	}
	if rows := stepRows(t, m, 80); len(rows) != 6 || !strings.Contains(rows[0], "Line 0") || m.depth != depthAbridged {
		t.Fatalf("click did not show the whole thought with the depth untouched: %q depth=%v", rows, m.depth)
	}
	link, ok = thoughtPartLink(m)
	if !ok || !r.activateToolOutput(m, link, nil) {
		t.Fatal("the whole thought is not a control")
	}
	if rows := stepRows(t, m, 80); len(rows) != reasoningPreviewLines {
		t.Fatalf("second click did not abridge the thought: %q", rows)
	}

	// A short thought has nothing to show and is no control.
	short := newManagedREPL(&Config{}, "ctx", 0, 0)
	short.model.beginTurn("think")
	shortUI := &gotuiTurnUI{repl: short, model: short.model, config: short.config, turnID: short.model.turnID}
	shortUI.ShowThinking("Brief.")
	shortUI.AppendAssistantText("Done.")
	short.endTurn(nil)
	if _, ok := thoughtPartLink(short.model); ok {
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
// blank rows.
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
	if len(rows) < 4 || blank(rows[1]) || blank(rows[2]) || !strings.Contains(rows[3], "read first.go") {
		t.Fatalf("one-line summaries want stacking: %q", rows)
	}
	if !blank(rows[4]) {
		t.Fatalf("the next step wants its gap: %q", rows)
	}
	rest := rows[5:]
	cut := -1
	for i, row := range rest {
		if blank(row) {
			cut = i
			break
		}
	}
	if cut < 2 || !strings.HasPrefix(rest[cut+1], "  Comparing main history") || !strings.Contains(rest[cut+2], "read second.go") {
		t.Fatalf("prose wants its paragraph break kept: %q", rest)
	}
}
