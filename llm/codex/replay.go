package codex

import (
	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/messages"
)

// replayScope is the name a reply's reasoning items are recorded under.
// The backend's encrypted reasoning is bound to the model that produced
// it, and the same model name reached through api.openai.com is another
// key, so the scope carries the provider as well as the model.
func replayScope(model string) string { return "codex/" + model }

// replayReasoning rebuilds the reasoning items a reply recorded for model
// through this provider.
func replayReasoning(msg messages.ChatMessage, model string) []openai.ResponseInputItem {
	return openai.ReplayReasoningItems(msg, replayScope(model))
}
