package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/adapters"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

const (
	defaultOpenAIBaseURL               = "https://api.openai.com/v1/"
	openAIResponsesReasoningSummaryKey = "openai_responses_reasoning_summary_seen"
)

type openAIAPIMode string

const (
	openAIAPIModeChat      openAIAPIMode = "chat"
	openAIAPIModeResponses openAIAPIMode = "responses"
)

var _ LLM = (*OpenAIClient)(nil)

type OpenAIClient struct {
	client  openai.Client
	baseURL string
	apiMode openAIAPIMode
}

func NewOpenAIClient(apiKey string, baseURL string) *OpenAIClient {
	trimmedBaseURL := strings.TrimSpace(baseURL)
	mode := openAIAPIModeResponses
	effectiveBaseURL := defaultOpenAIBaseURL
	if trimmedBaseURL != "" {
		mode = openAIAPIModeChat
		effectiveBaseURL = trimmedBaseURL
	}

	client := openai.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(effectiveBaseURL),
	)

	return &OpenAIClient{
		client:  client,
		baseURL: trimmedBaseURL,
		apiMode: mode,
	}
}

// ChatCompletionStream implements the event-based streaming interface.
func (o OpenAIClient) ChatCompletionStream(ctx context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	var adapter streaming.ProviderAdapter = adapters.NewOpenAIAdapter()
	if o.apiMode == openAIAPIModeResponses {
		adapter = adapters.NewOpenAIResponsesAdapter()
	}

	return runStream(ctx, processor, adapter, func(streamCore *streaming.StreamingCore) {
		if err := o.streamCompletion(ctx, req, streamCore); err != nil {
			streamCore.EmitError(err)
		}
	})
}

func (o OpenAIClient) streamCompletion(ctx context.Context, req *CompletionRequest, streamCore *streaming.StreamingCore) error {
	timeout, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()

	switch o.apiMode {
	case openAIAPIModeResponses:
		return o.streamResponses(timeout, req, streamCore)
	default:
		return o.streamChatCompletions(timeout, req, streamCore)
	}
}

func (o OpenAIClient) streamChatCompletions(ctx context.Context, req *CompletionRequest, streamCore *streaming.StreamingCore) error {
	params := buildChatCompletionRequestParams(req)
	isStreaming := req.Stream == nil || *req.Stream
	slog.Debug("openai_chat_completion_started", "stream", isStreaming, "base_url", o.baseURL)

	if isStreaming {
		return o.handleStreamingChatCompletion(ctx, params, streamCore)
	}
	return o.handleNonStreamingChatCompletion(ctx, params, streamCore)
}

func (o OpenAIClient) handleStreamingChatCompletion(ctx context.Context, params openai.ChatCompletionNewParams, streamCore *streaming.StreamingCore) error {
	params.StreamOptions = openai.ChatCompletionStreamOptionsParam{
		IncludeUsage: param.NewOpt(true),
	}

	stream := o.client.Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close()

	for stream.Next() {
		chunk := stream.Current()
		if err := streamCore.ProcessChunk(&chunk); err != nil {
			return err
		}

		if len(chunk.Choices) == 0 {
			continue
		}
		if delta := chunk.Choices[0].Delta.Content; delta != "" {
			streamCore.EmitContent(delta)
		}
	}

	if err := stream.Err(); err != nil {
		slog.Debug("openai_chat_stream_error", "error", err)
		return fmt.Errorf("error during chat completions streaming: %w", err)
	}

	streamCore.Complete()
	return nil
}

func (o OpenAIClient) handleNonStreamingChatCompletion(ctx context.Context, params openai.ChatCompletionNewParams, streamCore *streaming.StreamingCore) error {
	resp, err := o.client.Chat.Completions.New(ctx, params)
	if err != nil {
		slog.Debug("openai_chat_completion_failed", "error", err)
		return fmt.Errorf("failed to create chat completion: %w", err)
	}

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		if choice.Message.Content != "" {
			streamCore.EmitContent(choice.Message.Content)
		}
		for _, toolCall := range choice.Message.ToolCalls {
			if toolCall.Type != "function" {
				continue
			}
			streamCore.GetState().AddToolCall(messages.ChatMessageToolCall{
				ID:        toolCall.ID,
				Name:      toolCall.Function.Name,
				Arguments: toolCall.Function.Arguments,
			})
		}
		streamCore.SetStopReason(adapters.MapOpenAIFinishReason(choice.FinishReason))
	}

	if resp.JSON.Usage.Valid() {
		streamCore.SetTokenUsage(int(resp.Usage.PromptTokens), int(resp.Usage.CompletionTokens))
	}

	streamCore.Complete()
	return nil
}

func (o OpenAIClient) streamResponses(ctx context.Context, req *CompletionRequest, streamCore *streaming.StreamingCore) error {
	params := buildResponsesRequestParams(req)
	isStreaming := req.Stream == nil || *req.Stream
	slog.Debug("openai_responses_started", "stream", isStreaming, "base_url", o.baseURL)

	if isStreaming {
		return o.handleStreamingResponse(ctx, params, streamCore)
	}
	return o.handleNonStreamingResponse(ctx, params, streamCore)
}

func (o OpenAIClient) handleStreamingResponse(ctx context.Context, params responses.ResponseNewParams, streamCore *streaming.StreamingCore) error {
	stream := o.client.Responses.NewStreaming(ctx, params)
	defer stream.Close()

	var rawReasoningFallback strings.Builder
	summarySeen := false

	for stream.Next() {
		event := stream.Current()
		if err := streamCore.ProcessChunk(event); err != nil {
			return err
		}

		switch event.Type {
		case "response.output_text.delta":
			if event.Delta != "" {
				streamCore.EmitContent(event.Delta)
			}
		case "response.refusal.delta":
			if event.Delta != "" {
				streamCore.EmitContent(event.Delta)
			}
		case "response.reasoning_summary_text.delta":
			if event.Delta != "" {
				summarySeen = true
				streamCore.GetState().SetMetadata(openAIResponsesReasoningSummaryKey, true)
				streamCore.EmitReasoning(event.Delta)
			}
		case "response.reasoning_text.delta":
			if !summarySeen && event.Delta != "" {
				rawReasoningFallback.WriteString(event.Delta)
			}
		}
	}

	if err := stream.Err(); err != nil {
		slog.Debug("openai_responses_stream_error", "error", err)
		return fmt.Errorf("error during responses streaming: %w", err)
	}

	if !summarySeen && rawReasoningFallback.Len() > 0 {
		streamCore.EmitReasoning(rawReasoningFallback.String())
	}

	streamCore.Complete()
	return nil
}

func (o OpenAIClient) handleNonStreamingResponse(ctx context.Context, params responses.ResponseNewParams, streamCore *streaming.StreamingCore) error {
	resp, err := o.client.Responses.New(ctx, params)
	if err != nil {
		slog.Debug("openai_responses_failed", "error", err)
		return fmt.Errorf("failed to create response: %w", err)
	}

	o.emitResponseOutput(resp, streamCore)

	if resp.Usage.JSON.TotalTokens.Valid() {
		streamCore.SetTokenUsage(int(resp.Usage.InputTokens), int(resp.Usage.OutputTokens))
	}
	streamCore.SetStopReason(adapters.MapResponsesStopReason(resp.Status, resp.IncompleteDetails.Reason, len(streamCore.GetState().GetToolCalls()) > 0))

	streamCore.Complete()
	return nil
}

func (o OpenAIClient) emitResponseOutput(resp *responses.Response, streamCore *streaming.StreamingCore) {
	if resp == nil {
		return
	}

	for _, item := range resp.Output {
		switch variant := item.AsAny().(type) {
		case responses.ResponseOutputMessage:
			for _, content := range variant.Content {
				switch content.Type {
				case "output_text":
					if content.Text != "" {
						streamCore.EmitContent(content.Text)
					}
				case "refusal":
					if content.Refusal != "" {
						streamCore.EmitContent(content.Refusal)
					}
				}
			}
		case responses.ResponseReasoningItem:
			if len(variant.Summary) > 0 {
				for _, summary := range variant.Summary {
					if summary.Text != "" {
						streamCore.EmitReasoning(summary.Text)
					}
				}
				continue
			}
			for _, content := range variant.Content {
				if content.Text != "" {
					streamCore.EmitReasoning(content.Text)
				}
			}
		case responses.ResponseFunctionToolCall:
			streamCore.GetState().AddToolCall(messages.ChatMessageToolCall{
				ID:        responseToolCallID(variant.CallID, variant.ID),
				Name:      variant.Name,
				Arguments: variant.Arguments,
			})
		}
	}
}

func buildChatCompletionRequestParams(req *CompletionRequest) openai.ChatCompletionNewParams {
	params := openai.ChatCompletionNewParams{
		Messages: messagesToChatCompletionParams(req.Messages),
		Model:    shared.ChatModel(req.Model),
	}
	if req.Temperature != nil {
		params.Temperature = param.NewOpt(float64(*req.Temperature))
	}

	if req.MaxTokens > 0 {
		params.MaxCompletionTokens = param.NewOpt(int64(req.MaxTokens))
	}
	if req.ThinkingEffort.IsEnabled() {
		params.ReasoningEffort = shared.ReasoningEffort(req.ThinkingEffort)
	}
	if req.ResponseSchema != nil {
		params.ResponseFormat = chatResponseFormatFromSchema(req.ResponseSchema)
	}
	if len(req.Tools) > 0 {
		params.Tools = make([]openai.ChatCompletionToolUnionParam, 0, len(req.Tools))
		for _, tool := range req.Tools {
			params.Tools = append(params.Tools, toolToChatCompletionTool(tool.GetSchema()))
		}
	}

	return params
}

func buildResponsesRequestParams(req *CompletionRequest) responses.ResponseNewParams {
	inputItems, instructions := messagesToResponsesInput(req.Messages)

	params := responses.ResponseNewParams{
		Input: responses.ResponseNewParamsInputUnion{OfInputItemList: inputItems},
		Model: shared.ResponsesModel(req.Model),
	}
	if req.Temperature != nil {
		params.Temperature = param.NewOpt(float64(*req.Temperature))
	}

	if instructions != "" {
		params.Instructions = param.NewOpt(instructions)
	}
	if req.MaxTokens > 0 {
		params.MaxOutputTokens = param.NewOpt(int64(req.MaxTokens))
	}
	if reasoning, ok := responsesReasoningFromThinkingEffort(req.ThinkingEffort); ok {
		params.Reasoning = reasoning
	}
	if req.ResponseSchema != nil {
		params.Text = responsesTextConfigFromSchema(req.ResponseSchema)
	}
	if len(req.Tools) > 0 {
		params.Tools = make([]responses.ToolUnionParam, 0, len(req.Tools))
		for _, tool := range req.Tools {
			params.Tools = append(params.Tools, toolToResponsesFunctionTool(tool.GetSchema()))
		}
	}

	return params
}

func chatResponseFormatFromSchema(schema *Schema) openai.ChatCompletionNewParamsResponseFormatUnion {
	if schema == nil {
		return openai.ChatCompletionNewParamsResponseFormatUnion{}
	}

	return openai.ChatCompletionNewParamsResponseFormatUnion{
		OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
			JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
				Name:        "response",
				Description: param.NewOpt("Structured response"),
				Schema:      normalizeOpenAISchema(schema),
				Strict:      param.NewOpt(schema.Strict),
			},
		},
	}
}

func responsesTextConfigFromSchema(schema *Schema) responses.ResponseTextConfigParam {
	if schema == nil {
		return responses.ResponseTextConfigParam{}
	}

	format := responses.ResponseFormatTextConfigParamOfJSONSchema("response", normalizeOpenAISchema(schema))
	if format.OfJSONSchema != nil {
		format.OfJSONSchema.Description = param.NewOpt("Structured response")
		format.OfJSONSchema.Strict = param.NewOpt(schema.Strict)
	}

	return responses.ResponseTextConfigParam{
		Format: format,
	}
}

func toolToChatCompletionTool(schema *ToolSchema) openai.ChatCompletionToolUnionParam {
	params := toolParametersFromSchema(schema)

	definition := shared.FunctionDefinitionParam{
		Name:       toolNameFromSchema(schema),
		Parameters: shared.FunctionParameters(params),
	}
	if description := toolDescriptionFromSchema(schema); description != "" {
		definition.Description = param.NewOpt(description)
	}

	return openai.ChatCompletionToolUnionParam{
		OfFunction: &openai.ChatCompletionFunctionToolParam{
			Function: definition,
		},
	}
}

func toolToResponsesFunctionTool(schema *ToolSchema) responses.ToolUnionParam {
	params := toolParametersFromSchema(schema)
	strict := schema != nil && schema.Strict
	if strict {
		params = deepCopyMap(params)
		// Strict mode on the Responses API requires every object node to declare
		// additionalProperties=false; otherwise the API 400s with
		// "'additionalProperties' is required to be supplied and to be false".
		addObjectAdditionalPropertiesFalse(params)
	}

	tool := responses.FunctionToolParam{
		Name:       toolNameFromSchema(schema),
		Parameters: params,
		Strict:     param.NewOpt(strict),
	}
	if description := toolDescriptionFromSchema(schema); description != "" {
		tool.Description = param.NewOpt(description)
	}

	return responses.ToolUnionParam{
		OfFunction: &tool,
	}
}

func messagesToChatCompletionParams(msgs []messages.ChatMessage) []openai.ChatCompletionMessageParamUnion {
	result := make([]openai.ChatCompletionMessageParamUnion, 0, len(msgs))
	for _, msg := range msgs {
		result = append(result, messageToChatCompletionParam(msg))
	}
	return result
}

func messageToChatCompletionParam(msg messages.ChatMessage) openai.ChatCompletionMessageParamUnion {
	switch msg.Role {
	case messages.MessageRoleSystem:
		return openai.SystemMessage(msg.GetContent())
	case messages.MessageRoleTool:
		return openai.ToolMessage(msg.GetContent(), msg.ToolCallID)
	case messages.MessageRoleAssistant:
		assistant := openai.ChatCompletionAssistantMessageParam{}
		if content := msg.GetContent(); content != "" {
			assistant.Content.OfString = param.NewOpt(content)
		}
		if len(msg.ToolCalls) > 0 {
			assistant.ToolCalls = make([]openai.ChatCompletionMessageToolCallUnionParam, 0, len(msg.ToolCalls))
			for _, toolCall := range msg.ToolCalls {
				assistant.ToolCalls = append(assistant.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
						ID: toolCall.ID,
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name:      toolCall.Name,
							Arguments: toolCall.Arguments,
						},
					},
				})
			}
		}
		return openai.ChatCompletionMessageParamUnion{OfAssistant: &assistant}
	default:
		content := make([]openai.ChatCompletionContentPartUnionParam, 0, len(msg.Parts)+1)
		if len(msg.Parts) > 0 {
			for _, part := range msg.Parts {
				switch part.Type {
				case "text":
					content = append(content, openai.TextContentPart(part.Text))
				case "image_base64":
					content = append(content, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
						URL: "data:" + part.MimeType + ";base64," + part.ImageData,
					}))
				case "image_url":
					content = append(content, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
						URL: part.ImageURL,
					}))
				}
			}
		}
		if len(content) == 0 {
			content = append(content, openai.TextContentPart(msg.GetContent()))
		}
		return openai.UserMessage(content)
	}
}

func messagesToResponsesInput(msgs []messages.ChatMessage) (responses.ResponseInputParam, string) {
	items := make(responses.ResponseInputParam, 0, len(msgs))
	systemParts := make([]string, 0, len(msgs))

	for messageIndex, msg := range msgs {
		if msg.Role == messages.MessageRoleSystem {
			if content := strings.TrimSpace(msg.GetContent()); content != "" {
				systemParts = append(systemParts, content)
			}
			continue
		}
		items = append(items, messageToResponsesInputItems(msg, messageIndex)...)
	}

	return items, strings.Join(systemParts, "\n\n")
}

func messageToResponsesInputItems(msg messages.ChatMessage, messageIndex int) []responses.ResponseInputItemUnionParam {
	switch msg.Role {
	case messages.MessageRoleUser:
		content := responseInputContentFromMessage(msg)
		if len(content) == 0 {
			return nil
		}
		return []responses.ResponseInputItemUnionParam{
			responses.ResponseInputItemParamOfMessage(content, responses.EasyInputMessageRoleUser),
		}
	case messages.MessageRoleAssistant:
		items := make([]responses.ResponseInputItemUnionParam, 0, len(msg.ToolCalls)+1)
		if content := responseOutputContentFromMessage(msg); len(content) > 0 {
			items = append(items, responses.ResponseInputItemParamOfOutputMessage(
				content,
				responseReplayMessageID(messageIndex),
				responses.ResponseOutputMessageStatusCompleted,
			))
		}
		for toolIndex, toolCall := range msg.ToolCalls {
			callID := responseReplayToolCallID(toolCall.ID, messageIndex, toolIndex)
			items = append(items, responses.ResponseInputItemUnionParam{
				OfFunctionCall: &responses.ResponseFunctionToolCallParam{
					Arguments: toolCall.Arguments,
					CallID:    callID,
					Name:      toolCall.Name,
					Status:    responses.ResponseFunctionToolCallStatusCompleted,
				},
			})
		}
		return items
	case messages.MessageRoleTool:
		callID := strings.TrimSpace(msg.ToolCallID)
		if callID == "" {
			callID = fmt.Sprintf("tool_output_%d", messageIndex)
		}
		return []responses.ResponseInputItemUnionParam{
			{
				OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
					CallID: callID,
					Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
						OfString: param.NewOpt(msg.GetContent()),
					},
					Status: "completed",
				},
			},
		}
	default:
		return nil
	}
}

func responseInputContentFromMessage(msg messages.ChatMessage) responses.ResponseInputMessageContentListParam {
	content := make(responses.ResponseInputMessageContentListParam, 0, len(msg.Parts)+1)
	if len(msg.Parts) > 0 {
		for _, part := range msg.Parts {
			switch part.Type {
			case "text":
				content = append(content, responses.ResponseInputContentParamOfInputText(part.Text))
			case "image_base64":
				image := responses.ResponseInputContentParamOfInputImage(responses.ResponseInputImageDetailAuto)
				if image.OfInputImage != nil {
					image.OfInputImage.ImageURL = param.NewOpt("data:" + part.MimeType + ";base64," + part.ImageData)
				}
				content = append(content, image)
			case "image_url":
				image := responses.ResponseInputContentParamOfInputImage(responses.ResponseInputImageDetailAuto)
				if image.OfInputImage != nil {
					image.OfInputImage.ImageURL = param.NewOpt(part.ImageURL)
				}
				content = append(content, image)
			}
		}
	}
	if len(content) == 0 {
		if text := msg.GetContent(); text != "" {
			content = append(content, responses.ResponseInputContentParamOfInputText(text))
		}
	}
	return content
}

func responseOutputContentFromMessage(msg messages.ChatMessage) []responses.ResponseOutputMessageContentUnionParam {
	content := make([]responses.ResponseOutputMessageContentUnionParam, 0, len(msg.Parts)+1)
	if len(msg.Parts) > 0 {
		for _, part := range msg.Parts {
			if part.Type != "text" {
				continue
			}
			content = append(content, responseOutputTextContent(part.Text))
		}
	}
	if len(content) == 0 {
		if text := msg.GetContent(); text != "" {
			content = append(content, responseOutputTextContent(text))
		}
	}
	return content
}

func responseOutputTextContent(text string) responses.ResponseOutputMessageContentUnionParam {
	return responses.ResponseOutputMessageContentUnionParam{
		OfOutputText: &responses.ResponseOutputTextParam{
			Annotations: []responses.ResponseOutputTextAnnotationUnionParam{},
			Text:        text,
		},
	}
}

func responsesReasoningFromThinkingEffort(effort ThinkingEffort) (shared.ReasoningParam, bool) {
	if !effort.IsEnabled() {
		return shared.ReasoningParam{}, false
	}
	return shared.ReasoningParam{
		Effort:  shared.ReasoningEffort(effort),
		Summary: shared.ReasoningSummaryAuto,
	}, true
}

func normalizeOpenAISchema(schema *Schema) map[string]any {
	if schema == nil {
		return nil
	}

	schemaCopy := deepCopyMap(schema.Raw)
	if !schema.Strict {
		return schemaCopy
	}

	schemaCopy["additionalProperties"] = false

	if props, ok := schemaCopy["properties"].(map[string]any); ok {
		required := make([]string, 0, len(props))
		for key, prop := range props {
			required = append(required, key)
			if propMap, ok := prop.(map[string]any); ok {
				if propType, ok := propMap["type"].(string); ok && propType == "object" {
					propMap["additionalProperties"] = false
				}
			}
		}
		sort.Strings(required)
		schemaCopy["required"] = required
	}

	return schemaCopy
}

// addObjectAdditionalPropertiesFalse walks a JSON-schema map and sets
// additionalProperties=false on every object node that doesn't already set it.
// Used to prep tool parameter schemas for OpenAI strict mode without touching
// `required` (tools may legitimately have optional params; we don't want to
// silently promote optional fields, so callers using strict mode should ensure
// their tool schemas mark all fields required themselves).
func addObjectAdditionalPropertiesFalse(node map[string]any) {
	if node == nil {
		return
	}
	if t, ok := node["type"].(string); ok && t == "object" {
		if _, set := node["additionalProperties"]; !set {
			node["additionalProperties"] = false
		}
	}
	if props, ok := node["properties"].(map[string]any); ok {
		for _, prop := range props {
			if propMap, ok := prop.(map[string]any); ok {
				addObjectAdditionalPropertiesFalse(propMap)
			}
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		addObjectAdditionalPropertiesFalse(items)
	}
}

func toolParametersFromSchema(schema *ToolSchema) map[string]any {
	if schema == nil {
		return map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
	}

	params := map[string]any{
		"type":       "object",
		"properties": schema.Properties(),
	}
	if required := schema.Required(); len(required) > 0 {
		params["required"] = required
	}
	return params
}

func toolNameFromSchema(schema *ToolSchema) string {
	if schema == nil {
		return ""
	}
	return schema.Title()
}

func toolDescriptionFromSchema(schema *ToolSchema) string {
	if schema == nil {
		return ""
	}
	return schema.Description()
}

func responseReplayToolCallID(id string, messageIndex, toolIndex int) string {
	id = strings.TrimSpace(id)
	if id != "" {
		return id
	}
	return fmt.Sprintf("call_%d_%d", messageIndex, toolIndex)
}

func responseReplayMessageID(messageIndex int) string {
	return fmt.Sprintf("msg_%d", messageIndex)
}

func responseToolCallID(callID, itemID string) string {
	if strings.TrimSpace(callID) != "" {
		return callID
	}
	return itemID
}

func deepCopyMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	raw, err := json.Marshal(input)
	if err != nil {
		out := make(map[string]any, len(input))
		for key, value := range input {
			out[key] = value
		}
		return out
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		out = make(map[string]any, len(input))
		for key, value := range input {
			out[key] = value
		}
	}
	return out
}
