package main

import (
	"context"
	"maps"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
)

const contextWindowDiscoveryTimeout = 5 * time.Second
const defaultContextBudget = 256_000

func resolveContextBudget(ctx context.Context, state *conversationState) int {
	if state == nil {
		return 0
	}
	settings := &state.settings
	if !settings.AutoMaxContext {
		return settings.MaxHistoryTokens
	}
	window := state.contextWindowFor(ctx, settings.Model)
	return settings.contextBudget(window)
}

// The process map is a display snapshot only. Freshness and identity belong to
// the shared metadata service; legacy session ContextWindows are not consulted.
func (s *conversationState) contextWindowFor(ctx context.Context, model string) int {
	window := 0
	if s.agent != nil {
		bounded, cancel := context.WithTimeout(ctx, contextWindowDiscoveryTimeout)
		defer cancel()
		provider, name, _ := strings.Cut(model, "/")
		cat, _ := s.agent.LookupModel(bounded, llm.ModelTarget{Provider: provider, Model: name, Host: s.settings.ModelHost, BaseURL: s.metadataBaseURL}, false)
		if len(cat.Models) > 0 {
			host := s.settings.ModelHost
			if provider == "huggingface" {
				if _, h, ok := strings.Cut(name, ":"); ok {
					host = h
				}
			}
			window = cat.Models[0].EffectiveCapabilities(host).ContextWindow()
		}
	}
	s.contextWindowsMu.Lock()
	defer s.contextWindowsMu.Unlock()
	if s.contextWindows == nil {
		s.contextWindows = map[string]int{}
	}
	s.contextWindows[model] = window
	return window
}
func (s *conversationState) cachedContextWindows() map[string]int {
	s.contextWindowsMu.Lock()
	defer s.contextWindowsMu.Unlock()
	return maps.Clone(s.contextWindows)
}
func discoverModelContextWindow(ctx context.Context, state *conversationState, model string) (int, error) {
	if state != nil {
		n := state.contextWindowFor(ctx, model)
		if n > 0 {
			return n, nil
		}
	}
	return 0, llm.ErrContextWindowUnknown
}

// contextBudget keeps automatic selection separate from explicit numeric limits.
// An unavailable route must not reuse a previous model's display snapshot.
func (s *Settings) contextBudget(window int) int {
	limit := s.contextLimit(window)
	if !s.AutoMaxContext {
		return limit
	}
	return llm.ClampContextBudget(limit, window, s.MaxTokens)
}

// contextLimit resolves the configured limit before reserving output headroom.
func (s *Settings) contextLimit(window int) int {
	if !s.AutoMaxContext {
		return s.MaxHistoryTokens
	}
	if window > 0 {
		return window
	}
	return defaultContextBudget
}
