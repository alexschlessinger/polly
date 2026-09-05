package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/adapters"
	"github.com/alexschlessinger/pollytool/llm/gemini"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

type GeminiClient struct {
	client *gemini.Client
}

func NewGeminiClient(apiKey string) (*GeminiClient, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("gemini API key not configured")
	}
	return &GeminiClient{client: gemini.NewClient(apiKey)}, nil
}

// geminiThinkingConfig builds Gemini's thinking configuration from a
// provider-agnostic effort. Gemini 3.x uses a ThinkingLevel enum (no xhigh/max,
// so those clamp to high); Gemini 2.5 uses an integer ThinkingBudget where -1
// means dynamic. Callers must guard with ThinkingEffort.IsEnabled().
func geminiThinkingConfig(effort ThinkingEffort, model string) *gemini.ThinkingConfig {
	cfg := &gemini.ThinkingConfig{IncludeThoughts: true}

	if strings.HasPrefix(model, "gemini-3") {
		// 3.x: enum levels. Dynamic leaves the level unset (model default).
		if !effort.IsDynamic() {
			switch effort.AsLevel(LevelMedium) {
			case LevelMinimal:
				cfg.ThinkingLevel = gemini.ThinkingLevelMinimal
			case LevelLow:
				cfg.ThinkingLevel = gemini.ThinkingLevelLow
			case LevelMedium:
				cfg.ThinkingLevel = gemini.ThinkingLevelMedium
			default: // high, xhigh, max all clamp to high (Gemini's ceiling)
				cfg.ThinkingLevel = gemini.ThinkingLevelHigh
			}
		}
		return cfg
	}

	// 2.5 and older: integer budget. Dynamic uses -1 (model-managed).
	var budget int32
	if b, ok := effort.AsBudget(); ok {
		budget = clampGeminiBudget(int32(b), model)
	} else {
		budget = -1
	}
	cfg.ThinkingBudget = &budget
	return cfg
}

// clampGeminiBudget keeps a 2.5-family thinking budget within the model's
// documented range. Pro cannot fully disable thinking (floor 128); Flash can.
func clampGeminiBudget(budget int32, model string) int32 {
	var lo, hi int32 = 0, 24576 // Flash family
	if strings.Contains(model, "pro") {
		lo, hi = 128, 32768 // Pro family
	}
	if budget < lo {
		budget = lo
	}
	if budget > hi {
		budget = hi
	}
	return budget
}

// ChatCompletionStream implements the event-based streaming interface
func (g *GeminiClient) ChatCompletionStream(ctx context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	return runStream(ctx, req.Timeout, req.Deadline, processor, adapters.NewGeminiAdapter(), func(ctx context.Context, streamCore *streaming.StreamingCore) {
		// Convert session history to Gemini chat history
		contents, systemInstruction, _ := messagesToGeminiContent(req.Messages, requestProviderReplayCache(req))

		// Configure model parameters
		config := &gemini.GenerationConfig{
			MaxOutputTokens: int32(req.MaxTokens),
		}
		if req.Temperature != nil {
			temp := *req.Temperature
			config.Temperature = &temp
		}

		// Thinking: map the provider-agnostic effort onto Gemini's config and
		// request thought summaries so reasoning streams back (IncludeThoughts);
		// without it the model thinks silently and the stream stays empty until
		// the first answer token.
		if req.ThinkingEffort.IsEnabled() {
			config.ThinkingConfig = geminiThinkingConfig(req.ThinkingEffort, req.Model)
		}

		// Add structured output support. Preview models (3.x) silently ignore
		// the responseJsonSchema field, so route through the typed
		// responseSchema path (the API's canonical structured-output
		// mechanism) instead.
		if req.ResponseSchema != nil {
			config.ResponseMIMEType = "application/json"
			config.ResponseSchema = jsonSchemaToGeminiSchema(req.ResponseSchema.Raw)
		}

		genReq := &gemini.GenerateContentRequest{
			Contents:         contents,
			GenerationConfig: config,
		}

		// System instruction
		if systemInstruction != "" {
			genReq.SystemInstruction = &gemini.Content{
				Parts: []*gemini.Part{{Text: systemInstruction}},
			}
		}

		// Add tool support if available
		if len(req.Tools) > 0 {
			var geminiFuncs []*gemini.FunctionDeclaration
			for _, tool := range req.Tools {
				geminiTool := ConvertToolToGemini(tool.GetSchema())
				if geminiTool != nil && len(geminiTool.FunctionDeclarations) > 0 {
					geminiFuncs = append(geminiFuncs, geminiTool.FunctionDeclarations...)
				}
			}
			genReq.Tools = []*gemini.Tool{
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
			g.handleStreamingCompletion(ctx, req, genReq, streamCore)
		} else {
			g.handleNonStreamingCompletion(ctx, req, genReq, streamCore)
		}
	})
}

// handleStreamingCompletion handles streaming Gemini API requests
func (g *GeminiClient) handleStreamingCompletion(ctx context.Context, req *CompletionRequest, genReq *gemini.GenerateContentRequest, streamCore *streaming.StreamingCore) {
	iter := g.client.GenerateContentStream(ctx, req.Model, genReq)

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

		emitGeminiParts(streamCore, resp)
	}

	streamCore.CompleteStream()
}

// emitGeminiParts routes a response's text parts to the stream: parts flagged
// Thought are thought summaries (present when IncludeThoughts is on) and
// stream as reasoning; everything else is answer content. Without the split,
// thinking text would leak into the visible response.
func emitGeminiParts(streamCore *streaming.StreamingCore, resp *gemini.GenerateContentResponse) {
	if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
		return
	}
	for _, part := range resp.Candidates[0].Content.Parts {
		if part.Text == "" {
			continue
		}
		if part.Thought {
			streamCore.EmitReasoning(part.Text)
		} else {
			streamCore.EmitContent(part.Text)
		}
	}
}

// handleNonStreamingCompletion handles non-streaming Gemini API requests
func (g *GeminiClient) handleNonStreamingCompletion(ctx context.Context, req *CompletionRequest, genReq *gemini.GenerateContentRequest, streamCore *streaming.StreamingCore) {
	resp, err := g.client.GenerateContent(ctx, req.Model, genReq)
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
	if len(resp.Candidates) == 0 {
		// The API documents an empty candidate list as a prompt problem
		// (see promptFeedback, which ProcessChunk reports when present).
		streamCore.EmitError(errors.New("gemini returned no candidates"))
		return
	}

	emitGeminiParts(streamCore, resp)

	streamCore.Complete()
}

// jsonSchemaToGeminiSchema converts a JSON Schema map (as parsed from a
// user-supplied schema file) to the API's typed Schema. The typed path
// is enforced by Gemini's structured-output backend; the JSON-schema-shaped
// alternative responseJsonSchema is silently ignored on preview models.
// Only the subset of JSON Schema that maps cleanly to gemini.Schema is handled
// — that's enough for the structured-output feature polly exposes.
// geminiSchemaType maps a JSON Schema type name to the API's enum form.
func geminiSchemaType(t string) gemini.Type {
	switch t {
	case "string":
		return gemini.TypeString
	case "number":
		return gemini.TypeNumber
	case "integer":
		return gemini.TypeInteger
	case "boolean":
		return gemini.TypeBoolean
	case "array":
		return gemini.TypeArray
	case "object":
		return gemini.TypeObject
	case "null":
		return gemini.TypeNULL
	}
	return ""
}

func jsonSchemaToGeminiSchema(raw map[string]any) *gemini.Schema {
	if raw == nil {
		return nil
	}
	out := &gemini.Schema{}
	switch t := raw["type"].(type) {
	case string:
		out.Type = geminiSchemaType(t)
	case []any:
		// JSON Schema type unions, e.g. ["null","array"] as emitted by
		// jsonschema-go for nil-able Go types. Gemini's typed schema has a
		// single type plus a nullable flag.
		for _, v := range t {
			s, ok := v.(string)
			if !ok {
				continue
			}
			if s == "null" {
				out.Nullable = true
				continue
			}
			if out.Type == "" {
				out.Type = geminiSchemaType(s)
			}
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
		out.Properties = make(map[string]*gemini.Schema, len(props))
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
func ConvertToolToGemini(schema *ToolSchema) *gemini.Tool {
	if schema == nil {
		return &gemini.Tool{FunctionDeclarations: []*gemini.FunctionDeclaration{{}}}
	}
	// Build a parameters-only schema without title/description metadata.
	params := map[string]any{"type": "object", "properties": schema.Properties()}
	if req := schema.Required(); len(req) > 0 {
		params["required"] = req
	}
	return &gemini.Tool{
		FunctionDeclarations: []*gemini.FunctionDeclaration{{
			Name:                 schema.Title(),
			Description:          schema.Description(),
			ParametersJsonSchema: params,
		}},
	}
}

// nativeGeminiCallID returns the provider-issued function call ID, or "" when
// the ID is one polly synthesized (gemini-<nonce>-<n>) for internal pairing
// and must not be echoed back to the API.
func nativeGeminiCallID(id string) string {
	if strings.HasPrefix(id, "gemini-") {
		return ""
	}
	return id
}

// MessagesToGeminiContent converts messages to Gemini content format
func MessagesToGeminiContent(msgs []messages.ChatMessage) ([]*gemini.Content, string, map[string]string) {
	return messagesToGeminiContent(msgs, nil)
}

func messagesToGeminiContent(msgs []messages.ChatMessage, replay *providerReplayCache) ([]*gemini.Content, string, map[string]string) {
	var history []*gemini.Content
	var systemInstruction string
	callIDToName := make(map[string]string)

	for _, msg := range msgs {
		switch msg.Role {
		case messages.MessageRoleSystem:
			systemInstruction = msg.Content

		case messages.MessageRoleUser:
			// Handle multimodal content
			if len(msg.Parts) > 0 {
				var parts []*gemini.Part
				for _, part := range msg.Parts {
					switch part.Type {
					case "text":
						parts = append(parts, &gemini.Part{Text: part.Text})
					case "image_base64":
						if replay != nil {
							if replay.validGeminiImage(part.ImageData) {
								parts = append(parts, &gemini.Part{InlineData: gemini.NewBase64Blob(part.MimeType, part.ImageData)})
							}
							continue
						}
						// Decode base64 to bytes
						imageData, err := base64.StdEncoding.DecodeString(part.ImageData)
						if err == nil {
							parts = append(parts, &gemini.Part{
								InlineData: &gemini.Blob{
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
					history = append(history, &gemini.Content{
						Role:  "user",
						Parts: parts,
					})
				}
			} else if msg.Content != "" {
				// Backward compatibility: simple text content
				history = append(history, &gemini.Content{
					Role:  "user",
					Parts: []*gemini.Part{{Text: msg.Content}},
				})
			}

		case messages.MessageRoleAssistant:
			var parts []*gemini.Part
			if msg.Content != "" {
				parts = append(parts, &gemini.Part{Text: msg.Content})
			}
			if len(msg.ToolCalls) > 0 {
				for _, tc := range msg.ToolCalls {
					if tc.ID != "" {
						callIDToName[tc.ID] = tc.Name
					}
					var call *gemini.FunctionCall
					if replay != nil {
						if raw, valid := replay.geminiArguments(tc.Arguments); valid {
							call = gemini.NewRawFunctionCall(nativeGeminiCallID(tc.ID), tc.Name, raw)
						}
					} else {
						var args map[string]any
						if json.Unmarshal([]byte(tc.Arguments), &args) == nil {
							call = &gemini.FunctionCall{ID: nativeGeminiCallID(tc.ID), Name: tc.Name, Args: args}
						}
					}
					if call != nil {
						part := &gemini.Part{FunctionCall: call}

						// Check metadata for thought signature. In-process the
						// adapter stores map[string]string; after a JSON
						// session reload it comes back as map[string]any.
						if msg.Metadata != nil {
							var sigStr string
							switch signatures := msg.Metadata["gemini_thought_signatures"].(type) {
							case map[string]string:
								sigStr = signatures[tc.ID]
							case map[string]any:
								sigStr, _ = signatures[tc.ID].(string)
							}
							if sigStr != "" {
								if sig, err := base64.StdEncoding.DecodeString(sigStr); err == nil {
									part.ThoughtSignature = sig
								}
							}
						}

						parts = append(parts, part)
					}
				}
			}
			if len(parts) > 0 {
				history = append(history, &gemini.Content{
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

			var result *gemini.FunctionResponse
			if replay != nil {
				result = gemini.NewRawFunctionResponse(nativeGeminiCallID(msg.ToolCallID), funcName, replay.geminiResult(msg.Content))
			} else {
				var output any
				if err := json.Unmarshal([]byte(msg.Content), &output); err != nil {
					output = msg.Content
				}
				// Ensure output is a map[string]any as the API requires.
				response, ok := output.(map[string]any)
				if !ok {
					response = map[string]any{"result": output}
				}
				result = &gemini.FunctionResponse{ID: nativeGeminiCallID(msg.ToolCallID), Name: funcName, Response: response}
			}
			history = append(history, &gemini.Content{
				Role: "user",
				Parts: []*gemini.Part{{
					FunctionResponse: result,
				}},
			})
		}
	}

	return history, systemInstruction, callIDToName
}
