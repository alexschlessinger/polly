package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
)

func TestInspectorLiveOverviewUsesPairedExecutionNotBriefText(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	root := r.visibleTab()
	execution := &swarm.Execution{ID: "execution", Generation: 2, Status: "running", Request: swarm.AgentRequest{Task: "task"}}
	root.swarmSnapshot = &swarm.State{Members: map[string]*swarm.Member{"member": {ID: "member", Execution: execution.ID}}, Executions: map[string]*swarm.Execution{execution.ID: execution}}
	target := viewTarget{session: sessions.ViewTarget{ID: "member"}}
	r.inspect(target)
	m := newReplModel()
	m.appendUserPrompt("task\n\nCompletion: return through the assignment's completion path")
	m.appendAssistant("current execution update")
	m.finishAssistantBlock("")
	v := &viewInstance{target: target, model: m, view: conversationView{}, live: inspectorLiveStatus{identity: inspectorLiveIdentity{execution.ID, execution.Generation}}}
	r.workspace().inspector.current = v
	for _, active := range []bool{true, false} {
		v.live.active = active
		if text := overviewText(renderAgentOverview(m, 80, r.agentOverviewState())); !strings.Contains(text, "current execution update") {
			t.Fatalf("paired source hid actual assistant update (live=%v): %q", active, text)
		}
	}
	execution.Generation++
	if text := overviewText(renderAgentOverview(m, 80, r.agentOverviewState())); strings.Contains(text, "current execution update") {
		t.Fatalf("new generation inherited previous transcript: %q", text)
	}
	v.live.identity.generation = execution.Generation
	m.appendUserPrompt("new unanswered prompt")
	if text := overviewText(renderAgentOverview(m, 80, r.agentOverviewState())); strings.Contains(text, "current execution update") {
		t.Fatalf("unanswered follow-up inherited previous assistant answer: %q", text)
	}
}

func TestInspectorLiveToolClockSurvivesProjectionAndStaleActivity(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	root := r.visibleTab()
	call := messages.ChatMessageToolCall{ID: "provider", DisplayID: "host-call", Name: "bash", Arguments: `{"command":"pwd"}`}
	started := time.Now().Add(-75 * time.Second)
	root.swarmSnapshot = &swarm.State{Members: map[string]*swarm.Member{"member": {ID: "member", Execution: "e"}}, Executions: map[string]*swarm.Execution{"e": {ID: "e", Generation: 2, Status: "running"}}}
	root.swarmActivities = map[string]swarm.LiveActivity{"member": {Execution: "e", Generation: 2, Tools: []swarm.LiveToolActivity{{Call: call, Started: started}}}}
	target := viewTarget{session: sessions.ViewTarget{ID: "member"}}
	r.inspect(target)
	for _, width := range []int{70, 100, 50} {
		// Each revision/resize can create a catalog whose hydration timestamp
		// is now. The metadata start belongs to the actual tool invocation.
		m := newReplModel()
		m.displayCatalog.startTool(call)
		original := m.displayCatalog.tools[0].started
		r.workspace().inspector.current = &viewInstance{target: target, model: m, view: conversationView{}}
		state := r.agentOverviewState()
		if len(state.liveTools) != 0 || !state.toolStarted[call.DisplayID].Equal(started) {
			t.Fatalf("metadata was duplicated or lost its execution clock: %+v", state)
		}
		row := agentOverviewTools(m)[0].displayRow(state)
		if row.inline == nil {
			t.Fatal("active tool omitted its elapsed clock")
		}
		elapsed, err := time.ParseDuration(row.inline.duration)
		if err != nil || elapsed < 75*time.Second {
			t.Fatalf("width=%d clock restarted at projection: %+v", width, row.inline)
		}
		renderAgentOverview(m, width, state)
		if !m.displayCatalog.tools[0].started.Equal(original) {
			t.Fatal("render mutated the immutable source timestamp")
		}
		m.displayCatalog.finishTool(call, "done", time.Second, nil)
		row = agentOverviewTools(m)[0].displayRow(r.agentOverviewState())
		if !row.settled || row.inline == nil || row.inline.glyph == "→" {
			t.Fatal("stale activity revived a completed canonical result")
		}
	}
}

func TestInspectorSavedChildTabNeverAdvertisesLive(t *testing.T) {
	r, _, activity := savedChildViewFixture(t)
	child := clickChildView(t, r, activity)
	driveChildView(t, r, func() bool { return !child.viewLoading })
	child.keepOpen = true
	child.model.appendAssistant("stale display only, not durable")
	child.model.finishAssistantBlock("")
	target := tabViewTarget(child)
	r.showTab(0)
	if r.inspectionTab(target) != child || child.childView == nil {
		t.Fatal("fixture lost its saved-only tab")
	}
	r.inspect(target)
	v := waitInspector(t, r, 140)
	if v.live.active || v.saved == nil || strings.Contains(inspectorText(v), "stale display only") || !strings.Contains(inspectorText(v), "saved child answer") {
		t.Fatalf("saved tab used local model as live source: %+v %q", v.live, inspectorText(v))
	}
	if text := plainStyledText(r.inspectorHeader(100, 20, 0, 0).text); !strings.Contains(text, "Saved transcript") || strings.Contains(text, "Live transcript") {
		t.Fatalf("saved child falsely advertises live output: %q", text)
	}
}

func TestInspectorSavedCacheRewrapAndThemeDoNotRereadHistory(t *testing.T) {
	withDisplayTTY(t)
	restoreStyleEpochTestTheme(t)
	r, store, _ := savedChildViewFixture(t)
	counted := &countedInspectorViews{SessionStore: store, ViewStore: store}
	r.state.sessionStore = counted
	r.inspect(viewTarget{session: sessions.ViewTarget{Name: "child"}})
	state := r.workspace().viewState(r.workspace().inspector.target)
	state.promptExpanded = true
	v := waitInspector(t, r, 140)
	base, histories := v.saved, counted.histories.Load()
	style.Apply(style.Theme{Name: "epoch-test", Colors: epochTestThemeColors})
	v = waitInspector(t, r, 200)
	rows := v.view.Rows(v.model, r.inspectorGeometry(200).width)
	if v.saved != base || counted.histories.Load() != histories {
		t.Fatal("geometry/theme change rehydrated saved history instead of cloning cached display")
	}
	if _, _, ok := firstCellWithForeground(rows, epochThemeColor(t, "accent")); !ok {
		t.Fatal("cached display retained old theme colors on rewrap")
	}
}

func TestInspectorSwarmProjectionRevisionAndResize(t *testing.T) {
	r := newSwarmTestREPL(t, integrationModel(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return spawnTestReply("done") }), nil)
	target := tabViewTarget(r.visibleTab())
	target.kind, target.item = swarmViewKind, "members"
	r.inspect(target)
	v := waitInspector(t, r, 140)
	if !strings.HasPrefix(v.revision, "swarm:") || v.saved != nil || v.failures != 0 {
		t.Fatalf("swarm projected as saved conversation: revision=%q failures=%d", v.revision, v.failures)
	}
	model := v.model
	r.inspectorRefreshAt = time.Time{}
	v = waitInspector(t, r, 140)
	if v.model != model {
		t.Fatal("unchanged swarm revision unnecessarily rebuilt projection")
	}
	v = waitInspector(t, r, 200)
	if v.model == model || v.failures != 0 || !strings.HasPrefix(v.revision, "swarm:") {
		t.Fatal("swarm resize reused an empty revision or failed to reproject")
	}
}
