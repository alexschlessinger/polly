package llm

import (
	"slices"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// WithSandboxContext adds current registry permissions to a request-local copy
// of history. Agent uses it before every projection, never on durable history.
// Hosts inspecting the request can use the same composition after resolving
// skills. The caller's messages and nested content are left untouched.
func WithSandboxContext(history []messages.ChatMessage, registry *tools.ToolRegistry) ([]messages.ChatMessage, error) {
	context, err := registry.SandboxContext()
	if err != nil || context == "" {
		return history, err
	}
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
		return out, nil
	}
	return append([]messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: context}}, out...), nil
}
