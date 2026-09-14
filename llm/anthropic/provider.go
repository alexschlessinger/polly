package anthropic

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
)

const structuredOutputToolName = "extract_structured_data"

// legacyThinkingPrefixes enumerates the closed set of models that still use the
// legacy enabled/budget_tokens thinking mode. Everything from the 4.6 family
// onward uses adaptive thinking (type:"adaptive" + output_config effort), and
// all future models will too, so unknown models default to adaptive. This list
// can only shrink (as legacy models retire), never grow.
var legacyThinkingPrefixes = [...]string{
	"claude-2",
	"claude-3", // all 3.x, including claude-3-5-* and claude-3-7-*
	"claude-opus-4-0",
	"claude-opus-4-1",
	"claude-opus-4-5",
	"claude-opus-4-20250514", // dated full ID for opus 4.0
	"claude-sonnet-4-0",
	"claude-sonnet-4-20250514", // dated full ID for sonnet 4.0
	"claude-sonnet-4-5",
	"claude-haiku-4-5",
	"claude-mythos-preview",
}

// supportsAdaptiveThinking reports whether the model expects adaptive thinking
// (type:"adaptive" + output_config effort) rather than legacy enabled/budget_tokens.
// Everything past the 4.5 generation *rejects* the legacy enabled mode with a
// 400, so adaptive is the default and legacy models are the exception.
func supportsAdaptiveThinking(model string) bool {
	for _, p := range legacyThinkingPrefixes {
		if strings.HasPrefix(model, p) {
			return false
		}
	}
	return true
}

// rejectsSamplingParams reports whether the model 400s on temperature/top_p/top_k.
// The 4.6 family is the only adaptive generation that still accepts them; every
// later model rejects them (sonnet-5 rejects non-default values), so unknown
// models default to rejecting — the worst case of guessing wrong here is a
// dropped temperature rather than a 400.
func rejectsSamplingParams(model string) bool {
	return supportsAdaptiveThinking(model) &&
		!strings.HasPrefix(model, "claude-opus-4-6") &&
		!strings.HasPrefix(model, "claude-sonnet-4-6")
}

// mapEffort converts a ThinkingEffort to the Anthropic output_config effort
// level used with adaptive thinking. Callers must guard with
// ThinkingEffort.IsEnabled() and skip Dynamic (which uses adaptive thinking
// with no effort). Anthropic has no "minimal" tier, so minimal clamps to low.
func mapEffort(effort contract.ThinkingEffort) Effort {
	switch effort.AsLevel(contract.LevelMedium) {
	case contract.LevelMinimal, contract.LevelLow:
		return EffortLow
	case contract.LevelMedium:
		return EffortMedium
	case contract.LevelHigh:
		return EffortHigh
	case contract.LevelXHigh:
		return EffortXHigh
	case contract.LevelMax:
		return EffortMax
	default:
		return EffortMedium
	}
}

// Provider implements the completion contract over the Messages API: it
// builds requests from the provider-agnostic CompletionRequest, drives the
// wire Client, and translates responses through the streaming Adapter.
type Provider struct {
	client *Client
}

// NewProvider returns a Provider for the public endpoint, or for the first
// non-empty base URL.
func NewProvider(apiKey string, baseURLs ...string) *Provider {
	if apiKey == "" {
		slog.Debug("anthropic_missing_api_key")
	}

	return &Provider{
		client: NewClient(apiKey, baseURLs...),
	}
}

// thinkingConfig returns the thinking configuration based on effort level and
// the target model. Opus 4.7 rejects the legacy enabled/budget_tokens mode, and
// Anthropic recommends adaptive thinking for all 4.6+ family models.
func thinkingConfig(effort contract.ThinkingEffort, model string, maxTokens int) *ThinkingConfig {
	if supportsAdaptiveThinking(model) {
		return &ThinkingConfig{
			Type: ThinkingTypeAdaptive,
			// "summarized" keeps thinking text flowing through the stream;
			// the default "omitted" would make reasoning render as a long pause.
			Display: DisplaySummarized,
		}
	}

	// Legacy enabled/budget_tokens mode. A named level maps to its canonical
	// budget; a raw budget passes through; Dynamic has no legacy equivalent, so
	// it falls back to the medium canonical budget.
	if maxTokens <= minThinkingBudget {
		// budget_tokens must be at least the 1024 floor and strictly less
		// than max_tokens, so no valid budget exists: any value would be a
		// guaranteed 400. Dropping thinking lets the request succeed.
		slog.Debug("anthropic_thinking_disabled", "reason", "max_tokens_too_small", "max_tokens", maxTokens)
		return nil
	}
	budget, ok := effort.AsBudget()
	if !ok {
		budget = contract.LevelMedium.Budget()
	}
	return &ThinkingConfig{
		Type:         ThinkingTypeEnabled,
		BudgetTokens: int64(clampThinkingBudget(budget, maxTokens)),
	}
}

// minThinkingBudget is Anthropic's floor for legacy budget_tokens.
const minThinkingBudget = 1024

// defaultMaxTokens stands in when a request carries no max_tokens (the CLI's
// "0 = provider default"): a limit every current Claude model accepts.
const defaultMaxTokens = 8192

// clampThinkingBudget keeps a legacy thinking budget within Anthropic's limits:
// at least minThinkingBudget tokens, and strictly less than max_tokens (the API
// 400s otherwise). Callers guarantee maxTokens > minThinkingBudget; when it
// isn't, thinkingConfig drops thinking instead of clamping.
func clampThinkingBudget(budget, maxTokens int) int {
	if budget > maxTokens-1 {
		budget = maxTokens - 1
	}
	if budget < minThinkingBudget {
		budget = minThinkingBudget
	}
	return budget
}

// BuildRequest converts a completion request into the Messages API body it
// would send, without sending it. It reads nothing from the Provider, so a
// nil receiver is fine for inspecting request shapes.
func (p *Provider) BuildRequest(req *contract.CompletionRequest) *MessageRequest {
	// Convert messages to Anthropic format
	anthropicMessages, systemPrompt := messagesToParams(req.Messages, req.Replay)

	// Create the request
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		// The Messages API has no provider default: max_tokens is required,
		// and zero means "populate the prompt cache without generating",
		// which would return no reply at all.
		maxTokens = defaultMaxTokens
	}
	params := &MessageRequest{
		Model:     req.Model,
		MaxTokens: int64(maxTokens),
		Messages:  anthropicMessages,
	}
	if req.CacheSessionID != "" {
		params.CacheControl = &CacheControl{Type: "ephemeral"}
	}

	// Opus 4.7 rejects temperature/top_p/top_k with a 400.
	if req.Temperature != nil && !rejectsSamplingParams(req.Model) {
		temp := float64(*req.Temperature)
		params.Temperature = &temp
	}

	// Enable thinking for supported models if requested
	if req.ThinkingEffort.IsEnabled() {
		params.Thinking = thinkingConfig(req.ThinkingEffort, req.Model, maxTokens)
		// Adaptive thinking pairs with output_config effort to control depth,
		// replacing the legacy budget_tokens knob. Dynamic effort means "let the
		// model decide", so we send adaptive thinking with no explicit effort.
		if supportsAdaptiveThinking(req.Model) && !req.ThinkingEffort.IsDynamic() {
			params.OutputConfig = &OutputConfig{
				Effort: mapEffort(req.ThinkingEffort),
			}
		}
	}

	// Add system prompt if present
	if systemPrompt != "" {
		params.System = []*ContentBlock{textBlock(systemPrompt)}
	}

	// Add tools and/or structured output support
	var anthropicTools []*Tool

	// Add structured output tool if schema is provided
	if req.ResponseSchema != nil {
		anthropicTools = append(anthropicTools, structuredOutputTool(req.ResponseSchema))
	}

	// Add regular tools if provided
	for _, tool := range req.Tools {
		anthropicTools = append(anthropicTools, toolParam(tool.GetSchema()))
	}

	// Set tools if we have any
	if len(anthropicTools) > 0 {
		params.Tools = anthropicTools

		// Only force tool use if ONLY schema is provided (no regular tools).
		// Anthropic rejects thinking+forced tool_choice together, so skip the
		// force when thinking is enabled — the schema tool is still available,
		// the model just isn't compelled to call it.
		if req.ResponseSchema != nil && len(req.Tools) == 0 && !req.ThinkingEffort.IsEnabled() {
			params.ToolChoice = &ToolChoice{Type: "any"}
		}
	}

	return params
}

// ChatCompletionStream implements the event-based streaming interface
func (p *Provider) ChatCompletionStream(ctx context.Context, req *contract.CompletionRequest, processor contract.EventStreamProcessor) <-chan *messages.StreamEvent {
	adapter := NewAdapter()
	return contract.RunStream(ctx, req.Timeout, req.Deadline, processor, adapter, func(ctx context.Context, streamCore *streaming.StreamingCore) {
		params := p.BuildRequest(req)
		isStreaming := req.IsStreaming()
		slog.Debug("anthropic_completion_started", "model", req.Model, "stream", isStreaming)

		if isStreaming {
			p.processStream(ctx, params, req, streamCore)
		} else {
			p.processNonStreaming(ctx, params, req, streamCore)
		}
	})
}

// processStream handles the main stream processing logic
func (p *Provider) processStream(ctx context.Context, params *MessageRequest, req *contract.CompletionRequest, streamCore *streaming.StreamingCore) {
	for event, err := range p.client.CreateMessageStream(ctx, params) {
		if err != nil {
			streamCore.EmitError(err)
			return
		}

		// Process the event through the adapter
		if err := streamCore.ProcessChunk(event); err != nil {
			streamCore.EmitError(err)
			return
		}

		// Handle content and reasoning streaming
		if event.Type == EventContentBlockDelta && event.Delta != nil {
			// Stream thinking content
			if thinking := event.Delta.Thinking; thinking != "" {
				streamCore.EmitReasoning(thinking)
			}

			// Stream regular content
			if text := event.Delta.Text; text != "" {
				streamCore.EmitContent(text)
			}
		}
	}

	// Handle structured output response
	if req.ResponseSchema != nil && completeStructuredOutput(streamCore) {
		return
	}

	// Send final message with accumulated state
	streamCore.CompleteStream()
}

// processNonStreaming handles non-streaming API requests. The adapter
// records the reply's state (thinking blocks, tool calls, stop reason,
// usage); only text and reasoning emission happens here.
func (p *Provider) processNonStreaming(ctx context.Context, params *MessageRequest, req *contract.CompletionRequest, streamCore *streaming.StreamingCore) {
	resp, err := p.client.CreateMessage(ctx, params)
	if err != nil {
		slog.Debug("anthropic_completion_failed", "error", err)
		streamCore.EmitError(err)
		return
	}

	if err := streamCore.ProcessChunk(resp); err != nil {
		streamCore.EmitError(err)
		return
	}
	for _, block := range resp.Content {
		switch block.Type {
		case "thinking":
			streamCore.EmitReasoning(block.Thinking)
		case "text":
			streamCore.EmitContent(block.Text)
		}
	}

	// Handle structured output if needed
	if req.ResponseSchema != nil && completeStructuredOutput(streamCore) {
		return
	}

	streamCore.Complete()
}

// completeStructuredOutput finishes a structured-output turn. Anthropic has
// no native schema mode, so the schema rides on the synthetic
// extract_structured_data tool and the model's call is the answer: its data
// argument becomes the reply's content. The stop reason flips from ToolUse to
// EndTurn so the agent loop terminates instead of issuing another call
// against a transcript that ends with this synthetic assistant message
// (Anthropic 4.x rejects assistant prefill). It reports false, sending
// nothing, when no well-formed call to the tool was made.
func completeStructuredOutput(streamCore *streaming.StreamingCore) bool {
	for _, tc := range streamCore.GetState().GetToolCalls() {
		if tc.Name != structuredOutputToolName {
			continue
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			continue
		}
		data, ok := args["data"]
		if !ok {
			continue
		}
		dataJSON, err := json.Marshal(data)
		if err != nil {
			continue
		}
		streamCore.SetStopReason(messages.StopReasonEndTurn)
		streamCore.CompleteWithContent(string(dataJSON))
		return true
	}
	return false
}

// structuredOutputTool creates the synthetic tool that carries a structured
// output schema.
func structuredOutputTool(s *schema.Schema) *Tool {
	if s == nil {
		return &Tool{}
	}

	return &Tool{
		Name:        structuredOutputToolName,
		Description: "Extract and structure data according to the specified schema",
		InputSchema: InputSchema{
			Type:       "object",
			Properties: map[string]any{"data": s.Raw},
			Required:   []string{"data"},
		},
	}
}

// toolParam converts a tool schema to Anthropic format.
// InputSchema.Properties accepts a raw map, so we pass it directly.
func toolParam(s *schema.ToolSchema) *Tool {
	if s == nil {
		return &Tool{}
	}
	return &Tool{
		Name:        s.Title(),
		Description: s.Description(),
		InputSchema: InputSchema{
			Type:       "object",
			Properties: s.Properties(),
			Required:   s.Required(),
		},
	}
}

// textBlock builds a "text" content block.
func textBlock(text string) *ContentBlock {
	return &ContentBlock{Type: "text", Text: text}
}

// messagesToParams converts a transcript into Messages API turns, returning
// the system prompt separately (the API takes it at the top level).
func messagesToParams(msgs []messages.ChatMessage, replay *contract.ReplayCache) ([]MessageParam, string) {
	var anthropicMessages []MessageParam
	systemPrompt := ""

	for _, msg := range msgs {
		switch msg.Role {
		case messages.MessageRoleSystem:
			systemPrompt = msg.Content

		case messages.MessageRoleUser:
			// Handle multimodal content
			if len(msg.Parts) > 0 {
				var blocks []*ContentBlock
				for _, part := range msg.Parts {
					switch part.Type {
					case "text":
						if strings.TrimSpace(part.Text) != "" {
							blocks = append(blocks, textBlock(part.Text))
						}
					case "image_base64":
						// Anthropic expects base64 images with media type
						blocks = append(blocks, &ContentBlock{
							Type: "image",
							Source: &ImageSource{
								Type:      "base64",
								MediaType: part.MimeType,
								Data:      part.ImageData,
							},
						})
					case "image_url":
						// For URL images, we'd need to download and convert to base64
						// For now, skip URL images for Anthropic
					}
				}
				if len(blocks) > 0 {
					anthropicMessages = append(anthropicMessages, MessageParam{
						Role: "user", Content: blocks,
					})
				}
			} else if strings.TrimSpace(msg.Content) != "" {
				// Backward compatibility: simple text content
				anthropicMessages = append(anthropicMessages, MessageParam{
					Role: "user", Content: []*ContentBlock{textBlock(msg.Content)},
				})
			}

		case messages.MessageRoleAssistant:
			var blocks []*ContentBlock

			// Restore preserved thinking blocks with their signatures.
			for _, block := range contract.MetadataMapList(msg.Metadata[ThinkingBlocksKey]) {
				blockType, _ := block["type"].(string)
				switch blockType {
				case "thinking":
					thinking, _ := block["thinking"].(string)
					signature, _ := block["signature"].(string)
					if signature != "" && thinking != "" {
						blocks = append(blocks, &ContentBlock{
							Type:      "thinking",
							Thinking:  thinking,
							Signature: signature,
						})
					}
				case "redacted_thinking":
					if data, _ := block["data"].(string); data != "" {
						blocks = append(blocks, &ContentBlock{
							Type: "redacted_thinking",
							Data: data,
						})
					}
				}
			}

			if strings.TrimSpace(msg.Content) != "" {
				blocks = append(blocks, textBlock(msg.Content))
			}
			for _, tc := range msg.ToolCalls {
				// Anthropic requires the input field even for tools with
				// no parameters; invalid argument JSON degrades to {}.
				blocks = append(blocks, &ContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: replay.AnthropicInput(tc.Arguments),
				})
			}
			if len(blocks) > 0 {
				anthropicMessages = append(anthropicMessages, MessageParam{
					Role: "assistant", Content: blocks,
				})
			}

		case messages.MessageRoleTool:
			if strings.TrimSpace(msg.ToolCallID) != "" {
				// A durably recorded failure travels as is_error, so the
				// model can tell "ran and failed" from ordinary output.
				succeeded, known := msg.ToolSucceeded()
				isError := known && !succeeded
				result := &ContentBlock{
					Type:      "tool_result",
					ToolUseID: msg.ToolCallID,
					IsError:   &isError,
				}
				// The API rejects empty text blocks; a tool that produced no
				// output sends a bare tool_result (content is optional there).
				if strings.TrimSpace(msg.Content) != "" {
					result.Content = []*ContentBlock{textBlock(msg.Content)}
				}
				anthropicMessages = append(anthropicMessages, MessageParam{
					Role:    "user",
					Content: []*ContentBlock{result},
				})
			} else if strings.TrimSpace(msg.Content) != "" {
				anthropicMessages = append(anthropicMessages, MessageParam{
					Role: "user", Content: []*ContentBlock{textBlock(msg.Content)},
				})
			}
		}
	}

	return anthropicMessages, systemPrompt
}
