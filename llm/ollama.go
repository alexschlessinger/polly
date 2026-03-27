package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/alexschlessinger/pollytool/llm/adapters"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
	ollamaapi "github.com/ollama/ollama/api"
	"go.uber.org/zap"
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
		zap.S().Debugw("ollama_invalid_url", "url", baseURL, "error", err)
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
		zap.S().Debugw("ollama_bearer_auth_enabled")
	}

	client := ollamaapi.NewClient(u, httpClient)

	return &OllamaClient{
		client: client,
	}
}

// ChatCompletionStream implements the event-based streaming interface
func (o *OllamaClient) ChatCompletionStream(ctx context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	return runStream(ctx, processor, adapters.NewOllamaAdapter(), func(streamCore *streaming.StreamingCore) {
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
		// Default to non-streaming for Ollama (nil means streaming in Ollama API)
		stream := req.Stream
		if stream == nil {
			streamFalse := false
			stream = &streamFalse
		}
		chatReq := &ollamaapi.ChatRequest{
			Model:    req.Model,
			Messages: ollamaMessages,
			Stream:   stream,
			Options: map[string]any{
				"temperature": req.Temperature,
				"num_predict": req.MaxTokens,
			},
		}

		// Enable thinking for supported models if requested
		if req.ThinkingEffort.IsEnabled() {
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

		// For Ollama, we default to non-streaming (stream is already set above)
		isStreaming := *stream
		zap.S().Debugw("ollama_chat_started", "model", req.Model, "stream", isStreaming)

		// Track whether we've seen thinking content.
		// Some models output content before thinking, then repeat it after.
		// We only want to emit content that appears AFTER thinking has started.
		var sawThinking bool
		thinkingEnabled := req.ThinkingEffort.IsEnabled()

		// Execute chat - the callback is called for each streamed chunk (or once if non-streaming).
		err := o.client.Chat(ctx, chatReq, func(resp ollamaapi.ChatResponse) error {
			// Process the chunk through the adapter
			if err := streamCore.ProcessChunk(&resp); err != nil {
				return err
			}

			if isStreaming {
				// Streaming mode: emit tokens incrementally, skip final chunk which contains full content
				if resp.Message.Thinking != "" && !resp.Done {
					sawThinking = true
					streamCore.EmitReasoning(resp.Message.Thinking)
				}

				// When thinking is enabled, only emit content AFTER thinking has started
				// to avoid duplicate content (some models output content before AND after thinking)
				if resp.Message.Content != "" && !resp.Done {
					if !thinkingEnabled || sawThinking {
						streamCore.EmitContent(resp.Message.Content)
					}
				}
			} else {
				// Non-streaming mode: callback is called once with complete response
				if resp.Message.Thinking != "" {
					streamCore.EmitReasoning(resp.Message.Thinking)
				}
				if resp.Message.Content != "" {
					streamCore.EmitContent(resp.Message.Content)
				}
			}

			return nil
		})

		if err != nil {
			zap.S().Debugw("ollama_chat_error", "error", err)
			streamCore.EmitError(err)
			return
		}

		// Send the final message with accumulated state
		streamCore.Complete()
	})
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

// ConvertToolToOllama converts a tool schema to Ollama native format.
// Ollama's ToolFunctionParameters has custom UnmarshalJSON, so we round-trip
// through JSON to let the SDK handle the conversion.
func ConvertToolToOllama(schema *ToolSchema) ollamaapi.Tool {
	var params ollamaapi.ToolFunctionParameters
	if schema != nil {
		if b, err := json.Marshal(schema.Raw); err == nil {
			json.Unmarshal(b, &params)
		}
	}

	name, description := "", ""
	if schema != nil {
		name = schema.Title()
		description = schema.Description()
	}

	return ollamaapi.Tool{
		Type: "function",
		Function: ollamaapi.ToolFunction{
			Name:        name,
			Description: description,
			Parameters:  params,
		},
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
					tcArgs := ollamaapi.NewToolCallFunctionArguments()
					for k, v := range args {
						tcArgs.Set(k, v)
					}
					ollamaToolCalls = append(ollamaToolCalls, ollamaapi.ToolCall{
						Function: ollamaapi.ToolCallFunction{
							Name:      tc.Name,
							Arguments: tcArgs,
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
			ollamaMsg.ToolName = msg.ToolName
		}

		ollamaMessages = append(ollamaMessages, ollamaMsg)
	}

	return ollamaMessages
}
