package llm

import (
	"context"
	"errors"
	"testing"
)

func TestDiscoverModelContextWindowRouting(t *testing.T) {
	// Providers without a metadata endpoint resolve locally, with no network.
	for _, model := range []string{"openai/gpt-5.4", "ollama/llama3", "deepseek/deepseek-chat", "openrouter/foo/bar"} {
		if _, err := DiscoverModelContextWindow(context.Background(), model, ""); !errors.Is(err, ErrContextWindowUnknown) {
			t.Fatalf("model %q error = %v, want ErrContextWindowUnknown", model, err)
		}
	}
	if _, err := DiscoverModelContextWindow(context.Background(), "no-prefix", ""); err == nil || errors.Is(err, ErrContextWindowUnknown) {
		t.Fatalf("unprefixed model error = %v, want prefix failure", err)
	}
}

func TestClampContextBudget(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		budget, window, maxTokens int
		want                      int
	}{
		{name: "unlimited budget is respected", budget: 0, window: 200_000, maxTokens: 4_096, want: 0},
		{name: "unknown window leaves budget", budget: 256_000, window: 0, maxTokens: 4_096, want: 256_000},
		{name: "budget under window passes through", budget: 256_000, window: 1_000_000, maxTokens: 4_096, want: 256_000},
		{name: "budget over window clamps with headroom", budget: 256_000, window: 200_000, maxTokens: 4_096, want: 200_000 - 20_000 - 4_096},
		{name: "huge output budget floors at half the window", budget: 256_000, window: 200_000, maxTokens: 120_000, want: 100_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClampContextBudget(tc.budget, tc.window, tc.maxTokens); got != tc.want {
				t.Fatalf("ClampContextBudget(%d, %d, %d) = %d, want %d", tc.budget, tc.window, tc.maxTokens, got, tc.want)
			}
		})
	}
}
