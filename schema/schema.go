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
		return fmt.Errorf("schema is required")
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
	instance, err := DecodeJSON(jsonStr)
	if err != nil {
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
func SchemaFor(v any) (*Schema, error) {
	t := reflect.TypeOf(v)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return nil, fmt.Errorf("schema type is required")
	}
	s, err := jsonschema.ForType(t, nil)
	if err != nil {
		return nil, fmt.Errorf("schema for %v: %w", t, err)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("schema for %v: %w", t, err)
	}
	return SchemaFromBytes(raw)
}

// SchemaFromJSON parses and checks a JSON object schema. Invalid or empty input
// returns an error; an absent response constraint is represented by an explicit nil.
func SchemaFromJSON(s string) (*Schema, error) { return SchemaFromBytes([]byte(s)) }

// SchemaFromBytes parses and checks a JSON object schema, rejecting ambiguous JSON.
func SchemaFromBytes(data []byte) (*Schema, error) {
	value, err := DecodeJSON(string(data))
	if err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	raw, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("schema must be a JSON object")
	}
	if err := checkSchemaTypes(raw); err != nil {
		return nil, err
	}
	var compiled jsonschema.Schema
	if err := json.Unmarshal(data, &compiled); err != nil {
		return nil, fmt.Errorf("invalid schema: %w", err)
	}
	if _, err := compiled.Resolve(nil); err != nil {
		return nil, fmt.Errorf("invalid schema: %w", err)
	}
	return &Schema{Raw: raw, Strict: true}, nil
}

// MustSchemaFor is SchemaFor for static definitions; it panics on an invalid type.
func MustSchemaFor(v any) *Schema {
	s, err := SchemaFor(v)
	if err != nil {
		panic(err)
	}
	return s
}

// MustSchemaFromJSON is SchemaFromJSON for static definitions; it panics on error.
func MustSchemaFromJSON(text string) *Schema {
	s, err := SchemaFromJSON(text)
	if err != nil {
		panic(err)
	}
	return s
}

// The resolver checks references and keyword shapes, but does not reject unknown
// JSON type names. Walk only schema-valued keywords, never defaults or examples.
func checkSchemaTypes(raw map[string]any) error {
	if value, present := raw["type"]; present {
		var types []any
		switch v := value.(type) {
		case string:
			types = []any{v}
		case []any:
			types = v
		default:
			return fmt.Errorf("schema type must be a string or nonempty array")
		}
		if len(types) == 0 {
			return fmt.Errorf("schema type array must not be empty")
		}
		seen := map[string]bool{}
		for _, value := range types {
			name, ok := value.(string)
			if !ok {
				return fmt.Errorf("schema type must be a string")
			}
			switch name {
			case "null", "boolean", "object", "array", "number", "integer", "string":
			default:
				return fmt.Errorf("unknown schema type %q", name)
			}
			if seen[name] {
				return fmt.Errorf("duplicate schema type %q", name)
			}
			seen[name] = true
		}
	}
	check := func(value any) error {
		if child, ok := value.(map[string]any); ok {
			return checkSchemaTypes(child)
		}
		return nil
	}
	for key, value := range raw {
		switch key {
		case "$defs", "definitions", "properties", "patternProperties", "dependentSchemas", "dependencies":
			if children, ok := value.(map[string]any); ok {
				for _, child := range children {
					if err := check(child); err != nil {
						return fmt.Errorf("%s: %w", key, err)
					}
				}
			}
		case "allOf", "anyOf", "oneOf", "prefixItems", "items":
			if children, ok := value.([]any); ok {
				for _, child := range children {
					if err := check(child); err != nil {
						return fmt.Errorf("%s: %w", key, err)
					}
				}
			} else if err := check(value); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		case "additionalProperties", "additionalItems", "unevaluatedProperties", "unevaluatedItems", "propertyNames", "contains", "not", "if", "then", "else", "contentSchema":
			if err := check(value); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		}
	}
	return nil
}
