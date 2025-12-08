package llm

import (
	"context"
	"encoding/json"
	"strings"

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

// streamState holds the state during streaming
type streamState struct {
	responseContent      string
	thinkingContent      string
	toolCalls            []messages.ChatMessageToolCall
	thinkingBlocks       []map[string]any
	currentBlockType     string
	currentThinkingBlock map[string]any
	stopReason           messages.StopReason
	inputTokens          int
	outputTokens         int
}

// mapAnthropicStopReason converts Anthropic's stop reason to our normalized type
func mapAnthropicStopReason(sr anthropic.StopReason) messages.StopReason {
	switch sr {
	case "end_turn":
		return messages.StopReasonEndTurn
	case "tool_use":
		return messages.StopReasonToolUse
	case "max_tokens":
		return messages.StopReasonMaxTokens
	case "refusal":
		return messages.StopReasonContentFilter
	case "stop_sequence":
		return messages.StopReasonEndTurn
	default:
		return messages.StopReasonEndTurn
	}
}

type AnthropicClient struct {
	client anthropic.Client
}

func NewAnthropicClient(apiKey string) *AnthropicClient {
	if apiKey == "" {
		zap.S().Debug("anthropic: warning - no API key configured")
	}

	client := anthropic.NewClient(
		option.WithAPIKey(apiKey),
	)

	return &AnthropicClient{
		client: client,
	}
}

// getThinkingConfig returns the thinking configuration based on effort level
func (a *AnthropicClient) getThinkingConfig(effort string) anthropic.ThinkingConfigParamUnion {
	var budget int64
	switch effort {
	case "low":
		budget = thinkingBudgetLow
	case "medium":
		budget = thinkingBudgetMedium
	case "high":
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
	if req.ThinkingEffort != "off" {
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

		// Build request parameters
		params := a.buildRequestParams(req)

		zap.S().Debugf("anthropic: sending streaming request to model %s", req.Model)

		// Use streaming API
		stream := a.client.Messages.NewStreaming(ctx, params)

		// Process the stream
		a.processStream(stream, req, messageChannel)
	}()

	return processor.ProcessMessagesToEvents(messageChannel)
}

// processStream handles the main stream processing logic
func (a *AnthropicClient) processStream(stream *ssestream.Stream[anthropic.MessageStreamEventUnion], req *CompletionRequest, messageChannel chan messages.ChatMessage) {
	state := &streamState{}

	for stream.Next() {
		event := stream.Current()
		a.processStreamEvent(event, state, messageChannel)
	}

	// Check for stream error
	if err := stream.Err(); err != nil {
		a.handleStreamError(err, messageChannel)
		return
	}

	// Handle structured output response
	if req.ResponseSchema != nil && len(state.toolCalls) > 0 {
		if a.handleStructuredOutput(state.toolCalls, messageChannel) {
			return
		}
	}

	// Send final message with tool calls if any
	a.sendFinalMessage(state, messageChannel)

	// Log response details
	a.logResponseDetails(state.responseContent, state.toolCalls)
}

// processStreamEvent processes a single stream event
func (a *AnthropicClient) processStreamEvent(event anthropic.MessageStreamEventUnion, state *streamState, messageChannel chan messages.ChatMessage) {
	switch event.Type {
	case string(constant.ValueOf[constant.MessageStart]()):
		// Message started - capture input tokens
		msgStart := event.AsMessageStart()
		state.inputTokens = int(msgStart.Message.Usage.InputTokens)
	case string(constant.ValueOf[constant.ContentBlockStart]()):
		a.processContentBlockStart(event, state)
	case string(constant.ValueOf[constant.ContentBlockDelta]()):
		a.processContentBlockDelta(event, state, messageChannel)
	case string(constant.ValueOf[constant.ContentBlockStop]()):
		a.processContentBlockStop(state)
	case string(constant.ValueOf[constant.MessageDelta]()):
		// Message delta contains stop_reason and usage stats
		msgDelta := event.AsMessageDelta()
		state.stopReason = mapAnthropicStopReason(msgDelta.Delta.StopReason)
		state.outputTokens = int(msgDelta.Usage.OutputTokens)
	case string(constant.ValueOf[constant.MessageStop]()):
		// Message complete
	}
}

// processContentBlockStart handles content block start events
func (a *AnthropicClient) processContentBlockStart(event anthropic.MessageStreamEventUnion, state *streamState) {
	blockStart := event.AsContentBlockStart()
	// Marshal to JSON to inspect the type
	b, _ := json.Marshal(blockStart.ContentBlock)
	var block map[string]any
	if json.Unmarshal(b, &block) == nil {
		blockType, _ := block["type"].(string)
		state.currentBlockType = blockType

		switch blockType {
		case string(constant.ValueOf[constant.Thinking]()):
			// Start capturing a thinking block
			state.currentThinkingBlock = map[string]any{
				"type":     string(constant.ValueOf[constant.Thinking]()),
				"thinking": "", // Will be filled by deltas
			}
		case string(constant.ValueOf[constant.ToolUse]()):
			// Initialize a new tool call
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			state.toolCalls = append(state.toolCalls, messages.ChatMessageToolCall{
				ID:        id,
				Name:      name,
				Arguments: "{}", // Default to empty JSON object
			})
		}
	}
}

// processContentBlockDelta handles content block delta events
func (a *AnthropicClient) processContentBlockDelta(event anthropic.MessageStreamEventUnion, state *streamState, messageChannel chan messages.ChatMessage) {
	blockDelta := event.AsContentBlockDelta()

	// Check for thinking delta
	if thinking := blockDelta.Delta.Thinking; thinking != "" {
		state.thinkingContent += thinking
		// Add to current thinking block if we're capturing one
		if state.currentThinkingBlock != nil {
			if existingThinking, ok := state.currentThinkingBlock["thinking"].(string); ok {
				state.currentThinkingBlock["thinking"] = existingThinking + thinking
			} else {
				state.currentThinkingBlock["thinking"] = thinking
			}
		}
		// Stream the thinking
		messageChannel <- messages.ChatMessage{
			Role:      messages.MessageRoleAssistant,
			Reasoning: thinking,
		}
	}

	// Check for signature delta (comes after thinking content)
	if signature := blockDelta.Delta.Signature; signature != "" {
		if state.currentThinkingBlock != nil {
			state.currentThinkingBlock["signature"] = signature
		}
	}

	// Check for text delta (regular content)
	if text := blockDelta.Delta.Text; text != "" {
		state.responseContent += text
		// Stream partial content
		messageChannel <- messages.ChatMessage{
			Role:    messages.MessageRoleAssistant,
			Content: text,
		}
	}

	// Check if it's tool use input delta
	if blockDelta.Delta.PartialJSON != "" && len(state.toolCalls) > 0 {
		// Replace or append to the last tool call's arguments
		lastIdx := len(state.toolCalls) - 1
		if state.toolCalls[lastIdx].Arguments == "{}" {
			// First content, replace the default empty object
			state.toolCalls[lastIdx].Arguments = blockDelta.Delta.PartialJSON
		} else {
			// Append to existing content
			state.toolCalls[lastIdx].Arguments += blockDelta.Delta.PartialJSON
		}
	}
}

// processContentBlockStop handles content block stop events
func (a *AnthropicClient) processContentBlockStop(state *streamState) {
	if state.currentBlockType == string(constant.ValueOf[constant.Thinking]()) && state.currentThinkingBlock != nil {
		// Save completed thinking block
		state.thinkingBlocks = append(state.thinkingBlocks, state.currentThinkingBlock)
		state.currentThinkingBlock = nil
	}
	state.currentBlockType = ""
}

// handleStreamError handles stream errors
func (a *AnthropicClient) handleStreamError(err error, messageChannel chan messages.ChatMessage) {
	zap.S().Debugf("anthropic: stream error: %v", err)
	messageChannel <- messages.ChatMessage{
		Role:    messages.MessageRoleAssistant,
		Content: "Error: " + err.Error(),
	}
}

// handleStructuredOutput processes structured output responses
func (a *AnthropicClient) handleStructuredOutput(toolCalls []messages.ChatMessageToolCall, messageChannel chan messages.ChatMessage) bool {
	for _, tc := range toolCalls {
		if tc.Name == structuredOutputToolName {
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
					return true
				}
			}
		}
	}
	return false
}

// sendFinalMessage sends the final message with stop reason and tool calls if any
func (a *AnthropicClient) sendFinalMessage(state *streamState, messageChannel chan messages.ChatMessage) {
	// Always send a final message with stop reason (needed for agent completion detection)
	msg := messages.ChatMessage{
		Role:       messages.MessageRoleAssistant,
		Content:    "", // Content was already streamed, don't duplicate
		ToolCalls:  state.toolCalls,
		Reasoning:  "", // Reasoning was already streamed, don't duplicate
		StopReason: state.stopReason,
	}
	// Initialize metadata map
	if msg.Metadata == nil {
		msg.Metadata = make(map[string]any)
	}
	// Store token usage
	msg.SetTokenUsage(state.inputTokens, state.outputTokens)
	// Store thinking blocks for future use
	if len(state.thinkingBlocks) > 0 {
		msg.Metadata[metadataKeyThinkingBlocks] = state.thinkingBlocks
	}
	messageChannel <- msg
}

// logResponseDetails logs the completion details for debugging
func (a *AnthropicClient) logResponseDetails(responseContent string, toolCalls []messages.ChatMessageToolCall) {
	contentPreview := responseContent
	if len(contentPreview) > 200 {
		contentPreview = contentPreview[:200] + "..."
	}

	if len(toolCalls) > 0 {
		toolInfo := make([]string, len(toolCalls))
		for i, tc := range toolCalls {
			toolInfo[i] = tc.Name
		}
		zap.S().Debugf("anthropic: completed, content: '%s' (%d chars), tool calls: %d %v",
			contentPreview, len(responseContent), len(toolCalls), toolInfo)
	} else {
		zap.S().Debugf("anthropic: completed, content: '%s' (%d chars)",
			contentPreview, len(responseContent))
	}
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
