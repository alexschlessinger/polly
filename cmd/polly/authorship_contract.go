package main

import (
	"fmt"
	"strings"
)

// authorshipContract tells the model how a commit it creates is attributed.
// The trailer names the model that wrote the commit, so the guidance is
// rendered per turn with the session's current model rather than kept as a
// constant: a model switch changes the trailer on the next turn.
const authorshipContract = `Commit attribution: end every commit you create with a blank line and this trailer, which names the model that wrote it:
Co-authored-by: polly/%s <polly@pollytool>
Add it only to commits you create; never rewrite existing commits to add it. If the user asks for no co-author trailer, follow the user.`

// authorshipGuidance renders the commit trailer for settings' current model.
// It returns "" when no model is selected: there is no attribution to state.
func authorshipGuidance(settings *Settings) string {
	if settings == nil {
		return ""
	}
	model := attributionModelName(settings.Model)
	if model == "" {
		return ""
	}
	return fmt.Sprintf(authorshipContract, model)
}

// attributionModelName is the bare model id the trailer names, the same form
// the status row shows. The id can come from a flag or the setup form, so it
// is reduced to one line and stripped of the angle brackets that would end
// the trailer's address.
func attributionModelName(model string) string {
	model = shortModelName(strings.TrimSpace(model))
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '<' || r == '>' {
			return -1
		}
		return r
	}, model)
}
