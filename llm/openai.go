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
	mcpjsonschema "github.com/google/jsonschema-go/jsonschema"
	ai "github.com/sashabaranov/go-openai"
	"github.com/sashabaranov/go-openai/jsonschema"
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
	messageChannel := make(chan messages.ChatMessage, 10)

	go func() {
		defer close(messageChannel)

		// Create streaming core with OpenAI adapter
		adapter := adapters.NewOpenAIAdapter()
		streamCore := streaming.NewStreamingCore(ctx, messageChannel, adapter)

		// Process the request
		if err := o.streamCompletion(ctx, req, streamCore); err != nil {
			streamCore.EmitError(err)
		}
	}()

	return processor.ProcessMessagesToEvents(messageChannel)
}

func (o OpenAIClient) streamCompletion(ctx context.Context, req *CompletionRequest, streamCore *streaming.StreamingCore) error {
	timeout, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	zap.S().Debugw("openai_completion_started")

	// Convert agnostic messages to OpenAI format
	openAIMessages := MessagesToOpenAI(req.Messages)

	ccr := ai.ChatCompletionRequest{
		MaxCompletionTokens: req.MaxTokens,
		Model:               req.Model,
		Messages:            openAIMessages,
		Temperature:         req.Temperature,
		Stream:              true, // Enable streaming
		StreamOptions: &ai.StreamOptions{
			IncludeUsage: true, // Include token usage in final chunk
		},
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

	// Use streaming API
	stream, err := o.Client.CreateChatCompletionStream(timeout, ccr)
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

// convertSchemaToOpenAIDefinition recursively converts an MCP schema to an OpenAI Definition
func convertSchemaToOpenAIDefinition(schema *mcpjsonschema.Schema) jsonschema.Definition {
	if schema == nil {
		return jsonschema.Definition{}
	}

	def := jsonschema.Definition{
		Type:        jsonschema.DataType(schema.Type),
		Description: schema.Description,
	}

	// Handle different types
	switch schema.Type {
	case "array":
		// Handle array items
		if schema.Items != nil {
			items := convertSchemaToOpenAIDefinition(schema.Items)
			def.Items = &items
		}
	case "object":
		// Handle nested object properties recursively
		if schema.Properties != nil {
			props := make(map[string]jsonschema.Definition)
			for name, prop := range schema.Properties {
				if prop != nil {
					props[name] = convertSchemaToOpenAIDefinition(prop)
				}
			}
			def.Properties = props
		}
		if len(schema.Required) > 0 {
			def.Required = schema.Required
		}
	}

	// Handle enums if present
	if len(schema.Enum) > 0 {
		// Convert any type enums to string enums for OpenAI
		enumStrs := make([]string, 0, len(schema.Enum))
		for _, e := range schema.Enum {
			if s, ok := e.(string); ok {
				enumStrs = append(enumStrs, s)
			}
		}
		if len(enumStrs) > 0 {
			def.Enum = enumStrs
		}
	}

	return def
}

// ConvertToolToOpenAI converts a generic tool schema to OpenAI format
func ConvertToolToOpenAI(schema *mcpjsonschema.Schema) ai.Tool {
	// Convert properties to OpenAI jsonschema.Definition using recursive conversion
	props := make(map[string]jsonschema.Definition)
	if schema != nil && schema.Properties != nil {
		for k, v := range schema.Properties {
			if v != nil {
				props[k] = convertSchemaToOpenAIDefinition(v)
			}
		}
	}

	name := ""
	description := ""
	var required []string

	if schema != nil {
		name = schema.Title
		description = schema.Description
		required = schema.Required
	}

	// Create parameters definition
	// OpenAI requires Properties field to be present, even if empty.
	// The go-openai jsonschema omits empty maps, so for truly no-arg tools
	// we inject a benign optional placeholder property to keep the field present.
	if len(props) == 0 {
		props["__noargs"] = jsonschema.Definition{
			Type:        jsonschema.String,
			Description: "No arguments expected; value ignored.",
		}
	}

	params := jsonschema.Definition{
		Type:                 jsonschema.Object,
		Properties:           props,
		Required:             required,
		AdditionalProperties: false,
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
