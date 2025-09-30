package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/google/generative-ai-go/genai"
	mcpjsonschema "github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/api/option"
)

type GeminiClient struct {
	apiKey string
}

func NewGeminiClient(apiKey string) *GeminiClient {
	if apiKey == "" {
		log.Println("gemini: warning - no API key configured")
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
		client, err := genai.NewClient(ctx, option.WithAPIKey(g.apiKey))
		if err != nil {
			log.Printf("gemini: failed to create client: %v", err)
			messageChannel <- messages.ChatMessage{
				Role:    messages.MessageRoleAssistant,
				Content: "Error creating Gemini client: " + err.Error(),
			}
			return
		}
		defer client.Close()

		// Get the model
		model := client.GenerativeModel(req.Model)

		// Configure model parameters
		model.SetTemperature(req.Temperature)
		model.SetMaxOutputTokens(int32(req.MaxTokens))

		// Add structured output support
		if req.ResponseSchema != nil {
			model.ResponseMIMEType = "application/json"
			model.ResponseSchema = ConvertToGeminiSchema(req.ResponseSchema)
		}

		// Convert session history to Gemini chat history
		history, systemInstruction, _ := MessagesToGeminiContent(req.Messages)

		// Extract the last user message parts if present
		var userParts []genai.Part
		if len(req.Messages) > 0 {
			lastMsg := req.Messages[len(req.Messages)-1]
			switch lastMsg.Role {
			case messages.MessageRoleUser:
				// Use the already converted parts from history if available
				if len(history) > 0 && history[len(history)-1].Role == "user" {
					userParts = history[len(history)-1].Parts
					history = history[:len(history)-1]
				} else if lastMsg.Content != "" {
					// Fallback to simple content if no history
					userParts = append(userParts, genai.Text(lastMsg.Content))
				}
			case messages.MessageRoleAssistant:
				// If the last message is from assistant, we need to add empty user message
				userParts = append(userParts, genai.Text(""))
			}
		}

		// Set system instruction if provided
		if systemInstruction != "" {
			model.SystemInstruction = &genai.Content{
				Parts: []genai.Part{genai.Text(systemInstruction)},
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
			model.Tools = []*genai.Tool{
				{FunctionDeclarations: geminiFuncs},
			}
		}

		// Start a chat session with history
		chat := model.StartChat()
		chat.History = history

		log.Printf("gemini: sending streaming request to model %s", req.Model)

		// Send message and get streaming response
		iter := chat.SendMessageStream(ctx, userParts...)

		// Process the stream
		var responseContent string
		var toolCalls []messages.ChatMessageToolCall

		for {
			resp, err := iter.Next()
			if err != nil {
				// Check if we're done iterating
				if err.Error() == "no more items in iterator" {
					break
				}
				log.Printf("gemini: stream error: %v", err)
				messageChannel <- messages.ChatMessage{
					Role:    messages.MessageRoleAssistant,
					Content: "Error: " + err.Error(),
				}
				return
			}

			// Process each candidate's parts
			if len(resp.Candidates) > 0 {
				candidate := resp.Candidates[0]
				for _, part := range candidate.Content.Parts {
					switch p := part.(type) {
					case genai.Text:
						text := string(p)
						responseContent += text
						// Stream partial content
						messageChannel <- messages.ChatMessage{
							Role:    messages.MessageRoleAssistant,
							Content: text,
						}
					case genai.FunctionCall:
						// Accumulate tool calls
						argsJSON, _ := json.Marshal(p.Args)
						toolCalls = append(toolCalls, messages.ChatMessageToolCall{
							ID:        fmt.Sprintf("gemini-%d", len(toolCalls)),
							Name:      p.Name,
							Arguments: string(argsJSON),
						})
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
			log.Printf("gemini: completed, content: '%s' (%d chars), tool calls: %d %v",
				contentPreview, len(responseContent), len(toolCalls), toolInfo)
		} else {
			log.Printf("gemini: completed, content: '%s' (%d chars)",
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
				var parts []genai.Part
				for _, part := range msg.Parts {
					switch part.Type {
					case "text":
						parts = append(parts, genai.Text(part.Text))
					case "image_base64":
						// Decode base64 to bytes
						imageData, err := base64.StdEncoding.DecodeString(part.ImageData)
						if err == nil {
							parts = append(parts, genai.Blob{
								MIMEType: part.MimeType,
								Data:     imageData,
							})
						}
					case "image_url":
						// Gemini doesn't directly support URLs, would need to download
						// For now, skip URL images for Gemini
					}
				}
				if len(parts) > 0 {
					history = append(history, genai.NewUserContent(parts...))
				}
			} else if msg.Content != "" {
				// Backward compatibility: simple text content
				history = append(history, genai.NewUserContent(genai.Text(msg.Content)))
			}

		case messages.MessageRoleAssistant:
			var parts []genai.Part
			if msg.Content != "" {
				parts = append(parts, genai.Text(msg.Content))
			}
			if len(msg.ToolCalls) > 0 {
				for _, tc := range msg.ToolCalls {
					if tc.ID != "" {
						callIDToName[tc.ID] = tc.Name
					}
					var args map[string]any
					if err := json.Unmarshal([]byte(tc.Arguments), &args); err == nil {
						parts = append(parts, genai.FunctionCall{
							Name: tc.Name,
							Args: args,
						})
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
			history = append(history, genai.NewUserContent(genai.FunctionResponse{
				Name:     funcName,
				Response: response,
			}))
		}
	}

	return history, systemInstruction, callIDToName
}

// MessageFromGeminiCandidate converts a Gemini candidate to our message format
func MessageFromGeminiCandidate(candidate *genai.Candidate) messages.ChatMessage {
	msg := messages.ChatMessage{
		Role: messages.MessageRoleAssistant,
	}

	if candidate.Content != nil {
		for _, part := range candidate.Content.Parts {
			switch p := part.(type) {
			case genai.Text:
				msg.Content += string(p)
			case genai.FunctionCall:
				argsJSON, _ := json.Marshal(p.Args)
				msg.ToolCalls = append(msg.ToolCalls, messages.ChatMessageToolCall{
					ID:        "", // Gemini doesn't use IDs
					Name:      p.Name,
					Arguments: string(argsJSON),
				})
			}
		}
	}

	return msg
}
