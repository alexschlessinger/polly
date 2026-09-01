package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
)

const contextWindowDiscoveryTimeout = 5 * time.Second

// resolveContextBudget bounds the configured context budget by the model's
// advertised context window, so a budget larger than the window cannot build
// requests the provider will reject. The configured setting itself is never
// rewritten; the clamp applies per request. An unlimited budget (0) is an
// explicit opt-out and skips discovery entirely, and discovery failures leave
// the configured budget in place.
func resolveContextBudget(ctx context.Context, config *Config, state *conversationState) int {
	budget := config.MaxHistoryTokens
	if budget <= 0 || state == nil || state.session == nil {
		return budget
	}
	return llm.ClampContextBudget(budget, state.contextWindowFor(ctx, config.Model), config.MaxTokens)
}

// contextWindowFor returns the model's advertised context window, or 0 when
// unknown. Discovery runs at most once per model per process, and successful
// lookups persist into session metadata so later runs of this context skip
// the network entirely.
func (s *conversationState) contextWindowFor(ctx context.Context, model string) int {
	if window, attempted := s.contextWindows[model]; attempted {
		return window
	}

	window := 0
	md, mdErr := s.session.GetMetadata(ctx)
	if mdErr == nil && md != nil && md.ContextWindows[model] > 0 {
		window = md.ContextWindows[model]
	} else {
		discoverCtx, cancel := context.WithTimeout(ctx, contextWindowDiscoveryTimeout)
		discovered, err := discoverModelContextWindow(discoverCtx, s, model)
		cancel()
		switch {
		case err == nil:
			window = discovered
			if mdErr == nil && md != nil {
				if md.ContextWindows == nil {
					md.ContextWindows = make(map[string]int)
				}
				md.ContextWindows[model] = discovered
				if err := s.session.SetMetadata(ctx, md); err != nil {
					slog.Debug("context_window_cache_write_failed", "model", model, "error", err)
				}
			}
		case errors.Is(err, llm.ErrContextWindowUnknown):
			// The provider has no metadata endpoint; nothing to clamp.
		default:
			slog.Debug("context_window_discovery_failed", "model", model, "error", err)
		}
	}

	if s.contextWindows == nil {
		s.contextWindows = make(map[string]int)
	}
	s.contextWindows[model] = window
	return window
}

func discoverModelContextWindow(ctx context.Context, state *conversationState, model string) (int, error) {
	if state != nil && state.agent != nil {
		return state.agent.DiscoverModelContextWindow(ctx, model)
	}
	provider, _, ok := strings.Cut(model, "/")
	if !ok {
		return 0, fmt.Errorf("model %q lacks a provider prefix", model)
	}
	return llm.DiscoverModelContextWindow(ctx, model, loadAPIKeys()[strings.ToLower(provider)])
}
