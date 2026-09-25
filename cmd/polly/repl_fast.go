package main

import (
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/llm"
)

// fastModeWords are the values /set fast offers.
var fastModeWords = []string{"on", "off"}

// parseFastMode reads a fast-mode setting: on or off, or the usual
// spellings of a boolean.
func parseFastMode(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "on", "true", "yes", "1":
		return true, nil
	case "off", "false", "no", "0":
		return false, nil
	}
	return false, fmt.Errorf("fast must be on or off, got %q", value)
}

// fastModeDisplay renders the setting for /get, with the reason when the
// session's model cannot run in fast mode.
func fastModeDisplay(ctx *replCommandContext, settings *Settings) string {
	if settings == nil || !settings.Fast {
		return "off"
	}
	if err := llm.FastModeFor(settings.Model, cachedCapabilities(ctx, settings)); err != nil {
		return "on (" + metadataDisplayText(err.Error(), false) + ")"
	}
	return "on"
}

// validateFastMode refuses to turn fast mode on for a model that cannot
// run in it, the check /set runs before storing the setting.
func validateFastMode(ctx *replCommandContext, value string) error {
	on, err := parseFastMode(value)
	if err != nil || !on || ctx == nil || ctx.settings == nil {
		return err
	}
	return llm.FastModeFor(ctx.settings.Model, cachedCapabilities(ctx, ctx.settings))
}
