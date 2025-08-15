package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/pkdindustries/pollytool/llm"
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

// validateJSONAgainstSchema validates JSON output against a schema
// This is a basic implementation - could be enhanced with a proper JSON schema validator
func validateJSONAgainstSchema(data any, schema *llm.Schema) error {
	if schema == nil {
		return nil
	}

	// Basic type checking
	schemaType, ok := schema.Raw["type"].(string)
	if !ok {
		return fmt.Errorf("schema missing type field")
	}

	switch schemaType {
	case "object":
		obj, ok := data.(map[string]any)
		if !ok {
			return fmt.Errorf("expected object, got %T", data)
		}

		// Check required fields
		if required, ok := schema.Raw["required"].([]any); ok {
			for _, field := range required {
				fieldName, ok := field.(string)
				if !ok {
					continue
				}
				if _, exists := obj[fieldName]; !exists {
					return fmt.Errorf("missing required field: %s", fieldName)
				}
			}
		}

	case "array":
		_, ok := data.([]any)
		if !ok {
			return fmt.Errorf("expected array, got %T", data)
		}

	case "string":
		_, ok := data.(string)
		if !ok {
			return fmt.Errorf("expected string, got %T", data)
		}

	case "number":
		switch data.(type) {
		case float64, float32, int, int64, int32:
			// Valid number types
		default:
			return fmt.Errorf("expected number, got %T", data)
		}

	case "boolean":
		_, ok := data.(bool)
		if !ok {
			return fmt.Errorf("expected boolean, got %T", data)
		}
	}

	return nil
}
