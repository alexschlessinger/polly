package main

import (
	"strings"

	"github.com/alexschlessinger/pollytool/llm"
)

// cachedCapabilities returns what the session already knows about the
// settings' model and route, without network work: the zero value when
// nothing is cached, which every caller reads as "unknown".
func cachedCapabilities(ctx *replCommandContext, settings *Settings) llm.ModelCapabilities {
	if settings == nil || ctx == nil || ctx.state == nil || ctx.state.agent == nil {
		return llm.ModelCapabilities{}
	}
	provider, model, _ := strings.Cut(settings.Model, "/")
	target := llm.ModelTarget{Provider: provider, Model: model, Host: settings.ModelHost}
	if ctx.config != nil {
		target.BaseURL = ctx.config.BaseURL
	}
	if info := ctx.state.agent.CachedModelInfo(target); info != nil {
		return info.EffectiveCapabilities(settings.ModelHost)
	}
	return llm.ModelCapabilities{}
}

// thinkingEffortDisplay renders the saved effort as the model's provider
// resolves it: an empty string when the provider has nothing to add, and the
// reason when it would reject the effort outright.
func thinkingEffortDisplay(ctx *replCommandContext, settings *Settings) (string, error) {
	if settings == nil {
		return "", nil
	}
	effort, err := llm.ParseThinkingEffort(settings.ThinkingEffort)
	if err != nil {
		return "", err
	}
	return llm.ResolveThinkingFor(settings.Model, effort, cachedCapabilities(ctx, settings))
}

// validateThinkingEffort rejects an effort the session's model would refuse,
// the check /set runs before storing one and the setup form runs before
// saving one.
func validateThinkingEffort(ctx *replCommandContext, value string) error {
	if ctx == nil || ctx.settings == nil {
		return nil
	}
	effort, err := llm.ParseThinkingEffort(value)
	if err != nil {
		return err
	}
	_, err = llm.ResolveThinkingFor(ctx.settings.Model, effort, cachedCapabilities(ctx, ctx.settings))
	return err
}
