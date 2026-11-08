package main

import (
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
)

// newestLineRecords are the reasoning and tool records of the newest line:
// the turn holding the newest record, which a prompt awaiting its first
// activity has not yet replaced.
func (m *replModel) newestLineRecords() (thoughts, tools map[int64]bool) {
	thoughts, tools = map[int64]bool{}, map[int64]bool{}
	last := -1
	for _, record := range m.reasoningRecords.all() {
		last = max(last, record.transcriptIndex)
	}
	for _, record := range m.toolDisclosures.all() {
		last = max(last, record.transcriptIndex)
	}
	if last < 0 {
		return thoughts, tools
	}
	// Prompts can be echoed while an earlier turn still runs. Only actual
	// turn ownership groups its records, including activity on both sides of
	// a queued echo (even after that prompt is activated or discarded).
	turn := m.transcript[last].activityTurn
	for id, record := range m.reasoningRecords.all() {
		if m.transcript[record.transcriptIndex].activityTurn == turn {
			thoughts[id] = true
		}
	}
	for id, record := range m.toolDisclosures.all() {
		if m.transcript[record.transcriptIndex].activityTurn == turn {
			tools[id] = true
		}
	}
	return thoughts, tools
}

// Every turn's activity line is open by default: it opens as the turn
// runs, stays open once the turn settles and when the next turn begins. A
// line folded by hand stays folded for its turn.
func TestEveryLineOpensByDefaultAndStaysOpenAcrossTurns(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("first")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.ShowThinking("first reasoning")
	first := m.currentReasoningRecord()
	call := messages.ChatMessageToolCall{ID: "c1", Name: "bash", Arguments: `{"command":"go test ./..."}`}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	firstTool := m.currentToolDisclosure()
	if first == nil || firstTool == nil || !first.expanded || !firstTool.expanded {
		t.Fatalf("running turn's line did not open on its own: thought=%+v tools=%+v", first, firstTool)
	}
	tui.AppendToolEnd(call, "ok", time.Second, nil)
	tui.AppendAssistantText("done")
	r.endTurn(nil)
	if !first.expanded || !firstTool.expanded {
		t.Fatal("settlement folded the newest line")
	}
	if text := strings.Join(transcriptRowsText(m.transcriptRows(100)), "\n"); !strings.Contains(text, "▾ thought") || !strings.Contains(text, "go test ./...") {
		t.Fatalf("settled newest line is not open: %q", text)
	}

	// The next turn leaves it open and opens its own.
	m.beginTurn("second")
	if !first.expanded || !firstTool.expanded {
		t.Fatal("the next turn folded the previous line")
	}
	second := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	second.ShowThinking("second reasoning")
	if rec := m.currentReasoningRecord(); rec == nil || !rec.expanded || !first.expanded {
		t.Fatalf("new turn's line did not open beside the older one: new=%+v old=%v", rec, first.expanded)
	}
	// A fold by hand holds for the turn: a later batch joins the folded line.
	rec := m.currentReasoningRecord()
	if !m.toggleReasoning(rec.id, 100) || rec.expanded {
		t.Fatal("folding the running line by hand failed")
	}
	later := messages.ChatMessageToolCall{ID: "c2", Name: "bash", Arguments: `{"command":"go vet ./..."}`}
	second.AppendToolStart([]messages.ChatMessageToolCall{later})
	if tool := m.currentToolDisclosure(); tool == nil || tool.expanded {
		t.Fatalf("a later batch reopened a line folded by hand: %+v", tool)
	}
	second.AppendToolEnd(later, "ok", time.Second, nil)
	r.endTurn(nil)
	// The turn after that opens again.
	m.beginTurn("third")
	third := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	third.ShowThinking("third reasoning")
	if rec := m.currentReasoningRecord(); rec == nil || !rec.expanded {
		t.Fatalf("the turn after a hand-folded one did not open: %+v", rec)
	}
}

// Hydrated history opens every line, or folds every line in a collapsed
// view.
func TestHydratedHistoryOpensEveryLine(t *testing.T) {
	var history []messages.ChatMessage
	for _, turn := range []string{"one", "two", "three"} {
		call := messages.ChatMessageToolCall{ID: turn, Name: "bash", Arguments: `{"command":"make ` + turn + `"}`}
		result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: "ok"}
		result.SetToolSucceeded(true)
		history = append(history,
			messages.ChatMessage{Role: messages.MessageRoleUser, Content: turn},
			messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}},
			result,
			messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "did " + turn},
		)
	}
	for _, depth := range []disclosureDepth{depthAbridged, depthCollapsed} {
		m := newReplModel()
		m.depth = depth
		m.hydrateHistory(history, "ctx")
		open := depth != depthCollapsed
		if m.toolDisclosures.count() != 3 {
			t.Fatalf("hydrated %d lines, want 3", m.toolDisclosures.count())
		}
		for id, record := range m.toolDisclosures.all() {
			if record.expanded != open {
				t.Fatalf("depth %d: record %d expanded=%v", depth, id, record.expanded)
			}
		}
		text := strings.Join(transcriptRowsText(m.transcriptRows(100)), "\n")
		if open && (!strings.Contains(text, "✓ $ make three") || !strings.Contains(text, "✓ $ make one") || strings.Contains(text, " line")) {
			t.Fatalf("hydrated transcript: %q", text)
		}
	}
}

// Ctrl-O's depths leave every line open until collapsed folds them all, and
// leaving collapsed opens them all again; no depth touches an output.
func TestCollapsedFoldsEveryLineAndLeavingItOpensThem(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	var records []*toolDisclosureRecord
	for _, turn := range []string{"older", "newest"} {
		m.beginTurn(turn)
		tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
		call := messages.ChatMessageToolCall{ID: turn, Name: "bash", Arguments: `{"command":"` + turn + `"}`}
		tui.AppendToolStart([]messages.ChatMessageToolCall{call})
		tui.AppendToolEnd(call, "ok", time.Second, nil)
		tui.AppendAssistantText("done")
		records = append(records, m.currentToolDisclosure())
		r.endTurn(nil)
	}
	older, newest := records[0], records[1]
	check := func(stage string, depth disclosureDepth) {
		t.Helper()
		open := depth != depthCollapsed
		if m.depth != depth || older.expanded != open || newest.expanded != open || older.rows[0].outputExpanded || newest.rows[0].outputExpanded {
			t.Fatalf("%s: depth=%v older=%v newest=%v outputs=%v/%v", stage, m.depth, older.expanded, newest.expanded, older.rows[0].outputExpanded, newest.rows[0].outputExpanded)
		}
	}
	check("default", depthAbridged)
	press := func() { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"}) }
	for _, step := range []struct {
		name  string
		depth disclosureDepth
	}{{"full", depthFull}, {"calls", depthCalls}, {"collapsed", depthCollapsed}, {"abridged", depthAbridged}} {
		press()
		check(step.name, step.depth)
	}
}

func TestQueuedPromptDoesNotSplitActivityTurn(t *testing.T) {
	for _, activate := range []bool{true, false} {
		name := "discarded"
		if activate {
			name = "activated"
		}
		t.Run(name, func(t *testing.T) {
			withDisplayTTY(t)
			r := newManagedREPL(&Config{}, "ctx", 0, 0)
			m := r.model
			m.beginTurn("first")
			tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
			tui.ShowThinking("first reasoning")
			firstThought := m.currentReasoningRecord()
			firstCall := messages.ChatMessageToolCall{ID: "a", Name: "read_file", Arguments: `{"path":"a.go"}`}
			tui.AppendToolStart([]messages.ChatMessageToolCall{firstCall})
			tui.AppendToolEnd(firstCall, "ok", time.Second, nil)
			first := m.currentToolDisclosure()

			queuedTurn := textManagedTurn("second")
			item := queuedREPLInput{text: "second", turn: &queuedTurn}
			m.appendQueuedInput(&item)
			m.queue = append(m.queue, item)
			if !activate {
				m.discardQueuedInputs()
			}
			tui.AppendAssistantText("more work to do")
			tui.ShowThinking("later reasoning")
			laterThought := m.currentReasoningRecord()
			laterCall := messages.ChatMessageToolCall{ID: "b", Name: "read_file", Arguments: `{"path":"b.go"}`}
			tui.AppendToolStart([]messages.ChatMessageToolCall{laterCall})
			tui.AppendToolEnd(laterCall, "ok", time.Second, nil)
			later := m.currentToolDisclosure()
			r.endTurn(nil)

			thoughts, tools := m.newestLineRecords()
			if len(thoughts) != 2 || !thoughts[firstThought.id] || !thoughts[laterThought.id] || len(tools) != 2 || !tools[first.id] || !tools[later.id] {
				t.Fatalf("queued echo split one turn's activity: thoughts=%v tools=%v", thoughts, tools)
			}
			// Opening the lines again must include both sides of the echo too.
			foldEveryLine(m)
			m.openHydratedLines()
			if !first.expanded || !later.expanded || !firstThought.expanded || !laterThought.expanded {
				t.Fatal("the depth's opening did not open the whole turn")
			}
			if activate {
				m.queue = nil
				m.activateQueuedInput(item)
				m.beginManagedTurnState(queuedTurn)
			} else {
				m.beginTurn("next")
			}
			if !first.expanded || !later.expanded || !firstThought.expanded || !laterThought.expanded {
				t.Fatalf("next turn folded prior activity: tools=%v/%v thoughts=%v/%v", first.expanded, later.expanded, firstThought.expanded, laterThought.expanded)
			}
			next := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
			next.ShowThinking("next turn reasoning")
			thoughts, tools = m.newestLineRecords()
			if current := m.currentReasoningRecord(); !current.expanded || len(thoughts) != 1 || !thoughts[current.id] || len(tools) != 0 {
				t.Fatalf("next turn did not own its activity: thoughts=%v tools=%v", thoughts, tools)
			}
		})
	}
}

// Line ownership (which records one turn's line holds) survives a history
// continuation and a child display copy; the depth's opening opens every
// record whatever line it is on.
func TestNewestLineOwnershipSurvivesHistoryContinuation(t *testing.T) {
	var history []messages.ChatMessage
	for _, turn := range []string{"older", "newest"} {
		history = append(history, messages.ChatMessage{Role: messages.MessageRoleUser, Content: turn})
		for _, part := range []string{"first", "second"} {
			call := messages.ChatMessageToolCall{ID: turn + part, Name: "read_file"}
			history = append(history,
				messages.ChatMessage{Role: messages.MessageRoleAssistant, Reasoning: turn + part, ToolCalls: []messages.ChatMessageToolCall{call}},
				messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: "ok"},
				messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "more work"},
			)
		}
	}
	saved := newReplModel()
	saved.hydrateHistory(history, "ctx")
	check := func(m *replModel, count int) {
		t.Helper()
		thoughts, tools := m.newestLineRecords()
		if len(thoughts) != count || len(tools) != count {
			t.Fatalf("newest line has thoughts=%v tools=%v, want %d of each", thoughts, tools, count)
		}
		m.openHydratedLines()
		for id, record := range m.reasoningRecords.all() {
			if !record.expanded {
				t.Fatalf("thought %d did not open", id)
			}
		}
		for id, record := range m.toolDisclosures.all() {
			if !record.expanded {
				t.Fatalf("tool %d did not open", id)
			}
		}
	}
	check(saved, 2)
	check(childDisplayCopy(saved), 2)
	continuation := []messages.ChatMessage{{Role: messages.MessageRoleAssistant, Reasoning: "live thought", ToolCalls: []messages.ChatMessageToolCall{{ID: "live", Name: "bash"}}}}
	check(prepareLiveDisplay(saved, continuation), 3)
	check(prepareLiveDisplay(saved, append(messages.User("next"), continuation...)), 1)
	check(saved, 2)
	// An unanswered prompt has no activity yet, so the newest activity still
	// belongs to the preceding turn, including its prose-separated records.
	unanswered := newReplModel()
	unanswered.hydrateHistory(append(history, messages.User("unanswered")...), "ctx")
	check(unanswered, 2)
}

func TestNewestLineOwnershipSurvivesChildDisplayReplacement(t *testing.T) {
	withDisplayTTY(t)
	next := newReplModel()
	next.hydrateHistory([]messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "previous"},
		{Role: messages.MessageRoleAssistant, Reasoning: "previous reasoning", Content: "done"},
	}, "child")
	m := newReplModel()
	(&managedREPL{}).replaceChildDisplay(&replTab{model: m}, next)
	m.beginTurn("follow-up")
	m.appendThinking("new reasoning")
	current := m.currentReasoningRecord()
	thoughts, _ := m.newestLineRecords()
	if len(thoughts) != 1 || !thoughts[current.id] {
		t.Fatalf("replacement reused prior turn identity: activityTurn=%d thoughts=%v, want only %d", m.activityTurn, thoughts, current.id)
	}
	m.openHydratedLines()
	for id, record := range m.reasoningRecords.all() {
		if !record.expanded {
			t.Fatalf("thought %d did not open", id)
		}
	}
}

func TestNewActivityRecordInheritsLatestLineManualChoice(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		name := "folded"
		if reopen {
			name = "reopened"
		}
		t.Run(name, func(t *testing.T) {
			withDisplayTTY(t)
			r := newManagedREPL(&Config{}, "ctx", 0, 0)
			m := r.model
			m.beginTurn("first")
			tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
			firstCall := messages.ChatMessageToolCall{ID: "a", Name: "read_file", Arguments: `{"path":"a.go"}`}
			tui.AppendToolStart([]messages.ChatMessageToolCall{firstCall})
			tui.AppendToolEnd(firstCall, "ok", time.Second, nil)
			first := m.currentToolDisclosure()
			if reopen {
				m.toggleActivityLine(nil, []int64{first.id}, 100)
			}
			tui.AppendAssistantText("more work to do")
			tui.ShowThinking("a later reasoning line")
			thought := m.currentReasoningRecord()
			if !m.toggleActivityLine([]int64{thought.id}, nil, 100) || thought.expanded != reopen {
				t.Fatal("fixture did not change the current activity line")
			}
			laterCall := messages.ChatMessageToolCall{ID: "b", Name: "read_file", Arguments: `{"path":"b.go"}`}
			tui.AppendToolStart([]messages.ChatMessageToolCall{laterCall})
			later := m.currentToolDisclosure()
			if later.expanded != reopen || first.expanded == reopen {
				t.Fatalf("new tool lost current line's choice: oldTools=%v currentThought=%v newTools=%v", first.expanded, thought.expanded, later.expanded)
			}
			tui.AppendToolEnd(laterCall, "ok", time.Second, nil)
			tui.AppendAssistantText("another update")
			tui.ShowThinking("another thought")
			if current := m.currentReasoningRecord(); current.expanded != reopen {
				t.Fatalf("new thought lost latest line's choice: got %v, want %v", current.expanded, reopen)
			}
		})
	}
}
