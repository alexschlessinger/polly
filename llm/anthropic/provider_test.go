package anthropic

import (
	"testing"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
)

// TestBuildRequest_ModelFamilyBehavior verifies that BuildRequest
// branches correctly on Opus 4.7 (no temperature), 4.6+ family (adaptive thinking +
// effort), and legacy models (enabled/budget_tokens).
func TestBuildRequest_ModelFamilyBehavior(t *testing.T) {
	tests := []struct {
		name         string
		model        string
		effort       contract.ThinkingEffort
		maxTokens    int // 0 -> defaults to 1024
		wantTemp     bool
		wantAdaptive bool
		wantEnabled  bool
		wantBudget   int64
		wantEffort   Effort
	}{
		{
			name:         "opus_4_7_no_thinking",
			model:        "claude-opus-4-7",
			effort:       contract.EffortOff(),
			wantTemp:     false,
			wantAdaptive: false,
			wantEnabled:  false,
		},
		{
			name:         "opus_4_7_low",
			model:        "claude-opus-4-7",
			effort:       contract.EffortLevel(contract.LevelLow),
			wantTemp:     false,
			wantAdaptive: true,
			wantEffort:   EffortLow,
		},
		{
			name:         "opus_4_7_high",
			model:        "claude-opus-4-7",
			effort:       contract.EffortLevel(contract.LevelHigh),
			wantTemp:     false,
			wantAdaptive: true,
			wantEffort:   EffortHigh,
		},
		{
			// minimal has no Anthropic equivalent and clamps up to low.
			name:         "opus_4_7_minimal_clamps_to_low",
			model:        "claude-opus-4-7",
			effort:       contract.EffortLevel(contract.LevelMinimal),
			wantTemp:     false,
			wantAdaptive: true,
			wantEffort:   EffortLow,
		},
		{
			name:         "opus_4_7_xhigh",
			model:        "claude-opus-4-7",
			effort:       contract.EffortLevel(contract.LevelXHigh),
			wantTemp:     false,
			wantAdaptive: true,
			wantEffort:   EffortXHigh,
		},
		{
			name:         "opus_4_7_max",
			model:        "claude-opus-4-7",
			effort:       contract.EffortLevel(contract.LevelMax),
			wantTemp:     false,
			wantAdaptive: true,
			wantEffort:   EffortMax,
		},
		{
			// Dynamic -> adaptive thinking with NO explicit effort (model decides).
			name:         "opus_4_7_dynamic_has_no_effort",
			model:        "claude-opus-4-7",
			effort:       contract.EffortDynamic(),
			wantTemp:     false,
			wantAdaptive: true,
			wantEffort:   "", // OutputConfig.Effort left unset
		},
		{
			// A raw budget on an adaptive model reduces to its nearest level.
			name:         "opus_4_7_budget_maps_to_level",
			model:        "claude-opus-4-7",
			effort:       contract.EffortBudget(70000), // above the max threshold (65536) -> max
			wantTemp:     false,
			wantAdaptive: true,
			wantEffort:   EffortMax,
		},
		{
			name:         "opus_4_7_dated_variant",
			model:        "claude-opus-4-7-20260101",
			effort:       contract.EffortLevel(contract.LevelMedium),
			wantTemp:     false,
			wantAdaptive: true,
			wantEffort:   EffortMedium,
		},
		{
			// Regression: opus-4-8 must use adaptive thinking, not legacy
			// enabled/budget_tokens, which 400s ("hi" reproduced this).
			name:         "opus_4_8_high",
			model:        "claude-opus-4-8",
			effort:       contract.EffortLevel(contract.LevelHigh),
			wantTemp:     false,
			wantAdaptive: true,
			wantEffort:   EffortHigh,
		},
		{
			name:         "opus_4_8_dated_variant",
			model:        "claude-opus-4-8-20260601",
			effort:       contract.EffortLevel(contract.LevelMedium),
			wantTemp:     false,
			wantAdaptive: true,
			wantEffort:   EffortMedium,
		},
		{
			name:         "sonnet_4_6_medium",
			model:        "claude-sonnet-4-6",
			effort:       contract.EffortLevel(contract.LevelMedium),
			wantTemp:     true,
			wantAdaptive: true,
			wantEffort:   EffortMedium,
		},
		{
			name:         "opus_4_6_low",
			model:        "claude-opus-4-6",
			effort:       contract.EffortLevel(contract.LevelLow),
			wantTemp:     true,
			wantAdaptive: true,
			wantEffort:   EffortLow,
		},
		{
			name:        "sonnet_4_5_legacy_low",
			model:       "claude-sonnet-4-5-20250929",
			effort:      contract.EffortLevel(contract.LevelLow),
			maxTokens:   16000,
			wantTemp:    true,
			wantEnabled: true,
			wantBudget:  int64(contract.LevelLow.Budget()),
		},
		{
			// A raw budget passes through on legacy models...
			name:        "sonnet_4_5_legacy_raw_budget",
			model:       "claude-sonnet-4-5-20250929",
			effort:      contract.EffortBudget(6000),
			maxTokens:   16000,
			wantTemp:    true,
			wantEnabled: true,
			wantBudget:  6000,
		},
		{
			// ...but is clamped to strictly less than max_tokens (API 400s otherwise).
			name:        "sonnet_4_5_legacy_budget_clamped_to_maxtokens",
			model:       "claude-sonnet-4-5-20250929",
			effort:      contract.EffortBudget(50000),
			maxTokens:   16000,
			wantTemp:    true,
			wantEnabled: true,
			wantBudget:  15999,
		},
		{
			// Dynamic has no legacy equivalent: fall back to the medium budget.
			name:        "sonnet_4_5_legacy_dynamic_falls_back_to_medium",
			model:       "claude-sonnet-4-5-20250929",
			effort:      contract.EffortDynamic(),
			maxTokens:   16000,
			wantTemp:    true,
			wantEnabled: true,
			wantBudget:  int64(contract.LevelMedium.Budget()),
		},
		{
			// A legacy budget must be >=1024 and < max_tokens; when
			// max_tokens leaves no room, thinking is dropped rather than
			// sending a guaranteed 400.
			name:        "sonnet_4_5_legacy_small_maxtokens_drops_thinking",
			model:       "claude-sonnet-4-5-20250929",
			effort:      contract.EffortLevel(contract.LevelLow),
			wantTemp:    true,
			wantEnabled: false,
		},
		{
			name:         "sonnet_4_5_legacy_no_thinking",
			model:        "claude-sonnet-4-5-20250929",
			effort:       contract.EffortOff(),
			wantTemp:     true,
			wantAdaptive: false,
			wantEnabled:  false,
		},
	}

	client := NewProvider("", "")
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			maxTokens := tc.maxTokens
			if maxTokens == 0 {
				maxTokens = 1024
			}
			params := client.BuildRequest(&contract.CompletionRequest{
				Model:          tc.model,
				MaxTokens:      maxTokens,
				Temperature:    contract.Float32Ptr(1.0),
				ThinkingEffort: tc.effort,
				Messages: []messages.ChatMessage{
					{Role: messages.MessageRoleUser, Content: "hi"},
				},
			})

			if got := params.Temperature != nil; got != tc.wantTemp {
				t.Errorf("Temperature set = %v, want %v", got, tc.wantTemp)
			}

			gotAdaptive := params.Thinking != nil && params.Thinking.Type == ThinkingTypeAdaptive
			if gotAdaptive != tc.wantAdaptive {
				t.Errorf("thinking type adaptive = %v, want %v", gotAdaptive, tc.wantAdaptive)
			}
			if gotAdaptive {
				if got := params.Thinking.Display; got != DisplaySummarized {
					t.Errorf("adaptive Display = %q, want %q", got, DisplaySummarized)
				}
			}

			gotEnabled := params.Thinking != nil && params.Thinking.Type == ThinkingTypeEnabled
			if gotEnabled != tc.wantEnabled {
				t.Errorf("thinking type enabled = %v, want %v", gotEnabled, tc.wantEnabled)
			}
			if gotEnabled {
				if got := params.Thinking.BudgetTokens; got != tc.wantBudget {
					t.Errorf("budget_tokens = %d, want %d", got, tc.wantBudget)
				}
			}

			var gotEffort Effort
			if params.OutputConfig != nil {
				gotEffort = params.OutputConfig.Effort
			}
			if gotEffort != tc.wantEffort {
				t.Errorf("output_config effort = %q, want %q", gotEffort, tc.wantEffort)
			}
		})
	}
}

func TestCapabilityPredicates(t *testing.T) {
	adaptive := []string{
		"claude-opus-4-6",
		"claude-opus-4-7",
		"claude-opus-4-7-20260101",
		"claude-opus-4-8",
		"claude-opus-4-8-20260601",
		"claude-sonnet-4-6",
		"claude-opus-5",
		"claude-sonnet-5",
		"claude-fable-5",
		"claude-mythos-5",
		"claude-opus-6", // unknown future models default to adaptive
	}
	for _, m := range adaptive {
		if !supportsAdaptiveThinking(m) {
			t.Errorf("supportsAdaptiveThinking(%q) = false, want true", m)
		}
	}

	legacy := []string{
		"claude-sonnet-4-5-20250929",
		"claude-sonnet-4-20250514",
		"claude-opus-4-5",
		"claude-opus-4-1",
		"claude-opus-4-20250514",
		"claude-haiku-4-5",
		"claude-haiku-4-5-20251001",
		"claude-3-5-sonnet-20240620",
		"claude-mythos-preview",
	}
	for _, m := range legacy {
		if supportsAdaptiveThinking(m) {
			t.Errorf("supportsAdaptiveThinking(%q) = true, want false", m)
		}
	}

	if !rejectsSamplingParams("claude-opus-4-7") {
		t.Errorf("rejectsSamplingParams(claude-opus-4-7) = false, want true")
	}
	if !rejectsSamplingParams("claude-opus-4-7-20260101") {
		t.Errorf("rejectsSamplingParams(dated opus-4-7) = false, want true")
	}
	if !rejectsSamplingParams("claude-opus-4-8") {
		t.Errorf("rejectsSamplingParams(claude-opus-4-8) = false, want true")
	}
	if !rejectsSamplingParams("claude-opus-4-8-20260601") {
		t.Errorf("rejectsSamplingParams(dated opus-4-8) = false, want true")
	}
	for _, m := range []string{"claude-opus-5", "claude-sonnet-5", "claude-fable-5", "claude-mythos-5", "claude-opus-6"} {
		if !rejectsSamplingParams(m) {
			t.Errorf("rejectsSamplingParams(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"claude-opus-4-6", "claude-sonnet-4-6", "claude-sonnet-4-5-20250929", "claude-haiku-4-5"} {
		if rejectsSamplingParams(m) {
			t.Errorf("rejectsSamplingParams(%q) = true, want false", m)
		}
	}
}

// TestToolChoiceWithThinking verifies that when thinking is enabled,
// BuildRequest does NOT force tool_choice=any — Anthropic rejects the
// combination with "Thinking may not be enabled when tool_choice forces tool use".
func TestToolChoiceWithThinking(t *testing.T) {
	client := NewProvider("", "")
	responseSchema := &schema.Schema{
		Raw: map[string]any{
			"type":       "object",
			"properties": map[string]any{"answer": map[string]any{"type": "string"}},
			"required":   []any{"answer"},
		},
	}

	tests := []struct {
		name       string
		effort     contract.ThinkingEffort
		wantForced bool
	}{
		{"no_thinking_forces_tool_choice", contract.EffortOff(), true},
		{"thinking_low_skips_force", contract.EffortLevel(contract.LevelLow), false},
		{"thinking_medium_skips_force", contract.EffortLevel(contract.LevelMedium), false},
		{"thinking_high_skips_force", contract.EffortLevel(contract.LevelHigh), false},
		{"thinking_dynamic_skips_force", contract.EffortDynamic(), false},
		{"thinking_budget_skips_force", contract.EffortBudget(12000), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params := client.BuildRequest(&contract.CompletionRequest{
				Model:          "claude-haiku-4-5",
				MaxTokens:      1024,
				ResponseSchema: responseSchema,
				ThinkingEffort: tc.effort,
				Messages: []messages.ChatMessage{
					{Role: messages.MessageRoleUser, Content: "hi"},
				},
			})
			gotForced := params.ToolChoice != nil && params.ToolChoice.Type == "any"
			if gotForced != tc.wantForced {
				t.Errorf("tool_choice forced = %v, want %v", gotForced, tc.wantForced)
			}
		})
	}
}

// TestMessagesToParamsThinkingBlocksAfterReload verifies preserved
// thinking blocks are replayed both in-process ([]map[string]any) and after a
// JSON session reload ([]any of map[string]any).
func TestMessagesToParamsThinkingBlocksAfterReload(t *testing.T) {
	block := map[string]any{"type": "thinking", "thinking": "chain", "signature": "sig"}

	cases := []struct {
		name   string
		blocks any
	}{
		{"in-process []map[string]any", []map[string]any{block}},
		{"JSON-reloaded []any", []any{block}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := []messages.ChatMessage{{
				Role:     messages.MessageRoleAssistant,
				Content:  "answer",
				Metadata: map[string]any{"anthropic_thinking_blocks": tc.blocks},
			}}

			params, _ := messagesToParams(msgs, nil)
			if len(params) != 1 {
				t.Fatalf("param count = %d, want 1", len(params))
			}
			var sawThinking bool
			for _, b := range params[0].Content {
				if b.Type == "thinking" {
					sawThinking = true
					if b.Thinking != "chain" || b.Signature != "sig" {
						t.Errorf("thinking block content = %+v", b)
					}
				}
			}
			if !sawThinking {
				t.Errorf("thinking block was dropped")
			}
		})
	}
}

// TestMessagesToParamsRedactedThinking verifies redacted thinking
// blocks are replayed verbatim, in both the in-process and JSON-reloaded
// metadata shapes.
func TestMessagesToParamsRedactedThinking(t *testing.T) {
	block := map[string]any{"type": "redacted_thinking", "data": "opaque-blob"}

	cases := []struct {
		name   string
		blocks any
	}{
		{"in-process []map[string]any", []map[string]any{block}},
		{"JSON-reloaded []any", []any{block}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := []messages.ChatMessage{{
				Role:     messages.MessageRoleAssistant,
				Content:  "answer",
				Metadata: map[string]any{"anthropic_thinking_blocks": tc.blocks},
			}}

			params, _ := messagesToParams(msgs, nil)
			if len(params) != 1 {
				t.Fatalf("param count = %d, want 1", len(params))
			}
			var sawRedacted bool
			for _, b := range params[0].Content {
				if b.Type == "redacted_thinking" {
					sawRedacted = true
					if b.Data != "opaque-blob" {
						t.Errorf("redacted data = %q, want opaque-blob", b.Data)
					}
				}
			}
			if !sawRedacted {
				t.Errorf("redacted thinking block was dropped")
			}
		})
	}
}

// TestEmptyToolResultOmitsContent is a regression test: the API
// rejects empty text blocks, so a tool that produced no output must send a
// bare tool_result (content is optional there) instead of nesting one.
func TestEmptyToolResultOmitsContent(t *testing.T) {
	params, _ := messagesToParams([]messages.ChatMessage{
		{
			Role: messages.MessageRoleAssistant,
			ToolCalls: []messages.ChatMessageToolCall{
				{ID: "toolu_1", Name: "bash", Arguments: "{}"},
			},
		},
		{Role: messages.MessageRoleTool, ToolCallID: "toolu_1", Content: "  \n"},
	}, nil)

	var result *ContentBlock
	for _, param := range params {
		for _, block := range param.Content {
			if block.Type == "tool_result" {
				result = block
			}
		}
	}
	if result == nil {
		t.Fatal("no tool_result block produced")
	}
	if len(result.Content) != 0 {
		t.Fatalf("empty tool result content = %#v, want none", result.Content)
	}
}

// TestToolResultErrorFlag: a durably recorded tool failure travels
// to Anthropic as is_error:true; successes and results with no recorded
// outcome stay is_error:false.
func TestToolResultErrorFlag(t *testing.T) {
	failed := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: "toolu_1", Content: "boom"}
	failed.SetToolSucceeded(false)
	succeeded := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: "toolu_2", Content: "ok"}
	succeeded.SetToolSucceeded(true)
	unrecorded := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: "toolu_3", Content: "legacy"}

	params, _ := messagesToParams([]messages.ChatMessage{failed, succeeded, unrecorded}, nil)

	got := map[string]bool{}
	for _, param := range params {
		for _, block := range param.Content {
			if block.Type == "tool_result" && block.IsError != nil {
				got[block.ToolUseID] = *block.IsError
			}
		}
	}
	want := map[string]bool{"toolu_1": true, "toolu_2": false, "toolu_3": false}
	for id, wantErr := range want {
		gotErr, ok := got[id]
		if !ok {
			t.Fatalf("tool_result %s missing is_error", id)
		}
		if gotErr != wantErr {
			t.Errorf("is_error for %s = %v, want %v", id, gotErr, wantErr)
		}
	}
}

// TestMaxTokensZeroUsesDefault: the CLI's "0 = provider default"
// cannot be sent as max_tokens=0, which the Messages API reserves for
// warming the prompt cache without generating a reply.
func TestMaxTokensZeroUsesDefault(t *testing.T) {
	client := NewProvider("key", "")
	params := client.BuildRequest(&contract.CompletionRequest{
		Model:          "claude-sonnet-4-5",
		Messages:       messages.User("hi"),
		MaxTokens:      0,
		ThinkingEffort: contract.EffortLevel(contract.LevelMedium),
	})
	if params.MaxTokens != defaultMaxTokens {
		t.Fatalf("max_tokens = %d, want the %d default", params.MaxTokens, defaultMaxTokens)
	}
	if params.Thinking != nil && params.Thinking.Type == ThinkingTypeEnabled && params.Thinking.BudgetTokens >= params.MaxTokens {
		t.Fatalf("thinking budget %d not below max_tokens %d", params.Thinking.BudgetTokens, params.MaxTokens)
	}
	explicit := client.BuildRequest(&contract.CompletionRequest{Model: "claude-sonnet-4-5", Messages: messages.User("hi"), MaxTokens: 4096})
	if explicit.MaxTokens != 4096 {
		t.Fatalf("explicit max_tokens = %d, want 4096", explicit.MaxTokens)
	}
}
