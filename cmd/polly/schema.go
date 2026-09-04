package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/alexschlessinger/pollytool/llm"
)

// loadSchemaFile loads and parses a JSON schema from a file
func loadSchemaFile(path string) (*llm.Schema, error) {
	if path == "" {
		return nil, nil
	}

	// Read the schema file
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read schema file %s: %w", path, err)
	}

	// Parse the JSON schema
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil, fmt.Errorf("failed to parse schema file %s: %w", path, err)
	}

	// Validate basic schema structure
	if schemaType, ok := schema["type"]; ok {
		// Ensure it's a valid type
		switch schemaType.(type) {
		case string:
			// Valid
		default:
			return nil, fmt.Errorf("invalid schema: 'type' must be a string")
		}
	} else {
		return nil, fmt.Errorf("invalid schema: missing 'type' field")
	}

	return &llm.Schema{
		Raw:    schema,
		Strict: true, // Default to strict validation
	}, nil
}

// validateJSONAgainstSchema validates JSON output against a schema with the
// full JSON Schema validator, so nested constraints, enums, and
// additionalProperties are enforced, not just the top-level type.
func validateJSONAgainstSchema(content string, schema *llm.Schema) error {
	if schema == nil {
		return nil
	}
	return schema.Validate(content)
}
