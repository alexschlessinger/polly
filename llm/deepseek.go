package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/adapters"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/packages/respjson"
)

const defaultDeepSeekBaseURL = "https://api.deepseek.com"

var _ LLM = (*DeepSeekClient)(nil)

// DeepSeekClient talks to DeepSeek's OpenAI-compatible Chat Completions API.
//
// DeepSeek's reasoning models (e.g. v4-pro) emit a non-standard `reasoning_content`
// field in streamed deltas and require it to be echoed back on the assistant turn
// of subsequent requests. This client captures incoming reasoning_content into
// ChatMessage.Reasoning and replays it on outgoing assistant messages via sjson
// patches; the typed openai-go SDK has no field for it.
type DeepSeekClient struct {
	client  openai.Client
	baseURL string
}

func NewDeepSeekClient(apiKey, baseURL string) *DeepSeekClient {
	effectiveBaseURL := strings.TrimSpace(baseURL)
	if effectiveBaseURL == "" {
		effectiveBaseURL = defaultDeepSeekBaseURL
	}

	client := openai.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(effectiveBaseURL),
	)

	return &DeepSeekClient{
		client:  client,
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
	reasoningOpts := buildDeepSeekReasoningReplayOptions(req.Messages)
	isStreaming := req.Stream == nil || *req.Stream
	slog.Debug("deepseek_completion_started", "stream", isStreaming, "base_url", d.baseURL, "reasoning_replay_count", len(reasoningOpts))

	if isStreaming {
		return d.handleStreaming(timeout, params, reasoningOpts, streamCore)
	}
	return d.handleNonStreaming(timeout, params, reasoningOpts, streamCore)
}

func (d DeepSeekClient) handleStreaming(ctx context.Context, params openai.ChatCompletionNewParams, extraOpts []option.RequestOption, streamCore *streaming.StreamingCore) error {
	params.StreamOptions = openai.ChatCompletionStreamOptionsParam{
		IncludeUsage: param.NewOpt(true),
	}

	stream := d.client.Chat.Completions.NewStreaming(ctx, params, extraOpts...)
	defer stream.Close()

	for stream.Next() {
		chunk := stream.Current()
		if err := streamCore.ProcessChunk(&chunk); err != nil {
			return err
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if delta.Content != "" {
			streamCore.EmitContent(delta.Content)
		}
		if reasoning := extractReasoningContentDelta(delta); reasoning != "" {
			streamCore.EmitReasoning(reasoning)
		}
	}

	if err := stream.Err(); err != nil {
		slog.Debug("deepseek_stream_error", "error", err)
		return fmt.Errorf("error during deepseek streaming: %w", err)
	}

	streamCore.Complete()
	return nil
}

func (d DeepSeekClient) handleNonStreaming(ctx context.Context, params openai.ChatCompletionNewParams, extraOpts []option.RequestOption, streamCore *streaming.StreamingCore) error {
	resp, err := d.client.Chat.Completions.New(ctx, params, extraOpts...)
	if err != nil {
		slog.Debug("deepseek_completion_failed", "error", err)
		return fmt.Errorf("failed to create deepseek completion: %w", err)
	}

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		if reasoning := extractReasoningContentFromMessage(choice.Message); reasoning != "" {
			streamCore.EmitReasoning(reasoning)
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

	if resp.JSON.Usage.Valid() {
		streamCore.SetTokenUsage(int(resp.Usage.PromptTokens), int(resp.Usage.CompletionTokens))
	}

	streamCore.Complete()
	return nil
}

// buildDeepSeekReasoningReplayOptions builds sjson WithJSONSet options that inject
// `reasoning_content` onto each assistant message in the outgoing request body.
// DeepSeek's reasoning models reject the request with HTTP 400 if reasoning_content
// from a prior assistant turn is omitted on the follow-up.
//
// Indices map 1:1 to req.Messages because messagesToChatCompletionParams preserves
// order without filtering.
func buildDeepSeekReasoningReplayOptions(msgs []messages.ChatMessage) []option.RequestOption {
	var opts []option.RequestOption
	for i, msg := range msgs {
		if msg.Role != messages.MessageRoleAssistant || msg.Reasoning == "" {
			continue
		}
		path := fmt.Sprintf("messages.%d.reasoning_content", i)
		opts = append(opts, option.WithJSONSet(path, msg.Reasoning))
	}
	return opts
}

// extractReasoningContentDelta pulls the `reasoning_content` string fragment from
// a streamed chunk delta. The openai-go SDK doesn't have a typed field for it, so
// it lands in the JSON.ExtraFields map. The SDK marks unknown fields as "invalid"
// status (since it can't decode into a typed destination), so Field.Valid() is
// always false here — but Field.Raw() still carries the raw JSON value.
func extractReasoningContentDelta(delta openai.ChatCompletionChunkChoiceDelta) string {
	return extraStringField(delta.JSON.ExtraFields, "reasoning_content")
}

// extractReasoningContentFromMessage pulls the full `reasoning_content` string
// from a non-streaming response message.
func extractReasoningContentFromMessage(msg openai.ChatCompletionMessage) string {
	return extraStringField(msg.JSON.ExtraFields, "reasoning_content")
}

func extraStringField(extras map[string]respjson.Field, key string) string {
	field, ok := extras[key]
	if !ok {
		return ""
	}
	raw := field.Raw()
	if raw == "" || raw == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return ""
	}
	return s
}
