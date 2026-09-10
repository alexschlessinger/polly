package main

import (
	"cmp"
	"image"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/swarm"
	"github.com/alexschlessinger/pollytool/workflow"
	ui "github.com/metaspartan/gotui/v5"
)

// settledWorkflowState seeds one workflow with a finished member, a released
// member and three that still need someone, plus a direct member that is done.
func settledWorkflowState() *swarm.State {
	s := &swarm.State{Members: map[string]*swarm.Member{}, Executions: map[string]*swarm.Execution{}, Tasks: map[string]*swarm.Task{}, Workflows: map[string]*workflow.Report{
		"wf": {ID: "wf", Name: "judges", Status: "completed"},
	}}
	add := func(id, execution, task string, control swarm.MemberControl) {
		s.Members[id] = &swarm.Member{ID: id, Name: id, Label: id, Execution: id, Task: id + "-task", Control: control}
		s.Executions[id] = &swarm.Execution{ID: id, Member: id, Status: execution, Request: swarm.AgentRequest{CallID: "wf/" + id}}
		s.Tasks[id+"-task"] = &swarm.Task{ID: id + "-task", Owner: id, Execution: id, Status: task, Revision: 1}
		s.Workflows["wf"].Steps = append(s.Workflows["wf"].Steps, workflow.Step{Operation: workflow.Operation{ID: "wf/" + id, Kind: "agent"}})
	}
	add("finished", "completed", "done", "")
	add("gone", "completed", "done", swarm.MemberControlEnabled)
	add("review", "completed", "awaiting_review", "")
	add("busy", "running", "running", "")
	add("failed", "failed", "blocked", "")
	s.Members["direct"] = &swarm.Member{ID: "direct", Name: "direct", Label: "direct", Execution: "direct", Task: "direct-task"}
	s.Executions["direct"] = &swarm.Execution{ID: "direct", Member: "direct", Status: "completed"}
	s.Tasks["direct-task"] = &swarm.Task{ID: "direct-task", Owner: "direct", Execution: "direct", Status: "done", Revision: 1}
	return s
}

// expandedAgentIDs expands every agents disclosure and returns the record
// IDs with the workflow's record first: the registry iterates in map order
// and hydration creates records from a map too, so the tests order them.
func expandedAgentIDs(m *replModel) []int64 {
	var ids []int64
	workflow := map[int64]bool{}
	for _, record := range m.toolDisclosures.all() {
		record.agentsExpanded = true
		ids = append(ids, record.id)
		for _, row := range record.rows {
			if row.agent.workflowID != "" {
				workflow[record.id] = true
			}
		}
	}
	slices.SortFunc(ids, func(a, b int64) int {
		if workflow[a] != workflow[b] {
			if workflow[a] {
				return -1
			}
			return 1
		}
		return cmp.Compare(a, b)
	})
	return ids
}

// Settled workflow members fold into a count on the heading; rows that still
// need someone, and direct spawn rows, stay listed with their links intact.
func TestWorkflowGroupCollapsesSettledMembers(t *testing.T) {
	withDisplayTTY(t)
	m := newReplModel()
	s := settledWorkflowState()
	m.hydrateSwarmAgents(s)
	ids := expandedAgentIDs(m)
	detail, links := m.agentDetail(ids, 120)
	lines := strings.Split(plainStyledText(detail), "\n")
	// The review and the failed member each owe the parent a decision.
	if len(lines) != 5 || !strings.Contains(lines[0], "Workflow · judges · 2 need decision · ▸ 2 done") {
		t.Fatalf("collapsed detail:\n%s", plainStyledText(detail))
	}
	for i, want := range []string{"review · idle · awaiting review", "busy · active", "failed · paused · failed", "direct · idle · done"} {
		if !strings.Contains(lines[i+1], want) {
			t.Fatalf("line %d = %q, want %q", i+1, lines[i+1], want)
		}
	}
	for _, hidden := range []string{"finished ·", "gone ·"} {
		if strings.Contains(plainStyledText(detail), hidden) {
			t.Fatalf("settled member still listed: %s", hidden)
		}
	}
	headings := 0
	var headingRecord int64
	for _, link := range links {
		if link.workflow != "" {
			headings++
			headingRecord = link.recordID
			if link.Y != 0 || link.workflow != "wf" {
				t.Fatalf("heading link off its line: %+v", link)
			}
			continue
		}
		row := m.toolDisclosures.get(link.recordID).rows[link.rowIndex]
		if !strings.Contains(lines[link.Y], row.agent.label+" ·") {
			t.Fatalf("link %+v does not cover %q: %q", link, row.agent.label, lines[link.Y])
		}
	}
	if headings != 1 || len(links) != 5 {
		t.Fatalf("links = %+v", links)
	}
	narrow, narrowLinks := m.agentDetail(ids, 20)
	rows := len(style.VisualRows(narrow, ui.StyleClear, 20))
	for _, link := range narrowLinks {
		if link.Y < 0 || link.Y >= rows || link.X < 0 || link.X+link.Cols > 20 {
			t.Fatalf("narrow link %+v outside %d rows", link, rows)
		}
	}
	for _, id := range []string{"review", "busy", "failed"} {
		s.Executions[id].Status = "completed"
		s.Tasks[id+"-task"].Status = "done"
	}
	m.hydrateSwarmAgents(s)
	detail, _ = m.agentDetail(ids, 120)
	lines = strings.Split(plainStyledText(detail), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "Workflow · judges · ▸ 5 done") || strings.Contains(lines[0], "decision") || !strings.Contains(lines[1], "direct · idle · done") {
		t.Fatalf("fully settled detail:\n%s", plainStyledText(detail))
	}
	m.toggleSettledAgents(headingRecord, "wf")
	detail, links = m.agentDetail(ids, 120)
	text := plainStyledText(detail)
	if !strings.Contains(text, "▾ 5 done") || !strings.Contains(text, "gone · idle · done") || !strings.Contains(text, "finished · idle · done") || len(links) != 7 {
		t.Fatalf("expanded detail (%d links):\n%s", len(links), text)
	}
}

// A click on the heading count shows the settled members without moving the
// scroll position or closing the Agents disclosure; the inspector pane's click
// path folds them again.
func TestWorkflowHeadingClickTogglesSettledMembers(t *testing.T) {
	r, _ := newChildTestREPL(t)
	m := r.model
	s := settledWorkflowState()
	m.hydrateSwarmAgents(s)
	expandedAgentIDs(m)
	m.appendLine("anchor below the agents")
	heading := func() agentLink {
		t.Helper()
		rows := m.transcriptRows(80)
		m.agentLinkPlacements = m.visibleAgentLinks(fullViewport(len(rows), 80))
		for _, link := range m.agentLinkPlacements {
			if link.workflow == "wf" {
				return link
			}
		}
		t.Fatalf("no heading link among %+v", m.agentLinkPlacements)
		return agentLink{}
	}
	link := heading()
	rows := transcriptRowsText(m.transcriptRows(80))
	m.followBottom = false
	for i, row := range rows {
		if strings.Contains(row, "anchor below") {
			m.scrollAnchor = i
			break
		}
	}
	r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: link.X, Y: link.Y}})
	if !m.settledAgentsShown["wf"] || r.workspace().inspector.open {
		t.Fatalf("heading click did not toggle the settled members: shown=%v inspector=%v", m.settledAgentsShown, r.workspace().inspector.open)
	}
	rows = transcriptRowsText(m.transcriptRows(80))
	if text := strings.Join(rows, "\n"); !strings.Contains(text, "▾ 2 done") || !strings.Contains(text, "gone · idle · done") || !strings.Contains(text, "finished · idle · done") {
		t.Fatalf("expanded transcript:\n%s", text)
	}
	if !strings.Contains(rows[m.scrollAnchor], "anchor below") {
		t.Fatalf("expansion moved the scroll anchor to %q", rows[m.scrollAnchor])
	}
	for _, record := range m.toolDisclosures.all() {
		if !record.agentsExpanded {
			t.Fatal("the click collapsed the agents disclosure")
		}
	}
	link = heading()
	if !r.inspectViewAt(m, viewTarget{}, image.Point{X: link.X, Y: link.Y}) || m.settledAgentsShown["wf"] {
		t.Fatal("inspector click did not fold the settled members")
	}
	if text := strings.Join(transcriptRowsText(m.transcriptRows(80)), "\n"); strings.Contains(text, "gone · idle · done") || !strings.Contains(text, "▸ 2 done") {
		t.Fatalf("folded transcript:\n%s", text)
	}
}
