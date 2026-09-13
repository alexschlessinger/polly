package main

import (
	"context"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func TestAgentHistorySelectionAndDeferral(t *testing.T) {
	r := newSwarmTestREPL(t, integrationModel(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		return spawnTestReply("research complete")
	}), nil)
	root := r.visibleTab()
	runtime := root.state.swarm
	ctx := context.Background()
	report, err := runtime.RunWorkflow(ctx, `polly.defineWorkflow({name:"audit fixture",inputSchema:polly.schema.object({}),async run(){await polly.agent({label:"Test agent",task:"inspect",readOnly:true,review:true});polly.fail("incomplete")}})`, map[string]any{})
	if err == nil {
		t.Fatal("fixture did not fail")
	}
	if err := runtime.DeferWorkflow(ctx, report.ID, "retain for later review"); err != nil {
		t.Fatal(err)
	}
	refreshPickerSwarm(t, r)
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	r.model.ed.setText("unfinished main draft")
	r.openSessionsPicker()
	m := r.model.modal
	if len(m.items) != 1 || m.items[0].identity != root.viewID() || m.nested() {
		t.Fatalf("agent history leaked into sessions: %+v", m.items)
	}
	m.refresh()
	if pickerSelection(m) != root.viewID() || r.model.ed.text() != "unfinished main draft" {
		t.Fatal("refresh changed current session or draft")
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
		report, err := root.state.swarm.RunWorkflow(ctx, `polly.defineWorkflow({name:"`+name+`",inputSchema:polly.schema.object({}),async run(){const a=await polly.agent({label:"Test agent",task:"read",readOnly:true,review:true});const t=await polly.tasks.read(a.task);await polly.tasks.review({task:t.id,revision:t.revision,accept:true});return null}})`, map[string]any{})
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
	if len(order) != 0 {
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
	report, err := root.state.swarm.RunWorkflow(ctx, `polly.defineWorkflow({name:"saved audit",inputSchema:polly.schema.object({}),async run(){await polly.agent({label:"Test agent",task:"read",readOnly:true,review:true});polly.fail("incomplete")}})`, map[string]any{})
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
	item := pickerItem(t, r.model.modal, root.viewID())
	if len(r.model.modal.items) != 1 || r.model.ed.text() != "preserved draft" || len(r.tabs) != 1 {
		t.Fatalf("read-only history: %+v", item)
	}
	r.closeModal()
}
