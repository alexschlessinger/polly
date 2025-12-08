package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"

	"github.com/alexschlessinger/pollytool/messages"
	mcpjsonschema "github.com/google/jsonschema-go/jsonschema"
	ai "github.com/sashabaranov/go-openai"
	"github.com/sashabaranov/go-openai/jsonschema"
	"go.uber.org/zap"
)

var _ LLM = (*OpenAIClient)(nil)

// mapOpenAIFinishReason converts OpenAI's finish reason to our normalized type
func mapOpenAIFinishReason(fr ai.FinishReason) messages.StopReason {
	switch fr {
	case ai.FinishReasonStop:
		return messages.StopReasonEndTurn
	case ai.FinishReasonToolCalls, ai.FinishReasonFunctionCall:
		return messages.StopReasonToolUse
	case ai.FinishReasonLength:
		return messages.StopReasonMaxTokens
	case ai.FinishReasonContentFilter:
		return messages.StopReasonContentFilter
	default:
		return messages.StopReasonEndTurn
	}
}

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
		o.completion(ctx, req, messageChannel)
	}()

	return processor.ProcessMessagesToEvents(messageChannel)
}

func (o OpenAIClient) completion(ctx context.Context, req *CompletionRequest, respChannel chan<- messages.ChatMessage) error {
	timeout, cancel := context.WithTimeout(ctx, req.Timeout)
	defer close(respChannel)
	defer cancel()
	zap.S().Debug("completionTask: start")

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
	if req.ThinkingEffort != "off" {
		ccr.ReasoningEffort = req.ThinkingEffort
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
		zap.S().Debugf("completionTask: failed to create chat completion stream: %v", err)
		respChannel <- messages.ChatMessage{
			Role:    messages.MessageRoleAssistant,
			Content: "failed to create chat completion stream: " + err.Error(),
		}
		return err
	}
	defer stream.Close()

	// Accumulate the response
	var fullContent string
	var reasoningContent string
	var toolCalls []messages.ChatMessageToolCall
	var stopReason messages.StopReason
	var inputTokens, outputTokens int

	for {
		response, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				// Stream complete
				break
			}
			zap.S().Debugf("openai: stream error: %v", err)
			respChannel <- messages.ChatMessage{
				Role:    messages.MessageRoleAssistant,
				Content: "Error during streaming: " + err.Error(),
			}
			return err
		}

		// Capture usage from final chunk (sent when StreamOptions.IncludeUsage is true)
		if response.Usage != nil {
			inputTokens = response.Usage.PromptTokens
			outputTokens = response.Usage.CompletionTokens
		}

		if len(response.Choices) > 0 {
			choice := response.Choices[0]
			delta := choice.Delta

			// Capture finish reason when it's set
			if choice.FinishReason != "" {
				stopReason = mapOpenAIFinishReason(choice.FinishReason)
			}

			// Stream reasoning content if present (for models like DeepSeek)
			if delta.ReasoningContent != "" {
				reasoningContent += delta.ReasoningContent
				// Send reasoning for status display
				respChannel <- messages.ChatMessage{
					Role:      messages.MessageRoleAssistant,
					Reasoning: delta.ReasoningContent,
				}
			}

			// Stream content chunks as they arrive
			if delta.Content != "" {
				fullContent += delta.Content
				// Send partial content for streaming
				respChannel <- messages.ChatMessage{
					Role:    messages.MessageRoleAssistant,
					Content: delta.Content,
				}
			}

			// Accumulate tool calls
			if len(delta.ToolCalls) > 0 {
				// Handle tool call deltas (OpenAI sends them incrementally)
				for _, tc := range delta.ToolCalls {
					// Find or create the tool call by index
					for len(toolCalls) <= *tc.Index {
						toolCalls = append(toolCalls, messages.ChatMessageToolCall{
							Arguments: "{}", // Initialize with empty JSON object
						})
					}

					if tc.ID != "" {
						toolCalls[*tc.Index].ID = tc.ID
					}
					if tc.Function.Name != "" {
						toolCalls[*tc.Index].Name = tc.Function.Name
					}
					if tc.Function.Arguments != "" {
						if toolCalls[*tc.Index].Arguments == "{}" {
							// First content, replace the default empty object
							toolCalls[*tc.Index].Arguments = tc.Function.Arguments
						} else {
							// Append to existing content
							toolCalls[*tc.Index].Arguments += tc.Function.Arguments
						}
					}
				}
			}
		}
	}

	// Send the complete message with any tool calls
	// Only include full content if we haven't streamed it
	// (If we streamed chunks, the processor already has the content)
	finalContent := ""
	if fullContent != "" && len(toolCalls) == 0 {
		// No tool calls and we have content - this means we need to send a final message
		// But the content was already streamed, so send empty to avoid duplication
		finalContent = ""
	} else if len(toolCalls) > 0 {
		// With tool calls, don't include content since it was already streamed
		finalContent = ""
	}

	msg := messages.ChatMessage{
		Role:       messages.MessageRoleAssistant,
		Content:    finalContent,
		ToolCalls:  toolCalls,
		Reasoning:  "", // Reasoning was already streamed, don't duplicate
		StopReason: stopReason,
	}
	// Store token usage
	msg.SetTokenUsage(inputTokens, outputTokens)

	// Always send the final complete message (needed for processor to emit completion event)
	respChannel <- msg

	// Log detailed response information
	contentPreview := fullContent
	if len(contentPreview) > 200 {
		contentPreview = contentPreview[:200] + "..."
	}

	if len(toolCalls) > 0 {
		toolInfo := make([]string, len(toolCalls))
		for i, tc := range toolCalls {
			toolInfo[i] = fmt.Sprintf("%s(%s)", tc.Name, tc.Arguments)
		}
		zap.S().Debugf("openai: completed, content: '%s' (%d chars), tool calls: %d %v",
			contentPreview, len(fullContent), len(toolCalls), toolInfo)
	} else if len(fullContent) == 0 {
		zap.S().Debug("openai: completed, empty response (no content or tool calls)")
	} else {
		zap.S().Debugf("openai: completed, content: '%s' (%d chars)",
			contentPreview, len(fullContent))
	}
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
