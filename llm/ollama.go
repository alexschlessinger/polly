package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/adapters"
	"github.com/alexschlessinger/pollytool/llm/ollama"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

type OllamaClient struct {
	client *ollama.Client
}

// authTransport adds Bearer token authentication to HTTP requests
type authTransport struct {
	Token string
	Base  http.RoundTripper
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// RoundTrippers must not mutate the caller's request.
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.Token)
	return t.Base.RoundTrip(clone)
}

func NewOllamaClient(baseURL string, apiKey string) *OllamaClient {
	// Parse URL and create client
	u, err := url.Parse(baseURL)
	if err != nil {
		slog.Debug("ollama_invalid_url", "url", baseURL, "error", err)
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
		slog.Debug("ollama_bearer_auth_enabled")
	}

	client := ollama.NewClient(u, httpClient)

	return &OllamaClient{
		client: client,
	}
}

// ChatCompletionStream implements the event-based streaming interface
func (o *OllamaClient) ChatCompletionStream(ctx context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	return runStream(ctx, req.Timeout, req.Deadline, processor, adapters.NewOllamaAdapter(), func(ctx context.Context, streamCore *streaming.StreamingCore) {
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
				ollamaMessages = append([]ollama.Message{{
					Role:    "system",
					Content: schemaPrompt,
				}}, ollamaMessages...)
			}
		}

		// Create chat request. nil means streaming, per the CompletionRequest
		// contract; the value is sent explicitly so the request body states
		// the mode either way.
		stream := req.Stream
		if stream == nil {
			streamTrue := true
			stream = &streamTrue
		}
		options := map[string]any{
			"num_predict": req.MaxTokens,
		}
		if req.Temperature != nil {
			options["temperature"] = *req.Temperature
		}
		chatReq := &ollama.ChatRequest{
			Model:    req.Model,
			Messages: ollamaMessages,
			Stream:   stream,
			Options:  options,
		}

		// Enable thinking for supported models if requested
		if req.ThinkingEffort.IsEnabled() {
			// Ollama's think field can be bool or string
			// For now, we'll use boolean true for any effort level
			// Some models may support string values like "low", "medium", "high"
			chatReq.Think = true
		}

		// Pass the schema itself as the format so the server constrains
		// decoding to it; the prompt above still describes it to the model.
		// Plain "json" mode would accept any object, {} included.
		if req.ResponseSchema != nil {
			chatReq.Format = json.RawMessage(`"json"`)
			if format, err := json.Marshal(req.ResponseSchema.Raw); err == nil && len(req.ResponseSchema.Raw) > 0 {
				chatReq.Format = format
			}
		}

		// Add tool support if available
		if len(req.Tools) > 0 {
			var ollamaTools []ollama.Tool
			for _, tool := range req.Tools {
				ollamaTools = append(ollamaTools, ConvertToolToOllama(tool.GetSchema()))
			}
			chatReq.Tools = ollamaTools
		}

		isStreaming := *stream
		slog.Debug("ollama_chat_started", "model", req.Model, "stream", isStreaming)

		// Some models output content before thinking, then repeat it after.
		// With thinking on, content that arrives before any thinking chunk is
		// held back: it is dropped once thinking starts (the repeat is what
		// shows), and flushed at the end when no thinking ever came, so a
		// model that simply answers is not silenced.
		var sawThinking bool
		var heldContent strings.Builder
		thinkingEnabled := req.ThinkingEffort.IsEnabled()

		// Execute chat - the callback is called for each streamed chunk (or once if non-streaming).
		err := o.client.Chat(ctx, chatReq, func(resp ollama.ChatResponse) error {
			// Process the chunk through the adapter
			if err := streamCore.ProcessChunk(&resp); err != nil {
				return err
			}

			if isStreaming {
				// Streaming mode: emit tokens incrementally, skip final chunk which contains full content
				if resp.Message.Thinking != "" && !resp.Done {
					if !sawThinking {
						sawThinking = true
						heldContent.Reset()
					}
					streamCore.EmitReasoning(resp.Message.Thinking)
				}

				if resp.Message.Content != "" && !resp.Done {
					if !thinkingEnabled || sawThinking {
						streamCore.EmitContent(resp.Message.Content)
					} else {
						heldContent.WriteString(resp.Message.Content)
					}
				}
				if resp.Done && heldContent.Len() > 0 {
					streamCore.EmitContent(heldContent.String())
					heldContent.Reset()
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
			slog.Debug("ollama_chat_error", "error", err)
			streamCore.EmitError(err)
			return
		}

		// Send the final message with accumulated state
		streamCore.CompleteStream()
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
func ConvertToolToOllama(schema *ToolSchema) ollama.Tool {
	var params ollama.ToolParameters
	if schema != nil {
		params.Type = "object"
		if t, ok := schema.Raw["type"].(string); ok && t != "" {
			params.Type = t
		}
		params.Required = schema.Required()
		params.Properties = schema.Properties()
	}

	name, description := "", ""
	if schema != nil {
		name = schema.Title()
		description = schema.Description()
	}

	return ollama.Tool{
		Type: "function",
		Function: ollama.ToolFunction{
			Name:        name,
			Description: description,
			Parameters:  params,
		},
	}
}

// nativeOllamaCallID returns the provider-issued tool call ID, or "" when the
// ID is one polly synthesized (call_<nonce>_<n>) for internal pairing and
// must not be echoed back to the API.
func nativeOllamaCallID(id string) string {
	if strings.HasPrefix(id, "call_") {
		return ""
	}
	return id
}

// MessagesToOllama converts messages to Ollama format
func MessagesToOllama(msgs []messages.ChatMessage) []ollama.Message {
	var ollamaMessages []ollama.Message

	for _, msg := range msgs {
		ollamaMsg := ollama.Message{
			Role: msg.Role,
		}

		// Handle multimodal content
		if len(msg.Parts) > 0 {
			var textContent string
			var imageData []ollama.ImageData

			for _, part := range msg.Parts {
				switch part.Type {
				case "text":
					textContent += part.Text
				case "image_base64":
					// Ollama expects raw bytes, not base64
					decoded, err := base64.StdEncoding.DecodeString(part.ImageData)
					if err == nil {
						imageData = append(imageData, ollama.ImageData(decoded))
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
			var ollamaToolCalls []ollama.ToolCall
			for _, tc := range msg.ToolCalls {
				var args map[string]any
				if err := json.Unmarshal([]byte(tc.Arguments), &args); err == nil {
					ollamaToolCalls = append(ollamaToolCalls, ollama.ToolCall{
						ID: nativeOllamaCallID(tc.ID),
						Function: ollama.ToolCallFunction{
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
			ollamaMsg.ToolName = msg.ToolName
		}

		ollamaMessages = append(ollamaMessages, ollamaMsg)
	}

	return ollamaMessages
}
