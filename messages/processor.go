package messages

import (
	"encoding/json"
	"log"

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

		var accumulatedContent string
		var accumulatedReasoning string
		var lastMessageWithToolCalls *ChatMessage
		var lastMessageMetadata map[string]any

		for msg := range msgChan {
			// If there's reasoning, accumulate it and emit as reasoning event
			if msg.Reasoning != "" {
				accumulatedReasoning += msg.Reasoning
				eventChan <- &StreamEvent{
					Type:    EventTypeReasoning,
					Content: msg.Reasoning,
				}
			}

			// If there's content, emit it as a content event
			// This ensures content is always available for streaming
			if msg.Content != "" {
				accumulatedContent += msg.Content
				eventChan <- &StreamEvent{
					Type:    EventTypeContent,
					Content: msg.Content,
				}
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
						log.Printf("processMessagesToEvents: failed to parse tool call arguments: %v", err)
					}
				}
			}
		}

		// At the end, emit a complete event with the full message
		completeMsg := ChatMessage{
			Role:      MessageRoleAssistant,
			Content:   accumulatedContent,
			Reasoning: accumulatedReasoning,
			Metadata:  lastMessageMetadata,
		}

		// If we had tool calls, include them in the complete message
		if lastMessageWithToolCalls != nil {
			completeMsg.ToolCalls = lastMessageWithToolCalls.ToolCalls
		}

		eventChan <- &StreamEvent{
			Type:    EventTypeComplete,
			Message: &completeMsg,
		}
	}()

	return eventChan
}
