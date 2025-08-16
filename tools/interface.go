package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/jsonschema"
)

// Tool is the generic interface for all tools
type Tool interface {
	GetSchema() *jsonschema.Schema
	Execute(ctx context.Context, args map[string]any) (string, error)
}

// ToolCall represents a request to execute a tool
type ToolCall struct {
	ID   string         // Provider-specific ID (if any)
	Name string         // Tool name
	Args map[string]any // Parsed arguments
}

// ContextualTool is a tool that needs external context to execute
type ContextualTool interface {
	Tool
	SetContext(ctx any)
}
