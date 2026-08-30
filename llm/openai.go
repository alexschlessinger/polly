package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/adapters"
	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

const openAIResponsesReasoningSummaryKey = "openai_responses_reasoning_summary_seen"

type openAIAPIMode string

type openAICompatibleProvider string

const (
	openAIAPIModeChat          openAIAPIMode            = "chat"
	openAIAPIModeResponses     openAIAPIMode            = "responses"
	openAICompatibleGeneric    openAICompatibleProvider = "generic"
	openAICompatibleOpenRouter openAICompatibleProvider = "openrouter"
)

var _ LLM = (*OpenAIClient)(nil)

type OpenAIClient struct {
	client             *openai.Client
	baseURL            string
	apiMode            openAIAPIMode
	compatibleProvider openAICompatibleProvider
}

func NewOpenAIClient(apiKey string, baseURL string) *OpenAIClient {
	trimmedBaseURL := strings.TrimSpace(baseURL)
	mode := openAIAPIModeResponses
	if trimmedBaseURL != "" {
		mode = openAIAPIModeChat
	}

	return &OpenAIClient{
		client:             openai.NewClient(apiKey, trimmedBaseURL),
		baseURL:            trimmedBaseURL,
		apiMode:            mode,
		compatibleProvider: openAICompatibleGeneric,
	}
}

func newOpenRouterClient(apiKey, baseURL string) *OpenAIClient {
	client := NewOpenAIClient(apiKey, baseURL)
	client.compatibleProvider = openAICompatibleOpenRouter
	return client
}

// ChatCompletionStream implements the event-based streaming interface.
func (o OpenAIClient) ChatCompletionStream(ctx context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	var adapter streaming.ProviderAdapter = adapters.NewOpenAIAdapter()
	if o.apiMode == openAIAPIModeResponses {
		adapter = adapters.NewOpenAIResponsesAdapter(req.Model)
	}

	return runStream(ctx, req.Timeout, req.Deadline, processor, adapter, func(ctx context.Context, streamCore *streaming.StreamingCore) {
		if err := o.streamCompletion(ctx, req, streamCore); err != nil {
			streamCore.EmitError(err)
		}
	})
}

func (o OpenAIClient) streamCompletion(ctx context.Context, req *CompletionRequest, streamCore *streaming.StreamingCore) error {
	switch o.apiMode {
	case openAIAPIModeResponses:
		return o.streamResponses(ctx, req, streamCore)
	default:
		return o.streamChatCompletions(ctx, req, streamCore)
	}
}

func (o OpenAIClient) streamChatCompletions(ctx context.Context, req *CompletionRequest, streamCore *streaming.StreamingCore) error {
	params := buildChatCompletionRequestParams(req)
	if o.compatibleProvider == openAICompatibleOpenRouter {
		params.SessionID = req.CacheSessionID
	}
	isStreaming := req.Stream == nil || *req.Stream
	slog.Debug("openai_chat_completion_started", "stream", isStreaming, "base_url", o.baseURL)

	if isStreaming {
		return o.handleStreamingChatCompletion(ctx, params, streamCore)
	}
	return o.handleNonStreamingChatCompletion(ctx, params, streamCore)
}

func (o OpenAIClient) handleStreamingChatCompletion(ctx context.Context, params *openai.ChatCompletionRequest, streamCore *streaming.StreamingCore) error {
	for chunk, err := range o.client.StreamChatCompletion(ctx, params) {
		if err != nil {
			slog.Debug("openai_chat_stream_error", "error", err)
			return fmt.Errorf("error during chat completions streaming: %w", err)
		}
		if err := streamCore.ProcessChunk(chunk); err != nil {
			return err
		}

		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		// Reasoning first: it precedes the answer it produced, and emitting it
		// after would invert that order for anything displaying the stream.
		if reasoning := delta.ReasoningText(); reasoning != "" {
			streamCore.EmitReasoning(reasoning)
		}
		if delta.Content != "" {
			streamCore.EmitContent(delta.Content)
		}
	}

	streamCore.Complete()
	return nil
}

func (o OpenAIClient) handleNonStreamingChatCompletion(ctx context.Context, params *openai.ChatCompletionRequest, streamCore *streaming.StreamingCore) error {
	resp, err := o.client.CreateChatCompletion(ctx, params)
	if err != nil {
		slog.Debug("openai_chat_completion_failed", "error", err)
		return fmt.Errorf("failed to create chat completion: %w", err)
	}

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		if reasoning := choice.Message.ReasoningText(); reasoning != "" {
			streamCore.EmitReasoning(reasoning)
		}
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

	if resp.Usage != nil {
		streamCore.SetTokenUsage(int(resp.Usage.PromptTokens), int(resp.Usage.CompletionTokens))
		if read, write, reported := resp.Usage.PromptCacheUsage(); reported {
			streamCore.SetPromptCacheUsage(read, write)
		}
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

func (o OpenAIClient) handleStreamingResponse(ctx context.Context, params *openai.ResponsesRequest, streamCore *streaming.StreamingCore) error {
	var rawReasoningFallback strings.Builder
	summarySeen := false

	for event, err := range o.client.StreamResponse(ctx, params) {
		if err != nil {
			slog.Debug("openai_responses_stream_error", "error", err)
			return fmt.Errorf("error during responses streaming: %w", err)
		}
		if err := streamCore.ProcessChunk(event); err != nil {
			return err
		}

		switch event.Type {
		case "response.output_text.delta":
			if event.Delta != "" {
				streamCore.EmitContent(string(event.Delta))
			}
		case "response.refusal.delta":
			if event.Delta != "" {
				streamCore.EmitContent(string(event.Delta))
			}
		case "response.reasoning_summary_text.delta":
			if event.Delta != "" {
				summarySeen = true
				streamCore.GetState().SetMetadata(openAIResponsesReasoningSummaryKey, true)
				streamCore.EmitReasoning(string(event.Delta))
			}
		case "response.reasoning_text.delta":
			if !summarySeen && event.Delta != "" {
				rawReasoningFallback.WriteString(string(event.Delta))
			}
		}
	}

	if !summarySeen && rawReasoningFallback.Len() > 0 {
		streamCore.EmitReasoning(rawReasoningFallback.String())
	}

	streamCore.Complete()
	return nil
}

func (o OpenAIClient) handleNonStreamingResponse(ctx context.Context, params *openai.ResponsesRequest, streamCore *streaming.StreamingCore) error {
	resp, err := o.client.CreateResponse(ctx, params)
	if err != nil {
		slog.Debug("openai_responses_failed", "error", err)
		return fmt.Errorf("failed to create response: %w", err)
	}

	o.emitResponseOutput(resp, streamCore)

	if resp.Usage != nil {
		streamCore.SetTokenUsage(int(resp.Usage.InputTokens), int(resp.Usage.OutputTokens))
		if read, write, reported := resp.Usage.PromptCacheUsage(); reported {
			streamCore.SetPromptCacheUsage(read, write)
		}
	}
	incompleteReason := ""
	if resp.IncompleteDetails != nil {
		incompleteReason = resp.IncompleteDetails.Reason
	}
	streamCore.SetStopReason(adapters.MapResponsesStopReason(resp.Status, incompleteReason, len(streamCore.GetState().GetToolCalls()) > 0))

	streamCore.Complete()
	return nil
}

func (o OpenAIClient) emitResponseOutput(resp *openai.Response, streamCore *streaming.StreamingCore) {
	if resp == nil {
		return
	}

	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, content := range item.Content {
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
		case "reasoning":
			adapters.AppendResponsesReasoningItem(streamCore.GetState(), &item)
			if len(item.Summary) > 0 {
				for _, summary := range item.Summary {
					if summary.Text != "" {
						streamCore.EmitReasoning(summary.Text)
					}
				}
				continue
			}
			for _, content := range item.Content {
				if content.Text != "" {
					streamCore.EmitReasoning(content.Text)
				}
			}
		case "function_call":
			streamCore.GetState().AddToolCall(messages.ChatMessageToolCall{
				ID:        responseToolCallID(item.CallID, item.ID),
				Name:      item.Name,
				Arguments: string(item.Arguments),
			})
		}
	}
}

func buildChatCompletionRequestParams(req *CompletionRequest) *openai.ChatCompletionRequest {
	params := &openai.ChatCompletionRequest{
		Messages: messagesToChatCompletionParams(req.Messages),
		Model:    req.Model,
	}
	if req.Temperature != nil {
		temp := float64(*req.Temperature)
		params.Temperature = &temp
	}

	if req.MaxTokens > 0 {
		maxTokens := int64(req.MaxTokens)
		params.MaxCompletionTokens = &maxTokens
	}
	if effort, ok := openAIReasoningEffort(req.ThinkingEffort); ok {
		params.ReasoningEffort = effort
	}
	if req.ResponseSchema != nil {
		params.ResponseFormat = chatResponseFormatFromSchema(req.ResponseSchema)
	}
	if len(req.Tools) > 0 {
		params.Tools = make([]openai.ChatTool, 0, len(req.Tools))
		for _, tool := range req.Tools {
			params.Tools = append(params.Tools, toolToChatCompletionTool(tool.GetSchema()))
		}
	}

	return params
}

func buildResponsesRequestParams(req *CompletionRequest) *openai.ResponsesRequest {
	inputItems, instructions := messagesToResponsesInput(req.Messages, req.Model)

	// Reasoning models emit reasoning items whether or not an effort was
	// requested, so ask for the encrypted state unconditionally. Responses mode
	// only ever talks to api.openai.com (any custom base URL falls back to chat
	// completions), so there is no compatible-server risk here.
	stateless := false
	params := &openai.ResponsesRequest{
		Input:          inputItems,
		Model:          req.Model,
		Instructions:   instructions,
		PromptCacheKey: req.PromptCacheKey,
		Include:        []string{openai.IncludeReasoningEncryptedContent},
		Store:          &stateless,
	}
	if req.Temperature != nil {
		temp := float64(*req.Temperature)
		params.Temperature = &temp
	}

	if req.MaxTokens > 0 {
		maxTokens := int64(req.MaxTokens)
		params.MaxOutputTokens = &maxTokens
	}
	if reasoning, ok := responsesReasoningFromThinkingEffort(req.ThinkingEffort); ok {
		params.Reasoning = reasoning
	}
	if req.ResponseSchema != nil {
		params.Text = responsesTextConfigFromSchema(req.ResponseSchema)
	}
	if len(req.Tools) > 0 {
		params.Tools = make([]openai.ResponsesTool, 0, len(req.Tools))
		for _, tool := range req.Tools {
			params.Tools = append(params.Tools, toolToResponsesFunctionTool(tool.GetSchema()))
		}
	}

	return params
}

func chatResponseFormatFromSchema(schema *Schema) *openai.ResponseFormat {
	if schema == nil {
		return nil
	}

	strict := schema.Strict
	return &openai.ResponseFormat{
		Type: "json_schema",
		JSONSchema: &openai.JSONSchemaSpec{
			Name:        "response",
			Description: "Structured response",
			Schema:      normalizeOpenAISchema(schema),
			Strict:      &strict,
		},
	}
}

func responsesTextConfigFromSchema(schema *Schema) *openai.TextConfig {
	if schema == nil {
		return nil
	}

	strict := schema.Strict
	return &openai.TextConfig{
		Format: &openai.TextFormat{
			Type:        "json_schema",
			Name:        "response",
			Description: "Structured response",
			Schema:      normalizeOpenAISchema(schema),
			Strict:      &strict,
		},
	}
}

func toolToChatCompletionTool(schema *ToolSchema) openai.ChatTool {
	return openai.ChatTool{
		Type: "function",
		Function: openai.FunctionDef{
			Name:        toolNameFromSchema(schema),
			Description: toolDescriptionFromSchema(schema),
			Parameters:  toolParametersFromSchema(schema),
		},
	}
}

func toolToResponsesFunctionTool(schema *ToolSchema) openai.ResponsesTool {
	params := toolParametersFromSchema(schema)
	strict := schema != nil && schema.Strict
	if strict {
		params = deepCopyMap(params)
		if missing := strictJSONSchemaCompatibilityIssue(params); missing != "" {
			strict = false
			slog.Warn("openai_responses_tool_strict_downgraded",
				"tool", toolNameFromSchema(schema),
				"missing_required", missing,
			)
		} else {
			addObjectAdditionalPropertiesFalse(params)
		}
	}

	return openai.ResponsesTool{
		Type:        "function",
		Name:        toolNameFromSchema(schema),
		Description: toolDescriptionFromSchema(schema),
		Parameters:  params,
		Strict:      &strict,
	}
}

func messagesToChatCompletionParams(msgs []messages.ChatMessage) []openai.ChatMessage {
	result := make([]openai.ChatMessage, 0, len(msgs))
	for _, msg := range msgs {
		result = append(result, messageToChatCompletionParam(msg))
	}
	return result
}

func messageToChatCompletionParam(msg messages.ChatMessage) openai.ChatMessage {
	switch msg.Role {
	case messages.MessageRoleSystem:
		return openai.ChatMessage{Role: "system", Content: msg.GetContent()}
	case messages.MessageRoleTool:
		return openai.ChatMessage{Role: "tool", Content: msg.GetContent(), ToolCallID: msg.ToolCallID}
	case messages.MessageRoleAssistant:
		assistant := openai.ChatMessage{Role: "assistant"}
		if content := msg.GetContent(); content != "" {
			assistant.Content = content
		}
		if len(msg.ToolCalls) > 0 {
			assistant.ToolCalls = make([]openai.ChatToolCall, 0, len(msg.ToolCalls))
			for _, toolCall := range msg.ToolCalls {
				assistant.ToolCalls = append(assistant.ToolCalls, openai.ChatToolCall{
					ID:   toolCall.ID,
					Type: "function",
					Function: openai.ChatToolCallFunc{
						Name:      toolCall.Name,
						Arguments: toolCall.Arguments,
					},
				})
			}
		}
		return assistant
	default:
		content := make([]openai.ChatContentPart, 0, len(msg.Parts)+1)
		if len(msg.Parts) > 0 {
			for _, part := range msg.Parts {
				switch part.Type {
				case "text":
					content = append(content, openai.ChatContentPart{Type: "text", Text: part.Text})
				case "image_base64":
					content = append(content, openai.ChatContentPart{
						Type:     "image_url",
						ImageURL: &openai.ChatImageURL{URL: "data:" + part.MimeType + ";base64," + part.ImageData},
					})
				case "image_url":
					content = append(content, openai.ChatContentPart{
						Type:     "image_url",
						ImageURL: &openai.ChatImageURL{URL: part.ImageURL},
					})
				}
			}
		}
		if len(content) == 0 {
			content = append(content, openai.ChatContentPart{Type: "text", Text: msg.GetContent()})
		}
		return openai.ChatMessage{Role: "user", Content: content}
	}
}

func messagesToResponsesInput(msgs []messages.ChatMessage, model string) ([]openai.ResponseInputItem, string) {
	items := make([]openai.ResponseInputItem, 0, len(msgs))
	systemParts := make([]string, 0, len(msgs))
	replayedToolCallIDs := make(map[string]struct{})

	for messageIndex, msg := range msgs {
		if msg.Role == messages.MessageRoleSystem {
			if content := strings.TrimSpace(msg.GetContent()); content != "" {
				systemParts = append(systemParts, content)
			}
			continue
		}
		items = append(items, messageToResponsesInputItems(msg, model, messageIndex, replayedToolCallIDs)...)
	}

	return items, strings.Join(systemParts, "\n\n")
}

func messageToResponsesInputItems(msg messages.ChatMessage, model string, messageIndex int, replayedToolCallIDs map[string]struct{}) []openai.ResponseInputItem {
	switch msg.Role {
	case messages.MessageRoleUser:
		content := responseInputContentFromMessage(msg)
		if len(content) == 0 {
			return nil
		}
		return []openai.ResponseInputItem{
			{Role: "user", Content: content},
		}
	case messages.MessageRoleAssistant:
		// Reasoning leads the turn: the API wants every item between the last
		// user message and the function call output passed back untouched, in
		// the order the model emitted them.
		replayedReasoning := responsesReasoningReplayItems(msg, model)
		items := make([]openai.ResponseInputItem, 0, len(replayedReasoning)+len(msg.ToolCalls)+1)
		items = append(items, replayedReasoning...)
		if content := responseOutputContentFromMessage(msg); len(content) > 0 {
			items = append(items, openai.ResponseInputItem{
				Type:    "message",
				Role:    "assistant",
				ID:      responseReplayMessageID(messageIndex),
				Status:  "completed",
				Content: content,
			})
		}
		for toolIndex, toolCall := range msg.ToolCalls {
			if strings.TrimSpace(toolCall.Name) == "" {
				continue
			}
			callID := responseReplayToolCallID(toolCall.ID, messageIndex, toolIndex)
			replayedToolCallIDs[callID] = struct{}{}
			// arguments is required on the wire; parameterless calls replay
			// as "{}".
			arguments := toolCall.Arguments
			if strings.TrimSpace(arguments) == "" {
				arguments = "{}"
			}
			items = append(items, openai.ResponseInputItem{
				Type:      "function_call",
				CallID:    callID,
				Name:      toolCall.Name,
				Arguments: arguments,
				Status:    "completed",
			})
		}
		return items
	case messages.MessageRoleTool:
		callID := strings.TrimSpace(msg.ToolCallID)
		if _, ok := replayedToolCallIDs[callID]; !ok {
			return nil
		}
		output := msg.GetContent()
		return []openai.ResponseInputItem{
			{
				Type:   "function_call_output",
				CallID: callID,
				Output: &output,
				Status: "completed",
			},
		}
	default:
		return nil
	}
}

func responseInputContentFromMessage(msg messages.ChatMessage) []openai.ResponseInputContent {
	content := make([]openai.ResponseInputContent, 0, len(msg.Parts)+1)
	if len(msg.Parts) > 0 {
		for _, part := range msg.Parts {
			switch part.Type {
			case "text":
				content = append(content, openai.ResponseInputContent{Type: "input_text", Text: part.Text})
			case "image_base64":
				content = append(content, openai.ResponseInputContent{
					Type:     "input_image",
					Detail:   "auto",
					ImageURL: "data:" + part.MimeType + ";base64," + part.ImageData,
				})
			case "image_url":
				content = append(content, openai.ResponseInputContent{
					Type:     "input_image",
					Detail:   "auto",
					ImageURL: part.ImageURL,
				})
			}
		}
	}
	if len(content) == 0 {
		if text := msg.GetContent(); text != "" {
			content = append(content, openai.ResponseInputContent{Type: "input_text", Text: text})
		}
	}
	return content
}

func responseOutputContentFromMessage(msg messages.ChatMessage) []openai.ResponseOutputContent {
	content := make([]openai.ResponseOutputContent, 0, len(msg.Parts)+1)
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

func responseOutputTextContent(text string) openai.ResponseOutputContent {
	return openai.ResponseOutputContent{
		Type:        "output_text",
		Text:        text,
		Annotations: []any{},
	}
}

func responsesReasoningFromThinkingEffort(effort ThinkingEffort) (*openai.ReasoningParam, bool) {
	reasoning, ok := openAIReasoningEffort(effort)
	if !ok {
		return nil, false
	}
	return &openai.ReasoningParam{
		Effort:  reasoning,
		Summary: "auto",
	}, true
}

// openAIReasoningEffort maps a ThinkingEffort to OpenAI's reasoning_effort enum.
// Off and Dynamic return ok=false (omit the param; OpenAI has no dynamic mode,
// so it falls back to the model's default). A Budget is reduced to its nearest
// level. OpenAI has no "max", so max clamps to xhigh.
func openAIReasoningEffort(effort ThinkingEffort) (openai.ReasoningEffort, bool) {
	if !effort.IsEnabled() || effort.IsDynamic() {
		return "", false
	}
	switch effort.AsLevel(LevelMedium) {
	case LevelMinimal:
		return openai.ReasoningEffortMinimal, true
	case LevelLow:
		return openai.ReasoningEffortLow, true
	case LevelMedium:
		return openai.ReasoningEffortMedium, true
	case LevelHigh:
		return openai.ReasoningEffortHigh, true
	case LevelXHigh, LevelMax:
		return openai.ReasoningEffortXhigh, true
	default:
		return openai.ReasoningEffortMedium, true
	}
}

func normalizeOpenAISchema(schema *Schema) map[string]any {
	if schema == nil {
		return nil
	}

	schemaCopy := deepCopyMap(schema.Raw)
	if !schema.Strict {
		return schemaCopy
	}

	normalizeStrictJSONSchema(schemaCopy)
	return schemaCopy
}

// normalizeStrictJSONSchema walks a JSON-schema map and normalizes every object
// node to satisfy OpenAI Structured Outputs strict mode: sets
// additionalProperties=false and marks every declared property as required.
func normalizeStrictJSONSchema(node map[string]any) {
	if node == nil {
		return
	}
	if t, _ := node["type"].(string); t == "object" {
		node["additionalProperties"] = false
		if props, ok := node["properties"].(map[string]any); ok {
			node["required"] = sortedSchemaKeys(props)
		}
	}
	walkJSONSchemaChildren(node, normalizeStrictJSONSchema)
}

// addObjectAdditionalPropertiesFalse walks a JSON-schema map and sets
// additionalProperties=false on every object node that doesn't already set it.
// Preserves the declared required fields — callers using this for strict tool
// schemas should verify compatibility with strictJSONSchemaCompatibilityIssue.
func addObjectAdditionalPropertiesFalse(node map[string]any) {
	if node == nil {
		return
	}
	if t, _ := node["type"].(string); t == "object" {
		if _, set := node["additionalProperties"]; !set {
			node["additionalProperties"] = false
		}
	}
	walkJSONSchemaChildren(node, addObjectAdditionalPropertiesFalse)
}

// strictJSONSchemaCompatibilityIssue returns a comma-separated list of property
// names that are declared but not marked required on the first incompatible
// object node found. Returns "" when every object node in the tree marks all
// declared properties as required (the OpenAI strict-mode requirement).
func strictJSONSchemaCompatibilityIssue(node map[string]any) string {
	if node == nil {
		return ""
	}
	if t, _ := node["type"].(string); t == "object" {
		if props, ok := node["properties"].(map[string]any); ok {
			required := schemaRequiredSet(node["required"])
			missing := make([]string, 0, len(props))
			for name := range props {
				if _, ok := required[name]; !ok {
					missing = append(missing, name)
				}
			}
			if len(missing) > 0 {
				sort.Strings(missing)
				return strings.Join(missing, ", ")
			}
		}
	}

	var issue string
	walkJSONSchemaChildren(node, func(child map[string]any) {
		if issue != "" {
			return
		}
		issue = strictJSONSchemaCompatibilityIssue(child)
	})
	return issue
}

func walkJSONSchemaChildren(node map[string]any, visit func(child map[string]any)) {
	if node == nil || visit == nil {
		return
	}

	if props, ok := node["properties"].(map[string]any); ok {
		for _, prop := range props {
			if propMap, ok := prop.(map[string]any); ok {
				visit(propMap)
			}
		}
	}

	for _, defsKey := range []string{"$defs", "definitions"} {
		if defs, ok := node[defsKey].(map[string]any); ok {
			for _, def := range defs {
				if defMap, ok := def.(map[string]any); ok {
					visit(defMap)
				}
			}
		}
	}

	if items, ok := node["items"].(map[string]any); ok {
		visit(items)
	} else if itemsList, ok := node["items"].([]any); ok {
		for _, item := range itemsList {
			if itemMap, ok := item.(map[string]any); ok {
				visit(itemMap)
			}
		}
	}

	for _, unionKey := range []string{"anyOf", "oneOf"} {
		if variants, ok := node[unionKey].([]any); ok {
			for _, variant := range variants {
				if variantMap, ok := variant.(map[string]any); ok {
					visit(variantMap)
				}
			}
		}
	}
}

func sortedSchemaKeys(props map[string]any) []string {
	keys := make([]string, 0, len(props))
	for key := range props {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func schemaRequiredSet(raw any) map[string]struct{} {
	required := make(map[string]struct{})
	switch values := raw.(type) {
	case []string:
		for _, value := range values {
			required[value] = struct{}{}
		}
	case []any:
		for _, value := range values {
			if name, ok := value.(string); ok {
				required[name] = struct{}{}
			}
		}
	}
	return required
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

// responsesReasoningReplayItems rebuilds the reasoning items captured from a
// prior assistant turn. Encrypted reasoning is bound to the model that produced
// it — replaying it after a model switch fails to decrypt — so the items are
// dropped when the model no longer matches.
func responsesReasoningReplayItems(msg messages.ChatMessage, model string) []openai.ResponseInputItem {
	if msg.Metadata == nil {
		return nil
	}
	if recorded, _ := msg.Metadata[adapters.ResponsesReasoningModelKey].(string); recorded != model {
		return nil
	}
	// In-process the adapter stores []map[string]any; after a JSON session
	// reload the value comes back as []any.
	var entries []map[string]any
	switch v := msg.Metadata[adapters.ResponsesReasoningItemsKey].(type) {
	case []map[string]any:
		entries = v
	case []any:
		for _, entry := range v {
			if m, ok := entry.(map[string]any); ok {
				entries = append(entries, m)
			}
		}
	}

	items := make([]openai.ResponseInputItem, 0, len(entries))
	for _, entry := range entries {
		id, _ := entry["id"].(string)
		encrypted, _ := entry["encrypted_content"].(string)
		// Without the encrypted state the item carries no reasoning at all, and
		// a bare id points at a response that was never stored server-side.
		if id == "" || encrypted == "" {
			continue
		}
		summary := responsesReasoningSummary(entry["summary"])
		items = append(items, openai.ResponseInputItem{
			Type:             "reasoning",
			ID:               id,
			Summary:          &summary,
			EncryptedContent: encrypted,
		})
	}
	return items
}

// responsesReasoningSummary rebuilds the summary parts of a reasoning item.
// The result is never nil: the API requires the key on a reasoning item even
// when the model produced no summary text.
func responsesReasoningSummary(raw any) []openai.ResponseReasoningSummary {
	var parts []any
	switch v := raw.(type) {
	case []any:
		parts = v
	case []map[string]any:
		for _, part := range v {
			parts = append(parts, part)
		}
	}

	summary := make([]openai.ResponseReasoningSummary, 0, len(parts))
	for _, part := range parts {
		m, ok := part.(map[string]any)
		if !ok {
			continue
		}
		text, _ := m["text"].(string)
		if text == "" {
			continue
		}
		partType, _ := m["type"].(string)
		if partType == "" {
			partType = "summary_text"
		}
		summary = append(summary, openai.ResponseReasoningSummary{Type: partType, Text: text})
	}
	return summary
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
