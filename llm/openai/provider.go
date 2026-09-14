package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
)

const responsesReasoningSummaryKey = "openai_responses_reasoning_summary_seen"

type apiMode string

const (
	apiModeChat      apiMode = "chat"
	apiModeResponses apiMode = "responses"
)

var _ contract.LLM = (*Provider)(nil)

// Provider serves OpenAI's Responses API and, for any custom base URL, the
// Chat Completions API of OpenAI-compatible servers. Gateways with
// extensions of their own (llm/openrouter, llm/deepseek) build on this
// package's transport rather than on this provider.
type Provider struct {
	client  *Client
	baseURL string
	apiMode apiMode
}

// NewProvider returns a provider for the public OpenAI API when baseURL is
// empty (Responses mode), or the Chat Completions API at baseURL otherwise.
func NewProvider(apiKey string, baseURL string) *Provider {
	trimmedBaseURL := strings.TrimSpace(baseURL)
	mode := apiModeResponses
	if trimmedBaseURL != "" {
		mode = apiModeChat
	}

	return &Provider{
		client:  NewClient(apiKey, trimmedBaseURL),
		baseURL: trimmedBaseURL,
		apiMode: mode,
	}
}

// NewResponsesProvider returns a native Responses API provider whose
// transport points at baseURL. NewProvider selects the Chat Completions
// compatibility path for any explicit base URL; hosts (and tests) that serve
// the Responses API elsewhere use this constructor instead.
func NewResponsesProvider(apiKey, baseURL string) *Provider {
	p := NewProvider(apiKey, "")
	p.client = NewClient(apiKey, strings.TrimSpace(baseURL))
	return p
}

// ChatCompletionStream implements the event-based streaming interface.
func (o Provider) ChatCompletionStream(ctx context.Context, req *contract.CompletionRequest, processor contract.EventStreamProcessor) <-chan *messages.StreamEvent {
	var adapter streaming.ProviderAdapter = NewChatAdapter()
	if o.apiMode == apiModeResponses {
		adapter = NewResponsesAdapter(req.Model)
	}

	return contract.RunStream(ctx, req.Timeout, req.Deadline, processor, adapter, func(ctx context.Context, streamCore *streaming.StreamingCore) {
		if err := o.streamCompletion(ctx, req, streamCore); err != nil {
			streamCore.EmitError(err)
		}
	})
}

func (o Provider) streamCompletion(ctx context.Context, req *contract.CompletionRequest, streamCore *streaming.StreamingCore) error {
	switch o.apiMode {
	case apiModeResponses:
		return o.streamResponses(ctx, req, streamCore)
	default:
		return o.streamChatCompletions(ctx, req, streamCore)
	}
}

func (o Provider) streamChatCompletions(ctx context.Context, req *contract.CompletionRequest, streamCore *streaming.StreamingCore) error {
	params := BuildChatCompletionRequest(req)
	isStreaming := req.IsStreaming()
	slog.Debug("openai_chat_completion_started", "stream", isStreaming, "base_url", o.baseURL)

	if isStreaming {
		return StreamChat(ctx, o.client, params, streamCore)
	}
	return CompleteChat(ctx, o.client, params, streamCore)
}

func (o Provider) streamResponses(ctx context.Context, req *contract.CompletionRequest, streamCore *streaming.StreamingCore) error {
	params := BuildResponsesRequest(req)
	isStreaming := req.IsStreaming()
	slog.Debug("openai_responses_started", "stream", isStreaming, "base_url", o.baseURL)

	if isStreaming {
		return StreamResponses(ctx, o.client, params, streamCore)
	}
	return CompleteResponses(ctx, o.client, params, streamCore)
}

// BuildChatCompletionRequest converts a completion request into the Chat
// Completions wire shape shared by OpenAI-compatible providers. Messages map
// 1:1 and in order to req.Messages, so callers can annotate them by index.
func BuildChatCompletionRequest(req *contract.CompletionRequest) *ChatCompletionRequest {
	params := &ChatCompletionRequest{
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
	if effort, ok := reasoningEffortFromThinking(req.ThinkingEffort); ok {
		params.ReasoningEffort = effort
	}
	if req.ResponseSchema != nil {
		params.ResponseFormat = chatResponseFormatFromSchema(req.ResponseSchema)
	}
	if len(req.Tools) > 0 {
		params.Tools = make([]ChatTool, 0, len(req.Tools))
		for _, tool := range req.Tools {
			params.Tools = append(params.Tools, toolToChatCompletionTool(tool.GetSchema()))
		}
	}

	return params
}

// ReasoningReplay returns the reasoning items that lead a replayed assistant
// turn. BuildResponsesRequest replays the encrypted items the OpenAI API
// returned; a gateway with its own reasoning format supplies its own.
type ReasoningReplay func(msg messages.ChatMessage, model string) []ResponseInputItem

// BuildResponsesRequest converts a completion request into the Responses API
// wire shape, replaying OpenAI's encrypted reasoning items.
func BuildResponsesRequest(req *contract.CompletionRequest) *ResponsesRequest {
	return BuildResponsesRequestWith(req, responsesReasoningReplayItems)
}

// BuildResponsesRequestWith is BuildResponsesRequest with the reasoning
// replay a gateway supplies.
func BuildResponsesRequestWith(req *contract.CompletionRequest, replay ReasoningReplay) *ResponsesRequest {
	inputItems, instructions := messagesToResponsesInput(req.Messages, req.Model, replay)

	// Reasoning models emit reasoning items whether or not an effort was
	// requested, so ask for the encrypted state unconditionally. Responses mode
	// only ever talks to api.openai.com (any custom base URL falls back to chat
	// completions), so there is no compatible-server risk here.
	stateless := false
	params := &ResponsesRequest{
		Input:          inputItems,
		Model:          req.Model,
		Instructions:   instructions,
		PromptCacheKey: req.PromptCacheKey,
		Include:        []string{IncludeReasoningEncryptedContent},
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
		params.Tools = make([]ResponsesTool, 0, len(req.Tools))
		for _, tool := range req.Tools {
			params.Tools = append(params.Tools, toolToResponsesFunctionTool(tool.GetSchema()))
		}
	}

	return params
}

func chatResponseFormatFromSchema(s *schema.Schema) *ResponseFormat {
	if s == nil {
		return nil
	}

	strict := s.Strict
	return &ResponseFormat{
		Type: "json_schema",
		JSONSchema: &JSONSchemaSpec{
			Name:        "response",
			Description: "Structured response",
			Schema:      normalizeSchema(s),
			Strict:      &strict,
		},
	}
}

func responsesTextConfigFromSchema(s *schema.Schema) *TextConfig {
	if s == nil {
		return nil
	}

	strict := s.Strict
	return &TextConfig{
		Format: &TextFormat{
			Type:        "json_schema",
			Name:        "response",
			Description: "Structured response",
			Schema:      normalizeSchema(s),
			Strict:      &strict,
		},
	}
}

func toolToChatCompletionTool(ts *schema.ToolSchema) ChatTool {
	return ChatTool{
		Type: "function",
		Function: FunctionDef{
			Name:        ts.Title(),
			Description: ts.Description(),
			Parameters:  ts.Parameters(),
		},
	}
}

func toolToResponsesFunctionTool(ts *schema.ToolSchema) ResponsesTool {
	params := ts.Parameters()
	strict := ts != nil && ts.Strict
	if strict {
		params = deepCopyMap(params)
		if missing := strictJSONSchemaCompatibilityIssue(params); missing != "" {
			strict = false
			slog.Warn("openai_responses_tool_strict_downgraded",
				"tool", ts.Title(),
				"missing_required", missing,
			)
		} else {
			addObjectAdditionalPropertiesFalse(params)
		}
	}

	return ResponsesTool{
		Type:        "function",
		Name:        ts.Title(),
		Description: ts.Description(),
		Parameters:  params,
		Strict:      &strict,
	}
}

func messagesToChatCompletionParams(msgs []messages.ChatMessage) []ChatMessage {
	result := make([]ChatMessage, 0, len(msgs))
	for _, msg := range msgs {
		result = append(result, messageToChatCompletionParam(msg))
	}
	return result
}

func messageToChatCompletionParam(msg messages.ChatMessage) ChatMessage {
	switch msg.Role {
	case messages.MessageRoleSystem:
		return ChatMessage{Role: "system", Content: msg.GetContent()}
	case messages.MessageRoleTool:
		return ChatMessage{Role: "tool", Content: msg.GetContent(), ToolCallID: msg.ToolCallID}
	case messages.MessageRoleAssistant:
		assistant := ChatMessage{Role: "assistant"}
		if content := msg.GetContent(); content != "" {
			assistant.Content = content
		}
		if len(msg.ToolCalls) > 0 {
			assistant.ToolCalls = make([]ChatToolCall, 0, len(msg.ToolCalls))
			for _, toolCall := range msg.ToolCalls {
				assistant.ToolCalls = append(assistant.ToolCalls, ChatToolCall{
					ID:   toolCall.ID,
					Type: "function",
					Function: ChatToolCallFunc{
						Name:      toolCall.Name,
						Arguments: toolCall.Arguments,
					},
				})
			}
		}
		return assistant
	default:
		content := make([]ChatContentPart, 0, len(msg.Parts)+1)
		for _, part := range msg.Parts {
			switch part.Type {
			case "text":
				content = append(content, ChatContentPart{Type: "text", Text: part.Text})
			case "image_base64":
				content = append(content, ChatContentPart{
					Type:     "image_url",
					ImageURL: &ChatImageURL{URL: "data:" + part.MimeType + ";base64," + part.ImageData},
				})
			case "image_url":
				content = append(content, ChatContentPart{
					Type:     "image_url",
					ImageURL: &ChatImageURL{URL: part.ImageURL},
				})
			}
		}
		if len(content) == 0 {
			content = append(content, ChatContentPart{Type: "text", Text: msg.GetContent()})
		}
		return ChatMessage{Role: "user", Content: content}
	}
}

func messagesToResponsesInput(msgs []messages.ChatMessage, model string, replay ReasoningReplay) ([]ResponseInputItem, string) {
	items := make([]ResponseInputItem, 0, len(msgs))
	systemParts := make([]string, 0, len(msgs))
	replayedToolCallIDs := make(map[string]struct{})

	for messageIndex, msg := range msgs {
		if msg.Role == messages.MessageRoleSystem {
			if content := strings.TrimSpace(msg.GetContent()); content != "" {
				systemParts = append(systemParts, content)
			}
			continue
		}
		items = append(items, messageToResponsesInputItems(msg, model, messageIndex, replayedToolCallIDs, replay)...)
	}

	return items, strings.Join(systemParts, "\n\n")
}

func messageToResponsesInputItems(msg messages.ChatMessage, model string, messageIndex int, replayedToolCallIDs map[string]struct{}, replay ReasoningReplay) []ResponseInputItem {
	switch msg.Role {
	case messages.MessageRoleUser:
		content := responseInputContentFromMessage(msg)
		if len(content) == 0 {
			return nil
		}
		return []ResponseInputItem{
			{Role: "user", Content: content},
		}
	case messages.MessageRoleAssistant:
		// Reasoning leads the turn: the API wants every item between the last
		// user message and the function call output passed back untouched, in
		// the order the model emitted them.
		replayedReasoning := replay(msg, model)
		items := make([]ResponseInputItem, 0, len(replayedReasoning)+len(msg.ToolCalls)+1)
		items = append(items, replayedReasoning...)
		if content := responseOutputContentFromMessage(msg); len(content) > 0 {
			items = append(items, ResponseInputItem{
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
			items = append(items, ResponseInputItem{
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
		return []ResponseInputItem{
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

func responseInputContentFromMessage(msg messages.ChatMessage) []ResponseInputContent {
	content := make([]ResponseInputContent, 0, len(msg.Parts)+1)
	for _, part := range msg.Parts {
		switch part.Type {
		case "text":
			content = append(content, ResponseInputContent{Type: "input_text", Text: part.Text})
		case "image_base64":
			content = append(content, ResponseInputContent{
				Type:     "input_image",
				Detail:   "auto",
				ImageURL: "data:" + part.MimeType + ";base64," + part.ImageData,
			})
		case "image_url":
			content = append(content, ResponseInputContent{
				Type:     "input_image",
				Detail:   "auto",
				ImageURL: part.ImageURL,
			})
		}
	}
	if len(content) == 0 {
		if text := msg.GetContent(); text != "" {
			content = append(content, ResponseInputContent{Type: "input_text", Text: text})
		}
	}
	return content
}

func responseOutputContentFromMessage(msg messages.ChatMessage) []ResponseOutputContent {
	content := make([]ResponseOutputContent, 0, len(msg.Parts)+1)
	for _, part := range msg.Parts {
		if part.Type == "text" {
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

func responseOutputTextContent(text string) ResponseOutputContent {
	return ResponseOutputContent{
		Type:        "output_text",
		Text:        text,
		Annotations: []any{},
	}
}

func responsesReasoningFromThinkingEffort(effort contract.ThinkingEffort) (*ReasoningParam, bool) {
	reasoning, ok := reasoningEffortFromThinking(effort)
	if !ok {
		return nil, false
	}
	return &ReasoningParam{
		Effort:  reasoning,
		Summary: "auto",
	}, true
}

// reasoningEffortFromThinking maps a ThinkingEffort to OpenAI's reasoning_effort enum.
// Off and Dynamic return ok=false (omit the param; OpenAI has no dynamic mode,
// so it falls back to the model's default). A Budget is reduced to its nearest
// level. OpenAI has no "max", so max clamps to xhigh.
func reasoningEffortFromThinking(effort contract.ThinkingEffort) (ReasoningEffort, bool) {
	if !effort.IsEnabled() || effort.IsDynamic() {
		return "", false
	}
	switch effort.AsLevel(contract.LevelMedium) {
	case contract.LevelMinimal:
		return ReasoningEffortMinimal, true
	case contract.LevelLow:
		return ReasoningEffortLow, true
	case contract.LevelMedium:
		return ReasoningEffortMedium, true
	case contract.LevelHigh:
		return ReasoningEffortHigh, true
	case contract.LevelXHigh, contract.LevelMax:
		return ReasoningEffortXhigh, true
	default:
		return ReasoningEffortMedium, true
	}
}

func normalizeSchema(s *schema.Schema) map[string]any {
	if s == nil {
		return nil
	}

	schemaCopy := deepCopyMap(s.Raw)
	if !s.Strict {
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
			node["required"] = slices.Sorted(maps.Keys(props))
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
				slices.Sort(missing)
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
func responsesReasoningReplayItems(msg messages.ChatMessage, model string) []ResponseInputItem {
	if msg.Metadata == nil {
		return nil
	}
	if recorded, _ := msg.Metadata[ResponsesReasoningModelKey].(string); recorded != model {
		return nil
	}
	entries := contract.MetadataMapList(msg.Metadata[ResponsesReasoningItemsKey])

	items := make([]ResponseInputItem, 0, len(entries))
	for _, entry := range entries {
		id, _ := entry["id"].(string)
		encrypted, _ := entry["encrypted_content"].(string)
		// Without the encrypted state the item carries no reasoning at all, and
		// a bare id points at a response that was never stored server-side.
		if id == "" || encrypted == "" {
			continue
		}
		summary := responsesReasoningSummary(entry["summary"])
		items = append(items, ResponseInputItem{
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
func responsesReasoningSummary(raw any) []ResponseReasoningSummary {
	var parts []any
	switch v := raw.(type) {
	case []any:
		parts = v
	case []map[string]any:
		for _, part := range v {
			parts = append(parts, part)
		}
	}

	summary := make([]ResponseReasoningSummary, 0, len(parts))
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
		summary = append(summary, ResponseReasoningSummary{Type: partType, Text: text})
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
	if copied, ok := copySchemaValue(input, make(map[schemaVisit]bool)); ok {
		return copied.(map[string]any)
	}
	// Preserve the previous JSON-copy fallback for unsupported values and
	// cyclic caller-provided annotations, without recursing indefinitely.
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

type schemaVisit struct {
	mapValue  reflect.Value
	sliceData uintptr
	sliceLen  int
}

func copySchemaValue(value any, active map[schemaVisit]bool) (any, bool) {
	switch v := value.(type) {
	case map[string]any:
		if v == nil {
			return nil, true
		}
		visit := schemaVisit{mapValue: reflect.ValueOf(v)}
		if active[visit] {
			return nil, false
		}
		active[visit] = true
		defer delete(active, visit)
		out := make(map[string]any, len(v))
		for key, item := range v {
			copied, ok := copySchemaValue(item, active)
			if !ok {
				return nil, false
			}
			out[key] = copied
		}
		return out, true
	case []any:
		if v == nil {
			return nil, true
		}
		visit := schemaVisit{sliceData: reflect.ValueOf(v).Pointer(), sliceLen: len(v)}
		if active[visit] {
			return nil, false
		}
		active[visit] = true
		defer delete(active, visit)
		out := make([]any, len(v))
		for i, item := range v {
			copied, ok := copySchemaValue(item, active)
			if !ok {
				return nil, false
			}
			out[i] = copied
		}
		return out, true
	case []string:
		// Keep the JSON round trip's canonical container shape, including
		// schema unions and required-property lists supplied by Go callers.
		if v == nil {
			return nil, true
		}
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = item
		}
		return out, true
	case nil, bool, string, float64:
		return v, true
	default:
		// Typed containers and custom JSON marshalers are uncommon schema
		// leaves. Normalize those alone, rather than serializing every schema.
		if raw, err := json.Marshal(value); err == nil {
			var out any
			if json.Unmarshal(raw, &out) == nil {
				return out, true
			}
		}
		return nil, false
	}
}
