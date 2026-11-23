package llm

import (
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

const (
	// ServiceTierPriority selects priority processing (Codex fast mode).
	ServiceTierPriority = contract.ServiceTierPriority
	// ServiceTierUltrafast selects ultrafast processing on supported OpenAI models.
	ServiceTierUltrafast = contract.ServiceTierUltrafast
)

// tierUnavailable explains why a tier cannot reach the target, or returns
// empty when the request can pass. Codex Ultrafast requires catalog confirmation.
func tierUnavailable(providers map[string]providerSpec, model, baseURL string, c ModelCapabilities, tier string) string {
	switch tier {
	case "":
		return ""
	case ServiceTierPriority, ServiceTierUltrafast:
	default:
		return fmt.Sprintf("unknown service tier %q", tier)
	}
	provider, slug, ok := strings.Cut(model, "/")
	if !ok {
		return "not available for " + model
	}
	provider = strings.ToLower(provider)
	spec := providers[provider]
	if spec.supportsTier == nil {
		return "not available for " + provider + "/ models"
	}
	if tier == ServiceTierUltrafast && strings.TrimSpace(spec.scopeBaseURL(baseURL)) != "" {
		return "not available on Chat Completions endpoints"
	}
	if !spec.supportsTier(slug, tier, c) {
		if spec.signIn && tier == ServiceTierUltrafast {
			return "not advertised for " + model + " in the signed-in account's model catalog"
		}
		return "not available for " + model
	}
	return ""
}

// ServiceTierFor validates a service tier for model (provider/name) at
// baseURL. Empty baseURL uses the native endpoint; an empty tier is always
// valid and leaves the provider's default unchanged.
func ServiceTierFor(model, baseURL string, c ModelCapabilities, tier string) error {
	if why := tierUnavailable(providerTable(), model, baseURL, c, tier); why != "" {
		if tier == ServiceTierPriority {
			return fmt.Errorf("fast mode is %s", why)
		}
		return fmt.Errorf("cannot use service tier %q: %s", tier, why)
	}
	return nil
}

// FastModeFor reports whether model can use the priority service tier.
func FastModeFor(model string, c ModelCapabilities) error {
	return ServiceTierFor(model, "", c, ServiceTierPriority)
}
