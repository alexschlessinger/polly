package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/swarm"
)

func TestContextSessionUsageCountsRecordedRequests(t *testing.T) {
	first := turnUsageTestMessage(60_000, 200)
	first.SetPromptCacheUsage(30_000, 0)
	tool := turnUsageTestMessage(999_999, 999_999)
	tool.Role = messages.MessageRoleTool // A delivered child result is not a parent request.
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "question"}, first, tool,
		turnUsageTestMessage(1_000, 100).UsageRecord(), // Compaction summary request.
		(messages.Compaction{Summary: "summary"}).Message(),
		turnUsageTestMessage(9_000, 50).UsageRecord(), // Discarded retry response.
		turnUsageTestMessage(24_000, 300),
	}
	snapshot, err := collectContextSnapshot(context.Background(), &Config{SchemaPath: "schema.json"}, &conversationState{}, history)
	if err != nil {
		t.Fatal(err)
	}
	usage := snapshot.usage.parent
	if usage.input.tokens != 94_000 || usage.output.tokens != 650 || usage.input.missing || usage.output.missing {
		t.Fatalf("parent cumulative usage = %+v", usage)
	}
	if len(snapshot.trend) != 2 || snapshot.trend[1].tokens != 24_000 {
		t.Fatalf("cumulative usage changed the parent context trend: %+v", snapshot.trend)
	}
}

func TestContextSessionUsageCountsEveryExecutionOnce(t *testing.T) {
	usage := func(input, output int) swarm.Usage {
		return swarm.Usage{Samples: 2, InputTokens: &input, OutputTokens: &output}
	}
	state := &swarm.State{
		Members: map[string]*swarm.Member{"worker": {ID: "worker", Execution: "followup"}},
		Executions: map[string]*swarm.Execution{
			"first": {Member: "worker", Status: "completed", Usage: usage(10_000, 100),
				Result: &swarm.AgentResult{Usage: usage(10_000, 100)}},
			"followup": {Member: "worker", Status: "running", Generation: 3, Usage: usage(20_000, 200)},
			"failed":   {Member: "failed", Status: "failed", Usage: usage(3_000, 30)},
			"released": {Member: "released", Status: "completed", Usage: usage(4_000, 40)},
			"workflow": {Member: "workflow", Workflow: "report", Status: "completed", Usage: usage(5_000, 50)},
			"waiting":  {Member: "waiting", Status: "waiting", Usage: usage(6_000, 60)},
			"new":      {Member: "new", Status: "running"},
		},
	}
	var total contextSessionUsage
	total.addExecutions(state)
	if got := total.subagents; got.input.tokens != 48_000 || got.output.tokens != 480 || got.input.missing || got.output.missing {
		t.Fatalf("subagent usage = %+v", got)
	}
}

func TestContextSessionUsagePartialAndZero(t *testing.T) {
	for _, tc := range []struct {
		name    string
		history []messages.ChatMessage
		input   string
		output  string
		partial bool
	}{
		{name: "no requests", input: "0", output: "0"},
		{name: "reported zero", history: []messages.ChatMessage{turnUsageTestMessage(0, 0)}, input: "0", output: "0"},
		{name: "unknown", history: []messages.ChatMessage{{Role: messages.MessageRoleAssistant}}, input: "—", output: "—", partial: true},
		{name: "one missing count", history: []messages.ChatMessage{{Role: messages.MessageRoleAssistant, Metadata: map[string]any{messages.MetadataKeyInputTokens: 1200}}}, input: "1.2k", output: "—", partial: true},
		{name: "partial", history: []messages.ChatMessage{turnUsageTestMessage(1200, 30), {Role: messages.MessageRoleAssistant}}, input: "≥1.2k", output: "≥30", partial: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var usage contextSessionUsage
			for _, msg := range tc.history {
				usage.parent.addMessage(msg)
			}
			if usage.parent.input.text() != tc.input || usage.parent.output.text() != tc.output {
				t.Fatalf("usage = %+v, want %s / %s", usage, tc.input, tc.output)
			}
			if text := strings.Join(usage.details(), "\n"); strings.Contains(text, "partial") != tc.partial {
				t.Fatalf("partial reporting = %q", text)
			}
		})
	}
	input, output := 4000, 40
	var usage contextSessionUsage
	usage.parent.addMessage(turnUsageTestMessage(1200, 30))
	usage.addExecutions(&swarm.State{Executions: map[string]*swarm.Execution{
		"known":   {Usage: swarm.Usage{Samples: 1, InputTokens: &input, OutputTokens: &output}},
		"partial": {Usage: swarm.Usage{Samples: 2, OutputTokens: &output}},
	}})
	total := usage.parent.plus(usage.subagents)
	if usage.subagents.input.text() != "≥4.0k" || total.input.text() != "≥5.2k" || total.output.text() != "110" {
		t.Fatalf("partial execution did not propagate to total: %+v", usage)
	}
}

func TestContextSessionUsageLiveSwarmRefresh(t *testing.T) {
	r := newSwarmTestREPL(t, integrationModel(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		msg := spawnTestReply("done")
		msg.SetTokenUsage(4000, 40)
		return msg
	}), nil)
	ctx := context.Background()
	launch := func() {
		t.Helper()
		if _, err := r.state.swarm.Agent(ctx, "", swarm.AgentRequest{Task: "inspect", Label: "Reader", ReadOnly: true}); err != nil {
			t.Fatal(err)
		}
		waitSwarmIdle(t, r.state.swarm)
	}
	launch()
	testAddMessages(t, r.state.session, []messages.ChatMessage{turnUsageTestMessage(1200, 30)})
	r.openContextPopover()
	awaitContextSnapshot(t, r)
	d := r.model.modal.context
	if got := d.snapshot.usage.subagents; got.input.tokens != 4000 || got.output.tokens != 40 {
		t.Fatalf("initial worker usage = %+v", got)
	}
	launch()
	now := time.Now()
	d.nextRead = now.Add(-time.Second)
	r.refreshContextPopover(now)
	select {
	case result := <-d.pending:
		d.pending <- result
	case <-time.After(5 * time.Second):
		t.Fatal("usage refresh did not finish")
	}
	r.refreshContextPopover(now.Add(10 * time.Millisecond))
	total := d.snapshot.usage.parent.plus(d.snapshot.usage.subagents)
	if d.snapshot.usage.subagents.input.tokens != 8000 || total.input.tokens != 9200 || total.output.tokens != 110 || total.input.missing || total.output.missing {
		t.Fatalf("refreshed usage = %+v", d.snapshot.usage)
	}
	text := strings.Join(r.model.modal.details, "\n")
	for _, want := range []string{"Session usage", "↑ input", "↓ output", "Parent", "Subagents", "Total", "9.2k", "110"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q from report:\n%s", want, text)
		}
	}
	r.closeModal()

	// A failed coordination read must not hide the parent's context statistics
	// or present unavailable child usage as a complete zero.
	if err := r.state.session.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := collectContextSnapshot(ctx, &Config{SchemaPath: "schema.json"}, r.state, []messages.ChatMessage{turnUsageTestMessage(1200, 30)})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.usage.err == nil || snapshot.usage.parent.input.text() != "1.2k" || snapshot.usage.subagents.input.text() != "—" || snapshot.usage.parent.plus(snapshot.usage.subagents).input.text() != "≥1.2k" {
		t.Fatalf("unavailable worker usage = %+v", snapshot.usage)
	}
	if text := strings.Join(snapshot.details(), "\n"); !strings.Contains(text, "Subagent usage unavailable") {
		t.Fatalf("missing worker read error: %s", text)
	}
}
