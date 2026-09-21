package main

import (
	"context"
	"testing"
)

func TestResolveContextBudgetWithoutDiscovery(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "window-cache")
	ctx := context.Background()
	state := &conversationState{
		session:  session,
		settings: Settings{Model: "anthropic/claude-haiku-4-5", MaxHistoryTokens: 256_000, MaxTokens: 4_096},
	}

	// Without a discovered window the configured budget stands.
	if got := resolveContextBudget(ctx, state); got != 256_000 {
		t.Fatalf("clamped budget = %d", got)
	}

	// An unlimited budget opts out of clamping entirely.
	state.settings.MaxHistoryTokens = 0
	if got := resolveContextBudget(ctx, state); got != 0 {
		t.Fatalf("unlimited budget was clamped to %d", got)
	}
}

func TestContextWindowForUndiscoverableProviders(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "window-negative")
	state := &conversationState{session: session}
	ctx := context.Background()

	// A custom client without metadata keeps the limit unknown.
	if window := state.contextWindowFor(ctx, "ollama/llama3"); window != 0 {
		t.Fatalf("window = %d, want 0", window)
	}
}
