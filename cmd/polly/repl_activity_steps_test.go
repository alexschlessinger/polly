package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	rw "github.com/mattn/go-runewidth"
)

// stepRows expands the view and returns the rows under the first chunk
// header of the first activity line, plain, with the step starts the right
// column carries.
func stepRows(t *testing.T, m *replModel, width int) []string {
	t.Helper()
	m.depth, m.chunkFolds = depthExpanded, nil
	m.visual.invalidate()
	blocks := activityBlocks(m, width)
	if len(blocks) == 0 {
		t.Fatal("no activity line")
	}
	return strings.Split(plainStyledText(blocks[0].text), "\n")[1:]
}

func readCall(id string) messages.ChatMessageToolCall {
	return messages.ChatMessageToolCall{ID: id, Name: "read_file", Arguments: fmt.Sprintf(`{"path":"%s.go"}`, id)}
}

// An open chunk lays its steps out as they ran: each step's thought row
// above the calls it made, no blank row between steps, and the step's start
// since the prompt in the right column of its first row.
func TestOpenChunkLaysStepsOutInOrder(t *testing.T) {
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
	if len(rows) != 4 {
		t.Fatalf("two steps want thought, call, thought, call and nothing between: %q", rows)
	}
	if !strings.HasPrefix(rows[0], "  "+thoughtGlyph+" thought") || !strings.HasSuffix(rows[0], " 0:00") || strings.Contains(strings.Join(rows, "\n"), "Plan the review") {
		t.Fatalf("first step wants its thought as one row with the turn's start, its text closed: %q", rows)
	}
	if !strings.Contains(rows[1], "✓ read first.go") || strings.Contains(rows[1], "0.3s") || strings.HasSuffix(strings.TrimSpace(rows[1]), "0:00") {
		t.Fatalf("a quick call wants neither a duration nor a second offset: %q", rows[1])
	}
	if !strings.HasPrefix(rows[2], "  "+thoughtGlyph+" thought") || !strings.HasSuffix(rows[2], " 1:30") {
		t.Fatalf("second step wants its start in the right column: %q", rows[2])
	}
	if !strings.Contains(rows[3], "✓ read second.go") || !strings.Contains(rows[3], "2.5s") {
		t.Fatalf("a call from a second up keeps its duration inline: %q", rows[3])
	}
}

// A thought opens by a click on its own row and shows every retained row of
// that one step, under the row and above the step's calls. The other step's
// thought, which the same record holds, stays closed; a second click hides
// the text again.
func TestOpenedThoughtShowsItsStepAlone(t *testing.T) {
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
	if len(rows) != 4 || strings.Contains(strings.Join(rows, "\n"), "First") || strings.Contains(strings.Join(rows, "\n"), "Second step") {
		t.Fatalf("expanding the chunk wants two thought rows, two calls and no text: %q", rows)
	}
	record := m.reasoningRecords.get(activityBlocks(m, 80)[0].reasoningIDs[0])
	if len(record.segments) != 2 {
		t.Fatalf("one record should hold both steps' thoughts: %+v", record.segments)
	}
	clickThought(t, m, 80, record.id, 0)
	rows = stepRows(t, m, 80)
	want := []string{thoughtGlyph + " thought", "First 0", "First 1", "First 2", "First 3", "First 4", "First 5", "", "✓ read a.go", thoughtGlyph + " thought", "✓ read b.go"}
	if len(rows) != len(want) {
		t.Fatalf("rows = %q, want %d: %q", rows, len(want), want)
	}
	for i, row := range rows {
		if got := strings.TrimSpace(row); got != want[i] && !strings.HasPrefix(got, want[i]) {
			t.Fatalf("row %d = %q, want %q", i, row, want[i])
		}
	}
	if !strings.HasPrefix(rows[1], "    First 0") {
		t.Fatalf("a thought's text sits under its row like a call's output: %q", rows[1])
	}
	// The chunk folded and reopened shows the thought as the reader left it.
	block := activityBlocks(m, 80)[0]
	m.setChunkOpen(block.chunks[0].key, block.reasoningIDs, block.toolDisclosureIDs, 80, false)
	if text := activityText(t, m, 80, 0); strings.Contains(text, "First") || strings.Contains(text, "read a.go") {
		t.Fatalf("a folded chunk shows only its header: %q", text)
	}
	m.setChunkOpen(block.chunks[0].key, block.reasoningIDs, block.toolDisclosureIDs, 80, true)
	if text := activityText(t, m, 80, 0); !strings.Contains(text, "First 5") || strings.Contains(text, "Second step") {
		t.Fatalf("reopening lost the opened thought or opened its neighbour: %q", text)
	}
	clickThought(t, m, 80, record.id, 0)
	if rows := stepRows(t, m, 80); len(rows) != 4 {
		t.Fatalf("a second click did not hide the thought: %q", rows)
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
			// A replayed transcript opens collapsed: one header for both reads.
			if text := activityText(t, m, 80, 0); text != "▸ Read 2 files" {
				t.Fatalf("reloaded turn = %q, want one folded chunk", text)
			}
			rows := stepRows(t, m, 80)
			if len(rows) != 4 {
				t.Fatalf("reloaded turn wants two steps: %q", rows)
			}
			if !strings.HasPrefix(rows[0], "  "+thoughtGlyph+" thought") || !strings.HasSuffix(rows[0], " 0:00") {
				t.Fatalf("first step wants the audit's start: %q", rows[0])
			}
			if !strings.Contains(rows[1], "✓ read first.go") || strings.Contains(rows[1], "0.3s") {
				t.Fatalf("quick reloaded call wants no duration: %q", rows[1])
			}
			if !strings.HasPrefix(rows[2], "  "+thoughtGlyph+" thought") || !strings.HasSuffix(rows[2], " 1:15") {
				t.Fatalf("second step wants its start: %q", rows)
			}
			if !strings.Contains(rows[3], "✓ read second.go") || !strings.Contains(rows[3], "2m10s") {
				t.Fatalf("long reloaded call keeps its duration: %q", rows[3])
			}
		})
	}
}

// Steps run on without a blank row between them, whether or not a thought
// leads them; their starts in the right column tell them apart.
func TestStepsRunOnWithoutBlankRows(t *testing.T) {
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
	if len(rows) != 4 || !strings.HasPrefix(rows[2], "  "+thoughtGlyph+" thought") || !strings.Contains(rows[3], "s2.go") {
		t.Fatalf("two bare steps, then the thought step, with no row between: %q", rows)
	}
	for i, want := range []string{"0:00", "0:10"} {
		if !strings.Contains(rows[i], fmt.Sprintf("s%d.go", i)) || !strings.HasSuffix(rows[i], " "+want) {
			t.Fatalf("bare step %d wants its start at the right: %q", i, rows[i])
		}
	}
}

// A completed call before a new thought establishes their order even
// without an attempt hook. It supplies no step clock, though.
func TestUntaggedActivityPreservesObservedOrderWithoutOffsets(t *testing.T) {
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
	thought := "  " + thoughtGlyph + " thought"
	if len(rows) != 4 || rows[0] != thought || !strings.Contains(rows[1], "a.go") || rows[2] != thought || !strings.Contains(rows[3], "b.go") {
		t.Fatalf("untagged activity lost its observed thought/call order: %q", rows)
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

// Every row of an open chunk keeps the step column: a thought row carries
// its step's start, a call row with no start of its own ends where the
// others do, and an opened thought's text takes the row's whole width.
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
	if len(rows) != 4 || !strings.HasPrefix(rows[0], "  "+thoughtGlyph+" thought") || !strings.HasSuffix(rows[0], " 0:00") || rw.StringWidth(rows[0]) != width {
		t.Fatalf("a thought row wants its step's start at the edge: %q", rows)
	}
	edge := width - stepColumn(width)
	for _, row := range rows[1 : len(rows)-1] {
		if w := rw.StringWidth(row); w > edge || strings.HasSuffix(row, ":00") {
			t.Fatalf("a call row runs past the step column (%d > %d): %q", w, edge, row)
		}
	}
	last := rows[len(rows)-1]
	if !strings.Contains(last, "read later.go") || !strings.HasSuffix(last, " 1:05") || rw.StringWidth(last) != width {
		t.Fatalf("the next step's call wants its start at the edge: %q", last)
	}
	if !strings.Contains(strings.Join(rows, "\n"), "✓ $ grep -n") {
		t.Fatalf("the long command's row is missing: %q", rows)
	}
	// The opened thought wraps at the row's width less its indent.
	record := m.reasoningRecords.get(activityBlocks(m, width)[0].reasoningIDs[0])
	clickThought(t, m, width, record.id, 0)
	text := stepRows(t, m, width)[1:]
	if !strings.HasPrefix(text[0], "    words that wrap") || rw.StringWidth(text[0]) <= edge {
		t.Fatalf("an opened thought's text wants the whole row: %q", text[0])
	}
	clickThought(t, m, width, record.id, 0)

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

// An opened thought keeps a paragraph break only after prose of more than
// one row; one-line summaries stack, so they add text and no blank rows.
func TestOpenedThoughtStacksOneLineSummaries(t *testing.T) {
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
	record := m.reasoningRecords.get(activityBlocks(m, 80)[0].reasoningIDs[0])
	expandLines(m, nil)
	clickThought(t, m, 80, record.id, 0)
	clickThought(t, m, 80, record.id, 1)
	rows := stepRows(t, m, 80)
	blank := func(row string) bool { return strings.TrimSpace(row) == "" }
	// thought row, three stacked summaries, a row of air, then the call.
	if len(rows) < 6 || blank(rows[1]) || blank(rows[2]) || blank(rows[3]) || !blank(rows[4]) || !strings.Contains(rows[5], "read first.go") {
		t.Fatalf("one-line summaries want stacking: %q", rows)
	}
	rest := rows[7:] // past the second step's thought row
	cut := -1
	for i, row := range rest {
		if blank(row) {
			cut = i
			break
		}
	}
	if cut < 2 || !strings.HasPrefix(rest[cut+1], "    Comparing main history") || !blank(rest[cut+2]) || !strings.Contains(rest[cut+3], "read second.go") {
		t.Fatalf("prose wants its paragraph break kept: %q", rest)
	}
}
