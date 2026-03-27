package llm

import (
	"testing"

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
