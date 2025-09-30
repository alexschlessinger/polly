package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"

	"github.com/alexschlessinger/pollytool/messages"
	mcpjsonschema "github.com/google/jsonschema-go/jsonschema"
	ollamaapi "github.com/ollama/ollama/api"
)

type OllamaClient struct {
	client *ollamaapi.Client
}

// authTransport adds Bearer token authentication to HTTP requests
type authTransport struct {
	Token string
	Base  http.RoundTripper
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+t.Token)
	return t.Base.RoundTrip(req)
}

func NewOllamaClient(baseURL string, apiKey string) *OllamaClient {
	// Parse URL and create client
	u, err := url.Parse(baseURL)
	if err != nil {
		log.Printf("ollama: invalid URL %s: %v", baseURL, err)
		// Fall back to default if parsing fails
		u, _ = url.Parse("http://localhost:11434")
	}

	// Create HTTP client with optional Bearer token authentication
	httpClient := http.DefaultClient
	if apiKey != "" {
		httpClient = &http.Client{
			Transport: &authTransport{
				Token: apiKey,
				Base:  http.DefaultTransport,
			},
		}
		log.Printf("ollama: using Bearer token authentication")
	}

	client := ollamaapi.NewClient(u, httpClient)

	return &OllamaClient{
		client: client,
	}
}


// ChatCompletionStream implements the event-based streaming interface
func (o *OllamaClient) ChatCompletionStream(ctx context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	messageChannel := make(chan messages.ChatMessage, 10)

	go func() {
		defer close(messageChannel)

		// Convert messages to Ollama format
		ollamaMessages := MessagesToOllama(req.Messages)

		// Add schema to system prompt if specified
		if req.ResponseSchema != nil {
			schemaPrompt := ConvertToOllamaFormat(req.ResponseSchema)
			// Prepend schema instruction to the first system message or add new one
			found := false
			for i, msg := range ollamaMessages {
				if msg.Role == "system" {
					ollamaMessages[i].Content = schemaPrompt + "\n\n" + msg.Content
					found = true
					break
				}
			}
			if !found {
				// Add as first message
				ollamaMessages = append([]ollamaapi.Message{{
					Role:    "system",
					Content: schemaPrompt,
				}}, ollamaMessages...)
			}
		}

		// Create chat request
		chatReq := &ollamaapi.ChatRequest{
			Model:    req.Model,
			Messages: ollamaMessages,
			Options: map[string]any{
				"temperature": req.Temperature,
				"num_predict": req.MaxTokens,
			},
		}
		
		// Enable thinking for supported models if requested
		if req.ThinkingEffort != "off" {
			// Ollama's ThinkValue can be bool or string
			// For now, we'll use boolean true for any effort level
			// Some models may support string values like "low", "medium", "high"
			thinkValue := ollamaapi.ThinkValue{Value: true}
			chatReq.Think = &thinkValue
		}

		// Set JSON format if schema is specified
		if req.ResponseSchema != nil {
			chatReq.Format = json.RawMessage(`"json"`)
		}

		// Add tool support if available
		if len(req.Tools) > 0 {
			var ollamaTools []ollamaapi.Tool
			for _, tool := range req.Tools {
				ollamaTools = append(ollamaTools, ConvertToolToOllama(tool.GetSchema()))
			}
			chatReq.Tools = ollamaTools
		}

		log.Printf("ollama: chat request to model %s", req.Model)

		// Execute chat - the callback is called for each streamed chunk.
		// Stream content chunks as they arrive and capture any tool calls the model returns.
		var (
			responseContent string
			thinkingContent string
			toolCalls       []messages.ChatMessageToolCall
		)
		err := o.client.Chat(ctx, chatReq, func(resp ollamaapi.ChatResponse) error {
			// Stream thinking tokens as they arrive
			if resp.Message.Thinking != "" {
				thinkingContent += resp.Message.Thinking
				// Send thinking update for status display
				messageChannel <- messages.ChatMessage{
					Role:      messages.MessageRoleAssistant,
					Reasoning: resp.Message.Thinking,
				}
			}
			// Stream content tokens as they arrive
			if resp.Message.Content != "" {
				responseContent += resp.Message.Content
				// Send partial content for streaming
				messageChannel <- messages.ChatMessage{
					Role:    messages.MessageRoleAssistant,
					Content: resp.Message.Content,
				}
			}
			// Record tool calls if present on this chunk (found on the message)
			if len(resp.Message.ToolCalls) > 0 {
				// Reset and capture the latest set of tool calls.
				toolCalls = toolCalls[:0]
				for _, tc := range resp.Message.ToolCalls {
					tcArgStr, _ := json.Marshal(tc.Function.Arguments)
					toolCalls = append(toolCalls, messages.ChatMessageToolCall{
						ID:        fmt.Sprintf("call_%d", len(toolCalls)),
						Name:      tc.Function.Name,
						Arguments: string(tcArgStr),
					})
				}
			}
			return nil
		})

		if err != nil {
			log.Printf("ollama: chat error: %v", err)
			messageChannel <- messages.ChatMessage{
				Role:    messages.MessageRoleAssistant,
				Content: "Error: " + err.Error(),
			}
			return
		}

		// Use the full response content
		cleanContent := responseContent

		// Send the complete message with tool calls if any
		// Don't include content since it was already streamed
		if len(toolCalls) > 0 {
			messageChannel <- messages.ChatMessage{
				Role:      messages.MessageRoleAssistant,
				Content:   "", // Content was already streamed, don't duplicate
				ToolCalls: toolCalls,
				Reasoning: "", // Reasoning was already streamed, don't duplicate
			}
		}

		// Log response details
		contentPreview := cleanContent
		if len(contentPreview) > 200 {
			contentPreview = contentPreview[:200] + "..."
		}

		if len(toolCalls) > 0 {
			toolInfo := make([]string, len(toolCalls))
			for i, tc := range toolCalls {
				toolInfo[i] = tc.Name
			}
			log.Printf("ollama: completed, content: '%s' (%d chars), tool calls: %d %v",
				contentPreview, len(cleanContent), len(toolCalls), toolInfo)
		} else {
			log.Printf("ollama: completed, content: '%s' (%d chars)",
				contentPreview, len(cleanContent))
		}
	}()

	return processor.ProcessMessagesToEvents(messageChannel)
}

// ConvertToOllamaFormat adds format instructions for Ollama
func ConvertToOllamaFormat(schema *Schema) string {
	if schema == nil {
		return ""
	}

	// For Ollama, we'll include the schema in the system prompt
	schemaJSON, _ := json.MarshalIndent(schema.Raw, "", "  ")
	return fmt.Sprintf("You must respond with JSON that matches this schema:\n%s", string(schemaJSON))
}

// convertSchemaToOllamaProperty recursively converts an MCP schema to an Ollama ToolProperty
func convertSchemaToOllamaProperty(schema *mcpjsonschema.Schema) ollamaapi.ToolProperty {
	if schema == nil {
		return ollamaapi.ToolProperty{
			Type: ollamaapi.PropertyType{"string"},
		}
	}

	ollamaProp := ollamaapi.ToolProperty{
		Type:        ollamaapi.PropertyType{schema.Type},
		Description: schema.Description,
	}

	// Handle different types
	switch schema.Type {
	case "array":
		// Handle array items
		if schema.Items != nil {
			itemsProp := convertSchemaToOllamaProperty(schema.Items)
			ollamaProp.Items = itemsProp
		} else {
			// Default to string items if not specified
			ollamaProp.Items = ollamaapi.ToolProperty{
				Type: ollamaapi.PropertyType{"string"},
			}
		}
	case "object":
		// For nested objects, we need to handle properties recursively
		// Note: Ollama's ToolProperty doesn't have a Properties field for nested objects
		// So we'll need to handle this differently or flatten the structure
		if len(schema.Properties) > 0 {
			// We can't directly set nested properties in ToolProperty
			// This is a limitation of the Ollama API structure
			// For now, we'll just mark it as object type
		}
	}

	// Handle enums if present
	if len(schema.Enum) > 0 {
		ollamaProp.Enum = schema.Enum
	}

	return ollamaProp
}

// convertPropertiesToOllamaFromSchema converts schema properties to Ollama format
func convertPropertiesToOllamaFromSchema(schema *mcpjsonschema.Schema) map[string]ollamaapi.ToolProperty {
	result := make(map[string]ollamaapi.ToolProperty)

	if schema != nil && schema.Properties != nil {
		for name, prop := range schema.Properties {
			if prop != nil {
				result[name] = convertSchemaToOllamaProperty(prop)
			}
		}
	}

	return result
}

// ConvertToolToOllama converts a generic tool schema to Ollama native format
func ConvertToolToOllama(schema *mcpjsonschema.Schema) ollamaapi.Tool {
	name := ""
	description := ""
	typeStr := "object"
	var required []string

	if schema != nil {
		name = schema.Title
		description = schema.Description
		if schema.Type != "" {
			typeStr = schema.Type
		}
		required = schema.Required
	}

	// Create the tool function
	toolFunc := ollamaapi.ToolFunction{
		Name:        name,
		Description: description,
	}

	// Set parameters
	toolFunc.Parameters.Type = typeStr
	toolFunc.Parameters.Required = required
	toolFunc.Parameters.Properties = convertPropertiesToOllamaFromSchema(schema)

	return ollamaapi.Tool{
		Type:     "function",
		Function: toolFunc,
	}
}

// MessagesToOllama converts messages to Ollama format
func MessagesToOllama(msgs []messages.ChatMessage) []ollamaapi.Message {
	var ollamaMessages []ollamaapi.Message

	for _, msg := range msgs {
		ollamaMsg := ollamaapi.Message{
			Role: msg.Role,
		}

		// Handle multimodal content
		if len(msg.Parts) > 0 {
			var textContent string
			var imageData []ollamaapi.ImageData

			for _, part := range msg.Parts {
				switch part.Type {
				case "text":
					textContent += part.Text
				case "image_base64":
					// Ollama expects raw bytes, not base64
					decoded, err := base64.StdEncoding.DecodeString(part.ImageData)
					if err == nil {
						imageData = append(imageData, ollamaapi.ImageData(decoded))
					}
				case "image_url":
					// Ollama doesn't support URLs directly
					// Would need to download and convert
				}
			}

			ollamaMsg.Content = textContent
			if len(imageData) > 0 {
				ollamaMsg.Images = imageData
			}
		} else {
			// Backward compatibility: simple text content
			ollamaMsg.Content = msg.Content
		}

		if msg.Role == messages.MessageRoleAssistant && len(msg.ToolCalls) > 0 {
			var ollamaToolCalls []ollamaapi.ToolCall
			for _, tc := range msg.ToolCalls {
				var args map[string]any
				if err := json.Unmarshal([]byte(tc.Arguments), &args); err == nil {
					ollamaToolCalls = append(ollamaToolCalls, ollamaapi.ToolCall{
						Function: ollamaapi.ToolCallFunction{
							Name:      tc.Name,
							Arguments: args,
						},
					})
				}
			}
			ollamaMsg.ToolCalls = ollamaToolCalls
		}

		// Handle tool response messages
		if msg.Role == messages.MessageRoleTool {
			// Ollama expects tool responses to have "tool" role
			ollamaMsg.Role = "tool"
		}

		ollamaMessages = append(ollamaMessages, ollamaMsg)
	}

	return ollamaMessages
}
