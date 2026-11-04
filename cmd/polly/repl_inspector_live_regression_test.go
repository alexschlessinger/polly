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

func TestInspectorLiveToolClockSurvivesProjectionAndStaleActivity(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	root := r.visibleTab()
	call := messages.ChatMessageToolCall{ID: "provider", DisplayID: "host-call", Name: "bash", Arguments: `{"command":"pwd"}`}
	started := time.Now().Add(-75 * time.Second)
	root.swarmSnapshot = &swarm.State{Members: map[string]*swarm.Member{"member": {ID: "member", Execution: "e"}}, Executions: map[string]*swarm.Execution{"e": {ID: "e", Generation: 2, Status: "running"}}}
	root.swarmActivities = map[string]swarm.LiveActivity{"member": {Execution: "e", Generation: 2, Tools: []swarm.LiveToolActivity{{Call: call, Started: started}}}}
	target := viewTarget{session: sessions.ViewTarget{ID: "member"}}
	r.inspect(target)
	live := r.inspectedLiveTools()
	if len(live) != 1 || !live[0].Started.Equal(started) {
		t.Fatalf("live tools lost the execution clock: %+v", live)
	}
	for range 3 {
		// Each revision can create a row whose hydration timestamp is now.
		// The metadata start belongs to the actual tool invocation.
		m := newReplModel()
		m.appendUserPrompt("task")
		testToolStart(m, call)
		original := m.displayCatalog.tools[0].started
		m.adoptLiveTools(live)
		record := m.currentTurnToolRecord()
		if record == nil || len(record.rows) != 1 || len(m.activeTools) != 1 {
			t.Fatalf("metadata duplicated the recorded call: %+v active=%+v", record, m.activeTools)
		}
		row := record.rows[0]
		if row.inline == nil {
			t.Fatal("active tool omitted its elapsed clock")
		}
		elapsed, err := time.ParseDuration(row.inline.duration)
		if err != nil || elapsed < 75*time.Second {
			t.Fatalf("clock restarted at projection: %+v", row.inline)
		}
		if !m.displayCatalog.tools[0].started.Equal(original) {
			t.Fatal("adoption mutated the immutable source timestamp")
		}
		testToolEnd(m, call, "done", nil)
		m.adoptLiveTools(live)
		row = m.currentTurnToolRecord().rows[0]
		if !row.settled || row.inline == nil || row.inline.glyph == "→" {
			t.Fatal("stale activity revived a completed canonical result")
		}
	}
	// A stale generation must not supply current calls.
	a := root.swarmActivities["member"]
	a.Generation = 1
	root.swarmActivities["member"] = a
	if len(r.inspectedLiveTools()) != 0 {
		t.Fatal("stale generation supplied live tools")
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
	if text := r.inspectorHeader(100, 20, 0, 0).text; !headerSaved(text) || strings.Contains(text, "Live transcript") {
		t.Fatalf("saved child falsely advertises live output: %q", text)
	}
}

func TestInspectorSavedCacheRewrapAndThemeDoNotRereadHistory(t *testing.T) {
	withDisplayTTY(t)
	restoreStyleEpochTestTheme(t)
	r, store, _ := savedChildViewFixture(t)
	// An activity line gives the body themed cells.
	child := testAcquireSession(t, store, "child")
	testAddMessages(t, child, []messages.ChatMessage{
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "c1", Name: "read_file", Arguments: `{"path":"a.go"}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "c1", ToolName: "read_file", Content: "package main\n"},
		{Role: messages.MessageRoleAssistant, Content: "Read it."},
	})
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	counted := &countedInspectorViews{SessionStore: store, ViewStore: store}
	r.state.sessionStore = counted
	r.inspect(viewTarget{session: sessions.ViewTarget{Name: "child"}})
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
