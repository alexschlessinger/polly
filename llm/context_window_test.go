package llm

import (
	"context"
	"errors"
	"testing"
)

func TestDiscoverModelContextWindowRouting(t *testing.T) {
	// Missing credentials and unsupported providers resolve without network.
	for _, model := range []string{"openai/gpt-5.4", "anthropic/claude-test", "deepseek/deepseek-chat", "unknown/foo"} {
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
		{name: "budget over window keeps room for the reply", budget: 256_000, window: 200_000, maxTokens: 4_096, want: 200_000 - 4_096},
		{name: "a provider default output keeps the default room", budget: 256_000, window: 200_000, maxTokens: 0, want: 200_000 - 32_000},
		{name: "huge output limit keeps a quarter of the window", budget: 256_000, window: 200_000, maxTokens: 120_000, want: 150_000},
		{name: "small window keeps a quarter for the reply", budget: 256_000, window: 32_768, maxTokens: 32_000, want: 24_576},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClampContextBudget(tc.budget, tc.window, tc.maxTokens); got != tc.want {
				t.Fatalf("ClampContextBudget(%d, %d, %d) = %d, want %d", tc.budget, tc.window, tc.maxTokens, got, tc.want)
			}
		})
	}
}
