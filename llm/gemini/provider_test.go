package gemini

import (
	"encoding/base64"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
)

func TestConvertTool_PreservesRequiredFromSchemaTool(t *testing.T) {
	toolSchema := schema.Tool(
		"search",
		"Search for documents",
		schema.Params{
			"query": schema.S("Search query"),
			"limit": schema.Int("Max results"),
		},
		"query",
	)

	fd := convertTool(toolSchema)
	if fd == nil {
		t.Fatal("expected non-nil Gemini function declaration")
	}
	if fd.Name != "search" {
		t.Fatalf("name = %q, want %q", fd.Name, "search")
	}
	if fd.Description != "Search for documents" {
		t.Fatalf("description = %q, want %q", fd.Description, "Search for documents")
	}

	// ParametersJsonSchema should contain only parameter fields, not title/description
	params, ok := fd.ParametersJsonSchema.(map[string]any)
	if !ok {
		t.Fatalf("ParametersJsonSchema type = %T, want map[string]any", fd.ParametersJsonSchema)
	}
	if _, ok := params["title"]; ok {
		t.Fatal("ParametersJsonSchema should not contain title")
	}
	if _, ok := params["description"]; ok {
		t.Fatal("ParametersJsonSchema should not contain description")
	}
	req, ok := params["required"].([]string)
	if !ok {
		t.Fatalf("required type = %T, want []string", params["required"])
	}
	if len(req) != 1 || req[0] != "query" {
		t.Fatalf("required = %v, want [query]", req)
	}
}

// TestMessagesToContentThoughtSignatures verifies signatures are
// restored onto replayed function calls both in-process (map[string]string)
// and after a JSON session reload (map[string]any).
func TestMessagesToContentThoughtSignatures(t *testing.T) {
	sig := []byte("thought-sig-bytes")
	encoded := base64.StdEncoding.EncodeToString(sig)

	cases := []struct {
		name     string
		metadata map[string]any
	}{
		{
			name:     "in-process map[string]string",
			metadata: map[string]any{"gemini_thought_signatures": map[string]string{"gemini-ab-0": encoded}},
		},
		{
			name:     "JSON-reloaded map[string]any",
			metadata: map[string]any{"gemini_thought_signatures": map[string]any{"gemini-ab-0": encoded}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := []messages.ChatMessage{{
				Role: messages.MessageRoleAssistant,
				ToolCalls: []messages.ChatMessageToolCall{
					{ID: "gemini-ab-0", Name: "search", Arguments: "{}"},
				},
				Metadata: tc.metadata,
			}}

			contents, _ := messagesToContent(msgs, &contract.ReplayCache{})
			if len(contents) != 1 || len(contents[0].Parts) != 1 {
				t.Fatalf("unexpected content shape: %+v", contents)
			}
			part := contents[0].Parts[0]
			if part.FunctionCall == nil {
				t.Fatal("expected a function call part")
			}
			if string(part.ThoughtSignature) != string(sig) {
				t.Errorf("ThoughtSignature = %q, want %q", part.ThoughtSignature, sig)
			}
		})
	}
}

// TestMessagesToContentNativeCallIDs verifies provider-issued function
// call IDs are echoed back on both the replayed call and its response, while
// polly-synthesized IDs (gemini-<nonce>-<n>) are kept internal.
func TestMessagesToContentNativeCallIDs(t *testing.T) {
	msgs := []messages.ChatMessage{
		{
			Role: messages.MessageRoleAssistant,
			ToolCalls: []messages.ChatMessageToolCall{
				{ID: "native-id-1", Name: "search", Arguments: "{}"},
			},
		},
		{Role: messages.MessageRoleTool, ToolCallID: "native-id-1", ToolName: "search", Content: `{"ok":true}`},
		{
			Role: messages.MessageRoleAssistant,
			ToolCalls: []messages.ChatMessageToolCall{
				{ID: "gemini-ab12cd34-0", Name: "search", Arguments: "{}"},
			},
		},
		{Role: messages.MessageRoleTool, ToolCallID: "gemini-ab12cd34-0", ToolName: "search", Content: `{"ok":true}`},
	}

	contents, _ := messagesToContent(msgs, &contract.ReplayCache{})
	if len(contents) != 4 {
		t.Fatalf("content count = %d, want 4", len(contents))
	}

	if id := contents[0].Parts[0].FunctionCall.ID; id != "native-id-1" {
		t.Errorf("native FunctionCall.ID = %q, want native-id-1", id)
	}
	if id := contents[1].Parts[0].FunctionResponse.ID; id != "native-id-1" {
		t.Errorf("native FunctionResponse.ID = %q, want native-id-1", id)
	}
	if id := contents[2].Parts[0].FunctionCall.ID; id != "" {
		t.Errorf("synthetic FunctionCall.ID = %q, want empty", id)
	}
	if id := contents[3].Parts[0].FunctionResponse.ID; id != "" {
		t.Errorf("synthetic FunctionResponse.ID = %q, want empty", id)
	}
}

func TestJSONSchemaToSchemaTypeUnions(t *testing.T) {
	// jsonschema-go emits ["null","array"] for nil-able Go slices; Gemini's
	// typed schema needs a single type plus nullable, or the API 400s.
	raw := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tags": map[string]any{
				"type":  []any{"null", "array"},
				"items": map[string]any{"type": "string"},
			},
			"title": map[string]any{"type": "string"},
		},
		"required": []any{"tags", "title"},
	}

	s := jsonSchemaToSchema(raw)
	tags := s.Properties["tags"]
	if tags.Type != TypeArray {
		t.Errorf("tags.Type = %q, want ARRAY", tags.Type)
	}
	if !tags.Nullable {
		t.Error("tags.Nullable = false, want true")
	}
	if tags.Items == nil || tags.Items.Type != TypeString {
		t.Errorf("tags.Items = %+v, want STRING", tags.Items)
	}
	if title := s.Properties["title"]; title.Type != TypeString || title.Nullable {
		t.Errorf("title = %+v, want non-nullable STRING", title)
	}
}
