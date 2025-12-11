package messages

import (
	"encoding/json"
	"fmt"

	"github.com/alexschlessinger/pollytool/tools"
	"go.uber.org/zap"
)

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

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

	// Generate a unique ID for this processor instance for debugging
	processorID := fmt.Sprintf("%p", p)

	go func() {
		defer func() {
			close(eventChan)
		}()

		var accumulatedContent string
		var accumulatedReasoning string
		var lastMessageWithToolCalls *ChatMessage
		var lastMessageMetadata map[string]any
		var stopReason StopReason

		for msg := range msgChan {
			zap.S().Debugw("processor_message_received",
				"processor_id", processorID,
				"content_len", len(msg.Content),
				"has_tool_calls", len(msg.ToolCalls) > 0,
			)
			// Capture stop reason if set (usually on the final message)
			if msg.StopReason != "" {
				stopReason = msg.StopReason
			}
			// If there's reasoning, accumulate it and emit as reasoning event
			if msg.Reasoning != "" {
				zap.S().Debugw("processor_reasoning_chunk_received",
					"processor_id", processorID,
					"chunk_len", len(msg.Reasoning),
					"accumulated_len", len(accumulatedReasoning),
					"preview", msg.Reasoning[:min(50, len(msg.Reasoning))],
				)
				accumulatedReasoning += msg.Reasoning
				eventChan <- &StreamEvent{
					Type:    EventTypeReasoning,
					Content: msg.Reasoning,
				}
			}

			// If there's content, emit it as a content event
			// This ensures content is always available for streaming
			if msg.Content != "" {
				zap.S().Debugw("processor_content_chunk_received",
					"processor_id", processorID,
					"chunk_len", len(msg.Content),
					"accumulated_len_before", len(accumulatedContent),
					"accumulated_len_after", len(accumulatedContent)+len(msg.Content),
					"preview", msg.Content[:min(50, len(msg.Content))],
				)
				accumulatedContent += msg.Content
				eventChan <- &StreamEvent{
					Type:    EventTypeContent,
					Content: msg.Content,
				}
				zap.S().Debugw("processor_event_type_content_sent",
					"content", msg.Content,
				)
			}

			// Save metadata if present
			if len(msg.Metadata) > 0 {
				lastMessageMetadata = msg.Metadata
			}

			// If this message has tool calls, save it for the complete event
			if len(msg.ToolCalls) > 0 {
				lastMessageWithToolCalls = &msg

				// Emit individual tool call events
				for _, toolCall := range msg.ToolCalls {
					var args map[string]any
					if err := json.Unmarshal([]byte(toolCall.Arguments), &args); err == nil {
						tc := &tools.ToolCall{
							ID:   toolCall.ID,
							Name: toolCall.Name,
							Args: args,
						}
						eventChan <- &StreamEvent{
							Type:     EventTypeToolCall,
							ToolCall: tc,
						}
					} else {
						zap.S().Debugw("processor_tool_call_parse_failed", "error", err)
					}
				}
			}
		}

		// At the end, emit a complete event with the full message
		// For history purposes, we need the complete content, but streaming clients
		// should ignore this to avoid duplication
		zap.S().Debugw("processor_event_type_complete_created",
			"processor_id", processorID,
			"accumulated_content_len", len(accumulatedContent),
			"accumulated_reasoning_len", len(accumulatedReasoning),
			"has_tool_calls", lastMessageWithToolCalls != nil,
			"stop_reason", stopReason,
			"accumulated_content", accumulatedContent,
		)

		completeMsg := ChatMessage{
			Role:       MessageRoleAssistant,
			Content:    accumulatedContent,
			Reasoning:  accumulatedReasoning,
			Metadata:   lastMessageMetadata,
			StopReason: stopReason,
		}

		// If we had tool calls, include them in the complete message
		if lastMessageWithToolCalls != nil {
			completeMsg.ToolCalls = lastMessageWithToolCalls.ToolCalls
			zap.S().Debugw("processor_event_type_complete_created",
				"num_tools", len(lastMessageWithToolCalls.ToolCalls),
			)
		}

		eventChan <- &StreamEvent{
			Type:    EventTypeComplete,
			Message: &completeMsg,
		}
		zap.S().Debugw("processor_event_type_complete_sent",
			"content_in_complete", completeMsg.Content,
		)
	}()

	return eventChan
}
