package llm

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/alexschlessinger/pollytool/llm/adapters"
	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

// OpenAI-compatible providers prepare their own requests, then share response
// handling so reasoning, tool calls, usage, and terminal events stay consistent.
func streamChatCompletion(ctx context.Context, client *openai.Client, params *openai.ChatCompletionRequest, streamCore *streaming.StreamingCore) error {
	for chunk, err := range client.StreamChatCompletion(ctx, params) {
		if err != nil {
			slog.Debug("chat_completion_stream_error", "error", err)
			return fmt.Errorf("error during chat completions streaming: %w", err)
		}
		if err := streamCore.ProcessChunk(chunk); err != nil {
			return err
		}

		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		// Reasoning first: it precedes the answer it produced, and emitting it
		// after would invert that order for anything displaying the stream.
		if reasoning := delta.ReasoningText(); reasoning != "" {
			streamCore.EmitReasoning(reasoning)
		}
		if delta.Content != "" {
			streamCore.EmitContent(delta.Content)
		}
	}

	streamCore.CompleteStream()
	return nil
}

func completeChatCompletion(ctx context.Context, client *openai.Client, params *openai.ChatCompletionRequest, streamCore *streaming.StreamingCore) error {
	resp, err := client.CreateChatCompletion(ctx, params)
	if err != nil {
		slog.Debug("chat_completion_failed", "error", err)
		return fmt.Errorf("failed to create chat completion: %w", err)
	}

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		if reasoning := choice.Message.ReasoningText(); reasoning != "" {
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

	if resp.Usage != nil {
		streamCore.SetTokenUsage(int(resp.Usage.PromptTokens), int(resp.Usage.CompletionTokens))
		if read, write, reported := resp.Usage.PromptCacheUsage(); reported {
			streamCore.SetPromptCacheUsage(read, write)
		}
	}

	streamCore.Complete()
	return nil
}
