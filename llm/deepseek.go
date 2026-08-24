package llm

import (
	"context"
	"fmt"
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
	return runStream(ctx, processor, adapters.NewOpenAIAdapter(), func(streamCore *streaming.StreamingCore) {
		if err := d.streamCompletion(ctx, req, streamCore); err != nil {
			streamCore.EmitError(err)
		}
	})
}

func (d DeepSeekClient) streamCompletion(ctx context.Context, req *CompletionRequest, streamCore *streaming.StreamingCore) error {
	timeout, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()

	params := buildChatCompletionRequestParams(req)
	replayed := applyDeepSeekReasoningReplay(params, req.Messages)
	isStreaming := req.Stream == nil || *req.Stream
	slog.Debug("deepseek_completion_started", "stream", isStreaming, "base_url", d.baseURL, "reasoning_replay_count", replayed)

	if isStreaming {
		return d.handleStreaming(timeout, params, streamCore)
	}
	return d.handleNonStreaming(timeout, params, streamCore)
}

func (d DeepSeekClient) handleStreaming(ctx context.Context, params *openai.ChatCompletionRequest, streamCore *streaming.StreamingCore) error {
	for chunk, err := range d.client.StreamChatCompletion(ctx, params) {
		if err != nil {
			slog.Debug("deepseek_stream_error", "error", err)
			return fmt.Errorf("error during deepseek streaming: %w", err)
		}
		if err := streamCore.ProcessChunk(chunk); err != nil {
			return err
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if delta.Content != "" {
			streamCore.EmitContent(delta.Content)
		}
		if delta.ReasoningContent != "" {
			streamCore.EmitReasoning(delta.ReasoningContent)
		}
	}

	streamCore.Complete()
	return nil
}

func (d DeepSeekClient) handleNonStreaming(ctx context.Context, params *openai.ChatCompletionRequest, streamCore *streaming.StreamingCore) error {
	resp, err := d.client.CreateChatCompletion(ctx, params)
	if err != nil {
		slog.Debug("deepseek_completion_failed", "error", err)
		return fmt.Errorf("failed to create deepseek completion: %w", err)
	}

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		if choice.Message.ReasoningContent != "" {
			streamCore.EmitReasoning(choice.Message.ReasoningContent)
		}
		if choice.Message.Content != "" {
			streamCore.EmitContent(choice.Message.Content)
		}
		for _, toolCall := range choice.Message.ToolCalls {
			if toolCall.Type != "function" {
				continue
			}
			streamCore.GetState().AddToolCall(messages.ChatMessageToolCall{
				ID:        toolCall.ID,
				Name:      toolCall.Function.Name,
				Arguments: toolCall.Function.Arguments,
			})
		}
		streamCore.SetStopReason(adapters.MapOpenAIFinishReason(choice.FinishReason))
	}

	if resp.Usage != nil {
		streamCore.SetTokenUsage(int(resp.Usage.PromptTokens), int(resp.Usage.CompletionTokens))
	}

	streamCore.Complete()
	return nil
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
