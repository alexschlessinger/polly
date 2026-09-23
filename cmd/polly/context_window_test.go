package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
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

func TestOpenPrefetchesModelMetadata(t *testing.T) {
	t.Chdir(t.TempDir())
	var calls atomic.Int32
	requested := make(chan struct{}, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		requested <- struct{}{}
		fmt.Fprint(w, `{"id":"m"}`)
	}))
	defer server.Close()
	ctx := context.Background()
	opener := &conversationOpener{
		config:       &Config{NoSkills: true, NoSandbox: true, BaseURL: server.URL},
		llmClient:    llm.NewMultiPass(map[string]string{"openai": "key"}),
		sessionStore: testOpenMemoryStore(t, nil),
		cmd:          getCommand(),
	}
	state, err := opener.open(ctx, "prefetch", Settings{Model: "openai/m"}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	select {
	case <-requested:
	case <-time.After(5 * time.Second):
		t.Fatal("open did not prefetch the model's metadata")
	}
	// The turn's lookup joins the prefetch or reads what it cached.
	state.contextWindowFor(ctx, state.settings.Model)
	if n := calls.Load(); n != 1 {
		t.Fatalf("metadata fetched %d times, want 1", n)
	}
}
