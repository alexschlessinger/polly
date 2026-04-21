package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/alexschlessinger/pollytool/llm/adapters"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
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
	return runStream(ctx, processor, adapters.NewGeminiAdapter(), func(streamCore *streaming.StreamingCore) {
		client := g.client

		// Convert session history to Gemini chat history
		contents, systemInstruction, _ := MessagesToGeminiContent(req.Messages)

		// Configure model parameters
		maxTokens := int32(req.MaxTokens)

		config := &genai.GenerateContentConfig{
			MaxOutputTokens: maxTokens,
		}
		if req.Temperature != nil {
			temp := *req.Temperature
			config.Temperature = &temp
		}

		// Add structured output support. Preview models (3.x) silently ignore
		// ResponseJsonSchema, so route through the typed ResponseSchema path
		// (the SDK's canonical structured-output mechanism) instead.
		if req.ResponseSchema != nil {
			config.ResponseMIMEType = "application/json"
			config.ResponseSchema = jsonSchemaToGeminiSchema(req.ResponseSchema.Raw)
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
				if geminiTool != nil && len(geminiTool.FunctionDeclarations) > 0 {
					geminiFuncs = append(geminiFuncs, geminiTool.FunctionDeclarations...)
				}
			}
			config.Tools = []*genai.Tool{
				{FunctionDeclarations: geminiFuncs},
			}
		}

		isStreaming := req.Stream == nil || *req.Stream
		// Force non-streaming for structured output: streaming + responseSchema
		// is unreliable on preview models (3.x), which happily emit a prose
		// preamble like "Here is the JSON" before any object — and with
		// --maxtokens caps, the JSON often never arrives. The non-streaming
		// path applies the schema constraint to the full response in one shot.
		if req.ResponseSchema != nil {
			isStreaming = false
		}
		slog.Debug("gemini_completion_started", "model", req.Model, "stream", isStreaming)

		if isStreaming {
			g.handleStreamingCompletion(ctx, client, req, contents, config, streamCore)
		} else {
			g.handleNonStreamingCompletion(ctx, client, req, contents, config, streamCore)
		}
	})
}

// handleStreamingCompletion handles streaming Gemini API requests
func (g *GeminiClient) handleStreamingCompletion(ctx context.Context, client *genai.Client, req *CompletionRequest, contents []*genai.Content, config *genai.GenerateContentConfig, streamCore *streaming.StreamingCore) {
	iter := client.Models.GenerateContentStream(ctx, req.Model, contents, config)

	for resp, err := range iter {
		if err != nil {
			slog.Debug("gemini_stream_error", "error", err)
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
		slog.Debug("gemini_completion_failed", "error", err)
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

// jsonSchemaToGeminiSchema converts a JSON Schema map (as parsed from a
// user-supplied schema file) to the genai SDK's typed Schema. The typed path
// is enforced by Gemini's structured-output backend; the JSON-schema-shaped
// alternative ResponseJsonSchema is silently ignored on preview models.
// Only the subset of JSON Schema that maps cleanly to genai.Schema is handled
// — that's enough for the structured-output feature polly exposes.
func jsonSchemaToGeminiSchema(raw map[string]any) *genai.Schema {
	if raw == nil {
		return nil
	}
	out := &genai.Schema{}
	if t, ok := raw["type"].(string); ok {
		switch t {
		case "string":
			out.Type = genai.TypeString
		case "number":
			out.Type = genai.TypeNumber
		case "integer":
			out.Type = genai.TypeInteger
		case "boolean":
			out.Type = genai.TypeBoolean
		case "array":
			out.Type = genai.TypeArray
		case "object":
			out.Type = genai.TypeObject
		case "null":
			out.Type = genai.TypeNULL
		}
	}
	if d, ok := raw["description"].(string); ok {
		out.Description = d
	}
	if title, ok := raw["title"].(string); ok {
		out.Title = title
	}
	if format, ok := raw["format"].(string); ok {
		out.Format = format
	}
	if enum, ok := raw["enum"].([]any); ok {
		for _, e := range enum {
			if s, ok := e.(string); ok {
				out.Enum = append(out.Enum, s)
			}
		}
	}
	if items, ok := raw["items"].(map[string]any); ok {
		out.Items = jsonSchemaToGeminiSchema(items)
	}
	if props, ok := raw["properties"].(map[string]any); ok {
		out.Properties = make(map[string]*genai.Schema, len(props))
		for name, p := range props {
			if pm, ok := p.(map[string]any); ok {
				out.Properties[name] = jsonSchemaToGeminiSchema(pm)
			}
		}
	}
	if req, ok := raw["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				out.Required = append(out.Required, s)
			}
		}
	} else if req, ok := raw["required"].([]string); ok {
		out.Required = append(out.Required, req...)
	}
	return out
}

// ConvertToolToGemini converts a tool schema to Gemini format.
// Gemini's FunctionDeclaration.ParametersJsonSchema accepts any, so we pass a raw map.
// We strip title/description since those are set on the FunctionDeclaration itself.
func ConvertToolToGemini(schema *ToolSchema) *genai.Tool {
	if schema == nil {
		return &genai.Tool{FunctionDeclarations: []*genai.FunctionDeclaration{{}}}
	}
	// Build a parameters-only schema without title/description metadata.
	params := map[string]any{"type": "object", "properties": schema.Properties()}
	if req := schema.Required(); len(req) > 0 {
		params["required"] = req
	}
	return &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{{
			Name:                 schema.Title(),
			Description:          schema.Description(),
			ParametersJsonSchema: params,
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
