package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// Example native Go tools showing the pattern from IRC tools

// UpperCaseTool is a simple example of a native Go tool
type UpperCaseTool struct{}

// GetName returns the tool name
func (t *UpperCaseTool) GetName() string {
	return "uppercase"
}

// GetType returns "native" for built-in tools
func (t *UpperCaseTool) GetType() string {
	return "native"
}

// GetSource returns "builtin" for native tools
func (t *UpperCaseTool) GetSource() string {
	return "builtin"
}

func (t *UpperCaseTool) GetSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Title:       "uppercase",
		Description: "Convert text to uppercase",
		Type:        "object",
		Properties: map[string]*jsonschema.Schema{
			"text": {
				Type:        "string",
				Description: "The text to convert to uppercase",
			},
		},
		Required: []string{"text"},
	}
}

func (t *UpperCaseTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	text, ok := args["text"].(string)
	if !ok {
		return "", fmt.Errorf("text must be a string")
	}
	return strings.ToUpper(text), nil
}

// WordCountTool counts words in text
type WordCountTool struct{}

// GetName returns the tool name
func (t *WordCountTool) GetName() string {
	return "wordcount"
}

// GetType returns "native" for built-in tools
func (t *WordCountTool) GetType() string {
	return "native"
}

// GetSource returns "builtin" for native tools
func (t *WordCountTool) GetSource() string {
	return "builtin"
}

func (t *WordCountTool) GetSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Title:       "wordcount",
		Description: "Count words in text",
		Type:        "object",
		Properties: map[string]*jsonschema.Schema{
			"text": {
				Type:        "string",
				Description: "The text to count words in",
			},
		},
		Required: []string{"text"},
	}
}

func (t *WordCountTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	text, ok := args["text"].(string)
	if !ok {
		return "", fmt.Errorf("text must be a string")
	}

	words := strings.Fields(text)
	return fmt.Sprintf("Word count: %d", len(words)), nil
}

// Example of a contextual tool that needs runtime configuration

// LoggerTool is an example tool that needs context to be injected
type LoggerTool struct {
	logger Logger // Interface to be injected
}

// GetName returns the tool name
func (t *LoggerTool) GetName() string {
	return "logger"
}

// GetType returns "native" for built-in tools
func (t *LoggerTool) GetType() string {
	return "native"
}

// GetSource returns "builtin" for native tools
func (t *LoggerTool) GetSource() string {
	return "builtin"
}

// Logger interface for dependency injection
type Logger interface {
	Log(message string)
}

func (t *LoggerTool) SetContext(ctx any) {
	if logger, ok := ctx.(Logger); ok {
		t.logger = logger
	}
}

func (t *LoggerTool) GetSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Title:       "log",
		Description: "Log a message to the configured logger",
		Type:        "object",
		Properties: map[string]*jsonschema.Schema{
			"message": {
				Type:        "string",
				Description: "The message to log",
			},
			"level": {
				Type:        "string",
				Description: "Log level (info, warn, error)",
				Enum:        []any{"info", "warn", "error"},
			},
		},
		Required: []string{"message"},
	}
}

func (t *LoggerTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	if t.logger == nil {
		return "", fmt.Errorf("no logger context available")
	}

	message, ok := args["message"].(string)
	if !ok {
		return "", fmt.Errorf("message must be a string")
	}

	level := "info"
	if l, ok := args["level"].(string); ok {
		level = l
	}

	logMessage := fmt.Sprintf("[%s] %s", strings.ToUpper(level), message)
	t.logger.Log(logMessage)

	return fmt.Sprintf("Logged: %s", logMessage), nil
}
