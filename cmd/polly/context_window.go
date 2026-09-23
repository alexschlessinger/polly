package main

import (
	"context"
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
	window := state.contextWindowFor(ctx, settings.Model)
	return settings.contextBudget(window)
}

// contextWindowFor asks the model metadata service for the model's context
// window each turn; freshness and identity belong to that service, and legacy
// session ContextWindows are not consulted. 0 means unknown.
func (s *conversationState) contextWindowFor(ctx context.Context, model string) int {
	if s.agent == nil {
		return 0
	}
	bounded, cancel := context.WithTimeout(ctx, contextWindowDiscoveryTimeout)
	defer cancel()
	target := modelMetadataTarget(model, s.settings.ModelHost, s.metadataBaseURL)
	cat, _ := s.agent.LookupModel(bounded, target, false)
	if len(cat.Models) == 0 {
		return 0
	}
	provider, name := target.Provider, target.Model
	host := s.settings.ModelHost
	if provider == "huggingface" {
		if _, h, ok := strings.Cut(name, ":"); ok {
			host = h
		}
	}
	return cat.Models[0].EffectiveCapabilities(host).ContextWindow()
}

// contextBudget keeps automatic selection separate from explicit numeric limits.
// An unavailable route must not reuse a previous model's display snapshot.
func (s *Settings) contextBudget(window int) int {
	limit := s.contextLimit(window)
	return llm.ClampContextBudget(limit, window, s.MaxTokens)
}

// modelMetadataTarget is the lookup a turn makes for model's context window.
// A prefetch must build the same target, or it warms a different cache entry.
func modelMetadataTarget(model, host, baseURL string) llm.ModelTarget {
	provider, name, _ := strings.Cut(model, "/")
	return llm.ModelTarget{Provider: provider, Model: name, Host: host, BaseURL: modelMetadataBaseURL(provider, baseURL)}
}

// The global base URL configures compatible endpoints, not native provider APIs.
func modelMetadataBaseURL(provider, baseURL string) string {
	if provider == "anthropic" || provider == "gemini" {
		return ""
	}
	return baseURL
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
