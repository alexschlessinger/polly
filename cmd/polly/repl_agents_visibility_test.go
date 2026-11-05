package main

import (
	"image"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/swarm"
)

func TestAgentsInspectorRetainsEveryState(t *testing.T) {
	for _, tc := range []struct {
		name, execution, task string
		idle                  bool
	}{
		{"settled", "completed", "done", true},
		{"running", "running", "done", false},
		{"queued", "queued", "done", false},
		{"waiting", "waiting", "done", false},
		{"failed", "failed", "done", false},
		{"paused", "paused", "done", false},
		{"review", "completed", "awaiting_review", false},
		{"canceled", "completed", "canceled", true},
		{"unknown task", "completed", "", false},
		{"no task", "completed", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, ids := agentsInspectorFixture(t)
			root := r.visibleTab()
			s := root.swarmSnapshot
			id := ids[2]
			s.Executions[id].Status, s.Tasks[id].Status = tc.execution, tc.task
			if tc.name == "no task" {
				delete(s.Tasks, id)
			}
			r.refreshAgentIdle(root, time.Now().Add(-time.Hour))
			entry, exists := r.agentsInspectorEntries(80)[id]
			if !exists || entry.finished(time.Now()) != tc.idle {
				t.Fatalf("exists=%v, finished=%v for %s", exists, entry.finished(time.Now()), swarm.MemberState(s, s.Members[id]).Display)
			}
		})
	}
	for _, overlay := range []string{"approval", "queued approval", "stopped", "integration", "delivery"} {
		t.Run(overlay, func(t *testing.T) {
			r, ids := agentsInspectorFixture(t)
			root := r.visibleTab()
			s := root.swarmSnapshot
			id := ids[2]
			r.refreshAgentIdle(root, time.Now().Add(-time.Hour))
			member, task, execution := s.Members[id], s.Tasks[id], s.Executions[id]
			switch overlay {
			case "approval":
				r.model.approval = &approvalState{requester: id}
			case "queued approval":
				r.model.approvalQueue = []*approvalState{{requester: id}}
			case "stopped":
				member.Control = swarm.MemberControlStopped
			case "integration":
				task.Status, task.Snapshot = "awaiting_review", "snapshot"
				task.Revision, task.AcceptedRevision = 1, 1
			case "delivery":
				task.Status, task.Requirement = "running", swarm.RequirementDelivered
				execution.Result = &swarm.AgentResult{Task: id, Execution: id, Revision: task.Revision}
				if !swarm.MemberState(s, member).Delivering {
					t.Fatal("fixture is not delivering")
				}
			}
			entry, exists := r.agentsInspectorEntries(80)[id]
			if !exists || !entry.idleSince.IsZero() {
				t.Fatalf("%s agent missing or still aging: %+v", overlay, entry)
			}
		})
	}
}

func TestAgentsFinishedDelay(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name     string
		since    time.Time
		finished bool
	}{
		{"not idle", time.Time{}, false},
		{"future", now.Add(time.Minute), false},
		{"just idle", now, false},
		{"before boundary", now.Add(-agentsFinishedDelay + time.Nanosecond), false},
		{"boundary", now.Add(-agentsFinishedDelay), true},
		{"old", now.Add(-time.Hour), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := agentsInspectorEntry{history: true, idleSince: tc.since}
			if got := e.finished(now); got != tc.finished {
				t.Fatalf("finished=%v, want %v", got, tc.finished)
			}
		})
	}
	// An approval may arrive between observing the clock and building entries.
	if e := (agentsInspectorEntry{attention: true, idleSince: now.Add(-time.Hour)}); e.finished(now) {
		t.Fatal("an old clock hid an agent that now needs attention")
	}
}

func TestAgentsInspectorIdleDoneCleanupAndResume(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	root := r.visibleTab()
	id := addProgressWorker(t, store, root, "worker")
	member := root.swarmSnapshot.Members[id]
	member.Task = "task"
	task := &swarm.Task{ID: "task", Owner: id, Execution: member.Execution, Status: "running"}
	root.swarmSnapshot.Tasks = map[string]*swarm.Task{task.ID: task}
	r.openAgentsInspector()
	list := waitAllAgentProgress(t, r)
	list.selected = id
	if r.childViews.entries["progress:"+id] == nil {
		t.Fatal("running agent has no cached progress")
	}
	root.swarmSnapshot.Executions[member.Execution].Status, task.Status = "completed", "done"
	r.refreshInspector(160)
	if !list.visible(id) || list.selected != id || r.childViews.entries["progress:"+id] != nil {
		t.Fatal("newly finished agent lost visibility/selection or retained full checkpoint")
	}
	clock := root.agentIdle[id]
	clock.since = time.Now().Add(-agentsFinishedDelay)
	root.agentIdle[id] = clock
	r.refreshInspector(160)
	if len(list.order) != 1 || len(list.entries) != 1 || list.selected != agentsFinishedAction || list.visible(id) || list.progress[id] != nil || r.childViews.entries["progress:"+id] != nil {
		t.Fatal("collapsed agent lost identity or retained selection/progress")
	}
	if text := inspectorText(r.workspace().inspector.current); !strings.Contains(text, "▸ Finished (1)") || strings.Contains(text, "worker") || strings.Contains(text, "No active agents") {
		t.Fatalf("all-done state: %s", text)
	}
	if root.swarmSnapshot.Members[id] != member || task.Status != "done" {
		t.Fatal("folding an agent mutated its coordination state")
	}
	// A collapsed member has no action, even if an old frame's click lands.
	r.agentsInspectorAction("agents-open:" + id)
	if r.workspace().inspector.target.kind != agentsViewKind {
		t.Fatal("stale action opened a collapsed member")
	}
	root.swarmSnapshot.Executions[member.Execution].Status, task.Status = "running", "running"
	r.refreshInspector(160)
	if !list.visible(id) || len(list.order) != 1 || !list.entries[id].idleSince.IsZero() {
		t.Fatal("resumed agent did not reappear and reset its clock")
	}
	if text := inspectorText(r.workspace().inspector.current); strings.Contains(text, "Finished (") {
		t.Fatalf("empty Finished section remained: %s", text)
	}
	delete(root.swarmSnapshot.Members, id)
	r.refreshInspector(160)
	if len(list.entries) != 0 || len(root.agentIdle) != 0 || list.progress[id] != nil {
		t.Fatal("removed member retained list state")
	}
}

func TestAgentsInspectorFinishedNavigation(t *testing.T) {
	r, ids := agentsInspectorFixture(t)
	root := r.visibleTab()
	r.refreshAgentIdle(root, time.Now().Add(-agentsFinishedDelay))
	r.openAgentsInspector()
	r.refreshAgentsInspector(viewGeometry{width: 36})
	state := r.workspace().viewState(r.workspace().inspector.target)
	list := state.agents
	r.chrome.inner = image.Rect(0, 0, 36, 10)
	r.inspectorHeaderRows = 1
	want := []string{"agents-open:" + ids[0], "", "agents-open:" + ids[1], "", agentsFinishedAction}
	if got := r.workspace().inspector.current.agentsActions; !slices.Equal(got, want) {
		t.Fatalf("collapsed actions: %v", got)
	}
	r.navigateAgentsInspector("<End>")
	if list.selected != agentsFinishedAction {
		t.Fatal("End did not select Finished")
	}
	r.navigateAgentsInspector("<Right>")
	r.refreshAgentsInspector(viewGeometry{width: 36})
	if !list.finishedExpanded || !strings.Contains(inspectorText(r.workspace().inspector.current), "▾ Finished (1)") {
		t.Fatal("Right did not expand Finished")
	}
	r.navigateAgentsInspector("<Right>")
	if !list.finishedExpanded {
		t.Fatal("Right collapsed an already expanded section")
	}
	r.navigateAgentsInspector("<Left>")
	r.refreshAgentsInspector(viewGeometry{width: 36})
	if list.finishedExpanded || r.workspace().inspector.target.kind != agentsViewKind {
		t.Fatal("Left did not collapse Finished in place")
	}
	r.navigateAgentsInspector("<Enter>")
	r.refreshAgentsInspector(viewGeometry{width: 36})
	r.navigateAgentsInspector("<Down>")
	if list.selected != ids[2] {
		t.Fatal("Down did not select the finished member")
	}
	r.navigateAgentsInspector("<Enter>")
	waitInspector(t, r, 160)
	if r.workspace().inspector.target.session.ID != ids[2] {
		t.Fatal("Enter did not open the finished member")
	}
	r.inspectorAction("parent")
	r.refreshInspector(160)
	if !list.finishedExpanded || list.selected != ids[2] {
		t.Fatal("returning from a member lost expansion/selection")
	}
	r.closeInspector()
	r.openAgentsInspector()
	r.refreshInspector(160)
	if !list.finishedExpanded || list.selected != ids[2] {
		t.Fatal("reopening the list lost expansion/selection")
	}
}

func TestAgentIdleClockSurvivesClosedListAndResetsOnNewWork(t *testing.T) {
	r, ids := agentsInspectorFixture(t)
	root := r.visibleTab()
	id := ids[2]
	start := time.Now().Add(-time.Hour)
	// Observe completion without ever opening the list.
	r.refreshAgentIdle(root, start)
	r.openAgentsInspector()
	r.refreshInspector(160)
	if got := root.agentIdle[id].since; !got.Equal(start) {
		t.Fatalf("opening list reset clock: %v", got)
	}
	r.closeInspector()
	s := root.swarmSnapshot
	s.Members[id].Name, s.Members[id].Label = "renamed", "Renamed"
	s.Tasks[id].Revision++ // a review change is not a new execution
	r.refreshAgentIdle(root, time.Now())
	if !root.agentIdle[id].since.Equal(start) {
		t.Fatal("rename or task revision reset clock")
	}
	for _, change := range []string{"generation", "execution", "task", "busy", "approval"} {
		t.Run(change, func(t *testing.T) {
			now := time.Now()
			switch change {
			case "generation":
				s.Executions[s.Members[id].Execution].Generation++
			case "execution":
				s.Members[id].Execution = "new"
				s.Executions["new"] = &swarm.Execution{ID: "new", Status: "completed"}
			case "task":
				s.Members[id].Task = "new"
				s.Tasks["new"] = &swarm.Task{ID: "new", Status: "done"}
			case "busy":
				s.Executions[s.Members[id].Execution].Status = "running"
				r.refreshAgentIdle(root, now.Add(-time.Second))
				s.Executions[s.Members[id].Execution].Status = "completed"
			case "approval":
				r.model.approval = &approvalState{requester: id}
				r.refreshAgentIdle(root, now.Add(-time.Second))
				r.model.approval = nil
			}
			r.refreshAgentIdle(root, now)
			if !root.agentIdle[id].since.Equal(now) {
				t.Fatalf("%s did not restart grace period: %+v", change, root.agentIdle[id])
			}
		})
	}
}

func TestAgentsInspectorIdleTick(t *testing.T) {
	r, ids := agentsInspectorFixture(t)
	r.model.approval = nil
	s := r.visibleTab().swarmSnapshot
	for _, id := range ids {
		s.Executions[id].Status, s.Tasks[id].Status = "completed", "done"
	}
	r.openAgentsInspector()
	r.refreshInspector(160)
	if r.needsTick() {
		t.Fatal("idle list requested a frame before its refresh interval")
	}
	r.inspectorRefreshAt = time.Now().Add(-2 * time.Second)
	if !r.needsTick() {
		t.Fatal("idle list will never repaint to age finished agents")
	}
	r.refreshInspector(160)
	if r.needsTick() {
		t.Fatal("refresh left the idle list repainting continuously")
	}
}

func TestAgentsInspectorSwarmOwnsLegacyTabStatus(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root", "child")
	root, child := r.tabs[0], r.tabs[1]
	child.parent, child.workspaceRoot = root, false
	id := child.viewID()
	root.swarmSnapshot = &swarm.State{
		Members:    map[string]*swarm.Member{id: {ID: id, Name: "child", Execution: "execution", Task: "task"}},
		Executions: map[string]*swarm.Execution{"execution": {ID: "execution", Member: id, Status: "completed"}},
		Tasks:      map[string]*swarm.Task{"task": {ID: "task", Owner: id, Status: "done"}},
	}
	child.model.busy = true
	r.refreshAgentIdle(root, time.Now().Add(-time.Hour))
	if entry := r.agentsInspectorEntries(80)[id]; entry.active || !entry.finished(time.Now()) {
		t.Fatal("legacy tab replaced the swarm's completed status")
	}
	root.swarmSnapshot = nil
	if entry := r.agentsInspectorEntries(80)[id]; !entry.active || entry.finished(time.Now()) {
		t.Fatal("busy legacy child disappeared or retained swarm idle clock")
	}
	child.model.busy = false
	r.refreshAgentIdle(root, time.Now().Add(-time.Hour))
	if entry := r.agentsInspectorEntries(80)[id]; !entry.finished(time.Now()) {
		t.Fatal("idle legacy child did not age into Finished")
	}
	child.model.turnID++
	if entry := r.agentsInspectorEntries(80)[id]; entry.finished(time.Now()) {
		t.Fatal("new legacy turn retained its old idle clock")
	}
	// Visiting a legacy tab clears its unseen badge, not its actual outcome.
	for _, outcome := range []turnOutcome{turnOutcomeFailed, turnOutcomePaused, turnOutcomeCanceled, turnOutcomeIncomplete} {
		child.model.lastOutcome, child.model.unseenOutcome = outcome, turnOutcomeNone
		r.refreshAgentIdle(root, time.Now().Add(-time.Hour))
		if entry := r.agentsInspectorEntries(80)[id]; !entry.idleSince.IsZero() {
			t.Fatalf("viewed legacy outcome %v was eligible for Finished", outcome)
		}
	}
}
