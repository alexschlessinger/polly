package main

import (
	"context"
	"image"
	"slices"
	"strings"
	"testing"
	"time"

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
	if got := v.agentsActions; !slices.Equal(got, []string{"agents-open:" + ids[0], "", "agents-open:" + ids[1], "", "agents-open:" + ids[2]}) {
		t.Fatalf("list order: %v", got)
	}
	if text := inspectorText(v); !strings.Contains(text, "approval needed") || !strings.Contains(text, "Finished") || strings.Contains(text, "History") {
		t.Fatalf("list: %s", text)
	}
	for n, row := range v.view.Rows(v.model, r.inspectorGeometry(160).width) {
		text := plainCells(row)
		if v.agentsActions[n] == "" {
			if strings.TrimSpace(text) != "" {
				t.Fatalf("spacer has content: %q", text)
			}
			continue
		}
		label := row[strings.IndexFunc(text, func(c rune) bool { return c != ' ' && c != '›' })]
		if dimmed, want := label.Style.Fg == chromeColor("muted"), v.agentsActions[n] == "agents-open:"+ids[2]; dimmed != want {
			t.Fatalf("row %q dimmed=%v, want %v", text, dimmed, want)
		}
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
	// A newly settled agent remains selectable, after the live agents.
	r.model.approval = nil
	s := r.visibleTab().swarmSnapshot
	s.Executions[ids[0]].Status = "completed"
	s.Tasks[ids[0]].Status = "done"
	r.refreshInspector(160)
	if got := r.workspace().inspector.current.agentsActions; !slices.Equal(got, []string{"agents-open:" + ids[1], "", "agents-open:" + ids[0], "", "agents-open:" + ids[2]}) {
		t.Fatalf("newly settled agent list: %v", got)
	}
	if state.agents.selected != ids[0] {
		t.Fatal("settled agent lost selection")
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

func TestAgentsInspectorNavigationSkipsSpacing(t *testing.T) {
	r, ids := agentsInspectorFixture(t)
	r.visibleTab().swarmSnapshot.Executions[ids[2]].Status = "paused"
	r.openAgentsInspector()
	r.refreshAgentsInspector(viewGeometry{width: 36})
	state := r.workspace().viewState(r.workspace().inspector.target)
	// With one visible body row, keeping a trailing spacer on screen would
	// incorrectly scroll the selected agent's header out of view.
	r.chrome.inner = image.Rect(0, 0, 36, 2)
	r.inspectorHeaderRows = 1
	for _, step := range []struct {
		key string
		id  int
		top int
	}{
		{"<Home>", 0, 0}, {"<Down>", 1, 2}, {"<Up>", 0, 0},
		{"<PageDown>", 1, 2}, {"<End>", 2, 4}, {"<PageUp>", 1, 2},
	} {
		r.navigateAgentsInspector(step.key)
		if state.agents.selected != ids[step.id] || state.top != step.top {
			t.Fatalf("%s selected %q at top %d, want %q at %d", step.key, state.agents.selected, state.top, ids[step.id], step.top)
		}
	}
	if r.agentsInspectorAction("") || state.agents.selected != ids[1] {
		t.Fatal("spacer activated an agent")
	}
	r.navigateAgentsInspector("<Enter>")
	if r.workspace().inspector.target.session.ID != ids[1] {
		t.Fatal("Enter did not open the selected agent")
	}
}

func TestAgentsInspectorEnterDoesNotAnswerPendingApproval(t *testing.T) {
	for _, state := range []string{"ready", "loading", "empty"} {
		t.Run(state, func(t *testing.T) {
			r, ids := agentsInspectorFixture(t)
			r.model.approval = nil
			r.config.Confirm = true
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := startMemberApproval(r, ctx, ids[0])
			waitApprovalQueue(t, r.model, ids[0], 0)
			r.openAgentsInspector()
			waitInspector(t, r, 160)
			r.workspace().inspector.focused = true
			r.model.approvalPrompt(120)
			approval := r.model.approval
			if state == "loading" {
				r.workspace().inspector.current = nil
			} else if state == "empty" {
				r.workspace().inspector.current.agentsActions = nil
			} else {
				r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Home>"})
			}
			r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
			if r.model.approval != approval {
				t.Fatal("inspector Enter answered the approval")
			}
			select {
			case got := <-result:
				t.Fatalf("inspector Enter delivered approval result: %v", got)
			default:
			}
			if state == "ready" && r.workspace().inspector.target.session.ID != ids[0] {
				t.Fatal("Enter did not open the requesting agent")
			}
			cancel()
			assertApprovalResult(t, result, false)
		})
	}
}

func TestAgentsInspectorFromChildUsesRootSnapshot(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root", "child")
	root, child := r.tabs[0], r.tabs[1]
	child.parent = root
	child.workspaceRoot = false
	root.swarmSnapshot = &swarm.State{
		Members: map[string]*swarm.Member{"member": {ID: "member", Name: "Worker", Label: "Worker"}},
		Messages: map[string]*swarm.Mail{"mail": {
			ID: "mail", From: root.viewID(), To: "member", Text: "Check cancellation", Delivered: true,
		}},
	}
	r.openAgentsInspector()
	if r.workspace().inspector.target.session.ID != root.viewID() {
		t.Fatal("child inspector did not target its root")
	}
	entry, ok := r.agentsInspectorEntries(80)["member"]
	if !ok {
		t.Fatal("child inspector omitted root agents")
	}
	if entry.message == nil || entry.message.outgoing || entry.message.peer != "parent" || entry.message.text != "Check cancellation" {
		t.Fatalf("child inspector lost root message: %+v", entry.message)
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
	// Newly settled and paused agents both retain clickable rows.
	root.swarmSnapshot.Members["done"] = &swarm.Member{ID: "done", Name: "done", Label: "Completed", Execution: "done", Task: "done"}
	root.swarmSnapshot.Executions["done"] = &swarm.Execution{ID: "done", Member: "done", Status: "completed"}
	root.swarmSnapshot.Tasks["done"] = &swarm.Task{ID: "done", Owner: "done", Status: "done"}
	root.swarmSnapshot.Members["paused"] = &swarm.Member{ID: "paused", Name: "paused", Execution: "paused"}
	root.swarmSnapshot.Executions["paused"] = &swarm.Execution{ID: "paused", Member: "paused", Status: "paused"}
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
	if headerButton(r.inspectorButtons, "agents-open:done").Empty() {
		t.Fatal("newly finished agent is not clickable")
	}
	if headerButton(r.inspectorButtons, "agents-open:paused").Empty() {
		t.Fatal("paused agent row not clickable")
	}
	first := headerButton(r.inspectorButtons, "agents-open:m")
	gap := image.Pt(first.Min.X, first.Max.Y)
	for _, button := range r.inspectorButtons {
		if gap.In(button.rect) {
			t.Fatalf("spacer has a hit target: %+v", button)
		}
	}
	r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: gap.X, Y: gap.Y}})
	if r.workspace().inspector.target.kind != agentsViewKind {
		t.Fatal("clicking spacing opened an agent")
	}
	clock := root.agentIdle["done"]
	clock.since = time.Now().Add(-agentsFinishedDelay)
	root.agentIdle["done"] = clock
	r.render()
	if !headerButton(r.inspectorButtons, "agents-open:done").Empty() {
		t.Fatal("collapsed finished agent still clickable")
	}
	click(agentsFinishedAction)
	r.render()
	if headerButton(r.inspectorButtons, "agents-open:done").Empty() {
		t.Fatal("click did not reveal the finished agent")
	}
	click(agentsFinishedAction)
	r.render()
	if !headerButton(r.inspectorButtons, "agents-open:done").Empty() {
		t.Fatal("click did not collapse Finished")
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
