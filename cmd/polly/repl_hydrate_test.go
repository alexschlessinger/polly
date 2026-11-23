package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/subagent"
)

func TestHydratedCompactionNoticeSeparatesActivityRuns(t *testing.T) {
	step := func(id string) []messages.ChatMessage {
		call := readCall(id)
		call.DisplayID = "call-" + id
		proposal := continuationMessage("response-"+id, messages.MessageRoleAssistant, "")
		proposal.Reasoning = "reasoning " + id
		proposal.ToolCalls = []messages.ChatMessageToolCall{call}
		result := continuationMessage(call.DisplayID, messages.MessageRoleTool, "read complete")
		result.ToolCallID, result.ToolName = call.ID, call.Name
		result.SetToolSucceeded(true)
		return []messages.ChatMessage{proposal, result}
	}
	prefix := append(messages.User("Task"), step("before")...)
	prefix = append(prefix, messages.Compaction{ClearThrough: "call-before"}.Message())
	after := step("after")
	full := newReplModel()
	full.hydrateHistory(append(slices.Clone(prefix), after...), "worker")
	saved := newReplModel()
	saved.hydrateHistory(prefix, "worker")
	for name, m := range map[string]*replModel{"resume": full, "live continuation": prepareLiveDisplay(saved, after)} {
		t.Run(name, func(t *testing.T) {
			applyViewSections(m, viewState{depth: depthExpanded, sections: map[string]viewSection{
				"thought:response-before": {open: []bool{true}},
				"thought:response-after":  {open: []bool{true}},
			}})
			text := continuationText(m)
			at := -1
			for _, fragment := range []string{"reasoning before", "✓ read before.go", "Context compacted", "reasoning after", "✓ read after.go"} {
				if index := strings.Index(text, fragment); index > at {
					at = index
				} else {
					t.Fatalf("missing or out-of-order %q in\n%s", fragment, text)
				}
			}
			beforeRecord, before := callRow(t, m, "before")
			afterRecord, after := callRow(t, m, "after")
			if beforeRecord.id == afterRecord.id || before.step != 0 || after.step != 1 {
				t.Fatalf("notice did not split records within the same turn: records=%d/%d steps=%d/%d", beforeRecord.id, afterRecord.id, before.step, after.step)
			}
		})
	}
}

// The durable display order wins over replay order: rows are matched by call
// ID first, then by tool name when either side lacks an ID, denied calls show
// as denied whatever their row held, and rows the record never mentions keep
// their place at the end. Labels carry argument summaries and never take
// part in the pairing.
func TestHistoryHydratorAppliesDurableToolOrder(t *testing.T) {
	m := newReplModel()
	m.hydrating = true
	h := historyHydrator{m: m}
	h.toolRows = []toolDisclosureRow{
		{callID: "b", toolName: "read", label: "read main.go", line: "read-line", settled: true},
		{callID: "", toolName: "bash", label: "bash ls", line: "bash-line", settled: true},
		{callID: "z", toolName: "extra", label: "extra", line: "extra-line", settled: true},
	}
	h.flushTools()

	h.applyToolOrder([]durableDisplayToolCall{
		{ID: "a", Name: "bash"},
		{ID: "b", Name: "read"},
		{ID: "c", Name: "write", Denied: true},
	})

	if h.tools == nil || !h.tools.complete || m.lineOpenOf(h.tools) {
		t.Fatalf("disclosure after ordering = %#v", h.tools)
	}
	assertToolRows(t, h.tools.rows,
		[]string{"bash ls", "read main.go", "write", "extra"},
		[]string{"bash-line", "read-line", toolDeniedLine("write"), "extra-line"})
}

// Hydrated rows read like they did live (tool name plus argument summary),
// so the durable record pairs them by tool name: an ID-less allowed call (an
// OpenAI-compatible server may omit IDs) still finds its row. The name
// fallback only bridges a missing ID, so a denied call whose exchange was
// stripped never takes the row of a same-named sibling the record names by
// ID.
func TestHistoryHydratorPairsRowsByToolName(t *testing.T) {
	okLine := toolOKLine("bash ls -la", "", "1 line")
	cases := []struct {
		name       string
		call       messages.ChatMessageToolCall
		order      string
		wantLabels []string
		wantLines  []string
	}{
		{
			name:       "id-less allowed call beside a stripped denied call",
			call:       messages.ChatMessageToolCall{Name: "bash", Arguments: `{"command":"ls -la"}`},
			order:      `[{"name":"bash"},{"id":"x","name":"read","denied":true}]`,
			wantLabels: []string{"bash ls -la", "read"},
			wantLines:  []string{okLine, toolDeniedLine("read")},
		},
		{
			name:       "denied call listed before its same-named sibling",
			call:       messages.ChatMessageToolCall{ID: "a", Name: "bash", Arguments: `{"command":"ls -la"}`},
			order:      `[{"id":"d","name":"bash","denied":true},{"id":"a","name":"bash"}]`,
			wantLabels: []string{"bash", "bash ls -la"},
			wantLines:  []string{toolDeniedLine("bash"), okLine},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := historyHydrator{m: newReplModel()}
			h.assistant(messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{tc.call}})
			result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: tc.call.ID, Content: "ok"}
			result.SetToolSucceeded(true)
			h.tool(result)
			h.internal(messages.ChatMessage{Role: messages.MessageRoleInternal, Metadata: map[string]any{
				messages.MetadataKeyDisplayToolCalls: tc.order,
				messages.MetadataKeyTurnStatus:       messages.TurnStatusToolDenied,
			}})
			if h.tools == nil {
				t.Fatal("no disclosure after ordering")
			}
			assertToolRows(t, h.tools.rows, tc.wantLabels, tc.wantLines)
		})
	}
}

func assertToolRows(t *testing.T, got []toolDisclosureRow, wantLabels, wantLines []string) {
	t.Helper()
	if len(got) != len(wantLabels) {
		t.Fatalf("rows = %d, want %d: %#v", len(got), len(wantLabels), got)
	}
	for i := range got {
		if got[i].label != wantLabels[i] || got[i].line != wantLines[i] || !got[i].settled {
			t.Fatalf("row %d = %#v, want label %q line %q", i, got[i], wantLabels[i], wantLines[i])
		}
	}
}

// A result settles the row with its call ID, or the oldest waiting row when
// the ID matches nothing; a result that arrives before any call opens a row
// named after its tool.
func TestHistoryHydratorSettlesToolResults(t *testing.T) {
	h := historyHydrator{m: newReplModel()}
	h.assistant(messages.ChatMessage{
		Role:      messages.MessageRoleAssistant,
		ToolCalls: []messages.ChatMessageToolCall{{ID: "one", Name: "bash"}, {ID: "two", Name: "read"}},
	})
	h.tool(messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: "two", Content: "ok"})
	if h.toolRows[0].settled || !h.toolRows[1].settled {
		t.Fatalf("result for call two settled the wrong row: %#v", h.toolRows)
	}
	h.tool(messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: "unknown", Content: "ok"})
	if len(h.toolRows) != 2 || !h.toolRows[0].settled {
		t.Fatalf("unmatched result should settle the oldest waiting row: %#v", h.toolRows)
	}

	orphan := historyHydrator{m: newReplModel()}
	orphan.tool(messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: "x", ToolName: "grep", Content: "ok"})
	if len(orphan.toolRows) != 1 || orphan.toolRows[0].label != "grep" || !orphan.toolRows[0].settled {
		t.Fatalf("orphan result rows = %#v", orphan.toolRows)
	}
}

func TestResumedHistoryWindow(t *testing.T) {
	var history []messages.ChatMessage
	for i := 0; i < resumedTurnLimit+3; i++ {
		history = append(history,
			messages.ChatMessage{Role: messages.MessageRoleUser, Content: "q"},
			messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "a"},
		)
	}
	start, total, show := resumedHistoryWindow(history)
	if total != resumedTurnLimit+3 || show != resumedTurnLimit || start != 6 {
		t.Fatalf("window = (start %d, total %d, show %d)", start, total, show)
	}
	m := newReplModel()
	m.hydrateHistory(history, "context")
	if strings.Contains(m.fullTranscript(), "resumed context") {
		t.Fatal("history hydration added a routine resume notice")
	}

}

// readStep is one model response that reads a file, and that read's result.
func readStep(id string) (response, result messages.ChatMessage) {
	call := messages.ChatMessageToolCall{ID: id, DisplayID: "call-" + id, Name: "read_file", Arguments: `{"path":"` + id + `.go"}`}
	response = messages.ChatMessage{
		Role:      messages.MessageRoleAssistant,
		ToolCalls: []messages.ChatMessageToolCall{call},
		Metadata:  map[string]any{messages.MetadataKeyDisplayID: "response-" + id},
	}
	result = messages.ChatMessage{
		Role: messages.MessageRoleTool, ToolCallID: id, ToolName: call.Name, Content: "package main\n",
		Metadata: map[string]any{messages.MetadataKeyDisplayID: call.DisplayID},
	}
	result.SetToolSucceeded(true)
	return response, result
}

// readSteps is a run of such steps, each answered before the next begins.
func readSteps(ids ...string) []messages.ChatMessage {
	var run []messages.ChatMessage
	for _, id := range ids {
		response, result := readStep(id)
		run = append(run, response, result)
	}
	return run
}

// Replaying a step that only calls tools writes nothing to the transcript:
// its rows join the run's record, and the record's entry, which lists every
// row, is written when the run is over. Written at each step, resuming a
// turn cost the square of its length.
func TestHistoryHydratorWritesARunOnce(t *testing.T) {
	m := newReplModel()
	h := historyHydrator{m: m}
	replay := func(ids ...string) {
		for _, msg := range readSteps(ids...) {
			h.replay(msg)
		}
	}
	replay("a", "b") // the second step opens the record, with the first step's row
	written := m.visual.revision
	replay("c", "d", "e")
	if m.visual.revision != written || len(h.stale) != 1 {
		t.Fatalf("three more steps wrote the transcript %d times and left %d writes waiting, want none and one", m.visual.revision-written, len(h.stale))
	}
	h.flushTools()
	record := h.tools
	if len(record.displayRows) != 5 || m.transcript[record.transcriptIndex].text != toolRecordText(record, 0, m.showsRows(record)) {
		t.Fatalf("the flushed record shows %d of 5 rows:\n%s", len(record.displayRows), plainStyledText(m.transcript[record.transcriptIndex].text))
	}
}

// replayWritingEveryStep replays history as hydrateHistory does for a
// conversation shown whole, and writes every tool record again after every
// message: what replay did before a run's record waited for the run to end.
func replayWritingEveryStep(m *replModel, history []messages.ChatMessage) {
	m.hydrating = true
	h := historyHydrator{m: m}
	for _, msg := range history {
		h.replay(msg)
		for _, record := range m.toolDisclosures.all() {
			m.refreshToolDisclosure(record)
		}
	}
	h.flushTools()
	h.finish()
	m.hydrating = false
	m.hydrateDisplayCatalog(history)
}

// hydratedView is what a reader can see of a replayed transcript: its
// entries, then the pane with every chunk folded and, after opening them,
// with every chunk open.
func hydratedView(m *replModel) string {
	const width = 100
	entries := strings.Join(transcriptTexts(m), "\n")
	folded := shown(m, width)
	m.toggleAllDisclosures(width)
	return entries + "\n\nfolded:\n" + folded + "\n\nopen:\n" + shown(m, width)
}

// A run's record is written once, when prose, a turn marker, the next prompt
// or the end of the replay closes it, and nothing a reader sees depends on
// that: each history hydrates to what it does with every record written
// again after every message.
func TestHydratedTranscriptMatchesPerStepWrites(t *testing.T) {
	user := func(text string) []messages.ChatMessage {
		return []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: text}}
	}
	prose := func(text string) []messages.ChatMessage {
		return []messages.ChatMessage{{Role: messages.MessageRoleAssistant, Content: text}}
	}
	// proseStep is a response that says something and then reads.
	proseStep := func(text, id string) []messages.ChatMessage {
		response, result := readStep(id)
		response.Content = text
		return []messages.ChatMessage{response, result}
	}

	// b's result arrives after prose closed its record, e's after the next
	// response folded its row.
	callB, resultB := readStep("b")
	callE, resultE := readStep("e")
	callF, resultF := readStep("f")
	lateResults := slices.Concat(user("go"), readSteps("a"), []messages.ChatMessage{callB}, proseStep("halfway", "c"),
		[]messages.ChatMessage{resultB}, readSteps("d"), []messages.ChatMessage{callE, callF, resultE, resultF}, prose("done"))

	// The turn's marker lists the calls newest first, beside a denied call
	// whose exchange was stripped, and says when each response began.
	started := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	audit := messages.TurnAudit{StartedAt: started}
	order := []durableDisplayToolCall{{ID: "stripped", Name: "bash", Denied: true, Response: 2}}
	for i, id := range []string{"a", "b", "c", "d", "e", "f"} {
		audit.Attempts = append(audit.Attempts, messages.ModelAttemptAudit{ID: "response-" + id, StartedAt: started.Add(time.Duration(i+1) * 10 * time.Second)})
		order = append([]durableDisplayToolCall{{ID: id, DisplayID: "call-" + id, Name: "read_file", Response: i + 1}}, order...)
	}
	encoded, err := json.Marshal(order)
	if err != nil {
		t.Fatal(err)
	}
	marker := messages.ChatMessage{Role: messages.MessageRoleInternal, Metadata: map[string]any{
		messages.MetadataKeyDisplayToolCalls: string(encoded),
		messages.MetadataKeyTurnAudit:        audit,
		messages.MetadataKeyTurnStatus:       messages.TurnStatusToolDenied,
	}}

	// The record opens on an agent launch, which draws no row of its own, so
	// its entry is empty until a later step's row is written. The next prompt
	// reads that entry to decide whether a blank row separates the turns.
	spawn := messages.ChatMessageToolCall{ID: "spawn", Name: subagent.ToolName, Arguments: `{"label":"worker","task":"look around"}`}
	launch := []messages.ChatMessage{
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{spawn}},
		{Role: messages.MessageRoleTool, ToolCallID: spawn.ID, ToolName: spawn.Name, Content: "started"},
	}

	for _, tc := range []struct {
		name    string
		history []messages.ChatMessage
	}{
		{"one step", slices.Concat(user("go"), readSteps("a"), prose("done"))},
		{"many steps", slices.Concat(user("go"), readSteps("a", "b", "c", "d", "e", "f", "g", "h"), prose("done"))},
		{"prose between steps", slices.Concat(user("go"), readSteps("a", "b", "c"), proseStep("halfway", "d"), readSteps("e", "f"), prose("done"))},
		{"late results", lateResults},
		{"durable tool order", slices.Concat(user("go"), readSteps("a", "b", "c"), proseStep("halfway", "d"), readSteps("e", "f"), prose("done"), []messages.ChatMessage{marker})},
		{"run ended by the next prompt", slices.Concat(user("go"), launch, readSteps("a", "b"), user("and then"), prose("done"))},
		{"run ended by the replay", slices.Concat(user("go"), readSteps("a", "b", "c"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, want := newReplModel(), newReplModel()
			got.hydrateHistory(tc.history, "ctx")
			replayWritingEveryStep(want, tc.history)
			if got, want := hydratedView(got), hydratedView(want); got != want {
				t.Fatalf("hydrated transcript:\n%s\n\nwritten at every step:\n%s", got, want)
			}
		})
	}
}
