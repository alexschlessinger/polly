package openai

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

// StreamResponses runs a streaming Responses API call and feeds the events
// into streamCore. Gateways that speak the Responses dialect prepare their
// own bodies, then share this handling so reasoning, tool calls, usage, and
// terminal events stay consistent. Reasoning streams immediately, each part
// a paragraph of its own, with snapshots supplying only missing text.
func StreamResponses(ctx context.Context, client *Client, params ResponsesBody, streamCore *streaming.StreamingCore) error {
	reasoning := newResponsesReasoningStream(streamCore.EmitReasoning)
	textPhases := map[int]messages.AssistantPhase{}

	for event, err := range client.StreamResponse(ctx, params) {
		if err != nil {
			slog.Debug("openai_responses_stream_error", "error", err)
			return fmt.Errorf("error during responses streaming: %w", err)
		}
		if err := streamCore.ProcessChunk(event); err != nil {
			return err
		}

		switch event.Type {
		case "response.output_item.added", "response.output_item.done":
			if event.Item != nil && event.Item.Type == "message" {
				textPhases[int(event.OutputIndex)] = event.Item.Phase
			}
		case "response.output_text.delta", "response.content_part.delta":
			// content_part.delta is not an OpenAI event; a gateway documents
			// it for text, and OpenAI's content_part events carry no delta.
			if event.Delta != "" {
				streamCore.EmitAssistantText(messages.AssistantText{ID: responseTextID(int(event.OutputIndex)), Phase: textPhases[int(event.OutputIndex)], Text: string(event.Delta)})
			}
		case "response.refusal.delta":
			if event.Delta != "" {
				streamCore.EmitAssistantText(messages.AssistantText{ID: responseTextID(int(event.OutputIndex)), Phase: textPhases[int(event.OutputIndex)], Text: string(event.Delta)})
			}
		}
		reasoning.process(event)
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
	reasoning := newResponsesReasoningStream(streamCore.EmitReasoning)

	for index, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, content := range item.Content {
				switch content.Type {
				case "output_text":
					if content.Text != "" {
						streamCore.EmitAssistantText(messages.AssistantText{ID: responseTextID(index), Phase: item.Phase, Text: content.Text})
					}
				case "refusal":
					if content.Refusal != "" {
						streamCore.EmitAssistantText(messages.AssistantText{ID: responseTextID(index), Phase: item.Phase, Text: content.Refusal})
					}
				}
			}
		case "reasoning":
			appendResponsesReasoningItem(streamCore.GetState(), &item)
			reasoning.snapshot(index, &item)
		}
	}
}

func responseTextID(index int) string { return fmt.Sprintf("output_%d", index) }
