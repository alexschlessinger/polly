package main

import (
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/swarm"
)

func TestAgentsInspectorHidesOnlyIdleDone(t *testing.T) {
	for _, tc := range []struct {
		name, execution, task string
		visible               bool
	}{
		{"settled", "completed", "done", false},
		{"running", "running", "done", true},
		{"queued", "queued", "done", true},
		{"waiting", "waiting", "done", true},
		{"failed", "failed", "done", true},
		{"paused", "paused", "done", true},
		{"review", "completed", "awaiting_review", true},
		{"canceled", "completed", "canceled", true},
		{"unknown task", "completed", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, ids := agentsInspectorFixture(t)
			s := r.visibleTab().swarmSnapshot
			id := ids[2]
			s.Executions[id].Status, s.Tasks[id].Status = tc.execution, tc.task
			_, visible := r.agentsInspectorEntries(80)[id]
			if visible != tc.visible {
				t.Fatalf("visible=%v for %s", visible, swarm.MemberState(s, s.Members[id]).Display)
			}
		})
	}
	for _, overlay := range []string{"approval", "queued approval", "stopped", "integration", "delivery"} {
		t.Run(overlay, func(t *testing.T) {
			r, ids := agentsInspectorFixture(t)
			s := r.visibleTab().swarmSnapshot
			id := ids[2]
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
			if _, visible := r.agentsInspectorEntries(80)[id]; !visible {
				t.Fatalf("%s agent hidden", overlay)
			}
		})
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
	if len(list.order) != 0 || len(list.entries) != 0 || list.selected != "" || list.progress[id] != nil || r.childViews.entries["progress:"+id] != nil {
		t.Fatal("hidden agent retained list state or progress cache")
	}
	if text := inspectorText(r.workspace().inspector.current); !strings.Contains(text, "No active agents") || strings.Contains(text, "No agents yet") {
		t.Fatalf("all-done state: %s", text)
	}
	for _, key := range []string{"<Down>", "<Home>", "<End>", "<Enter>"} {
		if !r.navigateAgentsInspector(key) || r.workspace().inspector.target.kind != agentsViewKind {
			t.Fatalf("%s navigated to a hidden agent", key)
		}
	}
	if root.swarmSnapshot.Members[id] != member || task.Status != "done" {
		t.Fatal("hiding an agent mutated its coordination state")
	}
	root.swarmSnapshot.Executions[member.Execution].Status, task.Status = "running", "running"
	r.refreshInspector(160)
	if _, visible := list.entries[id]; !visible || len(list.order) != 1 {
		t.Fatal("resumed agent did not reappear")
	}
}

func TestAgentsInspectorDoesNotRestoreHiddenMemberFromChildTab(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root", "child")
	root, child := r.tabs[0], r.tabs[1]
	child.parent, child.workspaceRoot = root, false
	id := child.viewID()
	root.swarmSnapshot = &swarm.State{
		Members:    map[string]*swarm.Member{id: {ID: id, Name: "child", Execution: "execution", Task: "task"}},
		Executions: map[string]*swarm.Execution{"execution": {ID: "execution", Member: id, Status: "completed"}},
		Tasks:      map[string]*swarm.Task{"task": {ID: "task", Owner: id, Status: "done"}},
	}
	if _, visible := r.agentsInspectorEntries(80)[id]; visible {
		t.Fatal("legacy tab fallback restored an idle done swarm member")
	}
	// A real legacy child without swarm metadata is still listed.
	root.swarmSnapshot = nil
	if _, visible := r.agentsInspectorEntries(80)[id]; !visible {
		t.Fatal("legacy child disappeared without a completion record")
	}
}
