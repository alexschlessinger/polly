package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func createTestScript(t *testing.T, dir string) string {
	script := `#!/bin/bash
if [ "$1" = "--schema" ]; then
	echo '{
		"title": "test-tool",
		"description": "A test tool",
		"type": "object",
		"properties": {
			"message": {
				"type": "string",
				"description": "A test message"
			}
		},
		"required": ["message"]
	}'
elif [ "$1" = "--execute" ]; then
	# Parse JSON argument
	MESSAGE=$(echo "$2" | grep -oP '"message":\s*"\K[^"]+')
	echo "Received: $MESSAGE"
else
	echo "Unknown argument: $1"
	exit 1
fi
`
	scriptPath := filepath.Join(dir, "test-tool.sh")
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	if err != nil {
		t.Fatalf("Failed to create test script: %v", err)
	}
	return scriptPath
}

func TestNewShellTool(t *testing.T) {
	dir := t.TempDir()
	scriptPath := createTestScript(t, dir)

	tool, err := NewShellTool(scriptPath)
	if err != nil {
		t.Fatalf("Failed to create shell tool: %v", err)
	}

	schema := tool.GetSchema()
	if schema == nil {
		t.Fatal("Expected schema to be non-nil")
	}

	if schema.Title != "test-tool" {
		t.Errorf("Expected title 'test-tool', got %s", schema.Title)
	}

	if schema.Description != "A test tool" {
		t.Errorf("Expected description 'A test tool', got %s", schema.Description)
	}
}

func TestShellToolExecute(t *testing.T) {
	dir := t.TempDir()
	scriptPath := createTestScript(t, dir)

	tool, err := NewShellTool(scriptPath)
	if err != nil {
		t.Fatalf("Failed to create shell tool: %v", err)
	}

	args := map[string]any{
		"message": "Hello, World!",
	}

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Failed to execute tool: %v", err)
	}

	expected := "Received: Hello, World!"
	if result != expected {
		t.Errorf("Expected result '%s', got '%s'", expected, result)
	}
}

func TestShellToolExecuteWithCancel(t *testing.T) {
	// Create a script that sleeps to test cancellation
	script := `#!/bin/bash
if [ "$1" = "--schema" ]; then
	echo '{"title": "slow-tool", "type": "object"}'
elif [ "$1" = "--execute" ]; then
	sleep 10
	echo "Should not reach here"
fi
`
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "slow-tool.sh")
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	if err != nil {
		t.Fatalf("Failed to create test script: %v", err)
	}

	tool, err := NewShellTool(scriptPath)
	if err != nil {
		t.Fatalf("Failed to create shell tool: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err = tool.Execute(ctx, map[string]any{})
	if err == nil {
		t.Error("Expected error due to context cancellation")
	}
}

func TestLoadShellTools(t *testing.T) {
	dir := t.TempDir()
	
	// Create multiple test scripts
	script1 := createTestScript(t, dir)
	
	// Create a second test script
	script2 := `#!/bin/bash
if [ "$1" = "--schema" ]; then
	echo '{"title": "tool2", "type": "object"}'
elif [ "$1" = "--execute" ]; then
	echo "Tool 2 executed"
fi
`
	script2Path := filepath.Join(dir, "tool2.sh")
	err := os.WriteFile(script2Path, []byte(script2), 0755)
	if err != nil {
		t.Fatalf("Failed to create second test script: %v", err)
	}

	tools, err := LoadShellTools([]string{script1, script2Path})
	if err != nil {
		t.Fatalf("Failed to load shell tools: %v", err)
	}

	if len(tools) != 2 {
		t.Errorf("Expected 2 tools, got %d", len(tools))
	}
}

func TestShellToolInvalidJSON(t *testing.T) {
	// Create a script that returns invalid JSON for schema
	script := `#!/bin/bash
if [ "$1" = "--schema" ]; then
	echo 'not valid json'
fi
`
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "invalid.sh")
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	if err != nil {
		t.Fatalf("Failed to create test script: %v", err)
	}

	_, err = NewShellTool(scriptPath)
	if err == nil {
		t.Error("Expected error for invalid JSON schema")
	}
}

func TestShellToolExecuteError(t *testing.T) {
	// Create a script that exits with error during execution
	script := `#!/bin/bash
if [ "$1" = "--schema" ]; then
	echo '{"title": "error-tool", "type": "object"}'
elif [ "$1" = "--execute" ]; then
	echo "Error occurred" >&2
	exit 1
fi
`
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "error.sh")
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	if err != nil {
		t.Fatalf("Failed to create test script: %v", err)
	}

	tool, err := NewShellTool(scriptPath)
	if err != nil {
		t.Fatalf("Failed to create shell tool: %v", err)
	}

	_, err = tool.Execute(context.Background(), map[string]any{})
	if err == nil {
		t.Error("Expected error from tool execution")
	}
}

func TestShellToolMarshalArgsError(t *testing.T) {
	dir := t.TempDir()
	scriptPath := createTestScript(t, dir)

	tool, err := NewShellTool(scriptPath)
	if err != nil {
		t.Fatalf("Failed to create shell tool: %v", err)
	}

	// Create args that can't be marshaled to JSON
	args := map[string]any{
		"invalid": make(chan int), // channels can't be marshaled to JSON
	}

	_, err = tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Expected error when marshaling invalid arguments")
	}
}

func TestShellToolComplexArgs(t *testing.T) {
	// Create a script that handles complex JSON arguments
	script := `#!/bin/bash
if [ "$1" = "--schema" ]; then
	echo '{
		"title": "complex-tool",
		"type": "object",
		"properties": {
			"count": {"type": "integer"},
			"values": {"type": "array", "items": {"type": "string"}}
		}
	}'
elif [ "$1" = "--execute" ]; then
	# Just echo back the JSON to verify it was received
	echo "$2"
fi
`
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "complex.sh")
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	if err != nil {
		t.Fatalf("Failed to create test script: %v", err)
	}

	tool, err := NewShellTool(scriptPath)
	if err != nil {
		t.Fatalf("Failed to create shell tool: %v", err)
	}

	args := map[string]any{
		"count":  42,
		"values": []string{"a", "b", "c"},
	}

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Failed to execute tool: %v", err)
	}

	// Verify the result is valid JSON containing our args
	var resultMap map[string]any
	err = json.Unmarshal([]byte(result), &resultMap)
	if err != nil {
		t.Fatalf("Result is not valid JSON: %v", err)
	}

	if int(resultMap["count"].(float64)) != 42 {
		t.Errorf("Expected count 42, got %v", resultMap["count"])
	}
}

func TestShellToolNonExecutable(t *testing.T) {
	// Test with a non-executable file
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "not-executable.txt")
	err := os.WriteFile(scriptPath, []byte("not a script"), 0644) // Note: not executable
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	_, err = NewShellTool(scriptPath)
	if err == nil {
		t.Error("Expected error for non-executable file")
	}
}

func TestLoadShellToolsContinuesOnError(t *testing.T) {
	dir := t.TempDir()
	
	// Create one valid tool
	validScript := `#!/bin/bash
if [ "$1" = "--schema" ]; then
	echo '{"title": "valid-tool", "type": "object"}'
fi
`
	validPath := filepath.Join(dir, "valid.sh")
	err := os.WriteFile(validPath, []byte(validScript), 0755)
	if err != nil {
		t.Fatalf("Failed to create valid script: %v", err)
	}

	// Create one invalid tool (non-executable)
	invalidPath := filepath.Join(dir, "invalid.txt")
	err = os.WriteFile(invalidPath, []byte("not executable"), 0644)
	if err != nil {
		t.Fatalf("Failed to create invalid file: %v", err)
	}

	// Load should succeed with the valid tool and skip the invalid one
	tools, err := LoadShellTools([]string{invalidPath, validPath})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Should have loaded only the valid tool
	if len(tools) != 1 {
		t.Errorf("Expected 1 tool (valid only), got %d", len(tools))
	}
}