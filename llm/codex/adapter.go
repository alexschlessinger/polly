package codex

import (
	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

// MetadataKey is where a reply records what the backend said about the
// account: the plan's usage meters, under "usage".
const MetadataKey = "codex"

// usageStateKey carries the meters on the stream state until the reply is
// assembled.
const usageStateKey = "codex_usage"

// adapter owns one reply: OpenAI's Responses handling with the reasoning
// items recorded under this provider's scope, plus the usage meters.
type adapter struct {
	*openai.ResponsesAdapter
}

func newAdapter(model string) *adapter {
	return &adapter{ResponsesAdapter: openai.NewResponsesAdapter(replayScope(model))}
}

func (a *adapter) EnrichFinalMessage(msg *messages.ChatMessage, state streaming.StreamStateInterface) {
	a.ResponsesAdapter.EnrichFinalMessage(msg, state)
	raw, ok := state.GetMetadata(usageStateKey)
	if !ok {
		return
	}
	usage, ok := raw.(Usage)
	if !ok {
		return
	}
	if msg.Metadata == nil {
		msg.Metadata = map[string]any{}
	}
	msg.Metadata[MetadataKey] = map[string]any{"usage": usage.metadata()}
}
