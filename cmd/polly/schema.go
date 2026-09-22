package main

import (
	"fmt"
	"os"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/schema"
)

// loadSchemaFile loads and checks a JSON response schema from a file.
func loadSchemaFile(path string) (*llm.Schema, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read schema file %s: %w", path, err)
	}
	s, err := schema.SchemaFromBytes(data)
	if err != nil {
		return nil, fmt.Errorf("schema file %s: %w", path, err)
	}
	return s, nil
}
