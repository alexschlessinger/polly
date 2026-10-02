package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrContextWindowUnknown reports that a model's provider does not expose a
// discoverable context window.
var ErrContextWindowUnknown = errors.New("model context window is not discoverable")

// ModelName strips the provider prefix from a provider-qualified model id,
// returning ids without one unchanged.
func ModelName(model string) string {
	if _, name, ok := strings.Cut(model, "/"); ok {
		return name
	}
	return model
}

// DiscoverModelContextWindow is a compatibility wrapper over provider metadata.
func DiscoverModelContextWindow(ctx context.Context, model, apiKey string) (int, error) {
	provider, name, ok := strings.Cut(model, "/")
	if !ok {
		return 0, fmt.Errorf("model %q lacks a provider prefix", model)
	}
	m := NewMultiPass(map[string]string{strings.ToLower(provider): apiKey})
	return discoverContextWindow(ctx, m, ModelTarget{Provider: provider, Model: name})
}

// modelLookup is the catalog read shared by MultiPass and Agent.
type modelLookup interface {
	LookupModel(context.Context, ModelTarget, bool) (ModelCatalog, error)
}

// discoverContextWindow reads the target's effective context window from its
// cached or fetched metadata, reporting ErrContextWindowUnknown when the
// catalog has no model or the model advertises no window.
func discoverContextWindow(ctx context.Context, lookup modelLookup, t ModelTarget) (int, error) {
	cat, err := lookup.LookupModel(ctx, t, false)
	if err != nil || len(cat.Models) == 0 {
		return 0, ErrContextWindowUnknown
	}
	if n := cat.Models[0].EffectiveCapabilities(routeHost(t)).ContextWindow(); n > 0 {
		return n, nil
	}
	return 0, ErrContextWindowUnknown
}

// defaultReplyReserve is the room kept for a reply whose output limit is the
// provider's default.
const defaultReplyReserve = 32_000

// ContextReserve is the room a request on a window of window tokens keeps
// for its reply: its output limit, maxTokens (defaultReplyReserve when that is
// the provider's default, 0), within a quarter of the window. Requests are
// sized in the provider's count, so no further margin is kept.
func ContextReserve(window, maxTokens int) int {
	if maxTokens <= 0 {
		maxTokens = defaultReplyReserve
	}
	return min(maxTokens, window/4)
}

// windowFit is how a request whose output limit is maxTokens fits a window
// of window tokens on provider: its input budget, and the output limit it may
// ask for. A shared window keeps room for the reply (see ContextReserve) and
// holds the output limit to it, since providers that count the limit against
// the window reject one that outgrows that room. A window that bounds input
// alone (providerSpec.inputWindow) is the budget whole and leaves the output
// limit as it is.
func windowFit(provider string, window, maxTokens int) (budget, output int) {
	if providerFor(provider).inputWindow {
		return window, maxTokens
	}
	reserve := ContextReserve(window, maxTokens)
	return window - reserve, min(maxTokens, reserve)
}

// ClampContextBudget bounds a positive context budget by a discovered model
// window less the room its reply keeps (see ContextReserve). A zero or
// negative budget means the user chose unlimited and is respected verbatim,
// as is an unknown (non-positive) window.
func ClampContextBudget(budget, window, maxTokens int) int {
	if budget <= 0 || window <= 0 {
		return budget
	}
	return min(budget, window-ContextReserve(window, maxTokens))
}
