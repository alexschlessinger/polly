package main

import (
	"context"
	"testing"
)

func TestResolveContextBudgetIgnoresLegacyUnscopedCache(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "window-cache")
	ctx := context.Background()
	md, err := session.GetMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	md.ContextWindows = map[string]int{"anthropic/claude-haiku-4-5": 200_000}
	if err := session.SetMetadata(ctx, md); err != nil {
		t.Fatal(err)
	}

	state := &conversationState{
		session:  session,
		settings: Settings{Model: "anthropic/claude-haiku-4-5", MaxHistoryTokens: 256_000, MaxTokens: 4_096},
	}

	// A permanent session value no longer establishes the effective route limit.
	if got := resolveContextBudget(ctx, state); got != 256_000 {
		t.Fatalf("clamped budget = %d", got)
	}
	// The process cache holds the resolved window for later turns.
	if state.contextWindows[state.settings.Model] != 0 {
		t.Fatalf("process cache = %#v", state.contextWindows)
	}

	// An unlimited budget opts out of clamping entirely.
	state.settings.MaxHistoryTokens = 0
	if got := resolveContextBudget(ctx, state); got != 0 {
		t.Fatalf("unlimited budget was clamped to %d", got)
	}
}

func TestContextWindowForCachesUndiscoverableProviders(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "window-negative")
	state := &conversationState{session: session}
	ctx := context.Background()

	// A custom client without metadata keeps the limit unknown.
	if window := state.contextWindowFor(ctx, "ollama/llama3"); window != 0 {
		t.Fatalf("window = %d, want 0", window)
	}
	if window, attempted := state.contextWindows["ollama/llama3"]; !attempted || window != 0 {
		t.Fatalf("negative attempt was not cached: %#v", state.contextWindows)
	}
	md, err := session.GetMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(md.ContextWindows) != 0 {
		t.Fatalf("unknown window was durably cached: %#v", md.ContextWindows)
	}
}
