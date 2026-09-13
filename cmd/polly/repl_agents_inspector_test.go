package main

import (
	"context"
	"image"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
	ui "github.com/metaspartan/gotui/v5"
)

func agentsInspectorFixture(t *testing.T) (*managedREPL, []string) {
	t.Helper()
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	s := &swarm.State{Members: map[string]*swarm.Member{}, Executions: map[string]*swarm.Execution{}, Tasks: map[string]*swarm.Task{}}
	var ids []string
	for n, name := range []string{"Reviewer", "Builder", "Finished"} {
		child, err := store.Acquire(context.Background(), name, sessions.AcquireOptions{Parent: "root"})
		if err != nil {
			t.Fatal(err)
		}
		id := child.(sessions.ViewIdentity).ViewID()
		if err := child.Close(); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		status, taskStatus := "running", "running"
		if n == 2 {
			status, taskStatus = "completed", "done"
		}
		s.Members[id] = &swarm.Member{ID: id, Name: name, Label: name, Execution: id, Task: id}
		s.Executions[id] = &swarm.Execution{ID: id, Member: id, Status: status}
		s.Tasks[id] = &swarm.Task{ID: id, Owner: id, Status: taskStatus, Execution: id}
	}
	r.visibleTab().swarmSnapshot = s
	r.model.approval = &approvalState{requester: ids[0]}
	return r, ids
}

func TestAgentsInspectorListNavigationAndRefresh(t *testing.T) {
	r, ids := agentsInspectorFixture(t)
	r.model.ed.setText("preserve draft")
	r.openAttention()
	if r.model.modal != nil || r.workspace().inspector.target.kind != agentsViewKind {
		t.Fatal("badge did not open Agents inspector")
	}
	v := waitInspector(t, r, 160)
	if got := v.agentsActions; !slices.Equal(got, []string{"agents-open:" + ids[0], "agents-open:" + ids[1], "agents-history"}) {
		t.Fatalf("list order: %v", got)
	}
	if text := inspectorText(v); !strings.Contains(text, "approval needed") || strings.Contains(text, "Finished") {
		t.Fatalf("list: %s", text)
	}
	listTarget := r.workspace().inspector.target
	state := r.workspace().viewState(listTarget)
	state.top = 1
	r.inspectorAction("agents-open:" + ids[0])
	child := waitInspector(t, r, 160)
	if child.info.ID != ids[0] || len(r.tabs) != 1 || r.model.ed.text() != "preserve draft" {
		t.Fatal("agent inspection changed workspace or identity")
	}
	r.inspectorAction("parent")
	if r.workspace().inspector.target.kind != agentsViewKind || r.workspace().viewState(listTarget).top != 1 {
		t.Fatal("back lost list position")
	}
	v = waitInspector(t, r, 160)
	if state.agents.selected != ids[0] {
		t.Fatal("back lost selected agent")
	}
	// A finished selected agent remains reachable on return, under History.
	r.model.approval = nil
	s := r.visibleTab().swarmSnapshot
	s.Executions[ids[0]].Status = "completed"
	s.Tasks[ids[0]].Status = "done"
	r.refreshInspector(160)
	if !state.agents.historyExpanded {
		t.Fatal("finished selected agent disappeared into collapsed history")
	}
	if !slices.Contains(r.workspace().inspector.current.agentsActions, "agents-open:"+ids[0]) {
		t.Fatal("finished agent missing")
	}
	r.inspectorAction("agents-history")
	r.refreshInspector(160)
	if state.agents.historyExpanded || slices.Contains(r.workspace().inspector.current.agentsActions, "agents-open:"+ids[0]) {
		t.Fatal("history did not collapse")
	}
	// Opening the same agent independently must not inherit the list's back link.
	r.inspectorAction("agents-open:" + ids[1])
	waitInspector(t, r, 160)
	independent := r.workspace().inspector.target
	r.closeInspector()
	r.inspect(independent)
	waitInspector(t, r, 160)
	r.inspectorAction("parent")
	if r.workspace().inspector.open {
		t.Fatal("independent agent retained stale list parent")
	}
	// No runtime or lease was acquired by listing or inspecting a child.
	if sessionInUse(t, r.state.sessionStore, "Reviewer") || r.state.swarm != nil {
		t.Fatal("inspection activated an agent")
	}
}

func TestAgentsInspectorStableOrderAndWidth(t *testing.T) {
	r, ids := agentsInspectorFixture(t)
	r.openAgentsInspector()
	r.refreshAgentsInspector(viewGeometry{width: 36})
	list := r.workspace().viewState(r.workspace().inspector.target).agents
	before := slices.Clone(list.order)
	r.model.approval = &approvalState{requester: ids[1]}
	r.visibleTab().swarmSnapshot.Members[ids[1]].Label = strings.Repeat("界", 50)
	r.refreshAgentsInspector(viewGeometry{width: 36})
	if !slices.Equal(before, list.order) {
		t.Fatal("status refresh reordered rows")
	}
	v := r.workspace().inspector.current
	rows := v.view.Rows(v.model, 36)
	if len(rows) != len(v.agentsActions) {
		t.Fatalf("row hit targets lost alignment: %d/%d", len(rows), len(v.agentsActions))
	}
	for _, row := range rows {
		if style.CellsWidth(row) > 36 {
			t.Fatal("row overflow")
		}
	}
	r.workspace().inspector.focused = true
	r.model.approval = nil
	r.chrome.inner = image.Rect(0, 0, 36, 10)
	r.inspectorHeaderRows = 1
	r.navigateAgentsInspector("<End>")
	r.navigateAgentsInspector("<Enter>")
	r.refreshAgentsInspector(viewGeometry{width: 36})
	if !list.historyExpanded {
		t.Fatal("keyboard did not expand History")
	}
	r.navigateAgentsInspector("<End>")
	r.navigateAgentsInspector("<Enter>")
	if r.workspace().inspector.target.session.ID != ids[2] {
		t.Fatal("keyboard opened wrong agent")
	}
}

func TestAgentsInspectorMouseAndEmptyState(t *testing.T) {
	r, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = r.work.close() })
	screen.SetSize(140, 40)
	root := r.visibleTab()
	root.viewTarget.ID = "root"
	root.swarmSnapshot = decisionSnapshot()
	// Add enough completed rows to exercise History without another runtime.
	root.swarmSnapshot.Members["done"] = &swarm.Member{ID: "done", Name: "done", Label: "Completed", Execution: "done", Task: "done"}
	root.swarmSnapshot.Executions["done"] = &swarm.Execution{ID: "done", Member: "done", Status: "completed"}
	root.swarmSnapshot.Tasks["done"] = &swarm.Task{ID: "done", Owner: "done", Status: "done"}
	r.openAttention()
	r.render()
	click := func(action string) {
		t.Helper()
		rect := headerButton(r.inspectorButtons, action)
		if rect.Empty() {
			t.Fatalf("missing clickable row %s: %+v", action, r.inspectorButtons)
		}
		r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: rect.Min.X, Y: rect.Min.Y}})
	}
	click("agents-history")
	r.render()
	if headerButton(r.inspectorButtons, "agents-open:done").Empty() {
		t.Fatal("history row not clickable")
	}
	click("agents-open:m")
	if r.workspace().inspector.target.session.ID != "m" {
		t.Fatal("mouse opened wrong agent")
	}
	r.inspectorAction("parent")
	if r.workspace().inspector.target.kind != agentsViewKind {
		t.Fatal("mouse drill-down lost parent list")
	}
	root.swarmSnapshot = &swarm.State{}
	r.refreshInspector(140)
	if text := inspectorText(r.workspace().inspector.current); !strings.Contains(text, "No agents yet") {
		t.Fatalf("empty list: %s", text)
	}
}
