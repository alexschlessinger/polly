package main

import (
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

// A reopened session's meter starts from the size the provider last
// counted; without a count, from an estimate of what requests carry, which
// leaves out what a summary replaced, marked as an estimate.
func TestContextMeterSeedsFromTheLastCount(t *testing.T) {
	old := messages.ChatMessage{Role: messages.MessageRoleUser, Content: strings.Repeat("o", 40_000)}
	answer := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done"}
	answer.SetTokenUsage(12_000, 300)
	if size, ok := lastCountedSize([]messages.ChatMessage{old, answer}); !ok || size != 12_300 {
		t.Fatalf("last counted size = %d, %v; want 12300", size, ok)
	}
	history := []messages.ChatMessage{old, answer, messages.Compaction{Summary: "the user asked"}.Message(), {Role: messages.MessageRoleUser, Content: "next"}}
	// Input, tool results or a compaction after the counted reply leave
	// the count behind; other internal records do not.
	for _, after := range []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: strings.Repeat("added", 1_000)},
		{Role: messages.MessageRoleTool, ToolCallID: "c1", Content: "result"},
		messages.Compaction{ClearThrough: "c1"}.Message(),
	} {
		if _, ok := lastCountedSize([]messages.ChatMessage{old, answer, after}); ok {
			t.Fatalf("a count was taken past %+v", after)
		}
	}
	if size, ok := lastCountedSize([]messages.ChatMessage{old, answer, (messages.ChatMessage{}).UsageRecord()}); !ok || size != 12_300 {
		t.Fatalf("a usage record hid the count: %d, %v", size, ok)
	}
	answer.Metadata = nil
	if _, ok := lastCountedSize([]messages.ChatMessage{old, answer}); ok {
		t.Fatal("a reply without a count was taken as one")
	}
	if size := estimatedRequestSize(history, nil); size <= 0 || size >= 10_000 {
		t.Fatalf("estimated request size = %d, want the summary in place of the old exchange", size)
	}
	var status sessionStatus
	status.seedContextUsage(448, true, nil)
	if got := status.contextUsageText(); got != "~448 tok" {
		t.Fatalf("seeded estimate reads %q", got)
	}
	status.recordContextUsage(500, 1_000)
	if got := status.contextUsageText(); got != "500/1.0k" {
		t.Fatalf("a provider count reads %q", got)
	}
}

// /get maxcontext and /context show how the last request was sized, a
// budget a provider's rejection taught included, until the settings that size
// requests change; an unlimited budget reads as one.
func TestContextReportsTheBudgetTheLastRequestUsed(t *testing.T) {
	state := &conversationState{settings: Settings{Model: "test/model", AutoMaxContext: true, MaxTokens: 32_000}}
	state.recordBudget(&state.settings, contextBudgetDetails{window: 200_000, input: 150_000, response: 32_000, learned: true})
	spec, _ := settingSpecFor("maxcontext")
	ctx := &replCommandContext{state: state, settings: &state.settings}
	if got := spec.show(ctx, &state.settings); got != "auto (window 200000 → budget 150000)" {
		t.Fatalf("maxcontext = %q", got)
	}
	if budget := state.currentBudget(&state.settings); budget == nil || !budget.learned || !strings.Contains(strings.Join(budget.details(""), "\n"), "input budget: 150k (provider limit)") {
		t.Fatalf("current budget = %+v", budget)
	}
	state.settings.MaxTokens = 16_000
	if state.currentBudget(&state.settings) != nil {
		t.Fatal("a budget sized under other settings was reported")
	}
	state.settings.AutoMaxContext, state.settings.MaxHistoryTokens = false, 0
	state.recordBudget(&state.settings, contextBudgetDetails{window: 200_000})
	if got := spec.show(ctx, &state.settings); got != "0 (window 200000 → unlimited)" {
		t.Fatalf("unlimited maxcontext = %q", got)
	}
}

// A truncated reply names the output limit that cut it off, and suggests
// --maxtokens only where raising it would help.
func TestTruncationNamesTheLimitThatApplied(t *testing.T) {
	for _, tc := range []struct {
		applied, configured int
		want                string
	}{
		{32_000, 32_000, "hit 32000 token limit, use --maxtokens"},
		{8_192, 32_000, "8192-token output limit the model or its context window allows"},
		{0, 0, "provider's default output limit"},
	} {
		if got := truncationCause(tc.applied, tc.configured); !strings.Contains(got, tc.want) {
			t.Fatalf("truncation (%d of %d) = %q", tc.applied, tc.configured, got)
		}
	}
}

// Settings that leave requests sized as they were keep the meter as it is.
func TestSettingsThatDoNotResizeKeepTheMeter(t *testing.T) {
	cfg := &Config{Launch: Settings{Model: "anthropic/claude-sonnet-4-6", MaxHistoryTokens: 256_000, Temperature: 1}}
	r := newManagedREPL(cfg, "ctx", 0, 0)
	r.state = &conversationState{settings: cfg.Launch}
	budget := &contextBudgetDetails{window: 200_000, input: 168_000, response: 32_000}
	r.model.status.recordContextUsage(50_000, 168_000)
	r.model.status.contextBudget = budget
	if handled, _ := r.runCommand("/set temp 0.5"); !handled {
		t.Fatal("/set temp was not handled")
	}
	if s := r.model.status; s.contextUsed != 50_000 || s.contextLimit != 168_000 || s.contextEstimated || s.contextBudget != budget {
		t.Fatalf("meter after /set temp = %d/%d (estimated %v, budget %+v)", s.contextUsed, s.contextLimit, s.contextEstimated, s.contextBudget)
	}
	for _, key := range []string{"model", "modelhost", "maxcontext", "maxtokens"} {
		if !resizesRequests(key) {
			t.Fatalf("%s does not resize requests", key)
		}
	}
}
