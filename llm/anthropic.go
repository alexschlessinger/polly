package llm

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/adapters"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

const structuredOutputToolName = "extract_structured_data"

// legacyThinkingPrefixes enumerates the closed set of models that still use the
// legacy enabled/budget_tokens thinking mode. Everything from the 4.6 family
// onward uses adaptive thinking (type:"adaptive" + OutputConfig.Effort), and all
// future models will too, so unknown models default to adaptive. This list can
// only shrink (as legacy models retire), never grow.
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
// (type:"adaptive" + OutputConfig.Effort) rather than legacy enabled/budget_tokens.
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

// mapEffort converts a ThinkingEffort to the Anthropic OutputConfig effort level
// used with adaptive thinking. Callers must guard with ThinkingEffort.IsEnabled()
// and skip Dynamic (which uses adaptive thinking with no effort). Anthropic has
// no "minimal" tier, so minimal clamps to low.
func mapEffort(effort ThinkingEffort) anthropic.OutputConfigEffort {
	switch effort.AsLevel(LevelMedium) {
	case LevelMinimal, LevelLow:
		return anthropic.OutputConfigEffortLow
	case LevelMedium:
		return anthropic.OutputConfigEffortMedium
	case LevelHigh:
		return anthropic.OutputConfigEffortHigh
	case LevelXHigh:
		return anthropic.OutputConfigEffortXhigh
	case LevelMax:
		return anthropic.OutputConfigEffortMax
	default:
		return anthropic.OutputConfigEffortMedium
	}
}

type AnthropicClient struct {
	client anthropic.Client
}

func NewAnthropicClient(apiKey string) *AnthropicClient {
	if apiKey == "" {
		slog.Debug("anthropic_missing_api_key")
	}

	client := anthropic.NewClient(
		option.WithAPIKey(apiKey),
	)

	return &AnthropicClient{
		client: client,
	}
}

// getThinkingConfig returns the thinking configuration based on effort level and
// the target model. Opus 4.7 rejects the legacy enabled/budget_tokens mode, and
// Anthropic recommends adaptive thinking for all 4.6+ family models.
func (a *AnthropicClient) getThinkingConfig(effort ThinkingEffort, model string, maxTokens int) anthropic.ThinkingConfigParamUnion {
	if supportsAdaptiveThinking(model) {
		return anthropic.ThinkingConfigParamUnion{
			OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{
				// "summarized" keeps thinking text flowing through the stream;
				// the default "omitted" would make reasoning render as a long pause.
				Display: anthropic.ThinkingConfigAdaptiveDisplaySummarized,
			},
		}
	}

	// Legacy enabled/budget_tokens mode. A named level maps to its canonical
	// budget; a raw budget passes through; Dynamic has no legacy equivalent, so
	// it falls back to the medium canonical budget.
	budget, ok := effort.AsBudget()
	if !ok {
		budget = levelBudgets[LevelMedium]
	}
	return anthropic.ThinkingConfigParamOfEnabled(int64(clampThinkingBudget(budget, maxTokens)))
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
func (a *AnthropicClient) buildRequestParams(req *CompletionRequest) anthropic.MessageNewParams {
	// Convert messages to Anthropic format
	anthropicMessages, systemPrompt := MessagesToAnthropicParams(req.Messages)

	// Create the request
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: int64(req.MaxTokens),
		Messages:  anthropicMessages,
	}

	// Opus 4.7 rejects temperature/top_p/top_k with a 400.
	if req.Temperature != nil && !rejectsSamplingParams(req.Model) {
		params.Temperature = anthropic.Float(float64(*req.Temperature))
	}

	// Enable thinking for supported models if requested
	if req.ThinkingEffort.IsEnabled() {
		params.Thinking = a.getThinkingConfig(req.ThinkingEffort, req.Model, req.MaxTokens)
		// Adaptive thinking pairs with OutputConfig.Effort to control depth,
		// replacing the legacy budget_tokens knob. Dynamic effort means "let the
		// model decide", so we send adaptive thinking with no explicit effort.
		if supportsAdaptiveThinking(req.Model) && !req.ThinkingEffort.IsDynamic() {
			params.OutputConfig = anthropic.OutputConfigParam{
				Effort: mapEffort(req.ThinkingEffort),
			}
		}
	}

	// Add system prompt if present
	if systemPrompt != "" {
		params.System = []anthropic.TextBlockParam{
			{
				Type: "text",
				Text: systemPrompt,
			},
		}
	}

	// Add tools and/or structured output support
	var anthropicTools []anthropic.ToolUnionParam

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
			params.ToolChoice = anthropic.ToolChoiceUnionParam{
				OfAny: &anthropic.ToolChoiceAnyParam{
					Type: "any",
				},
			}
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
			stream := a.client.Messages.NewStreaming(ctx, params)
			a.processStream(stream, req, streamCore)
		} else {
			a.processNonStreaming(ctx, params, req, streamCore, adapter)
		}
	})
}

// processStream handles the main stream processing logic
func (a *AnthropicClient) processStream(stream *ssestream.Stream[anthropic.MessageStreamEventUnion], req *CompletionRequest, streamCore *streaming.StreamingCore) {
	for stream.Next() {
		event := stream.Current()

		// Process the event through the adapter
		if err := streamCore.ProcessChunk(event); err != nil {
			streamCore.EmitError(err)
			return
		}

		// Handle content and reasoning streaming
		switch event.Type {
		case string(constant.ValueOf[constant.ContentBlockDelta]()):
			blockDelta := event.AsContentBlockDelta()

			// Stream thinking content
			if thinking := blockDelta.Delta.Thinking; thinking != "" {
				streamCore.EmitReasoning(thinking)
			}

			// Stream regular content
			if text := blockDelta.Delta.Text; text != "" {
				streamCore.EmitContent(text)
			}
		}
	}

	// Check for stream error
	if err := stream.Err(); err != nil {
		streamCore.EmitError(err)
		return
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
func (a *AnthropicClient) processNonStreaming(ctx context.Context, params anthropic.MessageNewParams, req *CompletionRequest, streamCore *streaming.StreamingCore, adapter *adapters.AnthropicAdapter) {
	resp, err := a.client.Messages.New(ctx, params)
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
	streamCore.SetTokenUsage(int(resp.Usage.InputTokens), int(resp.Usage.OutputTokens))

	// Handle structured output if needed
	if req.ResponseSchema != nil {
		if streamCore.HandleStructuredOutput(structuredOutputToolName) {
			return
		}
	}

	streamCore.Complete()
}

// ConvertToAnthropicTool creates a synthetic tool for structured output with Anthropic
func ConvertToAnthropicTool(schema *Schema) anthropic.ToolUnionParam {
	if schema == nil {
		return anthropic.ToolUnionParam{}
	}

	properties := map[string]any{"data": schema.Raw}
	required := []string{"data"}

	toolParam := anthropic.ToolParam{
		Name:        structuredOutputToolName,
		Description: anthropic.String("Extract and structure data according to the specified schema"),
		InputSchema: anthropic.ToolInputSchemaParam{
			Type:       "object",
			Properties: properties,
			Required:   required,
		},
	}

	return anthropic.ToolUnionParam{
		OfTool: &toolParam,
	}
}

// ConvertToolToAnthropic converts a tool schema to Anthropic format.
// Anthropic's InputSchema.Properties accepts any, so we pass the raw map directly.
func ConvertToolToAnthropic(schema *ToolSchema) anthropic.ToolUnionParam {
	if schema == nil {
		return anthropic.ToolUnionParam{}
	}
	inputSchema := anthropic.ToolInputSchemaParam{
		Type:       "object",
		Properties: schema.Properties(),
		Required:   schema.Required(),
	}
	return anthropic.ToolUnionParam{
		OfTool: &anthropic.ToolParam{
			Name:        schema.Title(),
			Description: anthropic.String(schema.Description()),
			InputSchema: inputSchema,
		},
	}
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
				var blocks []anthropic.ContentBlockParamUnion
				for _, part := range msg.Parts {
					switch part.Type {
					case "text":
						if strings.TrimSpace(part.Text) != "" {
							blocks = append(blocks, anthropic.NewTextBlock(part.Text))
						}
					case "image_base64":
						// Anthropic expects base64 images with media type
						blocks = append(blocks, anthropic.NewImageBlockBase64(part.MimeType, part.ImageData))
					case "image_url":
						// For URL images, we'd need to download and convert to base64
						// For now, skip URL images for Anthropic
					}
				}
				if len(blocks) > 0 {
					anthropicMessages = append(anthropicMessages, anthropic.NewUserMessage(blocks...))
				}
			} else if strings.TrimSpace(msg.Content) != "" {
				// Backward compatibility: simple text content
				anthropicMessages = append(anthropicMessages, anthropic.NewUserMessage(
					anthropic.NewTextBlock(msg.Content),
				))
			}

		case messages.MessageRoleAssistant:
			var blocks []anthropic.ContentBlockParamUnion

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
						if blockType, _ := block["type"].(string); blockType == string(constant.ValueOf[constant.Thinking]()) {
							thinking, _ := block["thinking"].(string)
							signature, _ := block["signature"].(string)
							if signature != "" && thinking != "" {
								blocks = append(blocks, anthropic.NewThinkingBlock(signature, thinking))
							}
						}
					}
				}
			}

			if strings.TrimSpace(msg.Content) != "" {
				blocks = append(blocks, anthropic.NewTextBlock(msg.Content))
			}
			if len(msg.ToolCalls) > 0 {
				for _, tc := range msg.ToolCalls {
					var input any
					if argStr := strings.TrimSpace(tc.Arguments); argStr != "" {
						var tmp any
						if err := json.Unmarshal([]byte(argStr), &tmp); err == nil {
							input = tmp
						}
					} else {
						// Anthropic requires input field even for tools with no parameters
						input = make(map[string]any)
					}
					blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, input, tc.Name))
				}
			}
			if len(blocks) > 0 {
				anthropicMessages = append(anthropicMessages, anthropic.NewAssistantMessage(blocks...))
			}

		case messages.MessageRoleTool:
			if strings.TrimSpace(msg.ToolCallID) != "" {
				anthropicMessages = append(anthropicMessages, anthropic.NewUserMessage(
					anthropic.NewToolResultBlock(msg.ToolCallID, msg.Content, false),
				))
			} else if strings.TrimSpace(msg.Content) != "" {
				anthropicMessages = append(anthropicMessages, anthropic.NewUserMessage(
					anthropic.NewTextBlock(msg.Content),
				))
			}
		}
	}

	return anthropicMessages, systemPrompt
}
