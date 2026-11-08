package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/swarm"
	ui "github.com/metaspartan/gotui/v5"
)

// Ctrl-O moves the view through full, calls alone, collapsed and abridged:
// whole thoughts come and go, no output or diff opens, the line stays open
// until collapsed folds it and abridged opens it again, and a held viewport
// stays where it was.
func TestCtrlOCyclesDepthsAndCollapsedFoldsTheLine(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.ShowThinking("reasoning-visible-when-open")
	calls := []messages.ChatMessageToolCall{
		{ID: "read", Name: "read_file", Arguments: `{"path":"main.go"}`},
		{ID: "edit", Name: "edit_file", Arguments: `{"path":"main.go"}`},
		agentCall("agent", `{"label":"Review changes"}`),
		{ID: "image", Name: "view_image"},
	}
	tui.AppendToolStart(calls)
	for _, call := range calls {
		tui.AppendToolEnd(call, "done", time.Millisecond, nil)
	}
	tui.AppendToolResult(calls[0], messages.ChatMessage{Content: "read-output-body"})
	tui.AppendToolResult(calls[1], toolDataResult(t, calls[1], "Edited", editChanges("main.go")))
	path := filepath.Join(t.TempDir(), "image.png")
	writeImageFixture(t, path, 8, 4)
	tui.AppendToolMedia(calls[3], inspectionTranscriptImages(testToolImageResult(t, path, calls[3].ID), nil))
	record := m.currentToolDisclosure()
	m.appendLine(strings.Repeat("held viewport below activity\n", 30))
	rows := transcriptRowsText((conversationView{}).Rows(m, 80))
	m.followBottom, m.scrollAnchor = false, len(rows)-5
	anchor := rows[m.scrollAnchor]
	for _, depth := range []disclosureDepth{depthFull, depthCalls, depthCollapsed, depthAbridged} {
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
		if open := depth != depthCollapsed; m.depth != depth || record.expanded != open {
			t.Fatalf("depth %d: want the line open=%v: depth=%d record=%+v", depth, open, m.depth, record)
		}
		for _, row := range record.rows {
			if row.outputExpanded {
				t.Fatalf("depth %d: output %q opened without a click", depth, row.callID)
			}
		}
		rows = transcriptRowsText((conversationView{}).Rows(m, 80))
		painted := strings.Join(rows, "\n")
		if strings.Contains(painted, "read-output-body") || strings.Contains(painted, "+delta") {
			t.Fatalf("depth %d: an output or diff showed without a click:\n%s", depth, painted)
		}
		if strings.Contains(painted, "reasoning-visible-when-open") != (depth == depthFull || depth == depthAbridged) || strings.Contains(painted, "Review changes") != (depth != depthCollapsed) {
			t.Fatalf("depth %d: reasoning/agent visibility is wrong:\n%s", depth, painted)
		}
		if rows[m.scrollAnchor] != anchor {
			t.Fatalf("depth %d moved held viewport from %q to %q", depth, anchor, rows[m.scrollAnchor])
		}
	}
}

// A new line follows the depth: open at every depth but collapsed, with no
// output open; a projected agent's record follows it too.
func TestNewLinesFollowTheDepth(t *testing.T) {
	for _, depth := range []disclosureDepth{depthAbridged, depthFull, depthCalls, depthCollapsed} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			open := depth != depthCollapsed
			m := newReplModel()
			m.appendThinking("initial thought")
			for m.depth != depth {
				m.toggleAllDisclosures(80)
			}
			m.finishThinkingSegment()
			m.appendThinking("later thought")
			if m.currentReasoningRecord().expanded != open {
				t.Fatalf("new thought of the newest line open=%v, want %v", m.currentReasoningRecord().expanded, open)
			}
			first := m.appendToolCallStart(messages.ChatMessageToolCall{ID: "first", Name: "read_file"})
			m.appendToolCallStart(messages.ChatMessageToolCall{ID: "second", Name: "bash"})
			m.appendSettledToolRow(messages.ChatMessageToolCall{ID: "settled", Name: "bash"})
			if len(first.rows) != 3 {
				t.Fatal("fixture did not append to one tool batch")
			}
			m.completeToolDisclosure()
			m.appendToolCallStart(messages.ChatMessageToolCall{ID: "later", Name: "view_image"})
			m.appendCompletedToolDisclosure([]toolDisclosureRow{{callID: "saved", label: "bash", settled: true}})
			for _, record := range m.toolDisclosures.all() {
				if record.expanded != open {
					t.Fatalf("new or settled batch of the newest line open=%v, want %v: %+v", record.expanded, open, record)
				}
				for _, row := range record.rows {
					if row.outputExpanded {
						t.Fatalf("new row %q opened its output at depth %d", row.callID, depth)
					}
				}
			}
			m.projectSwarmAgents(&swarm.State{Members: map[string]*swarm.Member{"worker": {ID: "worker", Label: "Worker"}}})
			found := false
			for _, record := range m.toolDisclosures.all() {
				for _, row := range record.rows {
					if row.isProjectedAgent() {
						found = true
						if record.expanded != open || row.outputExpanded {
							t.Fatal("projected agent did not follow the depth as a line of its own, output closed")
						}
					}
				}
			}
			if !found {
				t.Fatal("fixture did not project an agent")
			}
		})
	}
}

// An agent's view opens every line, abridged; Ctrl-O there sets its depth.
func TestAgentViewOpensEveryLineAndCtrlOSetsItsDepth(t *testing.T) {
	withDisplayTTY(t)
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root", "agent")
	r.showTab(0)
	m := r.tabs[1].model
	m.appendUserPrompt(strings.Repeat("assignment line\n", 5) + "assignment-tail")
	// Two more calls than an abridged run holds: command-0 and command-1
	// leave it.
	const calls = activityRunRows + 2
	for n := range calls {
		call := testToolStart(m, messages.ChatMessageToolCall{ID: fmt.Sprint(n), Name: "bash", Arguments: fmt.Sprintf(`{"command":"command-%d"}`, n)})
		testToolEnd(m, call, fmt.Sprintf("output-body-%d", n), nil)
	}
	m.appendAssistant(strings.Repeat("update line\n", 8) + "update-tail")
	m.finishAssistantBlock("")
	r.inspect(tabViewTarget(r.tabs[1]))
	v := waitInspector(t, r, 140)
	s := r.workspace().viewState(v.target)
	last := fmt.Sprintf("command-%d", calls-1)
	check := func(depth disclosureDepth) {
		t.Helper()
		if s.depth != depth {
			t.Fatalf("agent view depth %d, want %d", s.depth, depth)
		}
		text := inspectorRowsText(v, 100)
		if !strings.Contains(text, "update-tail") || !strings.Contains(text, "assignment-tail") {
			t.Fatalf("depth %d: the body lost the task or the update: %q", depth, text)
		}
		// The run keeps its newest activityRunRows calls at abridged and
		// calls, every call at full, and the collapsed depth folds the line;
		// no depth opens an output.
		switch depth {
		case depthFull:
			if !strings.Contains(text, "command-0") || !strings.Contains(text, last) {
				t.Fatalf("depth %d: the full depth lost a call: %q", depth, text)
			}
		case depthCollapsed:
			if strings.Contains(text, "command-2") {
				t.Fatalf("depth %d: the collapsed depth left the line open: %q", depth, text)
			}
		default:
			if strings.Contains(text, "command-0") || strings.Contains(text, "command-1 ") || !strings.Contains(text, "command-2") || !strings.Contains(text, last) {
				t.Fatalf("depth %d: the window is wrong: %q", depth, text)
			}
		}
		if strings.Contains(text, fmt.Sprintf("output-body-%d", calls-1)) {
			t.Fatalf("depth %d: an output opened without a click: %q", depth, text)
		}
	}
	check(depthAbridged)
	r.workspace().inspector.focused = true
	for _, depth := range []disclosureDepth{depthFull, depthCalls, depthCollapsed, depthAbridged} {
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
		v = waitInspector(t, r, 140)
		check(depth)
	}
}

func TestAgentViewCtrlOKeepsHeldLineAcrossPromptAndActivityChanges(t *testing.T) {
	withDisplayTTY(t)
	fixture, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = fixture.work.close() })
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root", "agent")
	r.setupWidgets()
	r.showTab(0)
	var answer strings.Builder
	for n := range 100 {
		fmt.Fprintf(&answer, "stable-line-%03d\n", n)
	}
	r.tabs[1].model.hydrateHistory([]messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: strings.Repeat("multiline assignment\n", 20)},
		{Role: messages.MessageRoleAssistant, Reasoning: "reasoning before the held answer", ToolCalls: []messages.ChatMessageToolCall{{ID: "call", Name: "bash"}}},
		{Role: messages.MessageRoleTool, ToolCallID: "call", ToolName: "bash", Content: strings.Repeat("tool detail\n", 8)},
		{Role: messages.MessageRoleAssistant, Content: answer.String()},
	}, "agent")
	screen.SetSize(160, 36)
	r.inspect(tabViewTarget(r.tabs[1]))
	waitInspector(t, r, 160)
	settle := func() {
		r.render()
		waitInspector(t, r, 160)
		r.render()
	}
	settle()
	s := r.workspace().viewState(r.workspace().inspector.target)
	s.follow = false
	found := false
	for n, row := range transcriptRowsText(r.inspectorW.Rows) {
		if strings.Contains(row, "stable-line-050") {
			s.top, found = n, true
			break
		}
	}
	if !found {
		t.Fatal("fixture omitted the held answer line")
	}
	settle()
	before := transcriptRowsText(r.inspectorW.Rows[s.top : s.top+1])[0]
	r.workspace().inspector.focused = true
	for _, depth := range []disclosureDepth{depthFull, depthCalls, depthCollapsed, depthAbridged} {
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
		settle()
		after := transcriptRowsText(r.inspectorW.Rows[s.top : s.top+1])[0]
		if after != before || s.depth != depth || s.follow {
			t.Fatalf("depth %d moved held line: before=%q after=%q follow=%v", depth, before, after, s.follow)
		}
	}
}

// Ctrl-O names its depth in the status bar, over the turn clock, for a
// moment; then the slot is as it was.
func TestCtrlONamesTheDepthInTheStatusBar(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.ShowThinking("a thought")
	left := func() (string, string) {
		raw := m.statusRow(120)
		plain := plainStyledText(raw)
		return strings.TrimSpace(plain[:strings.Index(plain, "  ")]), raw
	}
	if word, _ := left(); !strings.HasSuffix(word, "s") || strings.Contains(word, "full") {
		t.Fatalf("a running turn wants its clock before any press: %q", word)
	}
	for _, want := range []string{"full", "calls only", "collapsed", "abridged"} {
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
		word, raw := left()
		if word != want || !strings.Contains(raw, style.Styled(want, "accent", "")) {
			t.Fatalf("a fresh press wants the depth named brightly over the clock: %q", word)
		}
	}
	// Once the notice is old the clock is back, and after the turn the
	// slot is empty whatever the depth.
	m.affordances.depthAt = time.Now().Add(-depthNoticeDuration)
	if !m.expireAffordances(time.Now()) || !m.affordances.depthAt.IsZero() {
		t.Fatal("the expired notice did not ask for a repaint")
	}
	if word, _ := left(); !strings.HasSuffix(word, "s") || strings.Contains(word, "abridged") {
		t.Fatalf("the clock wants its slot back: %q", word)
	}
	r.endTurn(nil)
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	m.affordances.depthAt = time.Now().Add(-depthNoticeDuration)
	if plain := plainStyledText(m.statusRow(120)); strings.Contains(plain, "full") {
		t.Fatalf("an old notice wants to be gone: %q", plain)
	}
}
