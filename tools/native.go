package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/schema"
)

// Example native Go tools

var UpperCaseTool = &Func{
	Name:     "uppercase",
	Desc:     "Convert text to uppercase",
	Params:   schema.Params{"text": schema.S("The text to convert to uppercase")},
	Required: []string{"text"},
	Run: func(_ context.Context, args Args) (string, error) {
		text, ok := args["text"].(string)
		if !ok {
			return "", fmt.Errorf("text must be a string")
		}
		return strings.ToUpper(text), nil
	},
}

var WordCountTool = &Func{
	Name:     "wordcount",
	Desc:     "Count words in text",
	Params:   schema.Params{"text": schema.S("The text to count words in")},
	Required: []string{"text"},
	Run: func(_ context.Context, args Args) (string, error) {
		if _, ok := args["text"].(string); !ok {
			return "", fmt.Errorf("text must be a string")
		}
		return fmt.Sprintf("Word count: %d", len(strings.Fields(args.String("text")))), nil
	},
}

// LoggerTool is an example tool that needs context to be injected
type LoggerTool struct {
	NativeTool
	logger Logger
}

// Logger interface for dependency injection
type Logger interface {
	Log(message string)
}

func (t *LoggerTool) GetName() string { return "logger" }

func (t *LoggerTool) SetContext(ctx any) {
	if logger, ok := ctx.(Logger); ok {
		t.logger = logger
	}
}

func (t *LoggerTool) GetSchema() *schema.ToolSchema {
	return schema.Tool("log", "Log a message to the configured logger",
		schema.Params{
			"message": schema.S("The message to log"),
			"level":   schema.Enum("Log level (info, warn, error)", "info", "warn", "error"),
		},
		"message",
	)
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
