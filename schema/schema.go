package schema

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
)

// Schema represents a JSON schema for structured output
type Schema struct {
	Raw    map[string]any // Raw JSON schema
	Strict bool           // Whether to enforce strict validation
}

// Validate checks that a JSON string conforms to this schema.
func (s *Schema) Validate(jsonStr string) error {
	if s == nil {
		return nil
	}
	schemaBytes, err := json.Marshal(s.Raw)
	if err != nil {
		return fmt.Errorf("schema marshal error: %w", err)
	}
	var compiled jsonschema.Schema
	if err := json.Unmarshal(schemaBytes, &compiled); err != nil {
		return fmt.Errorf("schema validation error: %w", err)
	}
	resolved, err := compiled.Resolve(nil)
	if err != nil {
		return fmt.Errorf("schema validation error: %w", err)
	}
	var instance any
	if err := json.Unmarshal([]byte(jsonStr), &instance); err != nil {
		return fmt.Errorf("schema validation error: %w", err)
	}
	if err := resolved.Validate(instance); err != nil {
		return fmt.Errorf("validation failed: %s", err)
	}
	return nil
}

// SchemaFor generates a strict JSON schema from a Go struct using reflection.
// Fields are derived from json tags; a field's jsonschema tag becomes its
// description. Fields without omitempty/omitzero are required, and object
// nodes reject additional properties.
func SchemaFor(v any) *Schema {
	t := reflect.TypeOf(v)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return nil
	}
	s, err := jsonschema.ForType(t, nil)
	if err != nil {
		return nil
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return &Schema{Raw: m, Strict: true}
}

// SchemaFromJSON parses a JSON schema string into a strict Schema.
// Returns nil if the string is empty or invalid.
func SchemaFromJSON(s string) *Schema {
	if s == "" {
		return nil
	}
	return SchemaFromBytes([]byte(s))
}

// SchemaFromBytes parses JSON bytes into a strict Schema.
// Returns nil if data is empty or invalid.
func SchemaFromBytes(data []byte) *Schema {
	if len(data) == 0 {
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	return &Schema{Raw: raw, Strict: true}
}
