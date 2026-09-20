// Package deepseek talks to DeepSeek's OpenAI-compatible Chat Completions API.
package deepseek

import (
	"context"
	"log/slog"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

// DefaultBaseURL is DeepSeek's public API endpoint.
const DefaultBaseURL = "https://api.deepseek.com"

var _ contract.LLM = (*Provider)(nil)

// Provider talks to DeepSeek's OpenAI-compatible Chat Completions API.
//
// DeepSeek's reasoning models (e.g. v4-pro) emit a non-standard `reasoning_content`
// field in streamed deltas and require it to be echoed back on the assistant turn
// of subsequent requests. This provider captures incoming reasoning_content into
// ChatMessage.Reasoning and replays it on outgoing assistant messages.
type Provider struct {
	client  *openai.Client
	baseURL string
}

// NewProvider returns a provider for baseURL, or DefaultBaseURL when empty.
func NewProvider(apiKey, baseURL string) *Provider {
	effectiveBaseURL := strings.TrimSpace(baseURL)
	if effectiveBaseURL == "" {
		effectiveBaseURL = DefaultBaseURL
	}

	return &Provider{
		client:  openai.NewClient(apiKey, effectiveBaseURL),
		baseURL: effectiveBaseURL,
	}
}

func (d Provider) ChatCompletionStream(ctx context.Context, req *contract.CompletionRequest, processor contract.EventStreamProcessor) <-chan *messages.StreamEvent {
	return contract.RunStream(ctx, req.Timeout, req.Deadline, processor, openai.NewChatAdapter(), func(ctx context.Context, streamCore *streaming.StreamingCore) {
		if err := d.streamCompletion(ctx, req, streamCore); err != nil {
			streamCore.EmitError(err)
		}
	})
}

func (d Provider) streamCompletion(ctx context.Context, req *contract.CompletionRequest, streamCore *streaming.StreamingCore) error {
	params := openai.BuildChatCompletionRequest(req)
	replayed := openai.ReplayAssistantReasoning(params, req.Messages)
	isStreaming := req.IsStreaming()
	slog.Debug("deepseek_completion_started", "stream", isStreaming, "base_url", d.baseURL, "reasoning_replay_count", replayed)

	if isStreaming {
		return openai.StreamChat(ctx, d.client, params, streamCore)
	}
	return openai.CompleteChat(ctx, d.client, params, streamCore)
}
