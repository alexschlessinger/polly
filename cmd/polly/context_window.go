package main

import (
	"context"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
)

const contextWindowDiscoveryTimeout = 5 * time.Second
const defaultContextBudget = 256_000

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
	return cat.Models[0], llm.RouteHost(target), true
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

// contextLimit resolves the configured limit before the room a request keeps
// for its reply: the agent clamps it to the model's window.
func (s *Settings) contextLimit(window int) int {
	if !s.AutoMaxContext {
		return s.MaxHistoryTokens
	}
	if window > 0 {
		return window
	}
	return defaultContextBudget
}
