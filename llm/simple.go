package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/pkdindustries/pollytool/messages"
	"github.com/pkdindustries/pollytool/tools"
)

// getDefaultClient creates a MultiPass client with API keys from environment
func getDefaultClient() LLM {
	apiKeys := map[string]string{
		"openai":    os.Getenv("POLLYTOOL_OPENAIKEY"),
		"anthropic": os.Getenv("POLLYTOOL_ANTHROPICKEY"),
		"gemini":    os.Getenv("POLLYTOOL_GEMINIKEY"),
		"ollama":    os.Getenv("POLLYTOOL_OLLAMAKEY"),
	}
	return NewMultiPass(apiKeys)
}

// QuickComplete performs a simple one-shot completion with minimal configuration
func QuickComplete(ctx context.Context, model, prompt string, maxTokens int) (string, error) {
	client := getDefaultClient()

	req := &CompletionRequest{
		Model: model,
		Messages: []messages.ChatMessage{
			{
				Role:    messages.MessageRoleUser,
				Content: prompt,
			},
		},
		Temperature: 1,
		MaxTokens:   maxTokens,
		Timeout:     120 * time.Second,
	}

	processor := &SimpleProcessor{}
	eventChan := client.ChatCompletionStream(ctx, req, processor)

	var result string
	for event := range eventChan {
		switch event.Type {
		case messages.EventTypeContent:
			result += event.Content
		case messages.EventTypeError:
			return "", event.Error
		}
	}
	return result, nil
}

// StreamComplete performs a streaming completion with a callback for each chunk
func StreamComplete(ctx context.Context, model, prompt string, maxTokens int, onChunk func(string)) error {
	client := getDefaultClient()

	req := &CompletionRequest{
		Model: model,
		Messages: []messages.ChatMessage{
			{
				Role:    messages.MessageRoleUser,
				Content: prompt,
			},
		},
		Temperature: 1,
		MaxTokens:   maxTokens,
		Timeout:     120 * time.Second,
	}

	processor := &SimpleProcessor{}
	eventChan := client.ChatCompletionStream(ctx, req, processor)

	for event := range eventChan {
		switch event.Type {
		case messages.EventTypeContent:
			if onChunk != nil {
				onChunk(event.Content)
			}
		case messages.EventTypeError:
			return event.Error
		}
	}
	return nil
}

// ChatWithHistory performs a completion with conversation history
func ChatWithHistory(ctx context.Context, model string, history []messages.ChatMessage, newMessage string, maxTokens int) (*messages.ChatMessage, error) {
	client := getDefaultClient()

	// Add the new message to history
	allMessages := append(history, messages.ChatMessage{
		Role:    messages.MessageRoleUser,
		Content: newMessage,
	})

	req := &CompletionRequest{
		Model:       model,
		Messages:    allMessages,
		Temperature: 1,
		MaxTokens:   maxTokens,
		Timeout:     120 * time.Second,
	}

	processor := &SimpleProcessor{}
	eventChan := client.ChatCompletionStream(ctx, req, processor)

	var response messages.ChatMessage
	for event := range eventChan {
		switch event.Type {
		case messages.EventTypeComplete:
			response = *event.Message
		case messages.EventTypeError:
			return nil, event.Error
		}
	}
	return &response, nil
}

// StructuredComplete performs a completion expecting a structured JSON response
func StructuredComplete(ctx context.Context, model, prompt string, schema *Schema, maxTokens int, result interface{}) error {
	client := getDefaultClient()

	req := &CompletionRequest{
		Model: model,
		Messages: []messages.ChatMessage{
			{
				Role:    messages.MessageRoleUser,
				Content: prompt,
			},
		},
		ResponseSchema: schema,
		Temperature:    0.3, // Lower temperature for structured output
		MaxTokens:      maxTokens,
		Timeout:        120 * time.Second,
	}

	processor := &SimpleProcessor{}
	eventChan := client.ChatCompletionStream(ctx, req, processor)

	var content string
	for event := range eventChan {
		switch event.Type {
		case messages.EventTypeContent:
			content += event.Content
		case messages.EventTypeComplete:
			// Try to unmarshal the response into the result
			if result != nil && content != "" {
				return json.Unmarshal([]byte(content), result)
			}
		case messages.EventTypeError:
			return event.Error
		}
	}

	// If we have content but haven't unmarshaled yet, try now
	if result != nil && content != "" {
		return json.Unmarshal([]byte(content), result)
	}

	return nil
}

// CompletionBuilder provides a fluent interface for building completion requests
type CompletionBuilder struct {
	req *CompletionRequest
}

// NewCompletionBuilder creates a new builder with defaults
func NewCompletionBuilder(model string) *CompletionBuilder {
	return &CompletionBuilder{
		req: &CompletionRequest{
			Model:       model,
			Messages:    []messages.ChatMessage{},
			Temperature: 1,
			MaxTokens:   2000,
			Timeout:     120 * time.Second,
		},
	}
}

// WithSystemPrompt adds a system message
func (b *CompletionBuilder) WithSystemPrompt(prompt string) *CompletionBuilder {
	b.req.Messages = append([]messages.ChatMessage{
		{
			Role:    messages.MessageRoleSystem,
			Content: prompt,
		},
	}, b.req.Messages...)
	return b
}

// WithUserMessage adds a user message
func (b *CompletionBuilder) WithUserMessage(content string) *CompletionBuilder {
	b.req.Messages = append(b.req.Messages, messages.ChatMessage{
		Role:    messages.MessageRoleUser,
		Content: content,
	})
	return b
}

// WithAssistantMessage adds an assistant message (for conversation history)
func (b *CompletionBuilder) WithAssistantMessage(content string) *CompletionBuilder {
	b.req.Messages = append(b.req.Messages, messages.ChatMessage{
		Role:    messages.MessageRoleAssistant,
		Content: content,
	})
	return b
}

// WithTemperature sets the temperature
func (b *CompletionBuilder) WithTemperature(temp float32) *CompletionBuilder {
	b.req.Temperature = temp
	return b
}

// WithMaxTokens sets the max tokens
func (b *CompletionBuilder) WithMaxTokens(tokens int) *CompletionBuilder {
	b.req.MaxTokens = tokens
	return b
}

// WithTimeout sets the timeout
func (b *CompletionBuilder) WithTimeout(timeout time.Duration) *CompletionBuilder {
	b.req.Timeout = timeout
	return b
}

// WithTools adds tools for function calling
func (b *CompletionBuilder) WithTools(tools []tools.Tool) *CompletionBuilder {
	b.req.Tools = tools
	return b
}

// WithSchema adds a response schema for structured output
func (b *CompletionBuilder) WithSchema(schema *Schema) *CompletionBuilder {
	b.req.ResponseSchema = schema
	return b
}

// WithHistory adds conversation history
func (b *CompletionBuilder) WithHistory(history []messages.ChatMessage) *CompletionBuilder {
	// Prepend history before any messages already added
	b.req.Messages = append(history, b.req.Messages...)
	return b
}

// Build returns the built CompletionRequest
func (b *CompletionBuilder) Build() *CompletionRequest {
	return b.req
}

// Execute runs the completion and returns the result
func (b *CompletionBuilder) Execute(ctx context.Context, client LLM) (string, error) {
	processor := &SimpleProcessor{}
	eventChan := client.ChatCompletionStream(ctx, b.req, processor)

	var result string
	for event := range eventChan {
		switch event.Type {
		case messages.EventTypeContent:
			result += event.Content
		case messages.EventTypeError:
			return "", event.Error
		}
	}
	return result, nil
}

// ExecuteStreaming runs the completion with streaming callback
func (b *CompletionBuilder) ExecuteStreaming(ctx context.Context, client LLM, onChunk func(string)) error {
	processor := &SimpleProcessor{}
	eventChan := client.ChatCompletionStream(ctx, b.req, processor)

	for event := range eventChan {
		switch event.Type {
		case messages.EventTypeContent:
			if onChunk != nil {
				onChunk(event.Content)
			}
		case messages.EventTypeError:
			return event.Error
		}
	}
	return nil
}

// ExecuteWithTools runs the completion and handles tool calls automatically
func (b *CompletionBuilder) ExecuteWithTools(ctx context.Context, client LLM, toolRegistry *tools.ToolRegistry) (*messages.ChatMessage, error) {
	processor := &SimpleProcessor{}

	// Add tools to request if not already added
	if len(b.req.Tools) == 0 && toolRegistry != nil {
		b.req.Tools = toolRegistry.All()
	}

	eventChan := client.ChatCompletionStream(ctx, b.req, processor)

	var response *messages.ChatMessage
	for event := range eventChan {
		switch event.Type {
		case messages.EventTypeComplete:
			response = event.Message

			// Handle tool calls if present
			if len(response.ToolCalls) > 0 && toolRegistry != nil {
				for _, toolCall := range response.ToolCalls {
					// Parse arguments
					var args map[string]any
					if err := json.Unmarshal([]byte(toolCall.Arguments), &args); err != nil {
						return nil, fmt.Errorf("failed to parse tool arguments: %w", err)
					}

					// Get and execute tool
					tool, exists := toolRegistry.Get(toolCall.Name)
					if !exists {
						return nil, fmt.Errorf("tool not found: %s", toolCall.Name)
					}

					result, err := tool.Execute(ctx, args)
					if err != nil {
						result = fmt.Sprintf("Error executing tool: %v", err)
					}

					// Add tool result to messages
					b.req.Messages = append(b.req.Messages, *response)
					b.req.Messages = append(b.req.Messages, messages.ChatMessage{
						Role:       messages.MessageRoleTool,
						Content:    result,
						ToolCallID: toolCall.ID,
					})
				}

				// Continue conversation with tool results
				return b.ExecuteWithTools(ctx, client, toolRegistry)
			}

		case messages.EventTypeError:
			return nil, event.Error
		}
	}

	return response, nil
}

// SimpleProcessor is a basic implementation of EventStreamProcessor
type SimpleProcessor struct{}

func (s *SimpleProcessor) ProcessMessagesToEvents(msgChan <-chan messages.ChatMessage) <-chan *messages.StreamEvent {
	eventChan := make(chan *messages.StreamEvent)

	go func() {
		defer close(eventChan)

		var fullContent string
		var lastMessage messages.ChatMessage

		for msg := range msgChan {
			lastMessage = msg

			if msg.Content != "" {
				fullContent += msg.Content
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

		// Send complete event with full message
		if fullContent != "" || len(lastMessage.ToolCalls) > 0 {
			lastMessage.Content = fullContent
			eventChan <- &messages.StreamEvent{
				Type:    messages.EventTypeComplete,
				Message: &lastMessage,
			}
		}
	}()

	return eventChan
}
