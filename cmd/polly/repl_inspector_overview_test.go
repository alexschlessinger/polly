package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/swarm"
	ui "github.com/metaspartan/gotui/v5"
)

func TestAgentInspectionWatchNavigation(t *testing.T) {
	withDisplayTTY(t)
	fixture, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = fixture.work.close() })
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root", "agent")
	r.setupWidgets()
	r.showTab(0)
	child := r.tabs[1]
	child.model.hydrateHistory([]messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "Review cancellation coverage.\nKeep the existing API."},
		{Role: messages.MessageRoleAssistant, Content: "Found a missing cancellation case."},
	}, "agent")
	r.inspect(tabViewTarget(child))
	screen.SetSize(160, 36)
	waitInspector(t, r, 160)
	r.render()
	i := &r.workspace().inspector
	s := r.workspace().viewState(i.target)
	if !r.inspectorOverview() || r.inspectorW.PinBottom || r.inspectorW.TopRow != 0 {
		t.Fatal("agent did not open in a top-aligned inspection")
	}
	body := strings.Join(transcriptRowsText(r.inspectorW.Rows), "\n")
	if !strings.Contains(body, "Review cancellation coverage.") || !strings.Contains(body, "Found a missing cancellation case.") {
		t.Fatalf("inspection omitted the task or update: %q", body)
	}
	if headerButton(r.inspectorButtons, "watch").Empty() || headerButton(r.inspectorButtons, "message").Empty() {
		t.Fatal("inspection has no Watch or Message action")
	}
	r.inspectorAction("overview:assignment")
	r.render()
	if !s.overview.assignmentExpanded || s.promptExpanded {
		t.Fatal("inspection disclosure changed the transcript disclosure")
	}
	// Reach Watch through the same action cycle that the mouse uses.
	i.focused = true
	for n := 0; n < 20; n++ {
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<S-Tab>"})
		if selected, ok := r.selectedInspectorAction(); ok && selected.key == "button:watch" {
			break
		}
	}
	selected, ok := r.selectedInspectorAction()
	if !ok || selected.key != "button:watch" {
		t.Fatal("Watch is not keyboard reachable")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	waitInspector(t, r, 160)
	r.render()
	if r.inspectorOverview() || !s.watch || !i.maximized || !s.follow {
		t.Fatal("Watch did not open the following full-width transcript")
	}
	s.top, s.follow = 1, false
	back := headerButton(r.inspectorButtons, "inspection")
	if back.Empty() {
		t.Fatal("Watch has no Back to inspection action")
	}
	r.handleEvent(mouseEvent("<MouseLeft>", back.Min))
	r.render()
	if !r.inspectorOverview() || i.maximized || !s.overview.assignmentExpanded || s.top != 1 || s.follow {
		t.Fatal("returning lost inspection choices, pane size, or Watch position")
	}
	// Pane geometry and presentation are independent.
	r.inspectorAction("maximize")
	screen.SetSize(80, 28)
	waitInspector(t, r, 80)
	r.render()
	if !r.inspectorOverview() || !s.overview.assignmentExpanded || r.inspectorW.PinBottom {
		t.Fatal("maximizing/resizing changed the inspection presentation")
	}
	r.inspectorAction("watch")
	if s.top != 1 || s.follow {
		t.Fatal("re-entering Watch lost its reading position")
	}
}

func TestAgentInspectionScrollingAndRefresh(t *testing.T) {
	withDisplayTTY(t)
	fixture, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = fixture.work.close() })
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root", "agent")
	r.setupWidgets()
	r.showTab(0)
	child := r.tabs[1]
	child.model.hydrateHistory([]messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: strings.Repeat("Keep this assignment visible.\n", 70)},
		{Role: messages.MessageRoleAssistant, Content: "First update."},
	}, "agent")
	r.inspect(tabViewTarget(child))
	screen.SetSize(140, 30)
	waitInspector(t, r, 140)
	r.inspectorAction("overview:assignment")
	r.render()
	s := r.workspace().viewState(r.workspace().inspector.target)
	r.inspectorScroll(3)
	if s.overviewTop != 3 || s.top != 0 || !s.follow {
		t.Fatal("inspection scrolling changed the Watch position")
	}
	r.render()
	r.setScrollTop("inspector", 8)
	if s.overviewTop != 8 || s.top != 0 {
		t.Fatal("inspection scrollbar addressed the transcript")
	}
	child.model.appendAssistant("A newer update.")
	waitInspector(t, r, 140)
	r.render()
	if s.overviewTop != 8 || !s.overview.assignmentExpanded || r.inspectorW.PinBottom {
		t.Fatal("refresh moved the reader or closed the assignment")
	}
	r.workspace().inspector.focused = true
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Home>"})
	r.render()
	if s.overviewTop != 0 || s.top != 0 || !s.follow {
		t.Fatal("Home did not address only the inspection")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<End>"})
	r.render()
	if s.overviewTop == 0 || r.inspectorW.PinBottom {
		t.Fatal("End did not scroll the inspection without enabling follow")
	}
}

func TestWatchReadingAnchorSurvivesInspectionRewrap(t *testing.T) {
	withDisplayTTY(t)
	fixture, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = fixture.work.close() })
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root", "agent")
	r.setupWidgets()
	r.showTab(0)
	var answer strings.Builder
	for n := range 100 {
		fmt.Fprintf(&answer, "line %03d: %s\n", n, strings.Repeat("word ", 25))
	}
	r.tabs[1].model.hydrateHistory([]messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "task"},
		{Role: messages.MessageRoleAssistant, Content: answer.String()},
	}, "agent")
	screen.SetSize(160, 36)
	r.inspect(tabViewTarget(r.tabs[1]))
	settle := func() {
		// Paint the old projection first, just as the event loop does. Waiting
		// before paint would conceal stale-width anchor bugs.
		r.render()
		waitInspector(t, r, 160)
		r.render()
	}
	settle()
	r.inspectorAction("watch")
	settle()
	s := r.workspace().viewState(r.workspace().inspector.target)
	s.top, s.follow = 80, false
	r.render()
	before := transcriptRowsText(r.inspectorW.Rows[s.top : s.top+1])[0]
	r.inspectorAction("inspection")
	settle()
	r.inspectorAction("watch")
	settle()
	after := transcriptRowsText(r.inspectorW.Rows[s.top : s.top+1])[0]
	if before != after || s.follow {
		t.Fatalf("Watch lost its reading position: before=%q after=%q follow=%v", before, after, s.follow)
	}
}

func TestSwarmInspectionUsesCurrentExecutionToolsAndAssignment(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	r.setupWidgets()
	root := r.visibleTab()
	member := &swarm.Member{ID: "worker", Execution: "execution"}
	root.swarmSnapshot = &swarm.State{
		Members:    map[string]*swarm.Member{member.ID: member},
		Executions: map[string]*swarm.Execution{"execution": {ID: "execution", Member: member.ID, Status: "running", Generation: 2, Request: swarm.AgentRequest{Task: "Follow-up assignment"}}},
	}
	call := messages.ChatMessageToolCall{ID: "call", Name: "bash", Arguments: `{"command":"go test ./llm/..."}`}
	root.swarmActivities = map[string]swarm.LiveActivity{member.ID: {Execution: "execution", Generation: 2, Phase: "tools", Tools: []swarm.LiveToolActivity{{Call: call, Started: time.Now()}}}}
	target := tabViewTarget(root)
	target.session.ID, target.session.Name = member.ID, "worker"
	r.inspect(target)
	m := newReplModel()
	m.appendUserPrompt("Original assignment")
	r.workspace().inspector.current = &viewInstance{target: target, model: m, view: conversationView{}}
	r.renderInspectorOverview(70, 30, 0, 0)
	body := strings.Join(transcriptRowsText(r.inspectorW.Rows), "\n")
	for _, want := range []string{"Follow-up assignment", "Current operation", "go test ./llm/...", "Output pending"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in %q", want, body)
		}
	}
	if strings.Contains(body, "Original assignment") || len(m.displayCatalog.tools) != 0 {
		t.Fatal("live activity polluted saved history or used the old assignment")
	}
	var toggle string
	for _, button := range r.inspectorButtons {
		if strings.HasPrefix(button.action, "overview:tool:") {
			toggle = button.action
			break
		}
	}
	if toggle == "" {
		t.Fatal("active tool has no disclosure")
	}
	r.inspectorAction(toggle)
	r.renderInspectorOverview(70, 30, 0, 0)
	if strings.Contains(strings.Join(transcriptRowsText(r.inspectorW.Rows), "\n"), "Output pending") {
		t.Fatal("explicit close lost to active default")
	}
	r.inspectorAction("watch")
	if text := plainStyledText(r.inspectorHeader(90, 20, 0, 0).text); !strings.Contains(text, "go test ./llm/...") || !strings.Contains(text, "checkpoints") {
		t.Fatalf("Watch lacks truthful live operation: %q", text)
	}
	// A stale generation must not supply current calls.
	a := root.swarmActivities[member.ID]
	a.Generation = 1
	root.swarmActivities[member.ID] = a
	if len(r.agentOverviewState().liveTools) != 0 {
		t.Fatal("stale generation supplied live tools")
	}
}

func TestInspectionLiveChoicesDoNotLeakIntoRecordedCalls(t *testing.T) {
	m := newReplModel()
	call := messages.ChatMessageToolCall{ID: "reused", Name: "bash", Arguments: `{"command":"go test ./llm/..."}`}
	m.displayCatalog.startTool(call)
	m.displayCatalog.finishTool(call, "ok", time.Second, nil)
	state := agentOverviewState{liveExecution: "execution:1", liveTools: []swarm.LiveToolActivity{{Call: call, Started: time.Now()}}, tools: map[string]bool{}}
	live := agentOverviewLiveTools(state)[0]
	state.tools[live.key] = true
	for _, recorded := range agentOverviewTools(m) {
		if recorded.expanded(state) {
			t.Fatal("a repeated historical call ID inherited a transient live choice")
		}
	}
}

func TestWatchActiveToolDefaultsRespectChoices(t *testing.T) {
	m := newReplModel()
	call := messages.ChatMessageToolCall{ID: "call", Name: "bash", Arguments: `{"command":"go test ./llm/..."}`}
	record := m.appendToolCallStart(call)
	s := &viewState{}
	prepareAgentWatch(m, s)
	if !record.expanded || !record.rows[0].outputExpanded {
		t.Fatal("Watch hid its active operation")
	}
	record.expanded, record.rows[0].outputExpanded = false, false
	s.sections = map[string]viewSection{record.rows[0].sectionKey: {}}
	prepareAgentWatch(m, s)
	if record.expanded || record.rows[0].outputExpanded {
		t.Fatal("Watch reopened a manually closed operation")
	}
}

func TestAgentInspectionUnavailableStillShowsError(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root", "agent")
	r.setupWidgets()
	r.showTab(0)
	r.inspect(tabViewTarget(r.tabs[1]))
	v := waitInspector(t, r, 140)
	v.unavailable = true
	v.model = newReplModel()
	v.model.appendErrorLine("saved view unavailable")
	r.renderInspectorOverview(60, 20, 0, 0)
	if body := strings.Join(transcriptRowsText(r.inspectorW.Rows), "\n"); !strings.Contains(body, "saved view unavailable") {
		t.Fatalf("inspection hid the load failure: %q", body)
	}
}
