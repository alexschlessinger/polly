package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools"
)

// GetDefaultClient creates a new MultiPass router with API keys from the environment.
func GetDefaultClient() LLM {
	apiKeys := map[string]string{
		"openai":      os.Getenv("POLLYTOOL_OPENAIKEY"),
		"anthropic":   os.Getenv("POLLYTOOL_ANTHROPICKEY"),
		"gemini":      os.Getenv("POLLYTOOL_GEMINIKEY"),
		"ollama":      os.Getenv("POLLYTOOL_OLLAMAKEY"),
		"huggingface": os.Getenv("POLLYTOOL_HUGGINGFACEKEY"),
	}
	return NewMultiPass(apiKeys)
}

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

// QuickComplete performs a simple one-shot completion with minimal configuration.
func QuickComplete(ctx context.Context, model, prompt string, maxTokens int) (string, error) {
	client := GetDefaultClient()

	req := &CompletionRequest{
		Model: model,
		Messages: []messages.ChatMessage{
			{
				Role:    messages.MessageRoleUser,
				Content: prompt,
			},
		},
		Temperature: Float32Ptr(1),
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

// StreamComplete performs a streaming completion with a callback for each chunk.
func StreamComplete(ctx context.Context, model, prompt string, maxTokens int, onChunk func(string)) error {
	client := GetDefaultClient()

	req := &CompletionRequest{
		Model: model,
		Messages: []messages.ChatMessage{
			{
				Role:    messages.MessageRoleUser,
				Content: prompt,
			},
		},
		Temperature: Float32Ptr(1),
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

// ChatWithHistory performs a completion with conversation history.
func ChatWithHistory(ctx context.Context, model string, history []messages.ChatMessage, newMessage string, maxTokens int) (*messages.ChatMessage, error) {
	client := GetDefaultClient()

	// Add the new message to history on a fresh slice, leaving the caller's
	// backing array untouched.
	allMessages := make([]messages.ChatMessage, 0, len(history)+1)
	allMessages = append(allMessages, history...)
	allMessages = append(allMessages, messages.ChatMessage{
		Role:    messages.MessageRoleUser,
		Content: newMessage,
	})

	req := &CompletionRequest{
		Model:       model,
		Messages:    allMessages,
		Temperature: Float32Ptr(1),
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

// StructuredComplete performs a completion expecting a structured JSON response.
func StructuredComplete(ctx context.Context, model, prompt string, schema *Schema, maxTokens int, result interface{}) error {
	client := GetDefaultClient()

	req := &CompletionRequest{
		Model: model,
		Messages: []messages.ChatMessage{
			{
				Role:    messages.MessageRoleUser,
				Content: prompt,
			},
		},
		ResponseSchema: schema,
		Temperature:    Float32Ptr(0.3), // Lower temperature for structured output
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
			if content == "" && event.Message != nil {
				content = event.Message.Content
			}
		case messages.EventTypeError:
			return event.Error
		}
	}

	// A reply with nothing to decode is a failure, not a success with an
	// untouched result: the caller asked for a value and got none.
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("structured completion returned no content")
	}
	if schema != nil {
		if err := schema.Validate(content); err != nil {
			return fmt.Errorf("structured completion: %w", err)
		}
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(content), result); err != nil {
		return fmt.Errorf("structured completion is not valid JSON: %w", err)
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
			Temperature: Float32Ptr(1),
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
	b.req.Temperature = &temp
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

// WithSkills sets a skill catalog for automatic system prompt augmentation
func (b *CompletionBuilder) WithSkills(catalog *skills.Catalog) *CompletionBuilder {
	b.req.Skills = catalog
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
	// Prepend history before any messages already added, on a fresh slice so
	// later appends never write into the caller's backing array.
	merged := make([]messages.ChatMessage, 0, len(history)+len(b.req.Messages))
	merged = append(merged, history...)
	merged = append(merged, b.req.Messages...)
	b.req.Messages = merged
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

// ExecuteWithTools runs the completion and handles tool calls automatically.
// Rounds are capped like Agent.Run's MaxIterations default; hitting the cap
// returns the last response together with ErrMaxIterations.
func (b *CompletionBuilder) ExecuteWithTools(ctx context.Context, client LLM, toolRegistry *tools.ToolRegistry) (*messages.ChatMessage, error) {
	// Add tools to request if not already added
	if len(b.req.Tools) == 0 && toolRegistry != nil {
		b.req.Tools = toolRegistry.All()
	}

	const maxToolRounds = 250
	var response *messages.ChatMessage
	for round := 0; round < maxToolRounds; round++ {
		processor := &SimpleProcessor{}
		eventChan := client.ChatCompletionStream(ctx, b.req, processor)

		response = nil
		for event := range eventChan {
			switch event.Type {
			case messages.EventTypeComplete:
				response = event.Message
			case messages.EventTypeError:
				return nil, event.Error
			}
		}

		if response == nil || len(response.ToolCalls) == 0 || toolRegistry == nil {
			return response, nil
		}

		// The assistant message joins history once, ahead of its results.
		b.req.Messages = append(b.req.Messages, *response)
		for _, toolCall := range response.ToolCalls {
			// Parse arguments
			var args map[string]any
			if err := json.Unmarshal([]byte(toolCall.Arguments), &args); err != nil {
				return nil, fmt.Errorf("failed to parse tool arguments: %w", err)
			}

			// Get and execute tool
			tool, exists, allowed := toolRegistry.GetIfAllowed(toolCall.Name)
			if !exists {
				return nil, fmt.Errorf("tool not found: %s", toolCall.Name)
			}
			if !allowed {
				return nil, fmt.Errorf("tool not allowed by active skill policy: %s", toolCall.Name)
			}

			result, err := tool.Execute(ctx, args)
			if err != nil {
				if msg, ok := tools.FormatToolError(err); ok {
					result = msg
				} else {
					result = fmt.Sprintf("Error executing tool: %v", err)
				}
			}

			b.req.Messages = append(b.req.Messages, messages.ChatMessage{
				Role:       messages.MessageRoleTool,
				Content:    result,
				ToolCallID: toolCall.ID,
				ToolName:   toolCall.Name,
			})
		}
		toolRegistry.CommitPendingChanges()
	}

	response.StopReason = messages.StopReasonMaxIterations
	return response, ErrMaxIterations
}

// SimpleProcessor is a basic implementation of EventStreamProcessor
type SimpleProcessor struct{}

func (s *SimpleProcessor) ProcessMessagesToEvents(msgChan <-chan messages.ChatMessage) <-chan *messages.StreamEvent {
	eventChan := make(chan *messages.StreamEvent)

	go func() {
		defer close(eventChan)

		var fullContent string
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

		// Send complete event with full message. A legitimately empty
		// completion (a refusal, a content-filter stop) still completes;
		// its stop reason is the caller's only signal.
		if received {
			lastMessage.Content = fullContent
			eventChan <- &messages.StreamEvent{
				Type:    messages.EventTypeComplete,
				Message: &lastMessage,
			}
		}
	}()

	return eventChan
}
