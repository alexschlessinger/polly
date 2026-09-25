package llm

import (
	"errors"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

// fastUnavailable explains why fast mode cannot reach model, or is empty
// when it can: the provider has no fast tier, or the model's catalog rules
// the tier out. Nil capabilities are unknown and let the request through,
// so the provider can answer for itself.
func fastUnavailable(model string, c *ModelCapabilities) string {
	provider, _, ok := strings.Cut(model, "/")
	if !ok {
		return "not available for " + model
	}
	if !providerFor(provider).fastTier {
		return "not available for " + strings.ToLower(provider) + "/ models"
	}
	if c != nil {
		if supported, known := c.Parameters[contract.ParameterServiceTier]; known && !supported {
			return "not available for " + model
		}
	}
	return ""
}

// FastModeFor reports whether fast mode reaches model, as an error saying
// why not, for settings display and the forms that save the setting. What
// the catalog does not rule out passes.
func FastModeFor(model string, c ModelCapabilities) error {
	if why := fastUnavailable(model, &c); why != "" {
		return errors.New("fast mode is " + why)
	}
	return nil
}
