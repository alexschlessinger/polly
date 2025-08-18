package llm

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	mcpjsonschema "github.com/modelcontextprotocol/go-sdk/jsonschema"
)

type AnthropicClient struct {
	client anthropic.Client
}

func NewAnthropicClient(apiKey string) *AnthropicClient {
	if apiKey == "" {
		log.Println("anthropic: warning - no API key configured")
	}

	client := anthropic.NewClient(
		option.WithAPIKey(apiKey),
	)

	return &AnthropicClient{
		client: client,
	}
}

// ChatCompletionStream implements the event-based streaming interface
func (a *AnthropicClient) ChatCompletionStream(ctx context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	messageChannel := make(chan messages.ChatMessage, 10)

	go func() {
		defer close(messageChannel)

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
		if req.ThinkingEffort != "" {
			// Map effort levels to token budgets
			var budget int64
			switch req.ThinkingEffort {
			case "low":
				budget = 4096
			case "medium":
				budget = 8192
			case "high":
				budget = 16384
			default:
				budget = 8192 // Default to medium
			}
			params.Thinking = anthropic.ThinkingConfigParamOfEnabled(budget)
		}

		// Add system prompt if present
		if systemPrompt != "" {
			// System messages in Anthropic SDK are handled as TextBlockParam
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

		log.Printf("anthropic: sending streaming request to model %s", req.Model)

		// Use streaming API
		stream := a.client.Messages.NewStreaming(ctx, params)

		// Process the stream
		var responseContent string
		var thinkingContent string
		var toolCalls []messages.ChatMessageToolCall

		for stream.Next() {
			event := stream.Current()

			switch event.Type {
			case "message_start":
				// Message started
			case "content_block_start":
				// Check block type
				blockStart := event.AsContentBlockStart()
				// Marshal to JSON to inspect the type
				b, _ := json.Marshal(blockStart.ContentBlock)
				var block struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				}
				if json.Unmarshal(b, &block) == nil && block.Type == "tool_use" {
					// Initialize a new tool call with empty JSON arguments by default
					toolCalls = append(toolCalls, messages.ChatMessageToolCall{
						ID:        block.ID,
						Name:      block.Name,
						Arguments: "{}", // Default to empty JSON object
					})
				}
			case "content_block_delta":
				// Handle content delta
				blockDelta := event.AsContentBlockDelta()
				
				// Check for thinking delta
				if thinking := blockDelta.Delta.Thinking; thinking != "" {
					thinkingContent += thinking
					// Stream the thinking
					messageChannel <- messages.ChatMessage{
						Role:      messages.MessageRoleAssistant,
						Reasoning: thinking,
					}
				}
				
				// Check for text delta (regular content)
				if text := blockDelta.Delta.Text; text != "" {
					responseContent += text
					// Stream partial content
					messageChannel <- messages.ChatMessage{
						Role:    messages.MessageRoleAssistant,
						Content: text,
					}
				}
				// Check if it's tool use input delta
				if blockDelta.Delta.PartialJSON != "" && len(toolCalls) > 0 {
					// Replace or append to the last tool call's arguments
					lastIdx := len(toolCalls) - 1
					if toolCalls[lastIdx].Arguments == "{}" {
						// First content, replace the default empty object
						toolCalls[lastIdx].Arguments = blockDelta.Delta.PartialJSON
					} else {
						// Append to existing content
						toolCalls[lastIdx].Arguments += blockDelta.Delta.PartialJSON
					}
				}
			case "content_block_stop":
				// Content block finished - arguments should already be properly initialized
			case "message_delta":
				// Message delta (usage stats, etc)
			case "message_stop":
				// Message complete
			}
		}

		// Check for stream error
		if err := stream.Err(); err != nil {
			log.Printf("anthropic: stream error: %v", err)
			messageChannel <- messages.ChatMessage{
				Role:    messages.MessageRoleAssistant,
				Content: "Error: " + err.Error(),
			}
			return
		}

		// Handle structured output response
		if req.ResponseSchema != nil && len(toolCalls) > 0 {
			// Extract structured data from tool call
			for _, tc := range toolCalls {
				if tc.Name == "extract_structured_data" {
					// Parse the arguments to get the data field
					var args map[string]any
					if err := json.Unmarshal([]byte(tc.Arguments), &args); err == nil {
						if data, ok := args["data"]; ok {
							// Return just the structured data as content
							dataJSON, _ := json.Marshal(data)
							messageChannel <- messages.ChatMessage{
								Role:    messages.MessageRoleAssistant,
								Content: string(dataJSON),
							}
							return
						}
					}
				}
			}
		}

		// Send the completed message with tool calls if any
		// Don't include content since it was already streamed
		if len(toolCalls) > 0 {
			messageChannel <- messages.ChatMessage{
				Role:      messages.MessageRoleAssistant,
				Content:   "", // Content was already streamed, don't duplicate
				ToolCalls: toolCalls,
				Reasoning: thinkingContent,
			}
		}

		// Log response details
		contentPreview := responseContent
		if len(contentPreview) > 200 {
			contentPreview = contentPreview[:200] + "..."
		}

		if len(toolCalls) > 0 {
			toolInfo := make([]string, len(toolCalls))
			for i, tc := range toolCalls {
				toolInfo[i] = tc.Name
			}
			log.Printf("anthropic: completed, content: '%s' (%d chars), tool calls: %d %v",
				contentPreview, len(responseContent), len(toolCalls), toolInfo)
		} else {
			log.Printf("anthropic: completed, content: '%s' (%d chars)",
				contentPreview, len(responseContent))
		}
	}()

	return processor.ProcessMessagesToEvents(messageChannel)
}

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
		Name:        "extract_structured_data",
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
