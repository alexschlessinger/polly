package messages

import "context"

// EventProcessor defines an interface for processing stream events.
// Implementations can handle events differently based on the context
// (e.g., CLI mode vs other consumers).
type EventProcessor interface {
	// OnReasoning is called when reasoning/thinking content is received
	OnReasoning(content string, totalLength int)

	// OnContent is called when regular content is streamed
	OnContent(content string, firstChunk bool)

	// OnToolCall is called when a tool call event is received
	OnToolCall(toolCall ChatMessageToolCall)

	// OnComplete is called when the stream is complete with the full message
	OnComplete(message *ChatMessage)

	// OnError is called when an error occurs during streaming
	OnError(err error)

	// GetResponse returns the accumulated response message
	GetResponse() ChatMessage
}

// BaseEventProcessor provides common functionality for event processors
type BaseEventProcessor struct {
	response        ChatMessage
	responseContent string
	reasoningLength int
}

// GetResponse returns the accumulated response message
func (p *BaseEventProcessor) GetResponse() ChatMessage {
	return p.response
}

// StreamHandler provides a unified way to process events from an event channel
func ProcessEventStream(ctx context.Context, eventChan <-chan *StreamEvent, processor EventProcessor) ChatMessage {
	var responseText string
	var reasoningLength int
	var firstChunk = true

	for event := range eventChan {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			return ChatMessage{}
		default:
		}

		switch event.Type {
		case EventTypeReasoning:
			reasoningLength += len(event.Content)
			processor.OnReasoning(event.Content, reasoningLength)

		case EventTypeContent:
			processor.OnContent(event.Content, firstChunk)
			if firstChunk && event.Content != "" {
				firstChunk = false
			}
			responseText += event.Content

		case EventTypeToolCall:
			// Tool calls will be handled in OnComplete when we have the full message
			// This event can be used for real-time updates if needed

		case EventTypeComplete:
			fullResponse := *event.Message
			// Use streamed content if available
			if responseText != "" {
				fullResponse.Content = responseText
			}
			processor.OnComplete(&fullResponse)
			return fullResponse

		case EventTypeError:
			processor.OnError(event.Error)
			return ChatMessage{
				Role:    MessageRoleAssistant,
				Content: event.Error.Error(),
			}
		}
	}

	return processor.GetResponse()
}
