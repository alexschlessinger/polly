package contract

import (
	"context"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
)

// Collect calls ChatCompletionStream on the given LLM client and returns the final content string.
func Collect(ctx context.Context, client LLM, req *CompletionRequest) (string, error) {
	events := client.ChatCompletionStream(ctx, req, &SimpleProcessor{})
	for event := range events {
		switch event.Type {
		case messages.EventTypeComplete:
			return event.Message.GetContent(), nil
		case messages.EventTypeError:
			return "", event.Error
		}
	}
	return "", fmt.Errorf("no response from LLM")
}

// SimpleProcessor is a basic implementation of EventStreamProcessor
type SimpleProcessor struct{}

func (s *SimpleProcessor) ProcessMessagesToEvents(msgChan <-chan messages.ChatMessage) <-chan *messages.StreamEvent {
	eventChan := make(chan *messages.StreamEvent)

	go func() {
		defer close(eventChan)

		var fullContent strings.Builder
		var lastMessage messages.ChatMessage
		received := false

		for msg := range msgChan {
			received = true
			lastMessage = msg

			if msg.IsError() {
				eventChan <- &messages.StreamEvent{
					Type:  messages.EventTypeError,
					Error: msg.GetError(),
				}
				return
			}

			if msg.Content != "" {
				fullContent.WriteString(msg.Content)
				eventChan <- &messages.StreamEvent{
					Type:    messages.EventTypeContent,
					Content: msg.Content,
				}
			}

			if len(msg.ToolCalls) > 0 {
				eventChan <- &messages.StreamEvent{
					Type:    messages.EventTypeToolCall,
					Message: &msg,
				}
			}
		}

		// Send complete event with full message. A legitimately empty
		// completion (a refusal, a content-filter stop) still completes;
		// its stop reason is the caller's only signal.
		if received {
			lastMessage.Content = fullContent.String()
			eventChan <- &messages.StreamEvent{
				Type:    messages.EventTypeComplete,
				Message: &lastMessage,
			}
		}
	}()

	return eventChan
}
