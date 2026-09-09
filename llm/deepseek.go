package llm

import (
	"context"
	"log/slog"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/adapters"
	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

const defaultDeepSeekBaseURL = "https://api.deepseek.com"

var _ LLM = (*DeepSeekClient)(nil)

// DeepSeekClient talks to DeepSeek's OpenAI-compatible Chat Completions API.
//
// DeepSeek's reasoning models (e.g. v4-pro) emit a non-standard `reasoning_content`
// field in streamed deltas and require it to be echoed back on the assistant turn
// of subsequent requests. This client captures incoming reasoning_content into
// ChatMessage.Reasoning and replays it on outgoing assistant messages.
type DeepSeekClient struct {
	client  *openai.Client
	baseURL string
}

func NewDeepSeekClient(apiKey, baseURL string) *DeepSeekClient {
	effectiveBaseURL := strings.TrimSpace(baseURL)
	if effectiveBaseURL == "" {
		effectiveBaseURL = defaultDeepSeekBaseURL
	}

	return &DeepSeekClient{
		client:  openai.NewClient(apiKey, effectiveBaseURL),
		baseURL: effectiveBaseURL,
	}
}

func (d DeepSeekClient) ChatCompletionStream(ctx context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	return runStream(ctx, req.Timeout, req.Deadline, processor, adapters.NewOpenAIAdapter(), func(ctx context.Context, streamCore *streaming.StreamingCore) {
		if err := d.streamCompletion(ctx, req, streamCore); err != nil {
			streamCore.EmitError(err)
		}
	})
}

func (d DeepSeekClient) streamCompletion(ctx context.Context, req *CompletionRequest, streamCore *streaming.StreamingCore) error {
	params := buildChatCompletionRequestParams(req)
	replayed := applyDeepSeekReasoningReplay(params, req.Messages)
	isStreaming := req.IsStreaming()
	slog.Debug("deepseek_completion_started", "stream", isStreaming, "base_url", d.baseURL, "reasoning_replay_count", replayed)

	if isStreaming {
		return streamChatCompletion(ctx, d.client, params, streamCore)
	}
	return completeChatCompletion(ctx, d.client, params, streamCore)
}

// applyDeepSeekReasoningReplay copies each assistant message's captured
// reasoning onto the outgoing request as `reasoning_content` and returns how
// many messages were annotated. DeepSeek's reasoning models reject the request
// with HTTP 400 if reasoning_content from a prior assistant turn is omitted on
// the follow-up.
//
// Indices map 1:1 to msgs because messagesToChatCompletionParams preserves
// order without filtering.
func applyDeepSeekReasoningReplay(params *openai.ChatCompletionRequest, msgs []messages.ChatMessage) int {
	replayed := 0
	for i, msg := range msgs {
		if msg.Role != messages.MessageRoleAssistant || msg.Reasoning == "" || i >= len(params.Messages) {
			continue
		}
		params.Messages[i].ReasoningContent = msg.Reasoning
		replayed++
	}
	return replayed
}
