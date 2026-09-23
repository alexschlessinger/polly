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
	info, host, ok := s.modelInfoFor(ctx, model, s.settings.ModelHost)
	if !ok {
		return 0
	}
	return info.EffectiveCapabilities(host).ContextWindow()
}

// modelInfoFor looks up the model's catalog entry for a route, with the host
// its capabilities and prices are read for.
func (s *conversationState) modelInfoFor(ctx context.Context, model, modelHost string) (llm.ModelInfo, string, bool) {
	if s.agent == nil {
		return llm.ModelInfo{}, "", false
	}
	bounded, cancel := context.WithTimeout(ctx, contextWindowDiscoveryTimeout)
	defer cancel()
	target := modelMetadataTarget(model, modelHost, s.metadataBaseURL)
	cat, _ := s.agent.LookupModel(bounded, target, false)
	if len(cat.Models) == 0 {
		return llm.ModelInfo{}, "", false
	}
	provider, name := target.Provider, target.Model
	host := modelHost
	if provider == "huggingface" {
		if _, h, ok := strings.Cut(name, ":"); ok {
			host = h
		}
	}
	return cat.Models[0], host, true
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
