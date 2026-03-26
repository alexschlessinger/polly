package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/alexschlessinger/pollytool/llm/adapters"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
	"go.uber.org/zap"
	"google.golang.org/genai"
)

type GeminiClient struct {
	client *genai.Client
}

func NewGeminiClient(apiKey string) (*GeminiClient, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("gemini API key not configured")
	}

	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("creating gemini client: %w", err)
	}

	return &GeminiClient{client: client}, nil
}

// ChatCompletionStream implements the event-based streaming interface
func (g *GeminiClient) ChatCompletionStream(ctx context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	messageChannel := make(chan messages.ChatMessage, 10)

	go func() {
		defer close(messageChannel)

		// Create streaming core with Gemini adapter
		adapter := adapters.NewGeminiAdapter()
		streamCore := streaming.NewStreamingCore(ctx, messageChannel, adapter)

		client := g.client

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

		isStreaming := req.Stream == nil || *req.Stream
		zap.S().Debugw("gemini_completion_started", "model", req.Model, "stream", isStreaming)

		if isStreaming {
			g.handleStreamingCompletion(ctx, client, req, contents, config, streamCore)
		} else {
			g.handleNonStreamingCompletion(ctx, client, req, contents, config, streamCore)
		}
	}()

	return processor.ProcessMessagesToEvents(messageChannel)
}

// handleStreamingCompletion handles streaming Gemini API requests
func (g *GeminiClient) handleStreamingCompletion(ctx context.Context, client *genai.Client, req *CompletionRequest, contents []*genai.Content, config *genai.GenerateContentConfig, streamCore *streaming.StreamingCore) {
	iter := client.Models.GenerateContentStream(ctx, req.Model, contents, config)

	for resp, err := range iter {
		if err != nil {
			zap.S().Debugw("gemini_stream_error", "error", err)
			streamCore.EmitError(err)
			return
		}

		// Process the chunk through the adapter
		if err := streamCore.ProcessChunk(resp); err != nil {
			streamCore.EmitError(err)
			return
		}

		// Emit content from each part
		if len(resp.Candidates) > 0 {
			candidate := resp.Candidates[0]
			if candidate.Content != nil {
				for _, part := range candidate.Content.Parts {
					if part.Text != "" {
						streamCore.EmitContent(part.Text)
					}
				}
			}
		}
	}

	streamCore.Complete()
}

// handleNonStreamingCompletion handles non-streaming Gemini API requests
func (g *GeminiClient) handleNonStreamingCompletion(ctx context.Context, client *genai.Client, req *CompletionRequest, contents []*genai.Content, config *genai.GenerateContentConfig, streamCore *streaming.StreamingCore) {
	resp, err := client.Models.GenerateContent(ctx, req.Model, contents, config)
	if err != nil {
		zap.S().Debugw("gemini_completion_failed", "error", err)
		streamCore.EmitError(err)
		return
	}

	// Process through adapter (handles tool calls, tokens, stop reason)
	if err := streamCore.ProcessChunk(resp); err != nil {
		streamCore.EmitError(err)
		return
	}

	// Emit content from response
	if len(resp.Candidates) > 0 {
		candidate := resp.Candidates[0]
		if candidate.Content != nil {
			for _, part := range candidate.Content.Parts {
				if part.Text != "" {
					streamCore.EmitContent(part.Text)
				}
			}
		}
	}

	streamCore.Complete()
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

// ConvertToolToGemini converts a tool schema to Gemini format
func ConvertToolToGemini(schema *ToolSchema) *genai.Tool {
	name := ""
	description := ""
	var params *genai.Schema

	if schema != nil {
		name = schema.Title()
		description = schema.Description()
		// Reuse the same map-based converter used for response schemas
		params = convertJSONSchemaToGemini(schema.Raw)
	}
	if params == nil {
		params = &genai.Schema{Type: genai.TypeObject}
	}

	return &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{{
			Name:        name,
			Description: description,
			Parameters:  params,
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
			funcName := msg.ToolName
			if funcName == "" && msg.ToolCallID != "" {
				// Fallback to map if ToolName not set (shouldn't happen)
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
