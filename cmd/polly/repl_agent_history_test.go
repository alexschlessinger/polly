package main

import (
	"context"
	"image"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	ui "github.com/metaspartan/gotui/v5"
)

func TestAgentHistorySelectionAndDeferral(t *testing.T) {
	r := newSwarmTestREPL(t, integrationModel(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		return spawnTestReply("research complete")
	}), nil)
	root := r.visibleTab()
	runtime := root.state.swarm
	ctx := context.Background()
	report, err := runtime.RunWorkflow(ctx, `polly.defineWorkflow({name:"audit fixture",inputSchema:polly.schema.object({}),async run(){await polly.agent({task:"inspect",readOnly:true,review:true});polly.fail("incomplete")}})`, map[string]any{})
	if err == nil {
		t.Fatal("fixture did not fail")
	}
	if err := runtime.DeferWorkflow(ctx, report.ID, "retain for later review"); err != nil {
		t.Fatal(err)
	}
	refreshPickerSwarm(t, r)
	var member string
	for id := range root.swarmSnapshot.Members {
		member = id
	}
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	r.model.ed.setText("unfinished main draft")
	r.openSessionsPicker()
	m := r.model.modal
	r.pickerExpanded[root.name] = true
	historyID := "history:" + root.viewID()
	groupID := "workflow-history:" + report.ID
	if item := pickerItem(t, m, groupID); !strings.Contains(item.label, "1 deferred") {
		t.Fatalf("missing deferral count: %+v", item)
	}
	for i, item := range m.filteredItems() {
		if item.identity == member {
			t.Fatal("history opened by default")
		}
		if item.identity == historyID {
			m.selected = i
		}
	}
	r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Right>"})
	if !m.expanded[historyID] {
		t.Fatal("Right did not expand history")
	}
	for i, item := range m.filteredItems() {
		if item.identity == groupID {
			m.selected = i
		}
	}
	// Mouse uses the same group action and must not dismiss the dialog.
	m.listBounds = image.Rect(0, 0, 100, 30)
	m.top = 0
	r.handleModalEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: 2, Y: m.selected}})
	if r.model.modal != m || !m.expanded[groupID] {
		t.Fatal("mouse group action opened a session")
	}
	for i, item := range m.filteredItems() {
		if item.identity == member {
			m.selected = i
		}
	}
	m.refresh()
	if pickerSelection(m) != member || len(r.tabs) != 1 || r.model.ed.text() != "unfinished main draft" {
		t.Fatal("history refresh changed selection, runtime or draft")
	}
	if text, _ := r.agentsStatus(); text != "" {
		t.Fatalf("deferred history raised attention: %s", text)
	}
	r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Left>"})
	if m.expanded[groupID] || pickerSelection(m) != groupID {
		t.Fatal("Left did not select collapsed group")
	}
	r.closeModal()
}

func TestNestedHistoryHidesDescendantsAndRevealsSelectedIdentity(t *testing.T) {
	m := &replModal{expanded: map[string]bool{"root": true, "history": true, "workflow": true}, items: []replModalItem{
		{value: "root", identity: "root", children: 1},
		{value: "history", identity: "history", parent: "root", children: 1, groupOnly: true},
		{value: "workflow", identity: "workflow", parent: "history", children: 1, groupOnly: true},
		{value: "agent", identity: "agent", parent: "workflow"},
	}}
	if len(m.filteredItems()) != 4 {
		t.Fatal("expanded hierarchy incomplete")
	}
	m.expanded["root"] = false
	if len(m.filteredItems()) != 1 {
		t.Fatal("descendants leaked through collapsed root")
	}
	expandPickerSelection(m, "agent")
	if len(m.filteredItems()) != 4 {
		t.Fatal("selected agent's ancestors not exposed")
	}
	m.input.setText("agent")
	if items := m.filteredItems(); len(items) != 1 || items[0].identity != "agent" {
		t.Fatal("search lost historical agent")
	}
}

func TestHistoryOrdersAttemptsWithoutStartingRuntimes(t *testing.T) {
	r := newSwarmTestREPL(t, integrationModel(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return spawnTestReply("done") }), nil)
	root := r.visibleTab()
	ctx := context.Background()
	for _, name := range []string{"older", "newer"} {
		report, err := root.state.swarm.RunWorkflow(ctx, `polly.defineWorkflow({name:"`+name+`",inputSchema:polly.schema.object({}),async run(){const a=await polly.agent({task:"read",readOnly:true,review:true});const t=await polly.tasks.read(a.task);await polly.tasks.review({task:t.id,revision:t.revision,accept:true});return null}})`, map[string]any{})
		if err != nil || report == nil {
			t.Fatal(err)
		}
	}
	refreshPickerSwarm(t, r)
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	r.openSessionsPicker()
	m := r.model.modal
	var order []string
	for _, item := range m.items {
		if strings.HasPrefix(item.identity, "workflow-history:") {
			order = append(order, item.label)
		}
	}
	if len(order) != 2 || !strings.HasPrefix(strings.TrimSpace(order[0]), "newer") || !strings.HasPrefix(strings.TrimSpace(order[1]), "older") {
		t.Fatalf("history order: %v", order)
	}
	// Picker construction used summaries and the cached coordination state.
	if len(r.tabs) != 1 {
		t.Fatal("history activated child runtimes")
	}
}

func TestAgentHistoryForExternallyLeasedRoot(t *testing.T) {
	r := newSwarmTestREPL(t, integrationModel(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		return spawnTestReply("done")
	}), nil)
	root := r.visibleTab()
	ctx := context.Background()
	report, err := root.state.swarm.RunWorkflow(ctx, `polly.defineWorkflow({name:"saved audit",inputSchema:polly.schema.object({}),async run(){await polly.agent({task:"read",readOnly:true,review:true});polly.fail("incomplete")}})`, map[string]any{})
	if err == nil || report == nil {
		t.Fatal("fixture did not fail")
	}
	if err := root.state.swarm.DeferWorkflow(ctx, report.ID, "review later"); err != nil {
		t.Fatal(err)
	}
	// Keep the owner and its lease alive, but construct the displayed root
	// exactly as the read-only fallback does, without an execution runtime.
	owner := root.state
	view, err := owner.sessionStore.(sessions.ViewStore).ReadView(ctx, sessions.ViewTarget{ID: root.viewID()}, "")
	if err != nil || !view.InUse {
		t.Fatalf("leased fixture: %+v %v", view, err)
	}
	root.viewTarget, root.childView = sessions.ViewTarget{ID: view.ID}, view
	root.state = readOnlyConversationState(r.config, owner.sessionStore, view)
	defer func() { root.state = owner }()
	root.swarmSnapshot = nil
	r.model.ed.setText("preserved draft")
	refreshPickerSwarm(t, r)
	if root.swarmSnapshot == nil || root.state.swarm != nil || root.state.session != nil {
		t.Fatal("read-only status did not load independently of runtime/lease")
	}
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	r.openSessionsPicker()
	item := pickerItem(t, r.model.modal, "workflow-history:"+report.ID)
	if !strings.Contains(item.label, "1 deferred") || r.model.ed.text() != "preserved draft" || len(r.tabs) != 1 {
		t.Fatalf("read-only history: %+v", item)
	}
	r.closeModal()
}
