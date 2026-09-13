package main

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/alexschlessinger/pollytool/llm"
)

func (r *managedREPL) applySelectedModelHost(model, host string, window int, contextOverride *string) error {
	ctx := newManagedReplCommandContext(r)
	if ctx.settings == nil {
		return fmt.Errorf("model settings unavailable")
	}
	candidate := ctx.settings.clone()
	spec, _ := settingSpecFor("model")
	if err := spec.parse(&candidate, model); err != nil {
		return err
	}
	spec, _ = settingSpecFor("modelhost")
	if err := spec.parse(&candidate, host); err != nil {
		return err
	}
	if contextOverride != nil {
		spec, _ = settingSpecFor("maxcontext")
		if err := spec.parse(&candidate, *contextOverride); err != nil {
			return err
		}
	}
	candidate.MaxHistoryTokens = candidate.contextLimit(window)
	old := *ctx.settings
	*ctx.settings = candidate
	if err := persistReplSettings(ctx); err != nil {
		*ctx.settings = old
		return fmt.Errorf("model change failed: %w", err)
	}
	if ctx.settingsApplied != nil {
		ctx.settingsApplied()
	}
	label := model
	if host != "" {
		label += " · " + host
	}
	r.model.appendNoticeLine("model: " + label)
	return nil
}

func metadataDisplayText(s string, multiline bool) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			if multiline {
				return r
			}
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// A direct-provider selection warms only that model's detail record. Active
// requests coalesce with this read if the user starts a turn immediately.
func (r *managedREPL) prefetchSelectedModel(model, host string) {
	if r.state == nil || r.state.agent == nil || r.work == nil {
		return
	}
	provider, _, _ := strings.Cut(model, "/")
	target := r.browserTarget(provider, model)
	target.Host = host
	agent := r.state.agent
	r.background(func() { _, _ = agent.LookupModel(r.work.ctx, target, false) })
}

func (r *managedREPL) browserTarget(provider, model string) llm.ModelTarget {
	t := llm.ModelTarget{Provider: provider, Model: strings.TrimPrefix(model, provider+"/")}
	if r.config != nil {
		t.BaseURL = r.config.BaseURL
	}
	return t
}
