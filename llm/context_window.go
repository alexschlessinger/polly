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
	info, err := m.GetModelInfo(ctx, ModelTarget{Provider: provider, Model: name})
	if err != nil || info == nil {
		return 0, ErrContextWindowUnknown
	}
	host := ""
	if provider == "huggingface" {
		_, host, _ = strings.Cut(name, ":")
	}
	window := info.EffectiveCapabilities(host).ContextWindow()
	if window <= 0 {
		return 0, ErrContextWindowUnknown
	}
	return window, nil
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
