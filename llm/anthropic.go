package llm

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/adapters"
	"github.com/alexschlessinger/pollytool/llm/anthropic"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
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
func mapEffort(effort ThinkingEffort) anthropic.Effort {
	switch effort.AsLevel(LevelMedium) {
	case LevelMinimal, LevelLow:
		return anthropic.EffortLow
	case LevelMedium:
		return anthropic.EffortMedium
	case LevelHigh:
		return anthropic.EffortHigh
	case LevelXHigh:
		return anthropic.EffortXHigh
	case LevelMax:
		return anthropic.EffortMax
	default:
		return anthropic.EffortMedium
	}
}

type AnthropicClient struct {
	client *anthropic.Client
}

func NewAnthropicClient(apiKey string) *AnthropicClient {
	if apiKey == "" {
		slog.Debug("anthropic_missing_api_key")
	}

	return &AnthropicClient{
		client: anthropic.NewClient(apiKey),
	}
}

// getThinkingConfig returns the thinking configuration based on effort level and
// the target model. Opus 4.7 rejects the legacy enabled/budget_tokens mode, and
// Anthropic recommends adaptive thinking for all 4.6+ family models.
func (a *AnthropicClient) getThinkingConfig(effort ThinkingEffort, model string, maxTokens int) *anthropic.ThinkingConfig {
	if supportsAdaptiveThinking(model) {
		return &anthropic.ThinkingConfig{
			Type: anthropic.ThinkingTypeAdaptive,
			// "summarized" keeps thinking text flowing through the stream;
			// the default "omitted" would make reasoning render as a long pause.
			Display: anthropic.DisplaySummarized,
		}
	}

	// Legacy enabled/budget_tokens mode. A named level maps to its canonical
	// budget; a raw budget passes through; Dynamic has no legacy equivalent, so
	// it falls back to the medium canonical budget.
	budget, ok := effort.AsBudget()
	if !ok {
		budget = levelBudgets[LevelMedium]
	}
	return &anthropic.ThinkingConfig{
		Type:         anthropic.ThinkingTypeEnabled,
		BudgetTokens: int64(clampThinkingBudget(budget, maxTokens)),
	}
}

// clampThinkingBudget keeps a legacy thinking budget within Anthropic's limits:
// at least 1024 tokens, and strictly less than max_tokens (the API 400s
// otherwise). When max_tokens is too small to leave room, the floor wins.
func clampThinkingBudget(budget, maxTokens int) int {
	const minThinkingBudget = 1024
	if maxTokens > minThinkingBudget && budget > maxTokens-1 {
		budget = maxTokens - 1
	}
	if budget < minThinkingBudget {
		budget = minThinkingBudget
	}
	return budget
}

// buildRequestParams creates the Anthropic API request parameters
func (a *AnthropicClient) buildRequestParams(req *CompletionRequest) *anthropic.MessageRequest {
	// Convert messages to Anthropic format
	anthropicMessages, systemPrompt := MessagesToAnthropicParams(req.Messages)

	// Create the request
	params := &anthropic.MessageRequest{
		Model:     req.Model,
		MaxTokens: int64(req.MaxTokens),
		Messages:  anthropicMessages,
	}

	// Opus 4.7 rejects temperature/top_p/top_k with a 400.
	if req.Temperature != nil && !rejectsSamplingParams(req.Model) {
		temp := float64(*req.Temperature)
		params.Temperature = &temp
	}

	// Enable thinking for supported models if requested
	if req.ThinkingEffort.IsEnabled() {
		params.Thinking = a.getThinkingConfig(req.ThinkingEffort, req.Model, req.MaxTokens)
		// Adaptive thinking pairs with output_config effort to control depth,
		// replacing the legacy budget_tokens knob. Dynamic effort means "let the
		// model decide", so we send adaptive thinking with no explicit effort.
		if supportsAdaptiveThinking(req.Model) && !req.ThinkingEffort.IsDynamic() {
			params.OutputConfig = &anthropic.OutputConfig{
				Effort: mapEffort(req.ThinkingEffort),
			}
		}
	}

	// Add system prompt if present
	if systemPrompt != "" {
		params.System = []*anthropic.ContentBlock{
			{Type: "text", Text: systemPrompt},
		}
	}

	// Add tools and/or structured output support
	var anthropicTools []*anthropic.Tool

	// Add structured output tool if schema is provided
	if req.ResponseSchema != nil {
		anthropicTools = append(anthropicTools, ConvertToAnthropicTool(req.ResponseSchema))
	}

	// Add regular tools if provided
	if len(req.Tools) > 0 {
		for _, tool := range req.Tools {
			anthropicTools = append(anthropicTools, ConvertToolToAnthropic(tool.GetSchema()))
		}
	}

	// Set tools if we have any
	if len(anthropicTools) > 0 {
		params.Tools = anthropicTools

		// Only force tool use if ONLY schema is provided (no regular tools).
		// Anthropic rejects thinking+forced tool_choice together, so skip the
		// force when thinking is enabled — the schema tool is still available,
		// the model just isn't compelled to call it.
		if req.ResponseSchema != nil && len(req.Tools) == 0 && !req.ThinkingEffort.IsEnabled() {
			params.ToolChoice = &anthropic.ToolChoice{Type: "any"}
		}
	}

	return params
}

// ChatCompletionStream implements the event-based streaming interface
func (a *AnthropicClient) ChatCompletionStream(ctx context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	adapter := adapters.NewAnthropicAdapter()
	return runStream(ctx, processor, adapter, func(streamCore *streaming.StreamingCore) {
		params := a.buildRequestParams(req)
		isStreaming := req.Stream == nil || *req.Stream
		slog.Debug("anthropic_completion_started", "model", req.Model, "stream", isStreaming)

		if isStreaming {
			a.processStream(ctx, params, req, streamCore)
		} else {
			a.processNonStreaming(ctx, params, req, streamCore, adapter)
		}
	})
}

// processStream handles the main stream processing logic
func (a *AnthropicClient) processStream(ctx context.Context, params *anthropic.MessageRequest, req *CompletionRequest, streamCore *streaming.StreamingCore) {
	for event, err := range a.client.CreateMessageStream(ctx, params) {
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
		if event.Type == anthropic.EventContentBlockDelta && event.Delta != nil {
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
	if req.ResponseSchema != nil {
		if streamCore.HandleStructuredOutput(structuredOutputToolName) {
			return
		}
	}

	// Send final message with accumulated state
	streamCore.Complete()
}

// processNonStreaming handles non-streaming API requests
func (a *AnthropicClient) processNonStreaming(ctx context.Context, params *anthropic.MessageRequest, req *CompletionRequest, streamCore *streaming.StreamingCore, adapter *adapters.AnthropicAdapter) {
	resp, err := a.client.CreateMessage(ctx, params)
	if err != nil {
		slog.Debug("anthropic_completion_failed", "error", err)
		streamCore.EmitError(err)
		return
	}

	// Process content blocks
	for _, block := range resp.Content {
		switch block.Type {
		case "thinking":
			streamCore.EmitReasoning(block.Thinking)
			// Add thinking block to adapter for metadata preservation
			adapter.AddThinkingBlock(block.Thinking, block.Signature)
		case "redacted_thinking":
			// Preserve verbatim; must be replayed unchanged in tool loops
			adapter.AddRedactedThinkingBlock(block.Data)
		case "text":
			streamCore.EmitContent(block.Text)
		case "tool_use":
			streamCore.GetState().AddToolCall(messages.ChatMessageToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: string(block.Input),
			})
		}
	}

	// Set stop reason
	streamCore.SetStopReason(adapters.MapAnthropicStopReason(resp.StopReason))

	// Set token usage
	if resp.Usage != nil {
		streamCore.SetTokenUsage(int(resp.Usage.InputTokens), int(resp.Usage.OutputTokens))
	}

	// Handle structured output if needed
	if req.ResponseSchema != nil {
		if streamCore.HandleStructuredOutput(structuredOutputToolName) {
			return
		}
	}

	streamCore.Complete()
}

// ConvertToAnthropicTool creates a synthetic tool for structured output with Anthropic
func ConvertToAnthropicTool(schema *Schema) *anthropic.Tool {
	if schema == nil {
		return &anthropic.Tool{}
	}

	return &anthropic.Tool{
		Name:        structuredOutputToolName,
		Description: "Extract and structure data according to the specified schema",
		InputSchema: anthropic.InputSchema{
			Type:       "object",
			Properties: map[string]any{"data": schema.Raw},
			Required:   []string{"data"},
		},
	}
}

// ConvertToolToAnthropic converts a tool schema to Anthropic format.
// InputSchema.Properties accepts a raw map, so we pass it directly.
func ConvertToolToAnthropic(schema *ToolSchema) *anthropic.Tool {
	if schema == nil {
		return &anthropic.Tool{}
	}
	return &anthropic.Tool{
		Name:        schema.Title(),
		Description: schema.Description(),
		InputSchema: anthropic.InputSchema{
			Type:       "object",
			Properties: schema.Properties(),
			Required:   schema.Required(),
		},
	}
}

// anthropicTextBlock builds a "text" content block.
func anthropicTextBlock(text string) *anthropic.ContentBlock {
	return &anthropic.ContentBlock{Type: "text", Text: text}
}

// MessagesToAnthropicParams converts messages to Anthropic message parameters
func MessagesToAnthropicParams(msgs []messages.ChatMessage) ([]anthropic.MessageParam, string) {
	var anthropicMessages []anthropic.MessageParam
	systemPrompt := ""

	for _, msg := range msgs {
		switch msg.Role {
		case messages.MessageRoleSystem:
			systemPrompt = msg.Content

		case messages.MessageRoleUser:
			// Handle multimodal content
			if len(msg.Parts) > 0 {
				var blocks []*anthropic.ContentBlock
				for _, part := range msg.Parts {
					switch part.Type {
					case "text":
						if strings.TrimSpace(part.Text) != "" {
							blocks = append(blocks, anthropicTextBlock(part.Text))
						}
					case "image_base64":
						// Anthropic expects base64 images with media type
						blocks = append(blocks, &anthropic.ContentBlock{
							Type: "image",
							Source: &anthropic.ImageSource{
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
					anthropicMessages = append(anthropicMessages, anthropic.MessageParam{
						Role: "user", Content: blocks,
					})
				}
			} else if strings.TrimSpace(msg.Content) != "" {
				// Backward compatibility: simple text content
				anthropicMessages = append(anthropicMessages, anthropic.MessageParam{
					Role: "user", Content: []*anthropic.ContentBlock{anthropicTextBlock(msg.Content)},
				})
			}

		case messages.MessageRoleAssistant:
			var blocks []*anthropic.ContentBlock

			// Check if we have preserved thinking blocks in metadata
			if msg.Metadata != nil {
				if thinkingBlocksData, ok := msg.Metadata["anthropic_thinking_blocks"]; ok {
					// Restore thinking blocks with their signatures. In-process
					// the adapter stores []map[string]any; after a JSON session
					// reload the value comes back as []any.
					var thinkingBlocksList []map[string]any
					switch v := thinkingBlocksData.(type) {
					case []map[string]any:
						thinkingBlocksList = v
					case []any:
						for _, item := range v {
							if m, ok := item.(map[string]any); ok {
								thinkingBlocksList = append(thinkingBlocksList, m)
							}
						}
					}

					for _, block := range thinkingBlocksList {
						blockType, _ := block["type"].(string)
						switch blockType {
						case "thinking":
							thinking, _ := block["thinking"].(string)
							signature, _ := block["signature"].(string)
							if signature != "" && thinking != "" {
								blocks = append(blocks, &anthropic.ContentBlock{
									Type:      "thinking",
									Thinking:  thinking,
									Signature: signature,
								})
							}
						case "redacted_thinking":
							if data, _ := block["data"].(string); data != "" {
								blocks = append(blocks, &anthropic.ContentBlock{
									Type: "redacted_thinking",
									Data: data,
								})
							}
						}
					}
				}
			}

			if strings.TrimSpace(msg.Content) != "" {
				blocks = append(blocks, anthropicTextBlock(msg.Content))
			}
			if len(msg.ToolCalls) > 0 {
				for _, tc := range msg.ToolCalls {
					// Anthropic requires the input field even for tools with
					// no parameters; invalid argument JSON degrades to {}.
					input := json.RawMessage("{}")
					if argStr := strings.TrimSpace(tc.Arguments); argStr != "" {
						if json.Valid([]byte(argStr)) {
							input = json.RawMessage(argStr)
						}
					}
					blocks = append(blocks, &anthropic.ContentBlock{
						Type:  "tool_use",
						ID:    tc.ID,
						Name:  tc.Name,
						Input: input,
					})
				}
			}
			if len(blocks) > 0 {
				anthropicMessages = append(anthropicMessages, anthropic.MessageParam{
					Role: "assistant", Content: blocks,
				})
			}

		case messages.MessageRoleTool:
			if strings.TrimSpace(msg.ToolCallID) != "" {
				isError := false
				anthropicMessages = append(anthropicMessages, anthropic.MessageParam{
					Role: "user",
					Content: []*anthropic.ContentBlock{{
						Type:      "tool_result",
						ToolUseID: msg.ToolCallID,
						Content:   []*anthropic.ContentBlock{anthropicTextBlock(msg.Content)},
						IsError:   &isError,
					}},
				})
			} else if strings.TrimSpace(msg.Content) != "" {
				anthropicMessages = append(anthropicMessages, anthropic.MessageParam{
					Role: "user", Content: []*anthropic.ContentBlock{anthropicTextBlock(msg.Content)},
				})
			}
		}
	}

	return anthropicMessages, systemPrompt
}
