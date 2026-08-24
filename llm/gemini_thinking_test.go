package llm

import (
	"context"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/adapters"
	"github.com/alexschlessinger/pollytool/llm/gemini"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

// TestGeminiThinkingConfig3x verifies that Gemini 3.x models receive a
// ThinkingLevel enum, with xhigh/max clamped to high (Gemini's ceiling).
func TestGeminiThinkingConfig3x(t *testing.T) {
	tests := []struct {
		name   string
		effort ThinkingEffort
		want   gemini.ThinkingLevel
	}{
		{"minimal", EffortLevel(LevelMinimal), gemini.ThinkingLevelMinimal},
		{"low", EffortLevel(LevelLow), gemini.ThinkingLevelLow},
		{"medium", EffortLevel(LevelMedium), gemini.ThinkingLevelMedium},
		{"high", EffortLevel(LevelHigh), gemini.ThinkingLevelHigh},
		{"xhigh clamps to high", EffortLevel(LevelXHigh), gemini.ThinkingLevelHigh},
		{"max clamps to high", EffortLevel(LevelMax), gemini.ThinkingLevelHigh},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := geminiThinkingConfig(tc.effort, "gemini-3.1-pro-preview")
			if cfg == nil {
				t.Fatal("nil config")
			}
			if !cfg.IncludeThoughts {
				t.Error("IncludeThoughts = false, want true")
			}
			if cfg.ThinkingLevel != tc.want {
				t.Errorf("ThinkingLevel = %q, want %q", cfg.ThinkingLevel, tc.want)
			}
			if cfg.ThinkingBudget != nil {
				t.Errorf("ThinkingBudget = %d, want nil for 3.x", *cfg.ThinkingBudget)
			}
		})
	}
}

// TestGeminiThinkingConfig3xDynamic verifies that Dynamic leaves the level unset
// (zero value, dropped by omitempty), letting the model decide.
func TestGeminiThinkingConfig3xDynamic(t *testing.T) {
	cfg := geminiThinkingConfig(EffortDynamic(), "gemini-3.1-pro-preview")
	if cfg.ThinkingLevel != "" {
		t.Errorf("ThinkingLevel = %q, want unset (empty) for dynamic", cfg.ThinkingLevel)
	}
	if cfg.ThinkingBudget != nil {
		t.Errorf("ThinkingBudget = %d, want nil for 3.x dynamic", *cfg.ThinkingBudget)
	}
}

// TestGeminiThinkingConfig25 verifies that Gemini 2.5 models receive an integer
// ThinkingBudget: levels map via the canonical table, raw budgets pass through,
// Dynamic uses -1, and out-of-range budgets clamp to the model's documented cap.
func TestGeminiThinkingConfig25(t *testing.T) {
	tests := []struct {
		name   string
		effort ThinkingEffort
		model  string
		want   int32
	}{
		{"level low maps to budget", EffortLevel(LevelLow), "gemini-2.5-flash", 4096},
		{"raw budget passthrough", EffortBudget(8000), "gemini-2.5-flash", 8000},
		{"dynamic uses -1", EffortDynamic(), "gemini-2.5-flash", -1},
		{"budget over flash cap clamps", EffortBudget(100000), "gemini-2.5-flash", 24576},
		{"budget over pro cap clamps", EffortBudget(100000), "gemini-2.5-pro", 32768},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := geminiThinkingConfig(tc.effort, tc.model)
			if cfg.ThinkingBudget == nil {
				t.Fatal("ThinkingBudget = nil, want set for 2.5")
			}
			if *cfg.ThinkingBudget != tc.want {
				t.Errorf("ThinkingBudget = %d, want %d", *cfg.ThinkingBudget, tc.want)
			}
			if cfg.ThinkingLevel != "" {
				t.Errorf("ThinkingLevel = %q, want unset for 2.5", cfg.ThinkingLevel)
			}
		})
	}
}

// TestEmitGeminiPartsRoutesThoughtsToReasoning verifies thought-summary parts
// stream as reasoning while ordinary parts stream as content, so thinking
// never leaks into the visible response.
func TestEmitGeminiPartsRoutesThoughtsToReasoning(t *testing.T) {
	ch := make(chan messages.ChatMessage, 10)
	core := streaming.NewStreamingCore(context.Background(), ch, adapters.NewGeminiAdapter())

	emitGeminiParts(core, &gemini.GenerateContentResponse{
		Candidates: []*gemini.Candidate{{
			Content: &gemini.Content{Parts: []*gemini.Part{
				{Text: "planning the answer", Thought: true},
				{Text: "the actual answer"},
				{Text: ""}, // empty parts are skipped entirely
			}},
		}},
	})
	close(ch)

	var reasoning, content []string
	for msg := range ch {
		if msg.Reasoning != "" {
			reasoning = append(reasoning, msg.Reasoning)
		}
		if msg.Content != "" {
			content = append(content, msg.Content)
		}
	}
	if len(reasoning) != 1 || reasoning[0] != "planning the answer" {
		t.Fatalf("reasoning chunks = %v, want the thought part only", reasoning)
	}
	if len(content) != 1 || content[0] != "the actual answer" {
		t.Fatalf("content chunks = %v, want the answer part only", content)
	}
}

// TestEmitGeminiPartsHandlesEmptyCandidates guards the nil paths.
func TestEmitGeminiPartsHandlesEmptyCandidates(t *testing.T) {
	ch := make(chan messages.ChatMessage, 1)
	core := streaming.NewStreamingCore(context.Background(), ch, adapters.NewGeminiAdapter())
	emitGeminiParts(core, &gemini.GenerateContentResponse{})
	emitGeminiParts(core, &gemini.GenerateContentResponse{Candidates: []*gemini.Candidate{{}}})
	close(ch)
	if msg, ok := <-ch; ok {
		t.Fatalf("empty responses should emit nothing, got %#v", msg)
	}
}
