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

// settledWorkflowState seeds one workflow with finished, released, and canceled
// members and three that still need someone, plus a direct member that is done.
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
	add("canceled", "completed", "canceled", "")
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
		record.expanded = true
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

// Full depth keeps workflow members in order with their links intact as they
// settle. Only the workflow's decision count changes; no extra fold hides rows.
func TestWorkflowGroupKeepsSettledMembersVisible(t *testing.T) {
	withDisplayTTY(t)
	m := newReplModel()
	m.depth = depthFull
	s := settledWorkflowState()
	m.hydrateSwarmAgents(s)
	ids := expandedAgentIDs(m)
	wantHeading := "Workflow · judges · 2 need decision"
	// Labels pad to the widest, "finished", so statuses share one column.
	wantRows := []string{
		"finished  idle · done", "gone      idle · done", "canceled  idle · canceled",
		"review    idle · awaiting review", "busy      active", "failed    paused · failed", "direct    idle · done",
	}
	for _, phase := range []string{"mixed", "settled"} {
		if phase == "settled" {
			for _, id := range []string{"review", "busy", "failed"} {
				s.Executions[id].Status = "completed"
				s.Tasks[id+"-task"].Status = "done"
			}
			m.hydrateSwarmAgents(s)
			wantHeading = "Workflow · judges"
			wantRows[3], wantRows[4], wantRows[5] = "review    idle · done", "busy      idle · done", "failed    idle · done"
		}
		detail, links := m.agentDetail(ids, 120, "  ")
		lines := strings.Split(plainStyledText(detail), "\n")
		if len(lines) != len(wantRows)+1 || strings.TrimSpace(lines[0]) != wantHeading {
			t.Fatalf("%s detail:\n%s", phase, plainStyledText(detail))
		}
		for i, want := range wantRows {
			if !strings.Contains(lines[i+1], want) {
				t.Fatalf("%s line %d = %q, want %q", phase, i+1, lines[i+1], want)
			}
		}
		if len(links) != len(wantRows) {
			t.Fatalf("%s links = %+v", phase, links)
		}
		for i, link := range links {
			if link.Y != i+1 {
				t.Fatalf("%s link off its member row: %+v", phase, link)
			}
			row := m.toolDisclosures.get(link.recordID).rows[link.rowIndex]
			if runes := []rune(lines[link.Y]); link.X+link.Cols > len(runes) || string(runes[link.X:link.X+link.Cols]) != row.agent.label {
				t.Fatalf("link %+v does not cover %q: %q", link, row.agent.label, lines[link.Y])
			}
		}
		narrow, narrowLinks := m.agentDetail(ids, 20, "  ")
		rows := len(style.VisualRows(narrow, ui.StyleClear, 20))
		if len(narrowLinks) != len(wantRows) {
			t.Fatalf("%s narrow links = %+v", phase, narrowLinks)
		}
		for _, link := range narrowLinks {
			if link.Y < 0 || link.Y >= rows || link.X < 0 || link.X+link.Cols > 20 {
				t.Fatalf("narrow link %+v outside %d rows", link, rows)
			}
		}
	}
}

func TestSettledWorkflowMemberClickOpensInspector(t *testing.T) {
	for _, pane := range []string{"transcript", "inspector"} {
		t.Run(pane, func(t *testing.T) {
			r, _ := newChildTestREPL(t)
			m := r.model
			m.depth = depthFull
			m.hydrateSwarmAgents(settledWorkflowState())
			expandedAgentIDs(m)
			rows := m.transcriptRows(80)
			m.agentLinkPlacements = m.visibleAgentLinks(fullViewport(len(rows), 80))
			var finished agentLink
			found := false
			for _, link := range m.agentLinkPlacements {
				row := m.toolDisclosures.get(link.recordID).rows[link.rowIndex]
				if row.agent.viewID == "finished" {
					finished, found = link, true
					break
				}
			}
			if !found {
				t.Fatalf("no finished member link among %+v", m.agentLinkPlacements)
			}
			if pane == "transcript" {
				r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: finished.X, Y: finished.Y}})
			} else if !r.inspectViewAt(m, viewTarget{}, image.Point{X: finished.X, Y: finished.Y}) {
				t.Fatal("inspector click did not handle the finished member")
			}
			if !r.workspace().inspector.open || r.workspace().inspector.target.session.ID != "finished" || r.model != m {
				t.Fatalf("finished member click did not open its inspector: %+v", r.workspace().inspector.target)
			}
			for _, record := range m.toolDisclosures.all() {
				if !record.expanded {
					t.Fatal("the click collapsed the agents disclosure")
				}
			}
		})
	}
}
