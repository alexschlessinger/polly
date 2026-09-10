package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
	"github.com/alexschlessinger/pollytool/tools"
)

func swarmTestRow(t *testing.T, m *replModel, callID string) *toolDisclosureRow {
	t.Helper()
	for _, record := range m.toolDisclosures.all() {
		for i := range record.rows {
			if record.rows[i].callID == callID {
				return &record.rows[i]
			}
		}
	}
	t.Fatalf("missing saved agent row %s", callID)
	return nil
}

func TestSwarmIterationPauseRendersReasonAndResumesThroughCommand(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	parent := testAcquireSession(t, store, "iteration-parent")
	registry := tools.NewToolRegistry(nil, tools.WithUnsafeNoSandbox())
	defer registry.Close()
	model := &scriptedStreamLLM{responses: []messages.ChatMessage{
		spawnTestToolCall("swarm_publish", `{"text":"saved review finding"}`),
		spawnTestReply("completed review"),
	}, failErr: errors.New("unexpected repeated model call")}
	runtime, err := swarm.New(swarm.Config{Store: store, Parent: parent, Registry: registry, Client: model, Root: t.TempDir(), Agent: llm.AgentConfig{MaxIterations: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := runtime.Agent(ctx, "", swarm.AgentRequest{Task: "review", Label: "review tests", ReadOnly: true, CallID: "review-call"})
	if !llm.IsIterationLimit(err) {
		t.Fatal(err)
	}
	s, err := runtime.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m := newReplModel()
	call := agentCall("review-call", `{"label":"review tests","background":true}`)
	m.hydrateHistory([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "review"}, {Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}}}, "iteration-parent")
	m.hydrateSwarmAgents(s)
	row := swarmTestRow(t, m, call.ID)
	want := "paused · iteration limit (1/1)"
	if row.agent.display() != want || row.agent.busy() || !strings.Contains(swarmInspectorText(s, nil, "members"), want) {
		t.Fatalf("pause not visible: %+v", row.agent)
	}
	command := &replCommandContext{ctx: ctx, state: &conversationState{swarm: runtime}}
	for _, suffix := range []string{"", " 0", " -1", " nope", " 1 extra"} {
		_, _, err := defaultReplCommands.dispatch("/swarm resume "+result.Session+suffix, command)
		if err == nil {
			t.Fatalf("invalid/unfunded continuation %q succeeded", suffix)
		}
	}
	dispatchDefaultCommandForTest(t, "/swarm resume "+result.Session+" 1", command)
	for runtime.HasActive() {
		if ctx.Err() != nil {
			t.Fatal("command did not resume the member")
		}
		time.Sleep(time.Millisecond)
	}
	s, err = runtime.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m.hydrateSwarmAgents(s)
	if row.agent.display() != "idle · awaiting review" || row.agent.viewID != result.Session || len(s.Executions) != 1 {
		t.Fatalf("continuation not reflected: %+v", row.agent)
	}
	for _, e := range s.Executions {
		if e.Iterations != 2 || e.Request.MaxIterations != 2 {
			t.Fatalf("command grant: %+v", e)
		}
	}
	stored := sessions.Report{Status: sessions.ReportPaused, Text: "partial", Error: llm.ErrMaxIterations.Error()}
	if spawnOutcomeStatus(stored.Status) != "paused · iteration limit" || !strings.Contains(reportHeader(stored), "paused") {
		t.Fatalf("legacy report labels pause as failure: %+v", stored)
	}
}

func TestSwarmRowsFollowMemberLifecycleWithoutChildTabs(t *testing.T) {
	m := newReplModel()
	call := agentCall("review", `{"label":"review llm tests","background":true}`)
	result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: "started"}
	result.SetToolSucceeded(true)
	m.hydrateHistory([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "review"}, {Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}}, result}, "parent")
	row := swarmTestRow(t, m, call.ID)
	if row == nil || row.agent.display() != "unknown" {
		t.Fatal("fixture must exercise the old background-result gap")
	}
	member := &swarm.Member{ID: "member-id", Name: "reviewer", Execution: "execution", Task: "task"}
	execution := &swarm.Execution{ID: "execution", Member: member.ID, Request: swarm.AgentRequest{CallID: call.ID}, Status: "queued"}
	task := &swarm.Task{ID: "task", Status: "running"}
	s := &swarm.State{Members: map[string]*swarm.Member{member.ID: member}, Executions: map[string]*swarm.Execution{execution.ID: execution}, Tasks: map[string]*swarm.Task{task.ID: task}}
	for _, tc := range []struct {
		member, execution, task, want string
		active                        bool
	}{
		{"queued", "queued", "running", "active · queued", true},
		{"running", "running", "running", "active", true},
		{"waiting", "waiting", "running", "waiting", true},
		{"paused", "paused", "running", "paused · interrupted", false},
		{"paused", "failed", "blocked", "paused · failed", false},
		{"idle", "completed", "awaiting_review", "idle · awaiting review", false},
		{"idle", "completed", "changes_requested", "idle · changes requested", false},
		{"idle", "completed", "done", "idle · done", false},
		{"idle", "completed", "canceled", "idle · canceled", false},
	} {
		execution.Status, task.Status = tc.execution, tc.task
		m.hydrateSwarmAgents(s)
		if row.agent.display() != tc.want || row.agent.busy() != tc.active || row.agent.viewID != member.ID || row.agent.session != member.Name || !row.agent.attached {
			t.Fatalf("%s/%s/%s: %+v", tc.member, tc.execution, tc.task, row.agent)
		}
	}
	// A deleted ID cannot be rebound to a different member reusing the name or
	// call ID. Rendering never acquires a child session or an execution lease.
	delete(s.Members, member.ID)
	s.Members["replacement"] = &swarm.Member{ID: "replacement", Name: member.Name}
	execution.Member = "replacement"
	m.hydrateSwarmAgents(s)
	if row.agent.viewID != member.ID || row.agent.busy() {
		t.Fatal("saved row was rebound to a replacement identity")
	}
}

func TestSwarmRowsRejectAmbiguousCallIdentity(t *testing.T) {
	m := newReplModel()
	call := agentCall("same-call", `{"background":true}`)
	m.hydrateHistory([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "review"}, {Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}}}, "parent")
	s := &swarm.State{Members: map[string]*swarm.Member{}, Executions: map[string]*swarm.Execution{}}
	for _, id := range []string{"first", "second"} {
		s.Members[id] = &swarm.Member{ID: id, Name: id}
		s.Executions[id] = &swarm.Execution{ID: id, Member: id, Request: swarm.AgentRequest{CallID: call.ID}}
	}
	m.hydrateSwarmAgents(s)
	row := swarmTestRow(t, m, call.ID)
	if row.agent.attached || row.agent.viewID != "" {
		t.Fatal("ambiguous call selected a member")
	}
}

func TestPausedAgentsAreNotCountedAsCompletedOrFailed(t *testing.T) {
	var counts activityAgentCounts
	counts.add(swarm.AgentPresentation{Lifecycle: swarm.LifecyclePaused, Outcome: "paused", StopReason: string(messages.StopReasonMaxIterations)})
	counts.add(swarm.AgentPresentation{Lifecycle: swarm.LifecycleActive, Busy: true})
	label := turnAgentSummaryLabel(counts.Total, counts.Running, counts.Failed, counts.Canceled, counts.Paused)
	if label != "1 agent running, 1 paused" {
		t.Fatalf("misleading aggregate: %q", label)
	}
	// Launch calls keep their own words; only they can be canceled.
	var local activityAgentCounts
	for _, word := range []string{"paused · iteration limit", "canceled", "denied", "done"} {
		local.addOutcome(word, false)
	}
	if local.Total != 4 || local.Paused != 1 || local.Canceled != 1 || local.Failed != 1 || local.Running != 0 {
		t.Fatalf("launch outcome buckets: %+v", local)
	}
	err := tools.NewToolError("agent paused", "ITERATION_LIMIT")
	if status := toolActivityOutcome(false, err); status != "paused · iteration limit" {
		t.Fatalf("live tool pause: %q", status)
	}
}

func TestSwarmTaskProgressAcrossViews(t *testing.T) {
	ctx := context.Background()
	r := newSwarmTestREPL(t, integrationModel(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		return spawnTestReply("findings")
	}), nil)
	call := agentCall("review-call", `{"label":"review tests","background":true}`)
	result, err := r.state.swarm.Agent(ctx, "", swarm.AgentRequest{Task: "review", Label: "review tests", ReadOnly: true, CallID: call.ID})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.state.swarm.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	member := s.Members[result.Session]
	execution, task := s.Executions[member.Execution], s.Tasks[member.Task]
	r.visibleTab().swarmSnapshot = s
	inline := newReplModel()
	inline.affordances.enabled = true
	inline.hydrateHistory([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "review"}, {Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}}}, "parent-work")
	row := swarmTestRow(t, inline, call.ID)
	r.inspect(viewTarget{session: sessions.ViewTarget{ID: member.ID, Name: member.Name}})
	waitInspector(t, r, 180)
	r.model.mu.Lock()
	r.openSessionsPickerSelected(member.ID)
	picker := r.model.modal
	r.model.mu.Unlock()

	for _, tc := range []struct {
		name, member, execution, task, want, wantTask, agents string
		accepted, active                                      bool
	}{
		{"running", "running", "running", "running", "active", "running", "1 agent running", false, true},
		{"accepted", "idle", "completed", "awaiting_review", "idle · integration pending", "integration pending", "1 needs decision", true, false},
		{"unreviewed", "idle", "completed", "awaiting_review", "idle · awaiting review", "awaiting review", "1 needs decision", false, false},
		{"retired accepted", "retired", "completed", "awaiting_review", "idle · retired · integration pending", "integration pending", "1 needs decision", true, false},
		{"retired unreviewed", "retired", "completed", "awaiting_review", "idle · retired · awaiting review", "awaiting review", "1 needs decision", false, false},
		{"done", "idle", "completed", "done", "idle · done", "done", "", true, false},
		{"retired done", "retired", "completed", "done", "idle · retired", "done", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Only this display snapshot changes: the runtime, lease, and IDs stay fixed.
			member.Control = swarm.MemberControlEnabled
			if tc.member == "retired" {
				member.Control = swarm.MemberControlRetired
			}
			execution.Status, task.Status = tc.execution, tc.task
			task.Snapshot, task.AcceptedRevision = "candidate", 0
			if tc.accepted {
				task.AcceptedRevision = task.Revision
			}
			inline.hydrateSwarmAgents(s)
			if row.agent.display() != tc.want || row.agent.busy() != tc.active || row.agent.viewID != member.ID {
				t.Fatalf("inline projection: %+v", row.agent)
			}
			if tc.name == "accepted" && len(inline.affordances.agents) != 1 {
				t.Fatal("acceptance before paint suppressed the completion cue")
			}
			r.model.mu.Lock()
			r.refreshSessionsPickerItems(r.sessionsPicker, picker, member.ID)
			item := pickerItem(t, picker, member.ID)
			selected := pickerSelection(picker)
			agents, _ := r.agentsStatus()
			r.model.mu.Unlock()
			if !strings.Contains(item.label, tc.want) || selected != member.ID {
				t.Fatalf("picker projection: %+v, selected=%s", item, selected)
			}
			if agents != tc.agents {
				t.Fatalf("status row = %q, want %q", agents, tc.agents)
			}
			header := r.inspectorHeader(180, 20, 0, 0)
			if !strings.Contains(plainStyledText(header.text), tc.want) || !headerButton(header.buttons, "stop").Empty() != tc.active {
				t.Fatalf("inspector projection: %s", header.text)
			}
			if text := swarmInspectorText(s, nil, "members"); !strings.Contains(text, "review tests — "+tc.want) || !strings.Contains(text, "Execution: "+tc.execution) {
				t.Fatalf("member inspector: %s", text)
			}
			text := swarmInspectorText(s, nil, "tasks")
			if !strings.HasPrefix(text, tc.wantTask+" · revision ") || !strings.Contains(text, "Acceptance criteria: "+task.Criteria) {
				t.Fatalf("task inspector: %s", text)
			}
			if strings.Contains(text, "Accepted revision:") != tc.accepted {
				t.Fatalf("task acceptance missing or fabricated: %s", text)
			}
			if len(r.tabs) != 1 || r.visibleTab().state.swarm == nil {
				t.Fatal("projection acquired a child runtime or changed the parent")
			}
		})
	}
}

func TestSwarmOpenRunDistinguishesActiveExecutionsFromPendingTasks(t *testing.T) {
	s := &swarm.State{
		Runs: map[string]*swarm.Run{"current": {ID: "current", Status: "running", Starts: 4, Limit: 256}},
		Executions: map[string]*swarm.Execution{
			"finished": {Run: "current", Status: "completed"},
			"old":      {Run: "old", Status: "running"},
		},
		Tasks: map[string]*swarm.Task{
			"pending":  {Run: "current", Status: "awaiting_review", Revision: 2, AcceptedRevision: 2, Snapshot: "candidate"},
			"done":     {Run: "current", Status: "done"},
			"canceled": {Run: "current", Status: "canceled"},
			"old":      {Run: "old", Status: "awaiting_review"},
		},
	}
	text := swarmInspectorText(s, nil, "members")
	if !strings.Contains(text, "open · active executions: 0 · pending tasks: 1 · 4 / 256 logical executions") {
		t.Fatalf("open run looked like running agents: %s", text)
	}
	for _, status := range []string{"queued", "running", "waiting"} {
		s.Executions[status] = &swarm.Execution{Run: "current", Status: status}
	}
	if text = swarmInspectorText(s, nil, "members"); !strings.Contains(text, "open · active executions: 3 · pending tasks: 1") {
		t.Fatalf("active execution count: %s", text)
	}
	s.Runs["current"].Status = "completed"
	for _, e := range s.Executions {
		e.Status = "completed"
	}
	s.Tasks["pending"].Status = "done"
	if text = swarmInspectorText(s, nil, "members"); !strings.Contains(text, "completed · active executions: 0 · pending tasks: 0") {
		t.Fatalf("closed run: %s", text)
	}
}
