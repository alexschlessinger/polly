package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOpenAIStructuralSchemaCopyIsolation(t *testing.T) {
	input := map[string]any{
		"type":       "object",
		"properties": map[string]any{"child": map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}}},
		"required":   []string{"child"},
		"anyOf":      []map[string]any{{"type": "object", "properties": map[string]any{"n": map[string]string{"type": "number"}}}},
		"default":    json.RawMessage(`{"a":1}`),
	}
	before, _ := json.Marshal(input)
	copy := deepCopyMap(input)
	normalizeStrictJSONSchema(copy)
	after, _ := json.Marshal(input)
	if string(before) != string(after) {
		t.Fatalf("normalization mutated input:\nbefore %s\nafter %s", before, after)
	}
	if copy["additionalProperties"] != false || copy["anyOf"].([]any)[0].(map[string]any)["additionalProperties"] != false {
		t.Fatalf("typed schema containers were not normalized: %+v", copy)
	}
}

func TestOpenAIStructuralSchemaCopyCycles(t *testing.T) {
	cycleMap := map[string]any{}
	cycleMap["self"] = cycleMap
	cycleSlice := make([]any, 1)
	cycleSlice[0] = cycleSlice
	for _, cycle := range []any{cycleMap, cycleSlice} {
		original := map[string]any{"type": "object", "x-annotation": cycle}
		copied := deepCopyMap(original)
		copied["type"] = "string"
		if original["type"] != "object" {
			t.Fatal("fallback aliases the original top-level map")
		}
		if _, err := json.Marshal(copied); err == nil {
			t.Fatal("fallback dropped the cyclic annotation")
		}
	}
}

func BenchmarkOpenAISchemaCopy(b *testing.B) {
	properties := make(map[string]any, 50)
	for i := range 50 {
		properties[string(rune('A'+i))] = map[string]any{"type": "string", "description": strings.Repeat("a", 128)}
	}
	input := map[string]any{"type": "object", "properties": properties, "required": []string{"A"}}
	for _, structural := range []bool{false, true} {
		name := "json"
		if structural {
			name = "structural"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if structural {
					_ = deepCopyMap(input)
				} else {
					raw, _ := json.Marshal(input)
					var copy map[string]any
					if err := json.Unmarshal(raw, &copy); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
