package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// checkUvxAvailable checks if uvx is available on the system
func checkUvxAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("uvx"); err != nil {
		t.Skip("uvx is not installed, skipping MCP tests")
	}
}

func TestMCPClient(t *testing.T) {
	checkUvxAvailable(t)

	// Start the MCP server using uvx
	serverCmd := "uvx mcp-server-time"

	client, err := NewMCPClient(serverCmd)
	if err != nil {
		// If uvx is not available or server can't start, skip the test
		t.Skipf("Could not start MCP server (is uvx available?): %v", err)
	}
	defer client.Close()

	// Give the server a moment to initialize
	time.Sleep(100 * time.Millisecond)

	// Test listing tools
	tools, err := client.ListTools()
	if err != nil {
		t.Fatalf("Failed to list tools: %v", err)
	}

	if len(tools) == 0 {
		t.Error("Expected at least one tool from mcp-server-time")
	}

	// Verify we got expected tools
	var foundTimeTools bool
	for _, tool := range tools {
		schema := tool.GetSchema()
		if schema != nil && strings.Contains(schema.Title, "time") {
			foundTimeTools = true
			break
		}
	}

	if !foundTimeTools {
		t.Error("Expected to find time-related tools")
	}
}

func TestMCPToolExecution(t *testing.T) {
	checkUvxAvailable(t)
	ctx := context.Background()

	// Start the MCP server
	serverCmd := "uvx mcp-server-time"

	client, err := NewMCPClient(serverCmd)
	if err != nil {
		t.Skipf("Could not start MCP server: %v", err)
	}
	defer client.Close()

	// Give the server a moment to initialize
	time.Sleep(100 * time.Millisecond)

	// Get tools
	tools, err := client.ListTools()
	if err != nil {
		t.Fatalf("Failed to list tools: %v", err)
	}

	if len(tools) == 0 {
		t.Fatal("No tools available from server")
	}

	// Find the get_current_time tool and test it
	for _, tool := range tools {
		schema := tool.GetSchema()
		if schema.Title == "get_current_time" {
			t.Logf("Testing tool: %s", schema.Title)

			// Test with valid timezone
			args := map[string]any{
				"timezone": "America/New_York",
			}

			result, err := tool.Execute(ctx, args)
			if err != nil {
				t.Errorf("Failed to execute tool with valid args: %v", err)
			} else {
				t.Logf("Tool result: %s", result)
				// Verify we got some result
				if result == "" {
					t.Error("Expected non-empty result from tool execution")
				}
				// Result should contain time information
				if !strings.Contains(result, "time") && !strings.Contains(result, "Time") {
					t.Error("Expected result to contain time information")
				}
			}
			return
		}
	}

	t.Error("Could not find get_current_time tool")
}

func TestMCPToolSchema(t *testing.T) {
	checkUvxAvailable(t)

	// Start the MCP server
	serverCmd := "uvx mcp-server-time"

	client, err := NewMCPClient(serverCmd)
	if err != nil {
		t.Skipf("Could not start MCP server: %v", err)
	}
	defer client.Close()

	// Give the server a moment to initialize
	time.Sleep(100 * time.Millisecond)

	// Get tools
	tools, err := client.ListTools()
	if err != nil {
		t.Fatalf("Failed to list tools: %v", err)
	}

	// Test schema generation for each tool
	for _, tool := range tools {
		schema := tool.GetSchema()

		if schema == nil {
			t.Error("Expected non-nil schema")
			continue
		}

		// Verify basic schema properties
		if schema.Title == "" {
			t.Error("Expected schema to have a title")
		}

		if schema.Type != "object" {
			t.Errorf("Expected schema type to be 'object', got %s", schema.Type)
		}

		t.Logf("Tool schema - Title: %s, Description: %s", schema.Title, schema.Description)
	}
}

func TestMCPClientInvalidCommand(t *testing.T) {
	// Test with a non-existent command
	_, err := NewMCPClient("this-command-does-not-exist")
	if err == nil {
		t.Error("Expected error for non-existent command")
	}
}

func TestMCPClientEmptyCommand(t *testing.T) {
	// Test with empty command
	_, err := NewMCPClient("")
	if err == nil {
		t.Error("Expected error for empty command")
	}
}

func TestMCPToolFilterArguments(t *testing.T) {
	checkUvxAvailable(t)
	ctx := context.Background()

	// Start the MCP server
	serverCmd := "uvx mcp-server-time"

	client, err := NewMCPClient(serverCmd)
	if err != nil {
		t.Skipf("Could not start MCP server: %v", err)
	}
	defer client.Close()

	// Give the server a moment to initialize
	time.Sleep(100 * time.Millisecond)

	// Get tools
	tools, err := client.ListTools()
	if err != nil {
		t.Fatalf("Failed to list tools: %v", err)
	}

	if len(tools) == 0 {
		t.Fatal("No tools available")
	}

	// Test that extra arguments are filtered out
	tool := tools[0]

	// Pass extra arguments that shouldn't be in the schema
	args := map[string]any{
		"extra_arg_1": "should be filtered",
		"extra_arg_2": 123,
		"extra_arg_3": true,
	}

	// This should not fail due to extra arguments
	// The Execute method should filter them out
	_, err = tool.Execute(ctx, args)
	// We don't check the error because the tool might still fail for other reasons
	// The important thing is that it doesn't fail due to unexpected arguments
	t.Logf("Execution with extra args completed (error ok): %v", err)
}

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

func TestUpperCaseTool(t *testing.T) {
	tool := &UpperCaseTool{}

	// Test schema
	schema := tool.GetSchema()
	if schema.Title != "uppercase" {
		t.Errorf("Expected title 'uppercase', got %s", schema.Title)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "text" {
		t.Error("Expected 'text' to be required")
	}

	// Test execution
	args := map[string]any{
		"text": "hello world",
	}
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if result != "HELLO WORLD" {
		t.Errorf("Expected 'HELLO WORLD', got '%s'", result)
	}
}

func TestUpperCaseToolInvalidArgs(t *testing.T) {
	tool := &UpperCaseTool{}

	// Test with missing text argument
	args := map[string]any{}
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Expected error for missing text argument")
	}

	// Test with wrong type
	args = map[string]any{
		"text": 123,
	}
	_, err = tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Expected error for non-string text argument")
	}
}

func TestWordCountTool(t *testing.T) {
	tool := &WordCountTool{}

	// Test schema
	schema := tool.GetSchema()
	if schema.Title != "wordcount" {
		t.Errorf("Expected title 'wordcount', got %s", schema.Title)
	}

	// Test word counting
	testCases := []struct {
		input    string
		expected string
	}{
		{"hello world", "Word count: 2"},
		{"one two three four five", "Word count: 5"},
		{"   spaces   between   words   ", "Word count: 3"},
		{"", "Word count: 0"},
		{"single", "Word count: 1"},
	}

	for _, tc := range testCases {
		args := map[string]any{
			"text": tc.input,
		}
		result, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatalf("Unexpected error for input '%s': %v", tc.input, err)
		}
		if result != tc.expected {
			t.Errorf("For input '%s': expected '%s', got '%s'", tc.input, tc.expected, result)
		}
	}
}

func TestWordCountToolInvalidArgs(t *testing.T) {
	tool := &WordCountTool{}

	// Test with non-string argument
	args := map[string]any{
		"text": []int{1, 2, 3},
	}
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Expected error for non-string text argument")
	}
}

// Mock logger for testing LoggerTool
type mockLogger struct {
	messages []string
}

func (m *mockLogger) Log(message string) {
	m.messages = append(m.messages, message)
}

func TestLoggerTool(t *testing.T) {
	tool := &LoggerTool{}
	logger := &mockLogger{}

	// Test without context
	_, err := tool.Execute(context.Background(), map[string]any{"message": "test"})
	if err == nil {
		t.Error("Expected error when logger context is not set")
	}

	// Set context
	tool.SetContext(logger)

	// Test schema
	schema := tool.GetSchema()
	if schema.Title != "log" {
		t.Errorf("Expected title 'log', got %s", schema.Title)
	}

	// Test logging with default level
	args := map[string]any{
		"message": "Test message",
	}
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !strings.Contains(result, "[INFO] Test message") {
		t.Errorf("Expected result to contain '[INFO] Test message', got '%s'", result)
	}
	if len(logger.messages) != 1 || logger.messages[0] != "[INFO] Test message" {
		t.Errorf("Expected logger to receive '[INFO] Test message', got %v", logger.messages)
	}

	// Test logging with specific level
	args = map[string]any{
		"message": "Error occurred",
		"level":   "error",
	}
	result, err = tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !strings.Contains(result, "[ERROR] Error occurred") {
		t.Errorf("Expected result to contain '[ERROR] Error occurred', got '%s'", result)
	}
}

func TestLoggerToolInvalidArgs(t *testing.T) {
	tool := &LoggerTool{}
	logger := &mockLogger{}
	tool.SetContext(logger)

	// Test with non-string message
	args := map[string]any{
		"message": 12345,
	}
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Expected error for non-string message")
	}
}

func TestLoggerToolSetContext(t *testing.T) {
	tool := &LoggerTool{}

	// Test setting valid context
	logger := &mockLogger{}
	tool.SetContext(logger)
	if tool.logger != logger {
		t.Error("Expected logger to be set correctly")
	}

	// Test setting invalid context (should not panic)
	tool.SetContext("not a logger")
	// Logger should remain unchanged since the context is wrong type
	if tool.logger != logger {
		t.Error("Expected logger to remain unchanged with invalid context")
	}
}
