package gemini

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
)

// Provider builds Gemini requests from the provider-agnostic completion
// request and streams the reply back through the adapter. It implements
// contract.LLM.
type Provider struct {
	client *Client
}

// NewProvider uses the public endpoint when baseURL is empty.
func NewProvider(apiKey, baseURL string, opts ...ClientOption) (*Provider, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("gemini API key not configured")
	}
	return &Provider{client: NewClient(apiKey, baseURL, opts...)}, nil
}

// thinkingConfig builds Gemini's thinking configuration from a
// provider-agnostic effort. Gemini 3.x uses a ThinkingLevel enum (no xhigh/max,
// so those clamp to high); Gemini 2.5 uses an integer ThinkingBudget where -1
// means dynamic. Callers must guard with ThinkingEffort.IsEnabled().
func thinkingConfig(effort contract.ThinkingEffort, model string) *ThinkingConfig {
	cfg := &ThinkingConfig{IncludeThoughts: true}

	if strings.HasPrefix(model, "gemini-3") {
		// 3.x: enum levels. Dynamic leaves the level unset (model default).
		if !effort.IsDynamic() {
			switch effort.AsLevel(contract.LevelMedium) {
			case contract.LevelMinimal:
				cfg.ThinkingLevel = ThinkingLevelMinimal
			case contract.LevelLow:
				cfg.ThinkingLevel = ThinkingLevelLow
			case contract.LevelMedium:
				cfg.ThinkingLevel = ThinkingLevelMedium
			default: // high, xhigh, max all clamp to high (Gemini's ceiling)
				cfg.ThinkingLevel = ThinkingLevelHigh
			}
		}
		return cfg
	}

	// 2.5 and older: integer budget. Dynamic uses -1 (model-managed).
	var budget int32
	if b, ok := effort.AsBudget(); ok {
		budget = clampBudget(int32(b), model)
	} else {
		budget = -1
	}
	cfg.ThinkingBudget = &budget
	return cfg
}

// clampBudget keeps a 2.5-family thinking budget within the model's
// documented range. Pro cannot fully disable thinking (floor 128); Flash can.
func clampBudget(budget int32, model string) int32 {
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
func (g *Provider) ChatCompletionStream(ctx context.Context, req *contract.CompletionRequest, processor contract.EventStreamProcessor) <-chan *messages.StreamEvent {
	return contract.RunStream(ctx, req.Timeout, req.Deadline, processor, NewAdapter(), func(ctx context.Context, streamCore *streaming.StreamingCore) {
		// Convert session history to Gemini chat history
		contents, systemInstruction := messagesToContent(req.Messages, req.Replay)

		// Configure model parameters
		config := &GenerationConfig{
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
			config.ThinkingConfig = thinkingConfig(req.ThinkingEffort, req.Model)
		}

		// Add structured output support. Preview models (3.x) silently ignore
		// the responseJsonSchema field, so route through the typed
		// responseSchema path (the API's canonical structured-output
		// mechanism) instead.
		if req.ResponseSchema != nil {
			config.ResponseMIMEType = "application/json"
			config.ResponseSchema = jsonSchemaToSchema(req.ResponseSchema.Raw)
		}

		genReq := &GenerateContentRequest{
			Contents:         contents,
			GenerationConfig: config,
		}

		// System instruction
		if systemInstruction != "" {
			genReq.SystemInstruction = &Content{
				Parts: []*Part{{Text: systemInstruction}},
			}
		}

		// Add tool support if available
		if len(req.Tools) > 0 {
			funcs := make([]*FunctionDeclaration, 0, len(req.Tools))
			for _, tool := range req.Tools {
				funcs = append(funcs, convertTool(tool.GetSchema()))
			}
			genReq.Tools = []*Tool{
				{FunctionDeclarations: funcs},
			}
		}

		isStreaming := req.IsStreaming()
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
func (g *Provider) handleStreamingCompletion(ctx context.Context, req *contract.CompletionRequest, genReq *GenerateContentRequest, streamCore *streaming.StreamingCore) {
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

		emitParts(streamCore, resp)
	}

	streamCore.CompleteStream()
}

// emitParts routes a response's text parts to the stream: parts flagged
// Thought are thought summaries (present when IncludeThoughts is on) and
// stream as reasoning; everything else is answer content. Without the split,
// thinking text would leak into the visible response.
func emitParts(streamCore *streaming.StreamingCore, resp *GenerateContentResponse) {
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
func (g *Provider) handleNonStreamingCompletion(ctx context.Context, req *contract.CompletionRequest, genReq *GenerateContentRequest, streamCore *streaming.StreamingCore) {
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

	emitParts(streamCore, resp)

	streamCore.Complete()
}

// schemaType maps a JSON Schema type name to the API's enum form.
func schemaType(t string) Type {
	switch t {
	case "string":
		return TypeString
	case "number":
		return TypeNumber
	case "integer":
		return TypeInteger
	case "boolean":
		return TypeBoolean
	case "array":
		return TypeArray
	case "object":
		return TypeObject
	case "null":
		return TypeNULL
	}
	return ""
}

// jsonSchemaToSchema converts a JSON Schema map (as parsed from a
// user-supplied schema file) to the API's typed Schema. The typed path
// is enforced by Gemini's structured-output backend; the JSON-schema-shaped
// alternative responseJsonSchema is silently ignored on preview models.
// Only the subset of JSON Schema that maps cleanly to Schema is handled
// — that's enough for the structured-output feature polly exposes.
func jsonSchemaToSchema(raw map[string]any) *Schema {
	if raw == nil {
		return nil
	}
	out := &Schema{}
	switch t := raw["type"].(type) {
	case string:
		out.Type = schemaType(t)
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
				out.Type = schemaType(s)
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
		out.Items = jsonSchemaToSchema(items)
	}
	if props, ok := raw["properties"].(map[string]any); ok {
		out.Properties = make(map[string]*Schema, len(props))
		for name, p := range props {
			if pm, ok := p.(map[string]any); ok {
				out.Properties[name] = jsonSchemaToSchema(pm)
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

// convertTool converts a tool schema to a Gemini function declaration.
// ParametersJsonSchema accepts any, so we pass a raw map, stripped of
// title/description since those are set on the declaration itself.
func convertTool(toolSchema *schema.ToolSchema) *FunctionDeclaration {
	if toolSchema == nil {
		return &FunctionDeclaration{}
	}
	return &FunctionDeclaration{
		Name:                 toolSchema.Title(),
		Description:          toolSchema.Description(),
		ParametersJsonSchema: toolSchema.Parameters(),
	}
}

// messagesToContent converts session history to Gemini contents, returning
// the system instruction separately since the API carries it outside the
// content list.
func messagesToContent(msgs []messages.ChatMessage, replay *contract.ReplayCache) ([]*Content, string) {
	var history []*Content
	var systemInstruction string
	callIDToName := make(map[string]string)

	for _, msg := range msgs {
		switch msg.Role {
		case messages.MessageRoleSystem:
			systemInstruction = msg.Content

		case messages.MessageRoleUser:
			// Handle multimodal content
			if len(msg.Parts) > 0 {
				var parts []*Part
				for _, part := range msg.Parts {
					switch part.Type {
					case "text":
						parts = append(parts, &Part{Text: part.Text})
					case "image_base64":
						if replay.ValidGeminiImage(part.ImageData) {
							parts = append(parts, &Part{InlineData: NewBase64Blob(part.MimeType, part.ImageData)})
						}
					case "image_url":
						// Gemini doesn't directly support URLs, would need to download
						// For now, skip URL images for Gemini
					}
				}
				if len(parts) > 0 {
					history = append(history, &Content{
						Role:  "user",
						Parts: parts,
					})
				}
			} else if msg.Content != "" {
				// Backward compatibility: simple text content
				history = append(history, &Content{
					Role:  "user",
					Parts: []*Part{{Text: msg.Content}},
				})
			}

		case messages.MessageRoleAssistant:
			var parts []*Part
			if msg.Content != "" {
				parts = append(parts, &Part{Text: msg.Content})
			}
			for _, tc := range msg.ToolCalls {
				if tc.ID != "" {
					callIDToName[tc.ID] = tc.Name
				}
				raw, valid := replay.GeminiArguments(tc.Arguments)
				if !valid {
					continue
				}
				part := &Part{FunctionCall: NewRawFunctionCall(streaming.NativeCallID(tc.ID), tc.Name, raw)}

				// Check metadata for thought signature. In-process the
				// adapter stores map[string]string; after a JSON
				// session reload it comes back as map[string]any.
				var sigStr string
				switch signatures := msg.Metadata[ThoughtSignaturesKey].(type) {
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

				parts = append(parts, part)
			}
			if len(parts) > 0 {
				history = append(history, &Content{
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

			result := NewRawFunctionResponse(streaming.NativeCallID(msg.ToolCallID), funcName, replay.GeminiResult(msg.Content))
			history = append(history, &Content{
				Role: "user",
				Parts: []*Part{{
					FunctionResponse: result,
				}},
			})
		}
	}

	return history, systemInstruction
}
