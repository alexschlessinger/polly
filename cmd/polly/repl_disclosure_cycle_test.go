package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/swarm"
	ui "github.com/metaspartan/gotui/v5"
)

func TestCtrlOCyclesGroupsOutputsDiffsAndMedia(t *testing.T) {
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
	rows := transcriptRowsText(m.transcriptRows(80))
	m.followBottom, m.scrollAnchor = false, len(rows)-5
	anchor := rows[m.scrollAnchor]
	// The one record is the newest turn's, so the default keeps it open.
	for _, mode := range []disclosureMode{disclosuresOpen, disclosuresDetailed, disclosuresFolded, disclosuresDefault, disclosuresOpen} {
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
		expand, details := mode != disclosuresFolded, mode == disclosuresDetailed
		if m.disclosures != mode || record.expanded != expand || record.agentsExpanded != expand || record.imagesExpanded != expand {
			t.Fatalf("mode %d: groups did not cycle: level=%d record=%+v", mode, m.disclosures, record)
		}
		for _, row := range record.rows {
			if row.outputExpanded != (details && !row.isAgent()) {
				t.Fatalf("mode %d: output %q expanded=%v", mode, row.callID, row.outputExpanded)
			}
		}
		rows = transcriptRowsText(m.transcriptRows(80))
		painted := strings.Join(rows, "\n")
		if strings.Contains(painted, "read-output-body") != details || strings.Contains(painted, "+delta") != details {
			t.Fatalf("mode %d: output/diff visibility is wrong:\n%s", mode, painted)
		}
		if strings.Contains(painted, "reasoning-visible-when-open") != expand || strings.Contains(painted, "Review changes") != expand {
			t.Fatalf("mode %d: reasoning/agent visibility is wrong:\n%s", mode, painted)
		}
		if rows[m.scrollAnchor] != anchor {
			t.Fatalf("mode %d moved held viewport from %q to %q", mode, anchor, rows[m.scrollAnchor])
		}
	}
}

func TestCtrlOLevelCarriesToNewRowsAndProjectedAgents(t *testing.T) {
	// The conversation's cycle: every level opens the newest turn's rows,
	// which these all are; details also opens their outputs.
	for _, mode := range []disclosureMode{disclosuresOpen, disclosuresDetailed, disclosuresFolded, disclosuresDefault} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			m := newReplModel()
			m.appendThinking("initial thought")
			for m.disclosures != mode || mode == disclosuresDefault && !m.currentReasoningRecord().expanded {
				m.cycleInlineDisclosures(80)
			}
			expand, details := mode != disclosuresFolded, mode == disclosuresDetailed
			m.finishThinkingSegment()
			m.appendThinking("later thought")
			if m.currentReasoningRecord().expanded != expand {
				t.Fatal("new thought did not inherit disclosure level")
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
				if record.expanded != expand || record.agentsExpanded != expand || record.imagesExpanded != expand {
					t.Fatalf("new or settled batch lost level %d: %+v", mode, record)
				}
				for _, row := range record.rows {
					if row.outputExpanded != details {
						t.Fatalf("new row %q lost details level %d", row.callID, mode)
					}
				}
			}
			m.projectSwarmAgents(&swarm.State{Members: map[string]*swarm.Member{"worker": {ID: "worker", Label: "Worker"}}})
			found := false
			for _, record := range m.toolDisclosures.all() {
				for _, row := range record.rows {
					if row.isProjectedAgent() {
						found = true
						if record.agentsExpanded != expand || row.outputExpanded {
							t.Fatal("projected agent did not inherit its list disclosure level")
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

func TestCtrlODetailsLoadsArtifactsWithoutReopeningFoldedOutput(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = r.work.close() })
	m := r.model
	store := &gatedInlineOutputStore{Store: testArtifactStore(t), gate: make(chan struct{}), started: make(chan struct{})}
	m.artifactStore = store
	ref, err := store.Put(context.Background(), artifacts.Blob{Kind: artifacts.KindText, Data: []byte("full artifact details")})
	if err != nil {
		t.Fatal(err)
	}
	m.beginTurn("read")
	call := messages.ChatMessageToolCall{ID: "read", Name: "read_file"}
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "preview", time.Millisecond, nil)
	tui.AppendToolResult(call, messages.ChatMessage{Content: "preview", Parts: []messages.ContentPart{{Type: "artifact", Artifact: &ref}}})
	screen.SetSize(140, 40)
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	r.render()
	if store.opens.Load() != 0 {
		t.Fatal("opened level read an artifact")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	r.render()
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("details level did not start the deferred output read")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	close(store.gate)
	runInlineOutputTask(t, r)
	r.render()
	record, row := m.toolDisclosureRowForCall(call.ID)
	if record.expanded || row.outputExpanded || strings.Contains(strings.Join(transcriptRowsText(m.transcriptRows(140)), "\n"), "full artifact details") {
		t.Fatal("late output read reopened folded content")
	}
	// Folded, then the default, then opened, then details again.
	for range 3 {
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
		r.render()
	}
	if store.opens.Load() != 1 || !strings.Contains(strings.Join(transcriptRowsText(m.transcriptRows(140)), "\n"), "full artifact details") {
		t.Fatal("details did not reuse the loaded artifact")
	}
}

func TestAgentViewOpensOpenAndCtrlOCyclesFromThere(t *testing.T) {
	withDisplayTTY(t)
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root", "agent")
	r.showTab(0)
	m := r.tabs[1].model
	m.appendUserPrompt(strings.Repeat("assignment line\n", 5) + "assignment-tail")
	for n := range 5 {
		call := testToolStart(m, messages.ChatMessageToolCall{ID: fmt.Sprint(n), Name: "bash", Arguments: fmt.Sprintf(`{"command":"command-%d"}`, n)})
		testToolEnd(m, call, fmt.Sprintf("output-body-%d", n), nil)
	}
	m.appendAssistant(strings.Repeat("update line\n", 8) + "update-tail")
	m.finishAssistantBlock("")
	r.inspect(tabViewTarget(r.tabs[1]))
	v := waitInspector(t, r, 140)
	s := r.workspace().viewState(v.target)
	check := func(mode disclosureMode) {
		t.Helper()
		if s.disclosures != mode {
			t.Fatalf("agent view level %d, want %d", s.disclosures, mode)
		}
		text := inspectorRowsText(v, 100)
		expand, details := mode >= disclosuresOpen, mode == disclosuresDetailed
		if strings.Contains(text, "command-0") != expand || !strings.Contains(text, "update-tail") || !strings.Contains(text, "assignment-tail") {
			t.Fatalf("mode %d: tool rows did not follow level, or the body lost the task: %q", mode, text)
		}
		if strings.Contains(text, "output-body-4") != details {
			t.Fatalf("mode %d: output did not follow level: %q", mode, text)
		}
	}
	// The view opens open: it is the agent's progress.
	check(disclosuresOpen)
	r.workspace().inspector.focused = true
	for _, mode := range []disclosureMode{disclosuresDetailed, disclosuresFolded, disclosuresOpen} {
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
		v = waitInspector(t, r, 140)
		check(mode)
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
	for _, mode := range []disclosureMode{disclosuresDetailed, disclosuresFolded, disclosuresOpen} {
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
		settle()
		after := transcriptRowsText(r.inspectorW.Rows[s.top : s.top+1])[0]
		if after != before || s.disclosures != mode || s.follow {
			t.Fatalf("mode %d moved held line: before=%q after=%q follow=%v", mode, before, after, s.follow)
		}
	}
}
