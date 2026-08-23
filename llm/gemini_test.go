package llm

import (
	"encoding/base64"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
)

func TestConvertToolToGemini_PreservesRequiredFromSchemaTool(t *testing.T) {
	toolSchema := schema.Tool(
		"search",
		"Search for documents",
		schema.Params{
			"query": schema.S("Search query"),
			"limit": schema.Int("Max results"),
		},
		"query",
	)

	tool := ConvertToolToGemini(toolSchema)
	if tool == nil {
		t.Fatal("expected non-nil Gemini tool")
	}
	if len(tool.FunctionDeclarations) != 1 {
		t.Fatalf("function declaration count = %d, want 1", len(tool.FunctionDeclarations))
	}

	fd := tool.FunctionDeclarations[0]
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

// TestMessagesToGeminiContentThoughtSignatures verifies signatures are
// restored onto replayed function calls both in-process (map[string]string)
// and after a JSON session reload (map[string]any).
func TestMessagesToGeminiContentThoughtSignatures(t *testing.T) {
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

			contents, _, _ := MessagesToGeminiContent(msgs)
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
