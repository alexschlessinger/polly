package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/jsonschema"
)

// Tool is the generic interface for all tools
type Tool interface {
	// Execution methods
	GetSchema() *jsonschema.Schema
	Execute(ctx context.Context, args map[string]any) (string, error)
	
	// Metadata methods
	GetName() string   // Returns the namespaced name (e.g., "script__toolname")
	GetType() string   // Returns the tool type: "shell", "mcp", or "native"
	GetSource() string // Returns the source path/spec (e.g., "/path/to/script.sh")
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

// ToolLoaderInfo stores information needed to reload a specific tool
type ToolLoaderInfo struct {
	Name   string `json:"name"`   // Full namespaced tool name
	Type   string `json:"type"`   // "shell", "mcp", or "native"
	Source string `json:"source"` // Path for shell, server spec for MCP, "builtin" for native
}
