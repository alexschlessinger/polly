package llm

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/adapters"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
	mcpjsonschema "github.com/google/jsonschema-go/jsonschema"
	"go.uber.org/zap"
)

// Metadata keys
const (
	metadataKeyThinkingBlocks = "anthropic_thinking_blocks"
)

// Tool names
const (
	structuredOutputToolName = "extract_structured_data"
)

// Thinking token budgets
const (
	thinkingBudgetLow    = 4096
	thinkingBudgetMedium = 8192
	thinkingBudgetHigh   = 16384
)

// Removed streamState - now using common StreamState
// Removed mapAnthropicStopReason - now in adapters/anthropic_adapter.go

type AnthropicClient struct {
	client anthropic.Client
}

func NewAnthropicClient(apiKey string) *AnthropicClient {
	if apiKey == "" {
		zap.S().Debugw("anthropic_missing_api_key")
	}

	client := anthropic.NewClient(
		option.WithAPIKey(apiKey),
	)

	return &AnthropicClient{
		client: client,
	}
}

// getThinkingConfig returns the thinking configuration based on effort level
func (a *AnthropicClient) getThinkingConfig(effort ThinkingEffort) anthropic.ThinkingConfigParamUnion {
	var budget int64
	switch effort {
	case ThinkingLow:
		budget = thinkingBudgetLow
	case ThinkingMedium:
		budget = thinkingBudgetMedium
	case ThinkingHigh:
		budget = thinkingBudgetHigh
	default:
		budget = thinkingBudgetMedium
	}
	return anthropic.ThinkingConfigParamOfEnabled(budget)
}

// buildRequestParams creates the Anthropic API request parameters
func (a *AnthropicClient) buildRequestParams(req *CompletionRequest) anthropic.MessageNewParams {
	// Convert messages to Anthropic format
	anthropicMessages, systemPrompt := MessagesToAnthropicParams(req.Messages)

	// Create the request
	params := anthropic.MessageNewParams{
		Model:       anthropic.Model(req.Model),
		MaxTokens:   int64(req.MaxTokens),
		Temperature: anthropic.Float(float64(req.Temperature)),
		Messages:    anthropicMessages,
	}

	// Enable thinking for supported models if requested
	if req.ThinkingEffort.IsEnabled() {
		params.Thinking = a.getThinkingConfig(req.ThinkingEffort)
	}

	// Add system prompt if present
	if systemPrompt != "" {
		params.System = []anthropic.TextBlockParam{
			{
				Type: "text",
				Text: systemPrompt,
			},
		}
	}

	// Add tools and/or structured output support
	var anthropicTools []anthropic.ToolUnionParam

	// Add structured output tool if schema is provided
	if req.ResponseSchema != nil {
		anthropicTools = append(anthropicTools, ConvertToAnthropicTool(req.ResponseSchema))
	}

	// Add regular tools if provided
	if len(req.Tools) > 0 {
		for _, tool := range req.Tools {
			anthropicTools = append(anthropicTools, ConvertToolToAnthropic(tool.GetSchema()))
		}
	}

	// Set tools if we have any
	if len(anthropicTools) > 0 {
		params.Tools = anthropicTools

		// Only force tool use if ONLY schema is provided (no regular tools)
		if req.ResponseSchema != nil && len(req.Tools) == 0 {
			params.ToolChoice = anthropic.ToolChoiceUnionParam{
				OfAny: &anthropic.ToolChoiceAnyParam{
					Type: "any",
				},
			}
		}
	}

	return params
}

// ChatCompletionStream implements the event-based streaming interface
func (a *AnthropicClient) ChatCompletionStream(ctx context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	messageChannel := make(chan messages.ChatMessage, 10)

	go func() {
		defer close(messageChannel)

		// Create streaming core with Anthropic adapter
		adapter := adapters.NewAnthropicAdapter()
		streamCore := streaming.NewStreamingCore(ctx, messageChannel, adapter)

		// Build request parameters
		params := a.buildRequestParams(req)

		zap.S().Debugw("anthropic_streaming_started", "model", req.Model)

		// Use streaming API
		stream := a.client.Messages.NewStreaming(ctx, params)

		// Process the stream
		a.processStream(stream, req, streamCore)
	}()

	return processor.ProcessMessagesToEvents(messageChannel)
}

// processStream handles the main stream processing logic
func (a *AnthropicClient) processStream(stream *ssestream.Stream[anthropic.MessageStreamEventUnion], req *CompletionRequest, streamCore *streaming.StreamingCore) {
	for stream.Next() {
		event := stream.Current()

		// Process the event through the adapter
		if err := streamCore.ProcessChunk(event); err != nil {
			streamCore.EmitError(err)
			return
		}

		// Handle content and reasoning streaming
		switch event.Type {
		case string(constant.ValueOf[constant.ContentBlockDelta]()):
			blockDelta := event.AsContentBlockDelta()

			// Stream thinking content
			if thinking := blockDelta.Delta.Thinking; thinking != "" {
				streamCore.EmitReasoning(thinking)
			}

			// Stream regular content
			if text := blockDelta.Delta.Text; text != "" {
				streamCore.EmitContent(text)
			}
		}
	}

	// Check for stream error
	if err := stream.Err(); err != nil {
		streamCore.EmitError(err)
		return
	}

	// Handle structured output response
	if req.ResponseSchema != nil {
		if streamCore.HandleStructuredOutput(structuredOutputToolName) {
			return
		}
	}

	// Send final message with accumulated state
	streamCore.Complete()
}

// Removed old streaming helper functions - now handled by StreamingCore and AnthropicAdapter:
// - processStreamEvent
// - processContentBlockStart
// - processContentBlockDelta
// - processContentBlockStop
// - handleStreamError
// - handleStructuredOutput (now in StreamingCore)
// - sendFinalMessage (now in StreamingCore)
// - logResponseDetails (now in StreamingCore)

// ConvertToAnthropicTool creates a synthetic tool for structured output with Anthropic
func ConvertToAnthropicTool(schema *Schema) anthropic.ToolUnionParam {
	if schema == nil {
		return anthropic.ToolUnionParam{}
	}

	// Create a tool that represents the structured output
	toolSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"data": schema.Raw,
		},
		"required": []string{"data"},
	}

	// Convert toolSchema properties for Anthropic
	properties := make(map[string]any)
	if props, ok := toolSchema["properties"].(map[string]any); ok {
		properties = props
	}

	required := []string{"data"}

	toolParam := anthropic.ToolParam{
		Name:        structuredOutputToolName,
		Description: anthropic.String("Extract and structure data according to the specified schema"),
		InputSchema: anthropic.ToolInputSchemaParam{
			Type:       "object",
			Properties: properties,
			Required:   required,
		},
	}

	return anthropic.ToolUnionParam{
		OfTool: &toolParam,
	}
}

// convertSchemaToAnthropicMap recursively converts an MCP schema to Anthropic format map
func convertSchemaToAnthropicMap(schema *mcpjsonschema.Schema) map[string]any {
	if schema == nil {
		return nil
	}

	propMap := make(map[string]any)

	// Always set type, default to string if empty
	if schema.Type != "" {
		propMap["type"] = schema.Type
	} else {
		propMap["type"] = "string"
	}

	// Only add description if non-empty
	if schema.Description != "" {
		propMap["description"] = schema.Description
	}

	// Handle different types
	switch schema.Type {
	case "array":
		// Handle array items
		if schema.Items != nil {
			propMap["items"] = convertSchemaToAnthropicMap(schema.Items)
		} else {
			// Array must have items defined for JSON Schema 2020-12
			propMap["items"] = map[string]any{
				"type": "string",
			}
		}
	case "object":
		// Handle nested object properties recursively
		if len(schema.Properties) > 0 {
			props := make(map[string]any)
			for name, prop := range schema.Properties {
				if prop != nil {
					props[name] = convertSchemaToAnthropicMap(prop)
				}
			}
			propMap["properties"] = props
		} else {
			// Object should have properties defined
			propMap["properties"] = make(map[string]any)
		}
		if len(schema.Required) > 0 {
			propMap["required"] = schema.Required
		}
	}

	// Handle enums if present
	if len(schema.Enum) > 0 {
		propMap["enum"] = schema.Enum
	}

	return propMap
}

// ConvertToolToAnthropic converts a generic tool schema to Anthropic format
func ConvertToolToAnthropic(schema *mcpjsonschema.Schema) anthropic.ToolUnionParam {
	// Convert properties to Anthropic format using recursive conversion
	properties := make(map[string]any)
	if schema != nil && schema.Properties != nil {
		for k, v := range schema.Properties {
			if v != nil {
				properties[k] = convertSchemaToAnthropicMap(v)
			}
		}
	}

	name := ""
	description := ""
	var required []string

	if schema != nil {
		name = schema.Title
		description = schema.Description
		// Only set required if it's not empty
		if len(schema.Required) > 0 {
			required = schema.Required
		}
	}

	// Build InputSchema with proper JSON Schema 2020-12 format
	inputSchema := anthropic.ToolInputSchemaParam{
		Type:       "object",
		Properties: properties,
	}

	// Only add required field if it's not empty
	if len(required) > 0 {
		inputSchema.Required = required
	}

	tool := anthropic.ToolParam{
		Name:        name,
		Description: anthropic.String(description),
		InputSchema: inputSchema,
	}

	// Wrap in ToolUnionParam
	return anthropic.ToolUnionParam{
		OfTool: &tool,
	}
}

// MessagesToAnthropicParams converts messages to Anthropic message parameters
func MessagesToAnthropicParams(msgs []messages.ChatMessage) ([]anthropic.MessageParam, string) {
	var anthropicMessages []anthropic.MessageParam
	systemPrompt := ""

	for _, msg := range msgs {
		switch msg.Role {
		case messages.MessageRoleSystem:
			systemPrompt = msg.Content

		case messages.MessageRoleUser:
			// Handle multimodal content
			if len(msg.Parts) > 0 {
				var blocks []anthropic.ContentBlockParamUnion
				for _, part := range msg.Parts {
					switch part.Type {
					case "text":
						if strings.TrimSpace(part.Text) != "" {
							blocks = append(blocks, anthropic.NewTextBlock(part.Text))
						}
					case "image_base64":
						// Anthropic expects base64 images with media type
						blocks = append(blocks, anthropic.NewImageBlockBase64(part.MimeType, part.ImageData))
					case "image_url":
						// For URL images, we'd need to download and convert to base64
						// For now, skip URL images for Anthropic
					}
				}
				if len(blocks) > 0 {
					anthropicMessages = append(anthropicMessages, anthropic.NewUserMessage(blocks...))
				}
			} else if strings.TrimSpace(msg.Content) != "" {
				// Backward compatibility: simple text content
				anthropicMessages = append(anthropicMessages, anthropic.NewUserMessage(
					anthropic.NewTextBlock(msg.Content),
				))
			}

		case messages.MessageRoleAssistant:
			var blocks []anthropic.ContentBlockParamUnion

			// Check if we have preserved thinking blocks in metadata
			if msg.Metadata != nil {
				if thinkingBlocksData, ok := msg.Metadata["anthropic_thinking_blocks"]; ok {
					// Restore thinking blocks with their signatures
					// Handle both []map[string]any and []interface{} types
					var thinkingBlocksList []map[string]any
					switch v := thinkingBlocksData.(type) {
					case []map[string]any:
						thinkingBlocksList = v
					}

					for _, block := range thinkingBlocksList {
						if blockType, _ := block["type"].(string); blockType == string(constant.ValueOf[constant.Thinking]()) {
							thinking, _ := block["thinking"].(string)
							signature, _ := block["signature"].(string)
							if signature != "" && thinking != "" {
								blocks = append(blocks, anthropic.NewThinkingBlock(signature, thinking))
							}
						}
					}
				}
			}

			if strings.TrimSpace(msg.Content) != "" {
				blocks = append(blocks, anthropic.NewTextBlock(msg.Content))
			}
			if len(msg.ToolCalls) > 0 {
				for _, tc := range msg.ToolCalls {
					var input any
					if argStr := strings.TrimSpace(tc.Arguments); argStr != "" {
						var tmp any
						if err := json.Unmarshal([]byte(argStr), &tmp); err == nil {
							input = tmp
						}
					} else {
						// Anthropic requires input field even for tools with no parameters
						input = make(map[string]any)
					}
					blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, input, tc.Name))
				}
			}
			if len(blocks) > 0 {
				anthropicMessages = append(anthropicMessages, anthropic.NewAssistantMessage(blocks...))
			}

		case messages.MessageRoleTool:
			if strings.TrimSpace(msg.ToolCallID) != "" {
				anthropicMessages = append(anthropicMessages, anthropic.NewUserMessage(
					anthropic.NewToolResultBlock(msg.ToolCallID, msg.Content, false),
				))
			} else if strings.TrimSpace(msg.Content) != "" {
				anthropicMessages = append(anthropicMessages, anthropic.NewUserMessage(
					anthropic.NewTextBlock(msg.Content),
				))
			}
		}
	}

	return anthropicMessages, systemPrompt
}
