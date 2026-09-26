package openai

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

// StreamResponses runs a streaming Responses API call and feeds the events
// into streamCore. Gateways that speak the Responses dialect prepare their
// own bodies, then share this handling so reasoning, tool calls, usage, and
// terminal events stay consistent. Reasoning summaries stream as reasoning,
// each part a paragraph of its own; raw reasoning text is emitted once at
// the end only when no summary came.
func StreamResponses(ctx context.Context, client *Client, params ResponsesBody, streamCore *streaming.StreamingCore) error {
	var rawReasoningFallback strings.Builder
	summarySeen := false
	// partEnded marks a summary part that closed, so the next part's text
	// starts on a paragraph of its own.
	partEnded := false

	for event, err := range client.StreamResponse(ctx, params) {
		if err != nil {
			slog.Debug("openai_responses_stream_error", "error", err)
			return fmt.Errorf("error during responses streaming: %w", err)
		}
		if err := streamCore.ProcessChunk(event); err != nil {
			return err
		}

		switch event.Type {
		case "response.output_text.delta", "response.content_part.delta":
			// content_part.delta is not an OpenAI event; a gateway documents
			// it for text, and OpenAI's content_part events carry no delta.
			if event.Delta != "" {
				streamCore.EmitContent(string(event.Delta))
			}
		case "response.refusal.delta":
			if event.Delta != "" {
				streamCore.EmitContent(string(event.Delta))
			}
		case "response.reasoning_summary_text.delta":
			if event.Delta != "" {
				if partEnded {
					streamCore.EmitReasoning("\n\n")
					partEnded = false
				}
				summarySeen = true
				streamCore.GetState().SetMetadata(responsesReasoningSummaryKey, true)
				streamCore.EmitReasoning(string(event.Delta))
			}
		case "response.reasoning_summary_text.done", "response.reasoning_summary_part.done":
			partEnded = summarySeen
		case "response.reasoning_text.delta":
			if !summarySeen && event.Delta != "" {
				rawReasoningFallback.WriteString(string(event.Delta))
			}
		}
	}

	if !summarySeen && rawReasoningFallback.Len() > 0 {
		streamCore.EmitReasoning(rawReasoningFallback.String())
	}

	streamCore.CompleteStream()
	return nil
}

// CompleteResponses runs a non-streaming Responses API call and feeds the
// result into streamCore. The whole response is offered to the adapter
// first, so gateways can record attribution and items they need to replay.
func CompleteResponses(ctx context.Context, client *Client, params ResponsesBody, streamCore *streaming.StreamingCore) error {
	resp, err := client.CreateResponse(ctx, params)
	if err != nil {
		slog.Debug("openai_responses_failed", "error", err)
		return fmt.Errorf("failed to create response: %w", err)
	}
	if err := streamCore.ProcessChunk(resp); err != nil {
		return err
	}

	emitResponseOutput(resp, streamCore)

	if resp.Usage != nil {
		streamCore.SetTokenUsage(int(resp.Usage.InputTokens), int(resp.Usage.OutputTokens))
		if read, write, reported := resp.Usage.PromptCacheUsage(); reported {
			streamCore.SetPromptCacheUsage(read, write)
		}
		applyReportedCost(streamCore, resp.Usage.Cost)
	}
	incompleteReason := ""
	if resp.IncompleteDetails != nil {
		incompleteReason = resp.IncompleteDetails.Reason
	}
	streamCore.SetStopReason(mapResponsesStopReason(resp.Status, incompleteReason, len(streamCore.GetState().GetToolCalls()) > 0))

	streamCore.Complete()
	return nil
}

func emitResponseOutput(resp *Response, streamCore *streaming.StreamingCore) {
	if resp == nil {
		return
	}
	// Reasoning arrives as parts, each a paragraph of its own.
	reasoningEmitted := false
	reasoning := func(text string) {
		if reasoningEmitted {
			streamCore.EmitReasoning("\n\n")
		}
		streamCore.EmitReasoning(text)
		reasoningEmitted = true
	}

	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, content := range item.Content {
				switch content.Type {
				case "output_text":
					if content.Text != "" {
						streamCore.EmitContent(content.Text)
					}
				case "refusal":
					if content.Refusal != "" {
						streamCore.EmitContent(content.Refusal)
					}
				}
			}
		case "reasoning":
			appendResponsesReasoningItem(streamCore.GetState(), &item)
			if len(item.Summary) > 0 {
				for _, summary := range item.Summary {
					if summary.Text != "" {
						reasoning(summary.Text)
					}
				}
				continue
			}
			for _, content := range item.Content {
				if content.Text != "" {
					reasoning(content.Text)
				}
			}
		case "function_call":
			streamCore.GetState().AddToolCall(messages.ChatMessageToolCall{
				ID:        responseToolCallID(item.CallID, item.ID),
				Name:      item.Name,
				Arguments: string(item.Arguments),
			})
		}
	}
}
