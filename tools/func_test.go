package tools

import "testing"

func TestFuncGetSchemaPropagatesStrict(t *testing.T) {
	tool := &Func{
		Name:   "lookup_weather",
		Desc:   "Get weather data",
		Params: map[string]any{"city": map[string]any{"type": "string"}},
		Strict: true,
	}

	schema := tool.GetSchema()
	if schema == nil {
		t.Fatal("expected schema to be non-nil")
	}
	if !schema.Strict {
		t.Fatal("expected Func.Strict to propagate to ToolSchema.Strict")
	}
}
