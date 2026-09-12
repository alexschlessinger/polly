package subagent

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// NormalizeLabel validates the initial human-facing name before a new agent
// allocates a session or workspace. Its bounds match session titles.
func NormalizeLabel(label string) (string, error) {
	if !utf8.ValidString(label) {
		return "", errors.New("agent label must be valid UTF-8")
	}
	label = strings.Join(strings.Fields(label), " ")
	if label == "" {
		return "", errors.New("label is required for a new agent: supply a short description of its job")
	}
	if utf8.RuneCountInString(label) > 80 {
		return "", errors.New("agent label must be at most 80 characters")
	}
	for _, r := range label {
		if unicode.IsControl(r) {
			return "", errors.New("agent label must not contain control characters")
		}
	}
	return label, nil
}
