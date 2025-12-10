package llm

import "fmt"

// ThinkingEffort represents the level of reasoning effort for models that support extended thinking
type ThinkingEffort string

const (
	ThinkingOff    ThinkingEffort = "off"
	ThinkingLow    ThinkingEffort = "low"
	ThinkingMedium ThinkingEffort = "medium"
	ThinkingHigh   ThinkingEffort = "high"
)

// ParseThinkingEffort converts a string to ThinkingEffort, returning error if invalid
func ParseThinkingEffort(s string) (ThinkingEffort, error) {
	switch s {
	case "", "off":
		return ThinkingOff, nil
	case "low":
		return ThinkingLow, nil
	case "medium":
		return ThinkingMedium, nil
	case "high":
		return ThinkingHigh, nil
	default:
		return "", fmt.Errorf("invalid thinking effort %q: must be off, low, medium, or high", s)
	}
}

// IsEnabled returns true if thinking is enabled (not off or empty)
func (e ThinkingEffort) IsEnabled() bool {
	return e != "" && e != ThinkingOff
}
