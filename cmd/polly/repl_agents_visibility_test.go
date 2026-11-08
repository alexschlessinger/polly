package main

import (
	"image"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/swarm"
)

// agentActionOrder is the list's targets in order, one per agent or heading.
func agentActionOrder(actions []string) []string {
	var order []string
	for n, action := range actions {
		if action != "" && (n == 0 || action != actions[n-1]) {
			order = append(order, action)
		}
	}
	return order
}

func TestAgentsInspectorSections(t *testing.T) {
	for _, tc := range []struct {
		name, execution, task string
		section               agentSection
		status, suffix        string
		anyStatus             bool
	}{
		{name: "settled", execution: "completed", task: "done", section: agentDone},
		{name: "running", execution: "running", task: "done", section: agentWorking},
		{name: "queued", execution: "queued", task: "done", section: agentWorking, status: "queued"},
		{name: "waiting", execution: "waiting", task: "done", section: agentWorking, status: "parked"},
		{name: "failed", execution: "failed", task: "done", section: agentDone},
		{name: "paused", execution: "paused", task: "done", section: agentDone},
		{name: "review", execution: "completed", task: "awaiting_review", section: agentWaiting, status: "review"},
		{name: "canceled", execution: "completed", task: "canceled", section: agentDone, suffix: "canceled"},
		{name: "unknown task", execution: "completed", task: "", section: agentWaiting, anyStatus: true},
		{name: "no task", execution: "completed", task: "", section: agentDone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, ids := agentsInspectorFixture(t)
			s := r.visibleTab().swarmSnapshot
			id := ids[2]
			s.Executions[id].Status, s.Tasks[id].Status = tc.execution, tc.task
			if tc.name == "no task" {
				delete(s.Tasks, id)
			}
			entry, exists := r.agentsInspectorEntries(80)[id]
			if !exists || entry.section != tc.section || !tc.anyStatus && entry.status != tc.status || entry.suffix != tc.suffix {
				t.Fatalf("exists=%v, entry %+v for %s", exists, entry, swarm.MemberState(s, s.Members[id]).Display)
			}
		})
	}
	for _, overlay := range []struct {
		name           string
		section        agentSection
		status, suffix string
	}{
		{"approval", agentWaiting, "approval", ""},
		{"queued approval", agentWaiting, "approval", ""},
		{"stopped", agentDone, "", "stopped"},
		{"integration", agentWaiting, "integration pending", ""},
		{"delivery", agentWorking, "delivering", ""},
	} {
		t.Run(overlay.name, func(t *testing.T) {
			r, ids := agentsInspectorFixture(t)
			s := r.visibleTab().swarmSnapshot
			id := ids[2]
			member, task, execution := s.Members[id], s.Tasks[id], s.Executions[id]
			switch overlay.name {
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
			if !exists || entry.section != overlay.section || entry.status != overlay.status || entry.suffix != overlay.suffix {
				t.Fatalf("%s: entry %+v", overlay.name, entry)
			}
		})
	}
}

func TestAgentsInspectorWaitReasons(t *testing.T) {
	r, ids := agentsInspectorFixture(t)
	r.model.approval = nil
	s := r.visibleTab().swarmSnapshot
	s.Runs = map[string]*swarm.Run{"run": {ID: "run", Status: "running", Starts: 3, Limit: 8}}
	for _, id := range ids {
		s.Executions[id].Run, s.Tasks[id].Run = "run", "run"
	}
	review, failed, asking := ids[0], ids[1], ids[2]
	s.Executions[review].Status, s.Tasks[review].Status = "completed", "awaiting_review"
	s.Tasks[review].Result = "\n# Summary\nMerged the slices; tests pass.\nDetails follow."
	s.Executions[failed].Status, s.Executions[failed].Error = "failed", "anthropic: 529 overloaded"
	s.Tasks[failed].Status = "blocked"
	s.Executions[asking].Status = "waiting"
	s.Messages = map[string]*swarm.Mail{
		"ask":  {ID: "ask", From: asking, To: r.visibleTab().viewID(), Kind: "request", Text: "Mention AppArmor?", Delivered: true},
		"done": {ID: "done", From: review, To: r.visibleTab().viewID(), Text: "Ready for review", Delivered: true},
	}
	// A working agent whose clock is warned spells the warning out.
	s.Members["retry"] = &swarm.Member{ID: "retry", Name: "retry", Label: "Audit worktree cleanup", Execution: "retry", Task: "retry-task"}
	s.Executions["retry"] = &swarm.Execution{ID: "retry", Run: "run", Member: "retry", Status: "running"}
	s.Tasks["retry-task"] = &swarm.Task{ID: "retry-task", Run: "run", Owner: "retry", Execution: "retry", Status: "running"}
	now := time.Now()
	activity := swarm.LiveActivity{Execution: "retry", Phase: "retrying", Since: now.Add(-3 * time.Minute), RetryCount: 2, NextRetryAt: now.Add(42*time.Second + 500*time.Millisecond), RetryReason: "429 rate limited", RequestActive: true, Attempt: 3}
	r.visibleTab().swarmActivities = map[string]swarm.LiveActivity{"retry": activity}
	extras := agentLiveExtras(activity, now)
	entries := r.agentsInspectorEntries(80)
	for id, want := range map[string]agentsInspectorEntry{
		review:  {section: agentWaiting, status: "review", why: "Merged the slices; tests pass."},
		failed:  {section: agentWaiting, status: "failed", why: "reason: anthropic: 529 overloaded"},
		asking:  {section: agentWaiting, status: "reply"},
		"retry": {section: agentWorking, status: "retrying  43s", why: extras, warn: true},
	} {
		if e := entries[id]; e.section != want.section || e.status != want.status || e.why != want.why || e.warn != want.warn {
			t.Fatalf("%s: section %v status %q why %q warn %v", s.Members[id].Name, e.section, e.status, e.why, e.warn)
		}
	}
	if extras == "" {
		t.Fatal("fixture has no warning to show")
	}
	// Abridged rows show the reason and, under every agent, its newest
	// message; the asking agent's message is its request.
	r.openAgentsInspector()
	r.refreshAgentsInspector(viewGeometry{width: 80})
	text := inspectorText(r.workspace().inspector.current)
	for _, line := range []string{"Waiting · 3", "Merged the slices; tests pass.\n    to parent: Ready for review", "reason: anthropic: 529 overloaded", "to parent: Mention AppArmor?", "Working · 1", "    " + extras} {
		if !strings.Contains(text, line) {
			t.Fatalf("list lacks %q: %s", line, text)
		}
	}
}

func TestAgentsInspectorDoneFoldAndResume(t *testing.T) {
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
	if !list.visible(id) || list.selected != id || list.entries[id].section != agentDone || r.childViews.entries["progress:"+id] != nil {
		t.Fatal("newly done agent lost visibility/selection or retained full checkpoint")
	}
	if text := inspectorText(r.workspace().inspector.current); !strings.Contains(text, "▾ Done · 1") || !strings.Contains(text, "worker") {
		t.Fatalf("done state: %s", text)
	}
	r.agentsInspectorAction(agentsDoneAction)
	r.refreshInspector(160)
	if len(list.order) != 1 || len(list.entries) != 1 || list.selected != agentsDoneAction || list.visible(id) || list.progress[id] != nil || r.childViews.entries["progress:"+id] != nil {
		t.Fatal("folded agent lost identity or retained selection/progress")
	}
	if text := inspectorText(r.workspace().inspector.current); !strings.Contains(text, "▸ Done · 1") || strings.Contains(text, "worker") {
		t.Fatalf("folded state: %s", text)
	}
	if root.swarmSnapshot.Members[id] != member || task.Status != "done" {
		t.Fatal("folding an agent mutated its coordination state")
	}
	// A folded member has no action, even if an old frame's click lands.
	r.agentsInspectorAction("agents-open:" + id)
	if r.workspace().inspector.target.kind != agentsViewKind {
		t.Fatal("stale action opened a folded member")
	}
	root.swarmSnapshot.Executions[member.Execution].Status, task.Status = "running", "running"
	r.refreshInspector(160)
	if !list.visible(id) || len(list.order) != 1 || list.entries[id].section != agentWorking {
		t.Fatal("resumed agent did not return to Working")
	}
	if text := inspectorText(r.workspace().inspector.current); strings.Contains(text, "Done ·") {
		t.Fatalf("empty Done section remained: %s", text)
	}
	delete(root.swarmSnapshot.Members, id)
	r.refreshInspector(160)
	if len(list.entries) != 0 || list.progress[id] != nil {
		t.Fatal("removed member retained list state")
	}
}

func TestAgentsInspectorDoneNavigation(t *testing.T) {
	r, ids := agentsInspectorFixture(t)
	r.openAgentsInspector()
	r.refreshAgentsInspector(viewGeometry{width: 36})
	state := r.workspace().viewState(r.workspace().inspector.target)
	list := state.agents
	r.chrome.inner = image.Rect(0, 0, 36, 10)
	r.inspectorHeaderRows = 1
	unfolded := []string{"agents-open:" + ids[0], "agents-open:" + ids[1], agentsDoneAction, "agents-open:" + ids[2]}
	if got := agentActionOrder(r.workspace().inspector.current.agentsActions); !slices.Equal(got, unfolded) {
		t.Fatalf("unfolded actions: %v", got)
	}
	r.navigateAgentsInspector("<End>")
	r.navigateAgentsInspector("<Up>")
	if list.selected != agentsDoneAction {
		t.Fatal("Up from the last done agent did not select the Done heading")
	}
	r.navigateAgentsInspector("<Left>")
	r.refreshAgentsInspector(viewGeometry{width: 36})
	if !list.doneCollapsed || r.workspace().inspector.target.kind != agentsViewKind || !strings.Contains(inspectorText(r.workspace().inspector.current), "▸ Done · 1") {
		t.Fatal("Left did not fold Done in place")
	}
	if got := agentActionOrder(r.workspace().inspector.current.agentsActions); !slices.Equal(got, unfolded[:3]) {
		t.Fatalf("folded actions: %v", got)
	}
	r.navigateAgentsInspector("<Right>")
	r.refreshAgentsInspector(viewGeometry{width: 36})
	if list.doneCollapsed || !strings.Contains(inspectorText(r.workspace().inspector.current), "▾ Done · 1") {
		t.Fatal("Right did not unfold Done")
	}
	r.navigateAgentsInspector("<Right>")
	if list.doneCollapsed {
		t.Fatal("Right folded an already unfolded section")
	}
	r.navigateAgentsInspector("<Down>")
	if list.selected != ids[2] {
		t.Fatal("Down did not select the done member")
	}
	r.navigateAgentsInspector("<Enter>")
	waitInspector(t, r, 160)
	if r.workspace().inspector.target.session.ID != ids[2] {
		t.Fatal("Enter did not open the done member")
	}
	r.inspectorAction("parent")
	r.refreshInspector(160)
	if list.doneCollapsed || list.selected != ids[2] {
		t.Fatal("returning from a member lost the fold state/selection")
	}
	r.agentsInspectorAction(agentsDoneAction)
	r.closeInspector()
	r.openAgentsInspector()
	r.refreshInspector(160)
	if !list.doneCollapsed || list.selected != agentsDoneAction {
		t.Fatal("reopening the list lost the fold state/selection")
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
		t.Fatal("idle list will never repaint")
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
	if entry := r.agentsInspectorEntries(80)[id]; entry.section != agentDone {
		t.Fatal("legacy tab replaced the swarm's completed status")
	}
	root.swarmSnapshot = nil
	if entry := r.agentsInspectorEntries(80)[id]; entry.section != agentWorking {
		t.Fatal("busy legacy child is not working")
	}
	child.model.busy = false
	if entry := r.agentsInspectorEntries(80)[id]; entry.section != agentDone || entry.status != "" {
		t.Fatalf("idle legacy child is not done: %+v", entry)
	}
	// A legacy tab's failed, paused or incomplete turn waits on the user.
	for _, outcome := range []turnOutcome{turnOutcomeFailed, turnOutcomePaused, turnOutcomeIncomplete} {
		child.model.lastOutcome, child.model.unseenOutcome = outcome, outcome
		if entry := r.agentsInspectorEntries(80)[id]; entry.section != agentWaiting || entry.status == "" {
			t.Fatalf("legacy outcome %v is not waiting: %+v", outcome, entry)
		}
	}
}
