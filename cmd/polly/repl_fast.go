package main

import (
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/llm"
)

// parseSpeed maps UI names and legacy Fast spellings to the provider tier.
func parseSpeed(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "fast", "on", "true", "t", "yes", "1":
		return llm.ServiceTierPriority, nil
	case "default", "off", "false", "f", "no", "0":
		return "", nil
	case llm.ServiceTierUltrafast:
		return llm.ServiceTierUltrafast, nil
	}
	return "", fmt.Errorf("speed must be default, fast, or ultrafast, got %q", value)
}

func speedName(tier string) string {
	switch tier {
	case "":
		return "Default"
	case llm.ServiceTierPriority:
		return "Fast"
	case llm.ServiceTierUltrafast:
		return "Ultrafast"
	default:
		return fmt.Sprintf("Speed %q", tier)
	}
}

// paidSpeed reports the saved paid tier and whether Polly can send it for the
// current route. It only consults in-memory metadata.
func paidSpeed(ctx *replCommandContext, s *Settings) (name string, usable, unconfirmed bool) {
	if s == nil || s.ServiceTier == "" {
		return "", false, false
	}
	caps := cachedCapabilities(ctx, s)
	err := llm.ServiceTierFor(s.Model, ctx.configOrDefault().BaseURL, caps, s.ServiceTier)
	return speedName(s.ServiceTier), err == nil, needsSpeedCatalog(s.Model, s.ServiceTier, caps)
}

func needsSpeedCatalog(model, tier string, caps llm.ModelCapabilities) bool {
	return tier == llm.ServiceTierUltrafast && caps.ServiceTiers == nil && !caps.ServiceTierRuledOut() && llm.ProviderRequiresLogin(model)
}

func speedDisplay(ctx *replCommandContext, s *Settings) string {
	name, usable, unconfirmed := paidSpeed(ctx, s)
	switch {
	case usable:
		return name
	case unconfirmed:
		return name + " saved · availability not yet confirmed for this account"
	case name != "":
		return "Default · " + name + " saved, unavailable for this model or endpoint"
	default:
		return "Default"
	}
}

func speedWordsFor(ctx *replCommandContext) []string {
	s := ctx.settingsOrDefault()
	caps := cachedCapabilities(ctx, s)
	baseURL := ctx.configOrDefault().BaseURL
	words := []string{"default"}
	if llm.ServiceTierFor(s.Model, baseURL, caps, llm.ServiceTierPriority) == nil {
		words = append(words, "fast")
	}
	if llm.ServiceTierFor(s.Model, baseURL, caps, llm.ServiceTierUltrafast) == nil {
		words = append(words, "ultrafast")
	}
	return words
}

func validateSpeed(ctx *replCommandContext, value string) error {
	tier, err := parseSpeed(value)
	if err != nil || tier == "" || ctx == nil || ctx.settings == nil {
		return err
	}
	s := ctx.settings
	caps := cachedCapabilities(ctx, s)
	// An explicit edit can be the first command after sign-in. Check the
	// account catalog then, without turning settings display into a fetch.
	if needsSpeedCatalog(s.Model, tier, caps) && ctx.state != nil {
		if info, host, ok := ctx.state.modelInfoFor(ctx.operationContext(), s.Model, s.ModelHost); ok {
			caps = info.EffectiveCapabilities(host)
		}
	}
	return llm.ServiceTierFor(s.Model, ctx.configOrDefault().BaseURL, caps, tier)
}

func (r *managedREPL) refreshStatusSpeed() {
	// Metadata can arrive after the session's first frame. Resolve at paint
	// time so the label follows the current route and cached catalog.
	ctx := replCommandContext{config: r.config, state: r.state}
	name, usable, _ := paidSpeed(&ctx, r.sessionSettings())
	r.model.status.modelSpeed = ""
	if usable {
		r.model.status.modelSpeed = name
	}
}
