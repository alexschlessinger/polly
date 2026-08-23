package schema

import (
	"reflect"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	s := SchemaFromJSON(`{
		"type": "object",
		"properties": {
			"animal": {"type": "string"},
			"legs": {"type": "integer"}
		},
		"required": ["animal", "legs"]
	}`)
	if s == nil {
		t.Fatal("schema failed to parse")
	}

	if err := s.Validate(`{"animal":"spider","legs":8}`); err != nil {
		t.Fatalf("valid instance rejected: %v", err)
	}

	err := s.Validate(`{"animal":"spider"}`)
	if err == nil || !strings.HasPrefix(err.Error(), "validation failed:") {
		t.Fatalf("missing required field: err = %v, want validation failed prefix", err)
	}

	err = s.Validate(`{"animal":"spider","legs":"eight"}`)
	if err == nil || !strings.HasPrefix(err.Error(), "validation failed:") {
		t.Fatalf("type mismatch: err = %v, want validation failed prefix", err)
	}

	err = s.Validate(`not json`)
	if err == nil || !strings.HasPrefix(err.Error(), "schema validation error:") {
		t.Fatalf("malformed instance: err = %v, want schema validation error prefix", err)
	}

	var nilSchema *Schema
	if err := nilSchema.Validate(`whatever`); err != nil {
		t.Fatalf("nil schema must validate nothing, got %v", err)
	}
}

func TestSchemaFor(t *testing.T) {
	type Nested struct {
		Country string `json:"country"`
	}
	type Sample struct {
		Animal string  `json:"animal" jsonschema:"the animal's name"`
		Legs   int     `json:"legs"`
		Nick   string  `json:"nick,omitempty"`
		Where  *Nested `json:"where,omitempty"`
	}

	s := SchemaFor(Sample{})
	if s == nil {
		t.Fatal("SchemaFor returned nil")
	}
	if !s.Strict {
		t.Error("SchemaFor schemas must be strict")
	}
	if s.Raw["type"] != "object" {
		t.Errorf("type = %v, want object", s.Raw["type"])
	}
	if s.Raw["additionalProperties"] != false {
		t.Errorf("additionalProperties = %v, want false", s.Raw["additionalProperties"])
	}

	props, ok := s.Raw["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties = %#v", s.Raw["properties"])
	}
	for _, name := range []string{"animal", "legs", "nick", "where"} {
		if _, ok := props[name]; !ok {
			t.Errorf("property %q missing", name)
		}
	}
	animal := props["animal"].(map[string]any)
	if animal["type"] != "string" || animal["description"] != "the animal's name" {
		t.Errorf("animal schema = %#v, want string with jsonschema-tag description", animal)
	}
	if legs := props["legs"].(map[string]any); legs["type"] != "integer" {
		t.Errorf("legs schema = %#v", legs)
	}

	// omitempty fields are optional; the rest are required.
	required, ok := s.Raw["required"].([]any)
	if !ok {
		t.Fatalf("required = %#v", s.Raw["required"])
	}
	var names []string
	for _, r := range required {
		names = append(names, r.(string))
	}
	if !reflect.DeepEqual(names, []string{"animal", "legs"}) {
		t.Errorf("required = %v, want [animal legs]", names)
	}

	// A pointer input describes the same schema as the value.
	if p := SchemaFor(&Sample{}); !reflect.DeepEqual(p.Raw, s.Raw) {
		t.Error("SchemaFor(&T{}) differs from SchemaFor(T{})")
	}

	// The generated schema validates its own instances.
	if err := s.Validate(`{"animal":"spider","legs":8}`); err != nil {
		t.Errorf("generated schema rejected valid instance: %v", err)
	}
	if err := s.Validate(`{"animal":"spider","legs":8,"extra":true}`); err == nil {
		t.Error("generated schema must reject additional properties")
	}
	if err := s.Validate(`{"animal":"spider"}`); err == nil {
		t.Error("generated schema must require non-omitempty fields")
	}
}
