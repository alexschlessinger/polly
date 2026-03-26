package schema

import (
	"encoding/json"
	"fmt"
	"strings"

	ijs "github.com/invopop/jsonschema"
	"github.com/xeipuuv/gojsonschema"
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
	result, err := gojsonschema.Validate(
		gojsonschema.NewBytesLoader(schemaBytes),
		gojsonschema.NewStringLoader(jsonStr),
	)
	if err != nil {
		return fmt.Errorf("schema validation error: %w", err)
	}
	if !result.Valid() {
		var msgs []string
		for _, e := range result.Errors() {
			msgs = append(msgs, e.String())
		}
		return fmt.Errorf("validation failed: %s", strings.Join(msgs, "; "))
	}
	return nil
}

// SchemaFor generates a strict JSON schema from a Go struct using reflection.
// Fields are derived from json tags; descriptions from jsonschema tags.
func SchemaFor(v any) *Schema {
	r := new(ijs.Reflector)
	r.DoNotReference = true
	s := r.Reflect(v)
	s.AdditionalProperties = ijs.FalseSchema
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
