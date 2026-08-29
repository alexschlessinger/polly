package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/anthropic"
	"github.com/alexschlessinger/pollytool/llm/gemini"
)

// ErrContextWindowUnknown reports that a model's provider does not expose a
// discoverable context window.
var ErrContextWindowUnknown = errors.New("model context window is not discoverable")

// DiscoverModelContextWindow fetches the advertised context window (in
// tokens) for a provider-prefixed model. Only providers with a model metadata
// endpoint resolve (anthropic, gemini); the rest return
// ErrContextWindowUnknown without any network traffic.
func DiscoverModelContextWindow(ctx context.Context, model, apiKey string) (int, error) {
	provider, name, ok := strings.Cut(model, "/")
	if !ok {
		return 0, fmt.Errorf("model %q lacks a provider prefix", model)
	}
	switch strings.ToLower(provider) {
	case "anthropic":
		info, err := anthropic.NewClient(apiKey).GetModel(ctx, name)
		if err != nil {
			return 0, err
		}
		if info.MaxInputTokens <= 0 {
			return 0, ErrContextWindowUnknown
		}
		return info.MaxInputTokens, nil
	case "gemini":
		info, err := gemini.NewClient(apiKey).GetModel(ctx, name)
		if err != nil {
			return 0, err
		}
		if info.InputTokenLimit <= 0 {
			return 0, ErrContextWindowUnknown
		}
		return info.InputTokenLimit, nil
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
