package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"

	"github.com/alexschlessinger/pollytool/llm/adapters"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
	ai "github.com/sashabaranov/go-openai"
	"go.uber.org/zap"
)

var _ LLM = (*OpenAIClient)(nil)

type OpenAIClient struct {
	ClientConfig ai.ClientConfig
	Client       *ai.Client
}

func NewOpenAIClient(apiKey string, baseURL string) *OpenAIClient {
	cfg := ai.DefaultConfig(apiKey)
	if baseURL != "" {
		cfg.BaseURL = baseURL
	}
	return &OpenAIClient{
		ClientConfig: cfg,
		Client:       ai.NewClientWithConfig(cfg),
	}
}

// ChatCompletionStream implements the event-based streaming interface
func (o OpenAIClient) ChatCompletionStream(ctx context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	return runStream(ctx, processor, adapters.NewOpenAIAdapter(), func(streamCore *streaming.StreamingCore) {
		if err := o.streamCompletion(ctx, req, streamCore); err != nil {
			streamCore.EmitError(err)
		}
	})
}

func (o OpenAIClient) streamCompletion(ctx context.Context, req *CompletionRequest, streamCore *streaming.StreamingCore) error {
	timeout, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()

	isStreaming := req.Stream == nil || *req.Stream
	zap.S().Debugw("openai_completion_started", "stream", isStreaming)

	// Convert agnostic messages to OpenAI format
	openAIMessages := MessagesToOpenAI(req.Messages)

	ccr := ai.ChatCompletionRequest{
		MaxCompletionTokens: req.MaxTokens,
		Model:               req.Model,
		Messages:            openAIMessages,
		Temperature:         req.Temperature,
	}

	// Enable reasoning for supported models (o1, DeepSeek, etc.)
	if req.ThinkingEffort.IsEnabled() {
		ccr.ReasoningEffort = string(req.ThinkingEffort)
	}

	// Add structured output support
	if req.ResponseSchema != nil {
		ccr.ResponseFormat = ConvertToOpenAISchema(req.ResponseSchema)
	}

	if len(req.Tools) > 0 {
		var openaiTools []ai.Tool
		for _, tool := range req.Tools {
			openaiTools = append(openaiTools, ConvertToolToOpenAI(tool.GetSchema()))
		}
		ccr.Tools = openaiTools
	}

	if isStreaming {
		return o.handleStreamingCompletion(timeout, ccr, streamCore)
	}
	return o.handleNonStreamingCompletion(timeout, ccr, streamCore)
}

func (o OpenAIClient) handleStreamingCompletion(ctx context.Context, ccr ai.ChatCompletionRequest, streamCore *streaming.StreamingCore) error {
	ccr.Stream = true
	ccr.StreamOptions = &ai.StreamOptions{
		IncludeUsage: true, // Include token usage in final chunk
	}

	stream, err := o.Client.CreateChatCompletionStream(ctx, ccr)
	if err != nil {
		zap.S().Debugw("openai_stream_creation_failed", "error", err)
		return fmt.Errorf("failed to create chat completion stream: %w", err)
	}
	defer stream.Close()

	// Process the stream using StreamingCore
	for {
		response, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				// Stream complete
				break
			}
			zap.S().Debugw("openai_stream_error", "error", err)
			return fmt.Errorf("error during streaming: %w", err)
		}

		// Process the chunk through the adapter
		if err := streamCore.ProcessChunk(&response); err != nil {
			return err
		}

		// Emit content if present
		if len(response.Choices) > 0 {
			choice := response.Choices[0]
			delta := choice.Delta

			// Stream reasoning content if present
			if delta.ReasoningContent != "" {
				streamCore.EmitReasoning(delta.ReasoningContent)
			}

			// Stream content chunks
			if delta.Content != "" {
				streamCore.EmitContent(delta.Content)
			}
		}
	}

	// Send the final message with accumulated state
	streamCore.Complete()
	return nil
}

func (o OpenAIClient) handleNonStreamingCompletion(ctx context.Context, ccr ai.ChatCompletionRequest, streamCore *streaming.StreamingCore) error {
	ccr.Stream = false

	resp, err := o.Client.CreateChatCompletion(ctx, ccr)
	if err != nil {
		zap.S().Debugw("openai_completion_failed", "error", err)
		return fmt.Errorf("failed to create chat completion: %w", err)
	}

	// Process single response
	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		msg := choice.Message

		// Emit reasoning if present
		if msg.ReasoningContent != "" {
			streamCore.EmitReasoning(msg.ReasoningContent)
		}

		// Emit content
		if msg.Content != "" {
			streamCore.EmitContent(msg.Content)
		}

		// Handle tool calls - add them to state
		for _, tc := range msg.ToolCalls {
			streamCore.GetState().AddToolCall(messages.ChatMessageToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}

		// Set stop reason
		streamCore.SetStopReason(adapters.MapOpenAIFinishReason(choice.FinishReason))
	}

	// Set token usage
	streamCore.SetTokenUsage(resp.Usage.PromptTokens, resp.Usage.CompletionTokens)

	// Send the final message with accumulated state
	streamCore.Complete()
	return nil
}

// ConvertToOpenAISchema converts a generic JSON schema to OpenAI's format
func ConvertToOpenAISchema(schema *Schema) *ai.ChatCompletionResponseFormat {
	if schema == nil {
		return nil
	}

	// Make a copy of the schema to modify
	schemaCopy := make(map[string]any)
	maps.Copy(schemaCopy, schema.Raw)

	// OpenAI requires additionalProperties: false for strict mode
	// and ALL properties must be in required array
	if schema.Strict {
		schemaCopy["additionalProperties"] = false

		// OpenAI strict mode requires ALL properties to be required
		if props, ok := schemaCopy["properties"].(map[string]any); ok {
			required := make([]string, 0, len(props))
			for key, prop := range props {
				required = append(required, key)

				// Also add additionalProperties: false to nested objects
				if propMap, ok := prop.(map[string]any); ok {
					if propType, ok := propMap["type"].(string); ok && propType == "object" {
						propMap["additionalProperties"] = false
					}
				}
			}
			// Replace the required array with all properties
			schemaCopy["required"] = required
		}
	}

	// Create a JSON marshaler for the schema
	schemaJSON, _ := json.Marshal(schemaCopy)

	return &ai.ChatCompletionResponseFormat{
		Type: ai.ChatCompletionResponseFormatTypeJSONSchema,
		JSONSchema: &ai.ChatCompletionResponseFormatJSONSchema{
			Name:        "response",
			Description: "Structured response",
			Schema:      json.RawMessage(schemaJSON),
			Strict:      schema.Strict,
		},
	}
}

// ConvertToolToOpenAI converts a tool schema to OpenAI format.
// OpenAI's FunctionDefinition.Parameters accepts any, so we pass a raw map.
func ConvertToolToOpenAI(schema *ToolSchema) ai.Tool {
	params := map[string]any{"type": "object", "properties": map[string]any{}}
	if schema != nil {
		params = map[string]any{
			"type":       "object",
			"properties": schema.Properties(),
		}
		if req := schema.Required(); len(req) > 0 {
			params["required"] = req
		}
	}

	name, description := "", ""
	if schema != nil {
		name = schema.Title()
		description = schema.Description()
	}

	return ai.Tool{
		Type: ai.ToolTypeFunction,
		Function: &ai.FunctionDefinition{
			Name:        name,
			Description: description,
			Parameters:  params,
		},
	}
}

// MessagesToOpenAI converts a slice of agnostic messages to OpenAI format
func MessagesToOpenAI(msgs []messages.ChatMessage) []ai.ChatCompletionMessage {
	result := make([]ai.ChatCompletionMessage, len(msgs))
	for i, msg := range msgs {
		result[i] = MessageToOpenAI(msg)
	}
	return result
}

// MessageToOpenAI converts our agnostic message to OpenAI format
func MessageToOpenAI(msg messages.ChatMessage) ai.ChatCompletionMessage {
	m := ai.ChatCompletionMessage{
		Role:       msg.Role,
		ToolCallID: msg.ToolCallID,
	}

	// Handle multimodal content
	if len(msg.Parts) > 0 {
		var multiContent []ai.ChatMessagePart
		for _, part := range msg.Parts {
			switch part.Type {
			case "text":
				multiContent = append(multiContent, ai.ChatMessagePart{
					Type: ai.ChatMessagePartTypeText,
					Text: part.Text,
				})
			case "image_base64":
				// OpenAI expects data URL format
				dataURL := "data:" + part.MimeType + ";base64," + part.ImageData
				multiContent = append(multiContent, ai.ChatMessagePart{
					Type: ai.ChatMessagePartTypeImageURL,
					ImageURL: &ai.ChatMessageImageURL{
						URL: dataURL,
					},
				})
			case "image_url":
				multiContent = append(multiContent, ai.ChatMessagePart{
					Type: ai.ChatMessagePartTypeImageURL,
					ImageURL: &ai.ChatMessageImageURL{
						URL: part.ImageURL,
					},
				})
			}
		}
		if len(multiContent) > 0 {
			m.MultiContent = multiContent
		}
	} else {
		// Backward compatibility: simple text content
		m.Content = msg.Content
	}

	for _, tc := range msg.ToolCalls {
		m.ToolCalls = append(m.ToolCalls, ai.ToolCall{
			ID:   tc.ID,
			Type: ai.ToolTypeFunction,
			Function: ai.FunctionCall{
				Name:      tc.Name,
				Arguments: tc.Arguments,
			},
		})
	}

	return m
}
