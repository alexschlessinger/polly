package llm

import (
	"context"
	"reflect"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
)

type testTool struct {
	name   string
	schema *schema.ToolSchema
}

func (t testTool) GetSchema() *schema.ToolSchema { return t.schema }
func (t testTool) Execute(context.Context, map[string]any) (string, error) {
	return "", nil
}
func (t testTool) GetName() string   { return t.name }
func (t testTool) GetType() string   { return "native" }
func (t testTool) GetSource() string { return "test" }

var _ tools.Tool = testTool{}

func requireSchemaRequired(t *testing.T, node map[string]any, want ...string) {
	t.Helper()
	got, ok := node["required"].([]string)
	if !ok {
		raw, ok := node["required"].([]any)
		if !ok {
			t.Fatalf("required = %#v, want []string", node["required"])
		}
		got = make([]string, 0, len(raw))
		for _, value := range raw {
			str, ok := value.(string)
			if !ok {
				t.Fatalf("required entry = %#v, want string", value)
			}
			got = append(got, str)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("required = %#v, want %#v", got, want)
	}
}

func requireClosedObjectSchema(t *testing.T, node map[string]any, wantRequired ...string) {
	t.Helper()
	if node["additionalProperties"] != false {
		t.Fatalf("additionalProperties = %#v, want false", node["additionalProperties"])
	}
	requireSchemaRequired(t, node, wantRequired...)
}

func TestOpenAIReasoningEffort(t *testing.T) {
	tests := []struct {
		name   string
		effort ThinkingEffort
		want   openai.ReasoningEffort
		wantOK bool
	}{
		{"off omitted", EffortOff(), "", false},
		{"dynamic omitted", EffortDynamic(), "", false},
		{"minimal", EffortLevel(LevelMinimal), openai.ReasoningEffortMinimal, true},
		{"low", EffortLevel(LevelLow), openai.ReasoningEffortLow, true},
		{"medium", EffortLevel(LevelMedium), openai.ReasoningEffortMedium, true},
		{"high", EffortLevel(LevelHigh), openai.ReasoningEffortHigh, true},
		{"xhigh", EffortLevel(LevelXHigh), openai.ReasoningEffortXhigh, true},
		{"max clamps to xhigh", EffortLevel(LevelMax), openai.ReasoningEffortXhigh, true},
		{"budget maps to nearest level", EffortBudget(4096), openai.ReasoningEffortLow, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := openAIReasoningEffort(tc.effort)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("openAIReasoningEffort(%+v) = (%q, %v), want (%q, %v)", tc.effort, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestChatCompletionReasoningEffort confirms the chat-completions path (which
// DeepSeek and OpenRouter also route through) sets reasoning_effort via the
// shared mapping.
func TestChatCompletionReasoningEffort(t *testing.T) {
	params := buildChatCompletionRequestParams(&CompletionRequest{
		Model:          "deepseek-reasoner",
		MaxTokens:      512,
		ThinkingEffort: EffortLevel(LevelXHigh),
		Messages: []messages.ChatMessage{
			{Role: messages.MessageRoleUser, Content: "hi"},
		},
	})
	if got := params.ReasoningEffort; got != openai.ReasoningEffortXhigh {
		t.Fatalf("reasoning_effort = %q, want %q", got, openai.ReasoningEffortXhigh)
	}
}

func TestNewOpenAIClientRoutesByBaseURL(t *testing.T) {
	native := NewOpenAIClient("key", "")
	if native.apiMode != openAIAPIModeResponses {
		t.Fatalf("native api mode = %q, want %q", native.apiMode, openAIAPIModeResponses)
	}
	if native.baseURL != "" {
		t.Fatalf("native baseURL = %q, want empty", native.baseURL)
	}

	compatible := NewOpenAIClient("key", "https://openrouter.ai/api/v1")
	if compatible.apiMode != openAIAPIModeChat {
		t.Fatalf("compatible api mode = %q, want %q", compatible.apiMode, openAIAPIModeChat)
	}
	if compatible.baseURL != "https://openrouter.ai/api/v1" {
		t.Fatalf("compatible baseURL = %q, want %q", compatible.baseURL, "https://openrouter.ai/api/v1")
	}
}

func TestBuildResponsesRequestParams(t *testing.T) {
	req := &CompletionRequest{
		Model:          "gpt-5.4",
		MaxTokens:      512,
		Temperature:    Float32Ptr(0.2),
		ThinkingEffort: EffortLevel(LevelHigh),
		Messages: []messages.ChatMessage{
			{Role: messages.MessageRoleSystem, Content: "System one"},
			{Role: messages.MessageRoleSystem, Content: "System two"},
			{
				Role: messages.MessageRoleUser,
				Parts: []messages.ContentPart{
					{Type: "text", Text: "look at this"},
					{Type: "image_url", ImageURL: "https://example.com/cat.png"},
				},
			},
			{
				Role:    messages.MessageRoleAssistant,
				Content: "Calling a tool",
				ToolCalls: []messages.ChatMessageToolCall{
					{ID: "call_123", Name: "lookup_weather", Arguments: `{"city":"SF"}`},
				},
			},
			{
				Role:       messages.MessageRoleTool,
				ToolCallID: "call_123",
				Content:    `{"temp_f":65}`,
			},
		},
		ResponseSchema: &Schema{
			Strict: true,
			Raw: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"answer": map[string]any{"type": "string"},
					"meta": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"confidence": map[string]any{"type": "number"},
						},
					},
				},
			},
		},
		Tools: []tools.Tool{
			testTool{
				name: "lookup_weather",
				schema: schema.Tool(
					"lookup_weather",
					"Get forecast data",
					schema.Params{"city": schema.S("City name")},
					"city",
				),
			},
		},
	}

	params := buildResponsesRequestParams(req)

	if got := params.Instructions; got != "System one\n\nSystem two" {
		t.Fatalf("instructions = %q, want %q", got, "System one\n\nSystem two")
	}
	if params.MaxOutputTokens == nil || *params.MaxOutputTokens != 512 {
		t.Fatalf("max output tokens = %v, want 512", params.MaxOutputTokens)
	}
	if params.Reasoning == nil {
		t.Fatal("expected reasoning to be set")
	}
	if got := params.Reasoning.Effort; got != openai.ReasoningEffortHigh {
		t.Fatalf("reasoning effort = %q, want %q", got, openai.ReasoningEffortHigh)
	}
	if got := params.Reasoning.Summary; got != "auto" {
		t.Fatalf("reasoning summary = %q, want %q", got, "auto")
	}
	if len(params.Tools) != 1 {
		t.Fatalf("expected one function tool in responses request")
	}
	if got := params.Tools[0].Name; got != "lookup_weather" {
		t.Fatalf("tool name = %q, want %q", got, "lookup_weather")
	}
	if params.Tools[0].Strict == nil || *params.Tools[0].Strict {
		t.Fatalf("expected responses tool strict mode to be disabled by default")
	}
	if _, ok := params.Tools[0].Parameters["additionalProperties"]; ok {
		t.Fatalf("expected non-strict tool params to omit additionalProperties, got %#v", params.Tools[0].Parameters["additionalProperties"])
	}

	inputItems := params.Input
	if len(inputItems) != 4 {
		t.Fatalf("input item count = %d, want 4", len(inputItems))
	}

	userItem := inputItems[0]
	if userItem.Role != "user" {
		t.Fatalf("user role = %q, want %q", userItem.Role, "user")
	}
	if userItem.Type != "" {
		t.Fatalf("user item type = %q, want empty (inferred message)", userItem.Type)
	}
	userContent, ok := userItem.Content.([]openai.ResponseInputContent)
	if !ok {
		t.Fatalf("user content = %#v, want []openai.ResponseInputContent", userItem.Content)
	}
	if len(userContent) != 2 {
		t.Fatalf("user content part count = %d, want 2", len(userContent))
	}
	if got := userContent[0].Text; got != "look at this" {
		t.Fatalf("user text = %q, want %q", got, "look at this")
	}
	if userContent[1].Type != "input_image" || userContent[1].ImageURL != "https://example.com/cat.png" || userContent[1].Detail != "auto" {
		t.Fatalf("unexpected image part: %#v", userContent[1])
	}

	assistantItem := inputItems[1]
	if assistantItem.Type != "message" || assistantItem.Role != "assistant" {
		t.Fatalf("assistant item = %#v, want message/assistant", assistantItem)
	}
	if got := assistantItem.ID; got != "msg_3" {
		t.Fatalf("assistant ID = %q, want %q", got, "msg_3")
	}
	if got := assistantItem.Status; got != "completed" {
		t.Fatalf("assistant status = %q, want %q", got, "completed")
	}
	assistantContent, ok := assistantItem.Content.([]openai.ResponseOutputContent)
	if !ok || len(assistantContent) != 1 {
		t.Fatalf("expected one assistant output_text content item, got %#v", assistantItem.Content)
	}
	if got := assistantContent[0].Text; got != "Calling a tool" {
		t.Fatalf("assistant text = %q, want %q", got, "Calling a tool")
	}
	if assistantContent[0].Type != "output_text" || assistantContent[0].Annotations == nil {
		t.Fatalf("assistant content = %#v, want output_text with annotations", assistantContent[0])
	}

	toolCallItem := inputItems[2]
	if toolCallItem.Type != "function_call" {
		t.Fatal("expected third item to be a function_call replay")
	}
	if got := toolCallItem.CallID; got != "call_123" {
		t.Fatalf("function call ID = %q, want %q", got, "call_123")
	}
	if got := toolCallItem.Name; got != "lookup_weather" {
		t.Fatalf("function call name = %q, want %q", got, "lookup_weather")
	}
	if got := toolCallItem.Arguments; got != `{"city":"SF"}` {
		t.Fatalf("function call arguments = %q, want %q", got, `{"city":"SF"}`)
	}
	if got := toolCallItem.Status; got != "completed" {
		t.Fatalf("function call status = %q, want %q", got, "completed")
	}

	toolOutputItem := inputItems[3]
	if toolOutputItem.Type != "function_call_output" {
		t.Fatal("expected fourth item to be function_call_output")
	}
	if got := toolOutputItem.CallID; got != "call_123" {
		t.Fatalf("function call output ID = %q, want %q", got, "call_123")
	}
	if got := toolOutputItem.Output; got != `{"temp_f":65}` {
		t.Fatalf("function call output = %q, want %q", got, `{"temp_f":65}`)
	}

	if params.Text == nil || params.Text.Format == nil || params.Text.Format.Type != "json_schema" {
		t.Fatal("expected responses text format to use JSON schema")
	}
	schemaMap := params.Text.Format.Schema
	if schemaMap["additionalProperties"] != false {
		t.Fatalf("expected top-level additionalProperties=false, got %#v", schemaMap["additionalProperties"])
	}
	metaProp := schemaMap["properties"].(map[string]any)["meta"].(map[string]any)
	if metaProp["additionalProperties"] != false {
		t.Fatalf("expected nested object additionalProperties=false, got %#v", metaProp["additionalProperties"])
	}
	requireSchemaRequired(t, metaProp, "confidence")
}

func TestBuildResponsesRequestParamsSkipsInvalidToolReplayItems(t *testing.T) {
	req := &CompletionRequest{
		Model: "gpt-5.4",
		Messages: []messages.ChatMessage{
			{Role: messages.MessageRoleUser, Content: "what containers are running"},
			{
				Role: messages.MessageRoleAssistant,
				ToolCalls: []messages.ChatMessageToolCall{
					{Arguments: `{"command":"ignored"}`},
					{ID: "call_bash", Name: "bash", Arguments: `{"command":"docker ps"}`},
				},
			},
			{
				Role:       messages.MessageRoleTool,
				Content:    "Tool not found:",
				ToolCallID: "",
			},
			{
				Role:       messages.MessageRoleTool,
				Content:    "CONTAINER ID   IMAGE",
				ToolCallID: "call_bash",
			},
		},
	}

	params := buildResponsesRequestParams(req)
	inputItems := params.Input
	if len(inputItems) != 3 {
		t.Fatalf("input item count = %d, want 3", len(inputItems))
	}
	if inputItems[1].Type != "function_call" {
		t.Fatal("expected second item to be a function_call replay")
	}
	if got := inputItems[1].Name; got != "bash" {
		t.Fatalf("function call name = %q, want %q", got, "bash")
	}
	if inputItems[2].Type != "function_call_output" {
		t.Fatal("expected third item to be a function_call_output")
	}
	if got := inputItems[2].CallID; got != "call_bash" {
		t.Fatalf("function call output ID = %q, want %q", got, "call_bash")
	}
}

func TestBuildChatCompletionRequestParams(t *testing.T) {
	req := &CompletionRequest{
		Model:       "gpt-5.4",
		MaxTokens:   256,
		Temperature: Float32Ptr(0),
		Messages: []messages.ChatMessage{
			{
				Role: messages.MessageRoleUser,
				Parts: []messages.ContentPart{
					{Type: "text", Text: "describe"},
					{Type: "image_base64", MimeType: "image/png", ImageData: "AAA"},
				},
			},
		},
		ResponseSchema: &Schema{
			Strict: true,
			Raw: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"answer": map[string]any{"type": "string"},
				},
			},
		},
		Tools: []tools.Tool{
			testTool{
				name: "lookup_weather",
				schema: schema.Tool(
					"lookup_weather",
					"Get forecast data",
					schema.Params{"city": schema.S("City name")},
					"city",
				),
			},
		},
	}

	params := buildChatCompletionRequestParams(req)

	if got := params.Model; got != "gpt-5.4" {
		t.Fatalf("chat model = %q, want %q", got, "gpt-5.4")
	}
	if params.MaxCompletionTokens == nil || *params.MaxCompletionTokens != 256 {
		t.Fatalf("max completion tokens = %v, want 256", params.MaxCompletionTokens)
	}
	if params.Temperature == nil || *params.Temperature != 0 {
		t.Fatalf("temperature = %v, want explicit 0", params.Temperature)
	}
	if len(params.Messages) != 1 || params.Messages[0].Role != "user" {
		t.Fatalf("expected one user chat message")
	}
	userParts, ok := params.Messages[0].Content.([]openai.ChatContentPart)
	if !ok {
		t.Fatalf("user content = %#v, want []openai.ChatContentPart", params.Messages[0].Content)
	}
	if len(userParts) != 2 {
		t.Fatalf("user part count = %d, want 2", len(userParts))
	}
	if got := userParts[0].Text; got != "describe" {
		t.Fatalf("user text = %q, want %q", got, "describe")
	}
	if userParts[1].ImageURL == nil || userParts[1].ImageURL.URL != "data:image/png;base64,AAA" {
		t.Fatalf("image URL = %#v, want data URI", userParts[1].ImageURL)
	}
	if params.ResponseFormat == nil || params.ResponseFormat.JSONSchema == nil {
		t.Fatal("expected chat response format to use JSON schema")
	}
	if len(params.Tools) != 1 || params.Tools[0].Type != "function" {
		t.Fatal("expected one chat function tool")
	}
	if got := params.Tools[0].Function.Name; got != "lookup_weather" {
		t.Fatalf("chat tool name = %q, want %q", got, "lookup_weather")
	}
}

func TestNormalizeOpenAISchemaStrictRecursesWithoutMutatingInput(t *testing.T) {
	raw := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"choice": map[string]any{
				"anyOf": []any{
					map[string]any{
						"type": "object",
						"properties": map[string]any{
							"kind": map[string]any{"type": "string"},
						},
					},
					map[string]any{"type": "string"},
				},
			},
			"steps": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name": map[string]any{"type": "string"},
					},
				},
			},
			"payload": map[string]any{
				"$ref": "#/$defs/payload",
			},
		},
		"$defs": map[string]any{
			"payload": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"meta": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"count": map[string]any{"type": "integer"},
						},
					},
				},
			},
		},
	}

	normalized := normalizeOpenAISchema(&Schema{Raw: raw, Strict: true})

	requireClosedObjectSchema(t, normalized, "choice", "payload", "steps")

	stepsItem := normalized["properties"].(map[string]any)["steps"].(map[string]any)["items"].(map[string]any)
	requireClosedObjectSchema(t, stepsItem, "name")

	defPayload := normalized["$defs"].(map[string]any)["payload"].(map[string]any)
	requireClosedObjectSchema(t, defPayload, "meta")

	meta := defPayload["properties"].(map[string]any)["meta"].(map[string]any)
	requireClosedObjectSchema(t, meta, "count")

	anyOfObject := normalized["properties"].(map[string]any)["choice"].(map[string]any)["anyOf"].([]any)[0].(map[string]any)
	requireClosedObjectSchema(t, anyOfObject, "kind")

	originalStepsItem := raw["properties"].(map[string]any)["steps"].(map[string]any)["items"].(map[string]any)
	if _, mutated := originalStepsItem["additionalProperties"]; mutated {
		t.Fatalf("expected original array item schema to remain unmodified, got %#v", originalStepsItem["additionalProperties"])
	}
	if _, mutated := originalStepsItem["required"]; mutated {
		t.Fatalf("expected original array item schema required list to remain unmodified, got %#v", originalStepsItem["required"])
	}

	originalPayload := raw["$defs"].(map[string]any)["payload"].(map[string]any)
	if _, mutated := originalPayload["additionalProperties"]; mutated {
		t.Fatalf("expected original $defs payload schema to remain unmodified, got %#v", originalPayload["additionalProperties"])
	}

	originalAnyOfObject := raw["properties"].(map[string]any)["choice"].(map[string]any)["anyOf"].([]any)[0].(map[string]any)
	if _, mutated := originalAnyOfObject["additionalProperties"]; mutated {
		t.Fatalf("expected original anyOf object schema to remain unmodified, got %#v", originalAnyOfObject["additionalProperties"])
	}
}

func TestToolToResponsesFunctionToolStrictModeRecursesWhenCompatible(t *testing.T) {
	toolSchema := schema.Tool(
		"batch_lookup",
		"Resolve a batch of lookups",
		schema.Params{
			"items": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"city": map[string]any{"type": "string"},
						"meta": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"country": map[string]any{"type": "string"},
							},
							"required": []string{"country"},
						},
					},
					"required": []string{"city", "meta"},
				},
			},
			"profile": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prefs": map[string]any{
						"$ref": "#/$defs/prefs",
					},
				},
				"required": []string{"prefs"},
				"$defs": map[string]any{
					"prefs": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"lang": map[string]any{"type": "string"},
						},
						"required": []string{"lang"},
					},
				},
			},
		},
		"items", "profile",
	)
	toolSchema.Strict = true

	tool := toolToResponsesFunctionTool(toolSchema)

	if tool.Strict == nil || !*tool.Strict {
		t.Fatalf("expected strict tool to enable Responses strict mode")
	}
	if tool.Parameters["additionalProperties"] != false {
		t.Fatalf("expected strict tool params to set top-level additionalProperties=false, got %#v", tool.Parameters["additionalProperties"])
	}

	itemsParam, ok := tool.Parameters["properties"].(map[string]any)["items"].(map[string]any)
	if !ok {
		t.Fatalf("expected array parameter schema, got %#v", tool.Parameters["properties"])
	}
	itemSchema, ok := itemsParam["items"].(map[string]any)
	if !ok {
		t.Fatalf("expected array item schema, got %#v", itemsParam["items"])
	}
	if itemSchema["additionalProperties"] != false {
		t.Fatalf("expected array item additionalProperties=false, got %#v", itemSchema["additionalProperties"])
	}
	nestedMeta := itemSchema["properties"].(map[string]any)["meta"].(map[string]any)
	if nestedMeta["additionalProperties"] != false {
		t.Fatalf("expected nested object additionalProperties=false, got %#v", nestedMeta["additionalProperties"])
	}

	profileParam, ok := tool.Parameters["properties"].(map[string]any)["profile"].(map[string]any)
	if !ok {
		t.Fatalf("expected profile parameter schema, got %#v", tool.Parameters["properties"])
	}
	if profileParam["additionalProperties"] != false {
		t.Fatalf("expected profile additionalProperties=false, got %#v", profileParam["additionalProperties"])
	}
	prefsDef := profileParam["$defs"].(map[string]any)["prefs"].(map[string]any)
	if prefsDef["additionalProperties"] != false {
		t.Fatalf("expected $defs object additionalProperties=false, got %#v", prefsDef["additionalProperties"])
	}

	originalItemsParam, ok := toolSchema.Properties()["items"].(map[string]any)
	if !ok {
		t.Fatalf("expected original array parameter schema, got %#v", toolSchema.Properties())
	}
	originalItemSchema, ok := originalItemsParam["items"].(map[string]any)
	if !ok {
		t.Fatalf("expected original array item schema, got %#v", originalItemsParam["items"])
	}
	if _, mutated := originalItemSchema["additionalProperties"]; mutated {
		t.Fatalf("expected original schema to remain unmodified, got %#v", originalItemSchema["additionalProperties"])
	}

	originalProfileParam, ok := toolSchema.Properties()["profile"].(map[string]any)
	if !ok {
		t.Fatalf("expected original profile schema, got %#v", toolSchema.Properties())
	}
	if _, mutated := originalProfileParam["additionalProperties"]; mutated {
		t.Fatalf("expected original profile schema to remain unmodified, got %#v", originalProfileParam["additionalProperties"])
	}
	originalPrefsDef := originalProfileParam["$defs"].(map[string]any)["prefs"].(map[string]any)
	if _, mutated := originalPrefsDef["additionalProperties"]; mutated {
		t.Fatalf("expected original $defs schema to remain unmodified, got %#v", originalPrefsDef["additionalProperties"])
	}
}

func TestToolToResponsesFunctionToolStrictModeDowngradesOptionalTopLevelField(t *testing.T) {
	toolSchema := schema.Tool(
		"lookup_weather",
		"Get weather data",
		schema.Params{
			"city": map[string]any{"type": "string"},
			"unit": map[string]any{"type": "string"},
		},
		"city",
	)
	toolSchema.Strict = true

	tool := toolToResponsesFunctionTool(toolSchema)

	if tool.Strict == nil || *tool.Strict {
		t.Fatalf("expected incompatible strict tool to downgrade to non-strict")
	}
	if _, ok := tool.Parameters["additionalProperties"]; ok {
		t.Fatalf("expected downgraded tool params to preserve original top-level schema, got %#v", tool.Parameters["additionalProperties"])
	}
	requireSchemaRequired(t, tool.Parameters, "city")
}

func TestToolToResponsesFunctionToolStrictModeDowngradesOptionalNestedField(t *testing.T) {
	toolSchema := schema.Tool(
		"lookup_weather",
		"Get weather data",
		schema.Params{
			"filters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"city": map[string]any{"type": "string"},
					"unit": map[string]any{"type": "string"},
				},
				"required": []string{"city"},
			},
		},
		"filters",
	)
	toolSchema.Strict = true

	tool := toolToResponsesFunctionTool(toolSchema)

	if tool.Strict == nil || *tool.Strict {
		t.Fatalf("expected incompatible nested strict tool to downgrade to non-strict")
	}
	if _, ok := tool.Parameters["additionalProperties"]; ok {
		t.Fatalf("expected downgraded tool params to preserve original top-level schema, got %#v", tool.Parameters["additionalProperties"])
	}

	filtersParam, ok := tool.Parameters["properties"].(map[string]any)["filters"].(map[string]any)
	if !ok {
		t.Fatalf("expected filters parameter schema, got %#v", tool.Parameters["properties"])
	}
	if _, ok := filtersParam["additionalProperties"]; ok {
		t.Fatalf("expected downgraded nested tool schema to preserve original object openness, got %#v", filtersParam["additionalProperties"])
	}
	requireSchemaRequired(t, filtersParam, "city")

	originalFilters := toolSchema.Properties()["filters"].(map[string]any)
	if _, mutated := originalFilters["additionalProperties"]; mutated {
		t.Fatalf("expected original nested schema to remain unmodified, got %#v", originalFilters["additionalProperties"])
	}
}
