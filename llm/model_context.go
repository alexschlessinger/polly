package llm

import (
	"slices"
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
)

// WithModelContext adds the request model's identity to a request-local copy
// of history. Agent uses it before every projection, never on durable history.
// Hosts inspecting the request can use the same composition after resolving
// skills. The caller's messages and nested content are left untouched.
func WithModelContext(history []messages.ChatMessage, model string) []messages.ChatMessage {
	model = modelContextName(BareModelName(model))
	if model == "" {
		return history
	}
	context := "<model_context>\nCurrent model: " + model + "\n</model_context>"
	out := slices.Clone(history)
	if len(out) > 0 && out[0].Role == messages.MessageRoleSystem {
		if len(out[0].Parts) > 0 {
			out[0].Parts = slices.Clone(out[0].Parts)
			promoteMessageContentToTextPart(&out[0])
			out[0].Parts = append(out[0].Parts, messages.ContentPart{Type: "text", Text: "\n\n" + context})
		} else {
			if out[0].Content != "" {
				out[0].Content += "\n\n"
			}
			out[0].Content += context
		}
		return out
	}
	return append([]messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: context}}, out...)
}

// modelContextName keeps the model id on one line and out of the block's
// delimiters: the id can come from a flag or the setup form.
func modelContextName(model string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '<' || r == '>' {
			return -1
		}
		return r
	}, model)
}
