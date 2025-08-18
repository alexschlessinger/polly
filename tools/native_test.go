package tools

import (
	"context"
	"strings"
	"testing"
)

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

func TestGetExampleNativeTools(t *testing.T) {
	tools := GetExampleNativeTools()

	if len(tools) != 2 {
		t.Errorf("Expected 2 example tools, got %d", len(tools))
	}

	// Verify the tools are the expected types
	if _, ok := tools[0].(*UpperCaseTool); !ok {
		t.Error("Expected first tool to be UpperCaseTool")
	}
	if _, ok := tools[1].(*WordCountTool); !ok {
		t.Error("Expected second tool to be WordCountTool")
	}
}