package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/swarm"
	"github.com/alexschlessinger/pollytool/workflow"
	ui "github.com/metaspartan/gotui/v5"
)

func projectedAgentRows(m *replModel) map[string][]*agentActivity {
	rows := map[string][]*agentActivity{}
	for _, record := range m.toolDisclosures.all() {
		for _, row := range record.rows {
			if row.agent != nil && row.agent.viewID != "" {
				rows[row.agent.viewID] = append(rows[row.agent.viewID], row.agent)
			}
		}
	}
	return rows
}

func TestSwarmProjectionGroupsWorkflowMembersAndPreservesDirectRows(t *testing.T) {
	m := newReplModel()
	direct := agentCall("direct", `{"label":"direct reviewer"}`)
	workflowCall := messages.ChatMessageToolCall{ID: "workflow-call", Name: "workflow_run", Arguments: `{}`}
	m.hydrateHistory([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "review"}, {Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{direct, workflowCall}}}, "parent")
	launch := m.currentToolDisclosure()
	// Hydrated history need not retain the live turn pointer.
	for _, record := range m.toolDisclosures.all() {
		for _, row := range record.rows {
			if row.callID == workflowCall.ID {
				launch = record
			}
		}
	}
	if launch == nil {
		t.Fatal("missing workflow launch disclosure")
	}
	s := &swarm.State{Members: map[string]*swarm.Member{}, Executions: map[string]*swarm.Execution{}, Workflows: map[string]*workflow.Report{
		"workflow": {ID: "workflow", CallID: workflowCall.ID, Name: "review change", Steps: []workflow.Step{
			{Operation: workflow.Operation{ID: "workflow/1", Kind: "agent"}},
			{Operation: workflow.Operation{ID: "workflow/2", Kind: "agent"}},
			{Operation: workflow.Operation{ID: "workflow/3", Kind: "agent"}},
		}},
	}}
	for _, id := range []string{"direct", "worker-a", "worker-b", "typed"} {
		s.Members[id] = &swarm.Member{ID: id, Name: id, Label: id, Status: "running", Execution: id}
	}
	for id, call := range map[string]string{"direct": "direct", "worker-a": "workflow/1", "worker-b": "workflow/2", "typed": ""} {
		s.Executions[id] = &swarm.Execution{ID: id, Member: id, Request: swarm.AgentRequest{CallID: call}}
	}
	// Continuation steps belong to the same member and must not duplicate it.
	s.Executions["continued"] = &swarm.Execution{ID: "continued", Member: "worker-a", Request: swarm.AgentRequest{CallID: "workflow/3"}}
	for range 3 {
		m.hydrateSwarmAgents(s)
	}
	rows := projectedAgentRows(m)
	if len(rows) != 4 {
		t.Fatalf("member rows=%v", rows)
	}
	for id, agents := range rows {
		if len(agents) != 1 {
			t.Errorf("member %s has %d rows", id, len(agents))
		}
	}
	if m.toolDisclosures.count() != 2 {
		t.Fatalf("expected original launch and typed disclosure, got %d", m.toolDisclosures.count())
	}
	if len(ordinaryToolRows(launch.rows)) != 1 {
		t.Fatal("projected agents inflated the tool count")
	}
	if rows["direct"][0].label != "direct reviewer" {
		t.Fatal("direct launch label changed")
	}
	for _, id := range []string{"worker-a", "worker-b"} {
		found := false
		for _, row := range launch.rows {
			found = found || row.agent != nil && row.agent.viewID == id
		}
		if !found {
			t.Fatalf("%s not attached to workflow launch", id)
		}
	}
	launch.agentsExpanded = true
	detail, links := m.agentDetail([]int64{launch.id}, 32)
	if strings.Count(plainStyledText(detail), "Workflow · review change") != 1 || len(links) < 3 {
		t.Fatalf("workflow group or links missing: %s, %+v", detail, links)
	}
	visual := transcriptVisualRows(detail, ui.StyleClear, 32)
	for _, link := range links {
		if link.Y >= len(visual) {
			t.Fatalf("agent link outside wrapped detail: %+v", link)
		}
	}
	// Queued approvals and updated usage use the same rows as direct launches.
	m.approvalQueue = []*approvalState{{requester: "worker-a"}}
	in, out := 123, 45
	s.Executions["worker-a"].Usage = swarm.Usage{InputTokens: &in, OutputTokens: &out}
	m.hydrateSwarmAgents(s)
	a := rows["worker-a"][0]
	if a.status != "approval needed" || a.inputTokens != 123 || a.outputTokens != 45 {
		t.Fatalf("live projection: %+v", a)
	}
}

func TestSwarmProjectionRestoresMembersWithoutToolHistoryAndKeepsStreaming(t *testing.T) {
	m := newReplModel()
	m.appendAssistant("answer in progress")
	current := m.currentAssistant
	s := &swarm.State{Members: map[string]*swarm.Member{"member": {ID: "member", Name: "renamable", Status: "waiting"}}}
	m.hydrateSwarmAgents(s)
	m.appendAssistant(" continues")
	m.renderPendingMarkdown()
	if m.currentAssistant != current || !strings.Contains(m.transcript[current].text, "answer in progress continues") {
		t.Fatalf("member projection split the parent reply: current=%d previous=%d transcript=%+v", m.currentAssistant, current, m.transcript)
	}
	if len(projectedAgentRows(m)["member"]) != 1 {
		t.Fatal("missing member without tool history")
	}
	view := viewState{}
	for _, record := range m.toolDisclosures.all() {
		record.agentsExpanded = true
	}
	rememberViewSections(m, &view)
	s.Members["member"].Name = "renamed"
	s.Members["member"].Status = "retired"
	m.hydrateSwarmAgents(s)
	a := projectedAgentRows(m)["member"][0]
	if a.session != "renamed" || a.status != "retired" || a.active {
		t.Fatalf("member update: %+v", a)
	}
	m.hydrateHistory(nil, "parent")
	m.hydrateSwarmAgents(s)
	applyViewSections(m, view)
	if len(projectedAgentRows(m)["member"]) != 1 {
		t.Fatal("restored member missing or duplicated")
	}
	for _, record := range m.toolDisclosures.all() {
		if !record.agentsExpanded {
			t.Fatal("restored member lost its inspector disclosure state")
		}
	}
}

func TestSwarmProjectionKeepsInterleavedWorkflowsGrouped(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "parent", 0, 0)
	m := r.model
	m.beginTurn("review")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.AppendToolStart([]messages.ChatMessageToolCall{
		{ID: "first", Name: "workflow_start"}, {ID: "second", Name: "workflow_start"},
	})
	s := &swarm.State{Members: map[string]*swarm.Member{}, Executions: map[string]*swarm.Execution{}, Workflows: map[string]*workflow.Report{}}
	add := func(id, group string) {
		s.Members[id] = &swarm.Member{ID: id, Name: id, Status: "idle"}
		s.Executions[id] = &swarm.Execution{Member: id, Request: swarm.AgentRequest{CallID: id}}
		s.Workflows[group].Steps = append(s.Workflows[group].Steps, workflow.Step{Operation: workflow.Operation{ID: id, Kind: "agent"}})
		m.hydrateSwarmAgents(s)
	}
	s.Workflows["first"] = &workflow.Report{ID: "first", CallID: "first", Name: "first [review]"}
	s.Workflows["second"] = &workflow.Report{ID: "second", CallID: "second", Name: "second review"}
	add("one", "first")
	lateCall := messages.ChatMessageToolCall{ID: "late", Name: "read_file"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{lateCall})
	add("two", "second")
	add("three", "first")
	if got := m.turnToolCallCount(); got != 3 {
		t.Fatalf("projected members inflated execution count to %d", got)
	}
	_, lastTool := m.toolDisclosureRowForCall("")
	if lastTool == nil || lastTool.callID != lateCall.ID {
		t.Fatal("tool media fallback selected a projected member")
	}
	tui.AppendToolEnd(lateCall, "contents", time.Millisecond, nil)
	_, lastTool = m.toolDisclosureRowForCall(lateCall.ID)
	if !lastTool.settled {
		t.Fatal("late workflow member displaced an active tool row")
	}
	blocks := activityBlocks(m, 120)
	if len(blocks) != 1 || !m.toggleAgentDisclosureGroup(blocks[0].toolDisclosureIDs) {
		t.Fatalf("workflow batch did not share an Agents disclosure: %+v", blocks)
	}
	text := strings.Join(transcriptRowsText(m.transcriptRows(120)), "\n")
	if strings.Count(text, "Workflow · first [review]") != 1 || strings.Count(text, "Workflow · second review") != 1 {
		t.Fatalf("workflow headings repeated or escaped incorrectly:\n%s", text)
	}
	if strings.Index(text, "three · idle") > strings.Index(text, "Workflow · second review") {
		t.Fatalf("late member appeared under the wrong workflow:\n%s", text)
	}
}

func TestTypedAndWorkflowLaunchesExposeClickableMemberRows(t *testing.T) {
	withDisplayTTY(t)
	release := make(chan struct{})
	r := newSwarmTestREPL(t, integrationModel(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return spawnTestReply("done")
	}), nil)
	defer close(release)
	r.runTabCommand("/spawn --read-only inspect typed work")
	runUITask(t, r)
	if len(projectedAgentRows(r.model)) != 1 {
		t.Fatal("typed launch did not show an agent row immediately")
	}
	call := messages.ChatMessageToolCall{ID: "workflow-launch", Name: "workflow_start", Arguments: `{}`}
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	source := `polly.defineWorkflow({name:"parallel review",inputSchema:polly.schema.object({}),async run(){return await polly.parallel(["one","two"], label=>polly.agent({task:"review "+label,label,readOnly:true,tools:[]}));}});`
	id, err := r.state.swarm.StartWorkflow(withToolCall(context.Background(), call), source, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var state *swarm.State
	deadline := time.After(5 * time.Second)
	for {
		state, err = r.state.swarm.State(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(state.Members) == 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("workflow members did not start")
		case <-time.After(time.Millisecond):
		}
	}
	if state.Workflows[id].CallID != call.ID {
		t.Fatal("workflow launch provenance was not saved")
	}
	r.model.mu.Lock()
	r.model.hydrateSwarmAgents(state)
	rows := projectedAgentRows(r.model)
	if len(rows) != 3 {
		r.model.mu.Unlock()
		t.Fatalf("only %d of 3 members are visible", len(rows))
	}
	var recordID int64
	var memberID string
	for _, record := range r.model.toolDisclosures.all() {
		for _, row := range record.rows {
			if row.agent != nil && row.agent.workflowID == id {
				recordID = record.id
				memberID = row.agent.viewID
				record.agentsExpanded = true
			}
		}
	}
	_, links := r.model.agentDetail([]int64{recordID}, 120)
	if len(links) == 0 {
		r.model.mu.Unlock()
		t.Fatal("workflow member has no inspector link")
	}
	link := links[0]
	memberID = r.model.toolDisclosures.get(link.recordID).rows[link.rowIndex].agent.viewID
	if !r.inspectAgent(r.model, tabViewTarget(r.visibleTab()), link) {
		r.model.mu.Unlock()
		t.Fatal("workflow member link did not open")
	}
	r.model.mu.Unlock()
	waitInspector(t, r, 140)
	if got := r.workspace().inspector.current.info.ID; got != memberID {
		t.Fatalf("inspected %s, wanted %s", got, memberID)
	}
	if len(r.tabs) != 1 {
		t.Fatal("inspecting a member created an execution tab")
	}
	// Reopening the parent hydrates members even when its saved transcript
	// contains no spawn tool calls (as with these command-driven launches).
	_, restored, err := r.newTabModelContext(context.Background(), r.state)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(projectedAgentRows(restored)); got != 3 {
		t.Fatalf("restored parent exposes %d of 3 members", got)
	}
}
