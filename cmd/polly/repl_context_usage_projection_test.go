package main

import (
	"reflect"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/swarm"
)

func contextProjectionTestUsage(input, output, samples int) swarm.Usage {
	return swarm.Usage{InputTokens: &input, OutputTokens: &output, Samples: samples}
}

func contextProjectionTestTokens(input, output int) contextTokenUsage {
	return contextTokenUsage{input: contextTokenTotal{tokens: input}, output: contextTokenTotal{tokens: output}}
}

func TestContextSessionUsageProjection(t *testing.T) {
	state := &swarm.State{
		Members: map[string]*swarm.Member{
			"a-worker": {ID: "a-worker", Label: "  Zulu\treader\n\x00", Model: "provider/saved", Execution: "a-followup"},
			"b-worker": {ID: "b-worker", Task: "task-b"},
			"d-queued": {ID: "d-queued", Label: "\x00"},
			"unused":   {ID: "unused", Label: "No execution"},
		},
		Tasks: map[string]*swarm.Task{
			"task-b": {Description: " Alpha\n task\treader "},
		},
		Contexts: map[string]*swarm.ExecutionContext{
			"active":   {ID: "active"},
			"retiring": {ID: "retiring", Release: swarm.WorkspaceReleasing},
		},
		Executions: map[string]*swarm.Execution{
			"z-initial": {
				Member: "a-worker", Status: "completed", Workspace: "removed",
				Usage:  contextProjectionTestUsage(10_000, 100, 2),
				Result: &swarm.AgentResult{Usage: contextProjectionTestUsage(900_000, 9000, 20)},
				Intent: []messages.ChatMessage{turnUsageTestMessage(800_000, 8000)},
			},
			"a-followup": {
				Member: "a-worker", Status: "running", Workspace: "active", Generation: 3,
				Request: swarm.AgentRequest{Session: "a-worker", Model: "request/ignored"},
				Usage:   contextProjectionTestUsage(20_000, 200, 3),
			},
			"b-releasing": {
				Member: "b-worker", Status: "completed", Workspace: "retiring",
				Request: swarm.AgentRequest{Model: "request/b"},
				Usage:   contextProjectionTestUsage(4000, 40, 1),
			},
			"z-workflow": {
				Member: "b-worker", Status: "failed", Workflow: "report", Workspace: "removed",
				Request: swarm.AgentRequest{Model: "request/workflow"},
				Usage:   contextProjectionTestUsage(5000, 50, 2),
			},
			"c-failed": {
				Member: "c-orphan", Status: "failed",
				Request: swarm.AgentRequest{Model: "request/orphan"},
				Usage:   contextProjectionTestUsage(3000, 30, 1),
			},
			"d-queued": {Member: "d-queued", Status: "queued"},
		},
	}
	want := []contextAgentUsage{
		{id: "a-worker", label: "Zulu reader", usage: contextProjectionTestTokens(30_000, 300)},
		{id: "b-worker", label: "Alpha", usage: contextProjectionTestTokens(9000, 90)},
		{id: "c-orphan", label: "c-orphan", usage: contextProjectionTestTokens(3000, 30)},
		{id: "d-queued", label: "d-queued"},
	}
	// Repeated projections must not inherit randomized map order, label order,
	// or the current execution pointer (the follow-up ID sorts before the first).
	for i := 0; i < 100; i++ {
		var usage contextSessionUsage
		usage.addExecutions(state)
		if !reflect.DeepEqual(usage.agents, want) {
			t.Fatalf("projection %d = %+v, want %+v", i, usage.agents, want)
		}
		if got := usage.subagents; got != contextProjectionTestTokens(42_000, 420) {
			t.Fatalf("subagents = %+v", got)
		}
		var sum contextTokenUsage
		for _, agent := range usage.agents {
			sum = sum.plus(agent.usage)
		}
		if sum != usage.subagents {
			t.Fatalf("agent sum = %+v, subagents = %+v", sum, usage.subagents)
		}
	}
}

func TestContextSessionUsageProjectionLabels(t *testing.T) {
	for _, tc := range []struct {
		name   string
		member *swarm.Member
		want   string
	}{
		{name: "explicit", member: &swarm.Member{Label: "  Display\tlabel\x00\nignored "}, want: "Display label"},
		// swarmMemberLabel already limits fallback briefs and names to one line.
		{name: "execution brief", member: &swarm.Member{Execution: "execution"}, want: "Execution"},
		{name: "task brief", member: &swarm.Member{Task: "task"}, want: "Task"},
		{name: "member name", member: &swarm.Member{Name: " Name\n fallback "}, want: "Name"},
		{name: "empty member", member: &swarm.Member{}, want: "member"},
		{name: "missing member", want: "member"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var usage contextSessionUsage
			usage.addExecutions(&swarm.State{
				Members: map[string]*swarm.Member{"member": tc.member},
				Tasks:   map[string]*swarm.Task{"task": {Description: " Task\n brief "}},
				Executions: map[string]*swarm.Execution{
					"execution": {Member: "member", Request: swarm.AgentRequest{Task: " Execution\n brief "}},
				},
			})
			if got := usage.agents[0].label; got != tc.want {
				t.Fatalf("label = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestContextSessionUsageProjectionUnknownAndZero(t *testing.T) {
	zero, input, output := 0, 1200, 30
	for _, tc := range []struct {
		name  string
		saved swarm.Usage
		want  contextTokenUsage
	}{
		{name: "no responses"},
		{name: "reported zero", saved: swarm.Usage{Samples: 1, InputTokens: &zero, OutputTokens: &zero}},
		{name: "unknown", saved: swarm.Usage{Samples: 2}, want: contextTokenUsage{
			input: contextTokenTotal{missing: true}, output: contextTokenTotal{missing: true},
		}},
		{name: "input only", saved: swarm.Usage{Samples: 1, InputTokens: &input}, want: contextTokenUsage{
			input: contextTokenTotal{tokens: 1200}, output: contextTokenTotal{missing: true},
		}},
		{name: "output only", saved: swarm.Usage{Samples: 1, OutputTokens: &output}, want: contextTokenUsage{
			input: contextTokenTotal{missing: true}, output: contextTokenTotal{tokens: 30},
		}},
		{name: "counts without samples", saved: swarm.Usage{InputTokens: &input, OutputTokens: &output}, want: contextProjectionTestTokens(1200, 30)},
		{name: "partial without samples", saved: swarm.Usage{InputTokens: &input}, want: contextTokenUsage{
			input: contextTokenTotal{tokens: 1200}, output: contextTokenTotal{missing: true},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var usage contextSessionUsage
			usage.addExecutions(&swarm.State{Executions: map[string]*swarm.Execution{
				"execution": {Member: "member", Usage: tc.saved},
			}})
			if len(usage.agents) != 1 {
				t.Fatalf("agent missing from projection: %+v", usage.agents)
			}
			if usage.agents[0].usage != tc.want || usage.subagents != tc.want {
				t.Fatalf("projection = %+v, want usage %+v", usage, tc.want)
			}
		})
	}

	var usage contextSessionUsage
	usage.addExecutions(&swarm.State{Executions: map[string]*swarm.Execution{
		"known":   {Member: "partial", Usage: contextProjectionTestUsage(1200, 30, 1)},
		"unknown": {Member: "partial", Usage: swarm.Usage{Samples: 1}},
		"queued":  {Member: "empty"},
	}})
	partial := contextTokenUsage{input: contextTokenTotal{tokens: 1200, missing: true}, output: contextTokenTotal{tokens: 30, missing: true}}
	if usage.agents[0].usage != (contextTokenUsage{}) || usage.agents[1].usage != partial || usage.subagents != partial {
		t.Fatalf("partial group totals = %+v", usage)
	}
	if usage.subagents.input.text() != "≥1.2k" || usage.subagents.output.text() != "≥30" {
		t.Fatalf("partial totals = %+v", usage.subagents)
	}
}

func TestContextSessionUsageProjectionOwnsValues(t *testing.T) {
	state := &swarm.State{
		Members:  map[string]*swarm.Member{"member": {Label: "Reader", Model: "provider/model"}},
		Contexts: map[string]*swarm.ExecutionContext{"workspace": {ID: "workspace"}},
		Executions: map[string]*swarm.Execution{
			"execution": {
				Member: "member", Status: "running", Workspace: "workspace",
				Request: swarm.AgentRequest{Session: "member"},
				Usage:   contextProjectionTestUsage(1200, 30, 1),
			},
		},
	}
	var usage contextSessionUsage
	usage.addExecutions(state)
	want := []contextAgentUsage{{id: "member", label: "Reader", usage: contextProjectionTestTokens(1200, 30)}}
	member, execution := state.Members["member"], state.Executions["execution"]
	member.ID, member.Label, member.Model = "changed", "Changed", "provider/changed"
	execution.ID, execution.Member, execution.Status = "changed", "changed", "completed"
	execution.Request.Session, execution.Workflow = "", "workflow"
	*execution.Usage.InputTokens, *execution.Usage.OutputTokens = 9000, 90
	execution.Usage.Samples = 3
	delete(state.Contexts, "workspace")
	delete(state.Members, "member")
	state.Executions["new"] = &swarm.Execution{Member: "new"}
	if !reflect.DeepEqual(usage.agents, want) || usage.subagents != contextProjectionTestTokens(1200, 30) {
		t.Fatalf("snapshot changed with source state: %+v", usage)
	}

	var next contextSessionUsage
	next.addExecutions(state)
	next.agents[0].label = "Other projection"
	next.agents[0].usage = contextTokenUsage{}
	if !reflect.DeepEqual(usage.agents, want) {
		t.Fatalf("snapshots share projection slices: %+v", usage.agents)
	}
	usage.agents[0].usage.input.tokens = 42
	if *execution.Usage.InputTokens != 9000 {
		t.Fatal("projection retained usage pointers into source state")
	}
}

func TestContextSessionUsageProjectionFollowupAndResume(t *testing.T) {
	state := &swarm.State{
		Members: map[string]*swarm.Member{"worker": {ID: "worker", Execution: "initial"}},
		Executions: map[string]*swarm.Execution{
			"initial": {Member: "worker", Status: "paused", Generation: 1, Usage: contextProjectionTestUsage(1200, 30, 1)},
		},
	}
	var before contextSessionUsage
	before.addExecutions(state)

	// Resume keeps one logical execution with cumulative counts, even when
	// multiple generations and a result receipt record the same work.
	initial := state.Executions["initial"]
	initial.Generation, initial.Status = 3, "completed"
	initial.Usage = contextProjectionTestUsage(4000, 90, 3)
	initial.Result = &swarm.AgentResult{Usage: initial.Usage}
	var resumed contextSessionUsage
	resumed.addExecutions(state)
	if got := resumed.agents[0]; len(resumed.agents) != 1 || got.usage != contextProjectionTestTokens(4000, 90) {
		t.Fatalf("resumed projection = %+v", got)
	}

	state.Members["worker"].Execution = "followup"
	state.Executions["followup"] = &swarm.Execution{
		Member: "worker", Status: "running", Generation: 2,
		Request: swarm.AgentRequest{Session: "worker"},
		Usage:   contextProjectionTestUsage(5000, 100, 2),
	}
	var followed contextSessionUsage
	followed.addExecutions(state)
	if len(followed.agents) != 1 || followed.agents[0].usage != contextProjectionTestTokens(9000, 190) || followed.subagents != contextProjectionTestTokens(9000, 190) {
		t.Fatalf("follow-up projection = %+v", followed)
	}
	if before.subagents != contextProjectionTestTokens(1200, 30) || resumed.subagents != contextProjectionTestTokens(4000, 90) {
		t.Fatalf("earlier snapshots changed: before %+v, resumed %+v", before, resumed)
	}
}
