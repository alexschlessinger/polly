package main

import (
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
)

const composerBoundaryAccentCols = 2

// composerBoundaryRole colors only the short prompt-edge segment, not the
// whole separator. Approval is steady attention, never progress or acceptance.
// Caller holds m.mu and supplies the loop-owned inspector focus.
func (m *replModel) composerBoundaryRole(inspectorFocused bool) string {
	if !m.affordancesVisible() || inspectorFocused {
		return "muted"
	}
	if m.approval != nil {
		return "active"
	}
	return "accent"
}

// composerBoundaryRule draws a quiet rule with a short prompt-edge cue.
// Its absolute columns let caller-link text take precedence without moving
// either the prompt-edge accent or the navigation hitbox.
func (m *replModel) composerBoundaryRule(x, cols int, inspectorFocused bool) string {
	cols = max(0, cols)
	accent := max(0, min(cols, composerBoundaryAccentCols-x))
	return style.Styled(strings.Repeat("─", accent), m.composerBoundaryRole(inspectorFocused), "") +
		style.Styled(strings.Repeat("─", cols-accent), "muted", "")
}
