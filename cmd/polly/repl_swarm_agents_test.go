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
	for _, record := range m.toolDisclosures {
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
	want := "paused · iteration limit reached (1/1)"
	if row.agent.status != want || row.agent.active || !strings.Contains(swarmInspectorText(s, "members"), want) {
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
	if row.agent.status != "awaiting review" || row.agent.viewID != result.Session || len(s.Executions) != 1 {
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
	if row == nil || row.agent.status != "unknown" {
		t.Fatal("fixture must exercise the old background-result gap")
	}
	member := &swarm.Member{ID: "member-id", Name: "reviewer", Status: "queued", Execution: "execution", Task: "task"}
	execution := &swarm.Execution{ID: "execution", Member: member.ID, Request: swarm.AgentRequest{CallID: call.ID}, Status: "queued"}
	task := &swarm.Task{ID: "task", Status: "running"}
	s := &swarm.State{Members: map[string]*swarm.Member{member.ID: member}, Executions: map[string]*swarm.Execution{execution.ID: execution}, Tasks: map[string]*swarm.Task{task.ID: task}}
	for _, tc := range []struct {
		member, execution, task, want string
		active                        bool
	}{
		{"queued", "queued", "running", "queued", true},
		{"running", "running", "running", "running", true},
		{"waiting", "waiting", "running", "waiting", true},
		{"paused", "paused", "running", "paused", false},
		{"paused", "failed", "blocked", "failed", false},
		{"idle", "completed", "awaiting_review", "awaiting review", false},
		{"idle", "completed", "changes_requested", "changes requested", false},
		{"idle", "completed", "done", "done", false},
		{"idle", "completed", "canceled", "canceled", false},
	} {
		member.Status, execution.Status, task.Status = tc.member, tc.execution, tc.task
		m.hydrateSwarmAgents(s)
		if row.agent.status != tc.want || row.agent.active != tc.active || row.agent.viewID != member.ID || row.agent.session != member.Name || !row.agent.attached {
			t.Fatalf("%s/%s/%s: %+v", tc.member, tc.execution, tc.task, row.agent)
		}
	}
	// A deleted ID cannot be rebound to a different member reusing the name or
	// call ID. Rendering never acquires a child session or an execution lease.
	delete(s.Members, member.ID)
	s.Members["replacement"] = &swarm.Member{ID: "replacement", Name: member.Name, Status: "running"}
	execution.Member = "replacement"
	m.hydrateSwarmAgents(s)
	if row.agent.viewID != member.ID || row.agent.active {
		t.Fatal("saved row was rebound to a replacement identity")
	}
}

func TestSwarmRowsRejectAmbiguousCallIdentity(t *testing.T) {
	m := newReplModel()
	call := agentCall("same-call", `{"background":true}`)
	m.hydrateHistory([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "review"}, {Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}}}, "parent")
	s := &swarm.State{Members: map[string]*swarm.Member{}, Executions: map[string]*swarm.Execution{}}
	for _, id := range []string{"first", "second"} {
		s.Members[id] = &swarm.Member{ID: id, Name: id, Status: "running"}
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
	counts.add("paused · iteration limit reached (8/8)", false)
	counts.add("running", true)
	label := turnAgentSummaryLabel(counts.Total, counts.Running, counts.Failed, counts.Canceled, counts.Paused)
	if label != "1 agent running, 1 paused" {
		t.Fatalf("misleading aggregate: %q", label)
	}
	err := tools.NewToolError("agent paused", "ITERATION_LIMIT")
	if status := toolActivityOutcome(false, err); status != "paused · iteration limit" {
		t.Fatalf("live tool pause: %q", status)
	}
}
