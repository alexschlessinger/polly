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

// ClampContextBudget bounds a positive context budget by a discovered model
// window, reserving a tenth of the window plus the response budget for output
// and estimator error, and never clamping below half the window. A zero or
// negative budget means the user chose unlimited and is respected verbatim,
// as is an unknown (non-positive) window.
func ClampContextBudget(budget, window, maxTokens int) int {
	if budget <= 0 || window <= 0 {
		return budget
	}
	safe := window - window/10 - maxTokens
	if safe < window/2 {
		safe = window / 2
	}
	if safe < budget {
		return safe
	}
	return budget
}
