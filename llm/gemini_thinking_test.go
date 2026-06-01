package llm

import (
	"testing"

	"google.golang.org/genai"
)

// TestGeminiThinkingConfig3x verifies that Gemini 3.x models receive a
// ThinkingLevel enum, with xhigh/max clamped to high (Gemini's ceiling).
func TestGeminiThinkingConfig3x(t *testing.T) {
	tests := []struct {
		name   string
		effort ThinkingEffort
		want   genai.ThinkingLevel
	}{
		{"minimal", EffortLevel(LevelMinimal), genai.ThinkingLevelMinimal},
		{"low", EffortLevel(LevelLow), genai.ThinkingLevelLow},
		{"medium", EffortLevel(LevelMedium), genai.ThinkingLevelMedium},
		{"high", EffortLevel(LevelHigh), genai.ThinkingLevelHigh},
		{"xhigh clamps to high", EffortLevel(LevelXHigh), genai.ThinkingLevelHigh},
		{"max clamps to high", EffortLevel(LevelMax), genai.ThinkingLevelHigh},
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
