package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/alexschlessinger/pollytool/messages"
	mcpjsonschema "github.com/google/jsonschema-go/jsonschema"
	"go.uber.org/zap"
	"google.golang.org/genai"
)

// mapGeminiFinishReason converts Gemini's finish reason to our normalized type
func mapGeminiFinishReason(fr genai.FinishReason) messages.StopReason {
	switch fr {
	case genai.FinishReasonStop:
		return messages.StopReasonEndTurn
	case genai.FinishReasonMaxTokens:
		return messages.StopReasonMaxTokens
	case genai.FinishReasonSafety, genai.FinishReasonRecitation,
		genai.FinishReasonBlocklist, genai.FinishReasonProhibitedContent,
		genai.FinishReasonSPII, genai.FinishReasonImageSafety,
		genai.FinishReasonImageProhibitedContent:
		return messages.StopReasonContentFilter
	case genai.FinishReasonMalformedFunctionCall:
		return messages.StopReasonError
	default:
		return messages.StopReasonEndTurn
	}
}

type GeminiClient struct {
	apiKey string
}

func NewGeminiClient(apiKey string) *GeminiClient {
	if apiKey == "" {
		zap.S().Debug("gemini: warning - no API key configured")
	}

	return &GeminiClient{
		apiKey: apiKey,
	}
}

// ChatCompletionStream implements the event-based streaming interface
func (g *GeminiClient) ChatCompletionStream(ctx context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	messageChannel := make(chan messages.ChatMessage, 10)

	go func() {
		defer close(messageChannel)

		if g.apiKey == "" {
			messageChannel <- messages.ChatMessage{
				Role:    messages.MessageRoleAssistant,
				Content: "Error: Gemini API key not configured",
			}
			return
		}

		// Create client with API key
		client, err := genai.NewClient(ctx, &genai.ClientConfig{
			APIKey:  g.apiKey,
			Backend: genai.BackendGeminiAPI,
		})
		if err != nil {
			zap.S().Debugf("gemini: failed to create client: %v", err)
			messageChannel <- messages.ChatMessage{
				Role:    messages.MessageRoleAssistant,
				Content: "Error creating Gemini client: " + err.Error(),
			}
			return
		}

		// Convert session history to Gemini chat history
		contents, systemInstruction, _ := MessagesToGeminiContent(req.Messages)

		// Configure model parameters
		temp := req.Temperature
		maxTokens := int32(req.MaxTokens)

		config := &genai.GenerateContentConfig{
			Temperature:     &temp,
			MaxOutputTokens: maxTokens,
		}

		// Add structured output support
		if req.ResponseSchema != nil {
			config.ResponseMIMEType = "application/json"
			config.ResponseSchema = ConvertToGeminiSchema(req.ResponseSchema)
		}

		// System instruction
		if systemInstruction != "" {
			config.SystemInstruction = &genai.Content{
				Parts: []*genai.Part{{Text: systemInstruction}},
			}
		}

		// Add tool support if available
		if len(req.Tools) > 0 {
			var geminiFuncs []*genai.FunctionDeclaration
			for _, tool := range req.Tools {
				geminiTool := ConvertToolToGemini(tool.GetSchema())
				// Extract function declarations from the tool
				if geminiTool != nil && len(geminiTool.FunctionDeclarations) > 0 {
					geminiFuncs = append(geminiFuncs, geminiTool.FunctionDeclarations...)
				}
			}
			config.Tools = []*genai.Tool{
				{FunctionDeclarations: geminiFuncs},
			}
		}

		zap.S().Debugf("gemini: sending streaming request to model %s", req.Model)

		// Send message and get streaming response
		iter := client.Models.GenerateContentStream(ctx, req.Model, contents, config)

		// Process the stream
		var responseContent string
		var toolCalls []messages.ChatMessageToolCall
		var stopReason messages.StopReason
		var inputTokens, outputTokens int
		signatures := make(map[string]string)

		for resp, err := range iter {
			if err != nil {
				zap.S().Debugf("gemini: stream error: %v", err)
				messageChannel <- messages.ChatMessage{
					Role:    messages.MessageRoleAssistant,
					Content: "Error: " + err.Error(),
				}
				return
			}

			// Capture token usage (available on each chunk, use latest values)
			if resp.UsageMetadata != nil {
				inputTokens = int(resp.UsageMetadata.PromptTokenCount)
				outputTokens = int(resp.UsageMetadata.CandidatesTokenCount)
			}

			// Process each candidate's parts
			if len(resp.Candidates) > 0 {
				candidate := resp.Candidates[0]

				// Capture finish reason when set
				if candidate.FinishReason != "" {
					stopReason = mapGeminiFinishReason(candidate.FinishReason)
				}

				if candidate.Content != nil {
					for _, part := range candidate.Content.Parts {
						if part.Text != "" {
							text := part.Text
							responseContent += text
							messageChannel <- messages.ChatMessage{
								Role:    messages.MessageRoleAssistant,
								Content: text,
							}
						}
						if part.FunctionCall != nil {
							// Accumulate tool calls
							argsJSON, _ := json.Marshal(part.FunctionCall.Args)
							toolCallID := fmt.Sprintf("gemini-%d", len(toolCalls))
							toolCalls = append(toolCalls, messages.ChatMessageToolCall{
								ID:        toolCallID,
								Name:      part.FunctionCall.Name,
								Arguments: string(argsJSON),
							})

							if len(part.ThoughtSignature) > 0 {
								signatures[toolCallID] = base64.StdEncoding.EncodeToString(part.ThoughtSignature)
							}
						}
					}
				}
			}
		}

		// If there are tool calls, override stop reason to ToolUse
		// (Gemini doesn't have a specific finish reason for tool calls - it uses "STOP")
		if len(toolCalls) > 0 {
			stopReason = messages.StopReasonToolUse
		}

		// Always send a final message with stop reason (needed for agent completion detection)
		msg := messages.ChatMessage{
			Role:       messages.MessageRoleAssistant,
			Content:    "", // Content was already streamed
			ToolCalls:  toolCalls,
			StopReason: stopReason,
		}

		// Store token usage
		msg.SetTokenUsage(inputTokens, outputTokens)

		// Store thought signatures if present
		if len(signatures) > 0 {
			msg.Metadata["gemini_thought_signatures"] = signatures
		}

		messageChannel <- msg

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
			zap.S().Debugf("gemini: completed, content: '%s' (%d chars), tool calls: %d %v",
				contentPreview, len(responseContent), len(toolCalls), toolInfo)
		} else {
			zap.S().Debugf("gemini: completed, content: '%s' (%d chars)",
				contentPreview, len(responseContent))
		}
	}()

	return processor.ProcessMessagesToEvents(messageChannel)
}

// ConvertToGeminiSchema converts a generic JSON schema to Gemini's format
func ConvertToGeminiSchema(schema *Schema) *genai.Schema {
	if schema == nil {
		return nil
	}

	return convertJSONSchemaToGemini(schema.Raw)
}

func convertJSONSchemaToGemini(schemaMap map[string]any) *genai.Schema {
	geminiSchema := &genai.Schema{}

	// Convert type
	if typeStr, ok := schemaMap["type"].(string); ok {
		switch typeStr {
		case "object":
			geminiSchema.Type = genai.TypeObject
		case "array":
			geminiSchema.Type = genai.TypeArray
		case "string":
			geminiSchema.Type = genai.TypeString
		case "number":
			geminiSchema.Type = genai.TypeNumber
		case "integer":
			geminiSchema.Type = genai.TypeInteger
		case "boolean":
			geminiSchema.Type = genai.TypeBoolean
		default:
			geminiSchema.Type = genai.TypeString
		}
	}

	// Convert properties for objects
	if props, ok := schemaMap["properties"].(map[string]any); ok {
		geminiSchema.Properties = make(map[string]*genai.Schema)
		for key, value := range props {
			if propMap, ok := value.(map[string]any); ok {
				geminiSchema.Properties[key] = convertJSONSchemaToGemini(propMap)
			}
		}
	}

	// Convert required fields
	if required, ok := schemaMap["required"].([]any); ok {
		for _, field := range required {
			if fieldStr, ok := field.(string); ok {
				geminiSchema.Required = append(geminiSchema.Required, fieldStr)
			}
		}
	}

	// Convert items for arrays
	if items, ok := schemaMap["items"].(map[string]any); ok {
		geminiSchema.Items = convertJSONSchemaToGemini(items)
	}

	// Convert description
	if desc, ok := schemaMap["description"].(string); ok {
		geminiSchema.Description = desc
	}

	// Convert enum
	if enum, ok := schemaMap["enum"].([]any); ok {
		for _, val := range enum {
			if strVal, ok := val.(string); ok {
				geminiSchema.Enum = append(geminiSchema.Enum, strVal)
			}
		}
	}

	return geminiSchema
}

// convertSchemaToGeminiSchema recursively converts an MCP schema to a Gemini schema
func convertSchemaToGeminiSchema(schema *mcpjsonschema.Schema) *genai.Schema {
	if schema == nil {
		return nil
	}

	geminiSchema := &genai.Schema{
		Description: schema.Description,
	}

	// Map type
	switch schema.Type {
	case "string":
		geminiSchema.Type = genai.TypeString
	case "number":
		geminiSchema.Type = genai.TypeNumber
	case "boolean":
		geminiSchema.Type = genai.TypeBoolean
	case "array":
		geminiSchema.Type = genai.TypeArray
		// Handle array items
		if schema.Items != nil {
			geminiSchema.Items = convertSchemaToGeminiSchema(schema.Items)
		}
	case "object":
		geminiSchema.Type = genai.TypeObject
		// Handle nested object properties recursively
		if schema.Properties != nil {
			props := make(map[string]*genai.Schema)
			for name, prop := range schema.Properties {
				if prop != nil {
					props[name] = convertSchemaToGeminiSchema(prop)
				}
			}
			geminiSchema.Properties = props
		}
		if len(schema.Required) > 0 {
			geminiSchema.Required = schema.Required
		}
	default:
		// Default to string for unknown types
		geminiSchema.Type = genai.TypeString
	}

	return geminiSchema
}

// ConvertToolToGemini converts a generic tool schema to Gemini format
func ConvertToolToGemini(schema *mcpjsonschema.Schema) *genai.Tool {
	// Convert properties to Gemini schema format using recursive conversion
	props := make(map[string]*genai.Schema)

	if schema != nil && schema.Properties != nil {
		for name, prop := range schema.Properties {
			if prop != nil {
				props[name] = convertSchemaToGeminiSchema(prop)
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

	return &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{{
			Name:        name,
			Description: description,
			Parameters: &genai.Schema{
				Type:       genai.TypeObject,
				Properties: props,
				Required:   required,
			},
		}},
	}
}

// MessagesToGeminiContent converts messages to Gemini content format
func MessagesToGeminiContent(msgs []messages.ChatMessage) ([]*genai.Content, string, map[string]string) {
	var history []*genai.Content
	var systemInstruction string
	callIDToName := make(map[string]string)

	for _, msg := range msgs {
		switch msg.Role {
		case messages.MessageRoleSystem:
			systemInstruction = msg.Content

		case messages.MessageRoleUser:
			// Handle multimodal content
			if len(msg.Parts) > 0 {
				var parts []*genai.Part
				for _, part := range msg.Parts {
					switch part.Type {
					case "text":
						parts = append(parts, &genai.Part{Text: part.Text})
					case "image_base64":
						// Decode base64 to bytes
						imageData, err := base64.StdEncoding.DecodeString(part.ImageData)
						if err == nil {
							parts = append(parts, &genai.Part{
								InlineData: &genai.Blob{
									MIMEType: part.MimeType,
									Data:     imageData,
								},
							})
						}
					case "image_url":
						// Gemini doesn't directly support URLs, would need to download
						// For now, skip URL images for Gemini
					}
				}
				if len(parts) > 0 {
					history = append(history, &genai.Content{
						Role:  "user",
						Parts: parts,
					})
				}
			} else if msg.Content != "" {
				// Backward compatibility: simple text content
				history = append(history, &genai.Content{
					Role:  "user",
					Parts: []*genai.Part{{Text: msg.Content}},
				})
			}

		case messages.MessageRoleAssistant:
			var parts []*genai.Part
			if msg.Content != "" {
				parts = append(parts, &genai.Part{Text: msg.Content})
			}
			if len(msg.ToolCalls) > 0 {
				for _, tc := range msg.ToolCalls {
					if tc.ID != "" {
						callIDToName[tc.ID] = tc.Name
					}
					var args map[string]any
					if err := json.Unmarshal([]byte(tc.Arguments), &args); err == nil {
						part := &genai.Part{
							FunctionCall: &genai.FunctionCall{
								Name: tc.Name,
								Args: args,
							},
						}

						// Check metadata for thought signature
						if msg.Metadata != nil {
							if signatures, ok := msg.Metadata["gemini_thought_signatures"].(map[string]string); ok {
								if sigStr, exists := signatures[tc.ID]; exists {
									if sig, err := base64.StdEncoding.DecodeString(sigStr); err == nil {
										part.ThoughtSignature = sig
									}
								}
							}
						}

						parts = append(parts, part)
					}
				}
			}
			if len(parts) > 0 {
				history = append(history, &genai.Content{
					Role:  "model",
					Parts: parts,
				})
			}

		case messages.MessageRoleTool:
			funcName := ""
			if msg.ToolCallID != "" {
				funcName = callIDToName[msg.ToolCallID]
			}

			var output any
			if err := json.Unmarshal([]byte(msg.Content), &output); err != nil {
				output = msg.Content
			}

			// Ensure output is a map[string]any as required by genai
			var response map[string]any
			if m, ok := output.(map[string]any); ok {
				response = m
			} else {
				response = map[string]any{"result": output}
			}
			history = append(history, &genai.Content{
				Role: "user",
				Parts: []*genai.Part{{
					FunctionResponse: &genai.FunctionResponse{
						Name:     funcName,
						Response: response,
					},
				}},
			})
		}
	}

	return history, systemInstruction, callIDToName
}
