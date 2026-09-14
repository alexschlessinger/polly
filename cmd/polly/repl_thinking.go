package main

import (
	"strings"

	"github.com/alexschlessinger/pollytool/llm"
)

func cachedThinkingCapabilities(ctx *replCommandContext, settings *Settings) (llm.ModelCapabilities, bool) {
	if settings == nil {
		return llm.ModelCapabilities{}, false
	}
	provider, model, _ := strings.Cut(settings.Model, "/")
	if !strings.EqualFold(provider, "openrouter") {
		return llm.ModelCapabilities{}, false
	}
	if ctx == nil || ctx.state == nil || ctx.state.agent == nil {
		return llm.ModelCapabilities{}, true
	}
	target := llm.ModelTarget{Provider: provider, Model: model, Host: settings.ModelHost}
	if ctx.config != nil {
		target.BaseURL = ctx.config.BaseURL
	}
	if info := ctx.state.agent.CachedModelInfo(target); info != nil {
		return info.EffectiveCapabilities(settings.ModelHost), true
	}
	return llm.ModelCapabilities{}, true
}
