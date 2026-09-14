package messages

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/alexschlessinger/pollytool/tools"
)

// StreamProcessor is a simple processor for message streams from LLMs
// It converts messages into a unified event stream
type StreamProcessor struct{}

// NewStreamProcessor creates a new stream processor
func NewStreamProcessor() *StreamProcessor {
	return &StreamProcessor{}
}

// ProcessMessagesToEvents converts a stream of ChatMessages into StreamEvents
// This is the new event-based streaming approach
func (p *StreamProcessor) ProcessMessagesToEvents(msgChan <-chan ChatMessage) <-chan *StreamEvent {
	eventChan := make(chan *StreamEvent, 10)

	go func() {
		defer close(eventChan)

		var accumulatedContent strings.Builder
		var accumulatedReasoning strings.Builder
		var toolCalls []ChatMessageToolCall
		var lastMessageMetadata map[string]any
		var stopReason StopReason

		for msg := range msgChan {
			// Terminal error messages should emit an explicit error event and stop.
			if msg.IsError() {
				err := msg.GetError()
				if err == nil {
					err = fmt.Errorf("unknown stream error")
				}
				eventChan <- &StreamEvent{Type: EventTypeError, Error: err}
				return
			}

			// Capture stop reason if set (usually on the final message)
			if msg.StopReason != "" {
				stopReason = msg.StopReason
			}
			// If there's reasoning, accumulate it and emit as reasoning event
			if msg.Reasoning != "" {
				accumulatedReasoning.WriteString(msg.Reasoning)
				eventChan <- &StreamEvent{
					Type:    EventTypeReasoning,
					Content: msg.Reasoning,
				}
			}

			// If there's content, emit it as a content event
			// This ensures content is always available for streaming
			if msg.Content != "" {
				accumulatedContent.WriteString(msg.Content)
				eventChan <- &StreamEvent{
					Type:    EventTypeContent,
					Content: msg.Content,
				}
			}

			// Save metadata if present
			if len(msg.Metadata) > 0 {
				lastMessageMetadata = msg.Metadata
			}

			// If this message has tool calls, keep them for the complete event
			if len(msg.ToolCalls) > 0 {
				toolCalls = msg.ToolCalls

				// Emit individual tool call events
				for _, toolCall := range msg.ToolCalls {
					var args map[string]any
					if err := json.Unmarshal([]byte(toolCall.Arguments), &args); err != nil {
						slog.Warn("processor_tool_call_parse_failed", "error", err)
						continue
					}
					eventChan <- &StreamEvent{
						Type: EventTypeToolCall,
						ToolCall: &tools.ToolCall{
							ID:   toolCall.ID,
							Name: toolCall.Name,
							Args: args,
						},
					}
				}
			}
		}

		// At the end, emit a complete event with the full message
		// For history purposes, we need the complete content, but streaming clients
		// should ignore this to avoid duplication
		slog.Debug("processor_event_type_complete_created",
			"accumulated_content_len", accumulatedContent.Len(),
			"accumulated_reasoning_len", accumulatedReasoning.Len(),
			"num_tools", len(toolCalls),
			"stop_reason", stopReason,
		)

		eventChan <- &StreamEvent{
			Type: EventTypeComplete,
			Message: &ChatMessage{
				Role:       MessageRoleAssistant,
				Content:    accumulatedContent.String(),
				Reasoning:  accumulatedReasoning.String(),
				ToolCalls:  toolCalls,
				Metadata:   lastMessageMetadata,
				StopReason: stopReason,
			},
		}
	}()

	return eventChan
}
