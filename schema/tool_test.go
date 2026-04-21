package schema

import "testing"

func TestToolSchemaDefaultStrictFalse(t *testing.T) {
	toolSchema := Tool("lookup_weather", "Get weather data", Params{"city": S("City")}, "city")

	if toolSchema.Strict {
		t.Fatal("expected Tool() schemas to default to Strict=false")
	}
}

func TestToolSchemaCopyPreservesStrict(t *testing.T) {
	toolSchema := Tool("lookup_weather", "Get weather data", Params{"city": S("City")}, "city")
	toolSchema.Strict = true

	copied := toolSchema.Copy()
	if copied == nil {
		t.Fatal("expected copy to be non-nil")
	}
	if !copied.Strict {
		t.Fatal("expected copy to preserve Strict=true")
	}
	if copied.Raw["title"] != "lookup_weather" {
		t.Fatalf("copy title = %#v, want %q", copied.Raw["title"], "lookup_weather")
	}
}

func TestToolSchemaFromJSONParsesStrictMetadata(t *testing.T) {
	toolSchema := ToolSchemaFromJSON([]byte(`{
		"title": "lookup_weather",
		"type": "object",
		"strict": true,
		"properties": {
			"city": {"type": "string"}
		}
	}`))

	if toolSchema == nil {
		t.Fatal("expected schema to parse")
	}
	if !toolSchema.Strict {
		t.Fatal("expected top-level strict metadata to set ToolSchema.Strict")
	}
	if _, ok := toolSchema.Raw["strict"]; ok {
		t.Fatalf("expected strict metadata to be removed from Raw, got %#v", toolSchema.Raw["strict"])
	}
}

func TestToolSchemaFromStringIgnoresInvalidStrictMetadata(t *testing.T) {
	toolSchema := ToolSchemaFromString(`{
		"title": "lookup_weather",
		"type": "object",
		"strict": "yes",
		"properties": {
			"city": {"type": "string"}
		}
	}`)

	if toolSchema == nil {
		t.Fatal("expected schema to parse")
	}
	if toolSchema.Strict {
		t.Fatal("expected invalid strict metadata to be ignored")
	}
	if got := toolSchema.Raw["strict"]; got != "yes" {
		t.Fatalf("raw strict metadata = %#v, want %q", got, "yes")
	}
}
