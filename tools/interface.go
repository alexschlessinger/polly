package tools

import (
	"context"
	"log"

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

// ToolRegistry manages available tools
type ToolRegistry struct {
	tools map[string]Tool
}

// NewToolRegistry creates a new tool registry from a list of tools
func NewToolRegistry(tools []Tool) *ToolRegistry {
	registry := &ToolRegistry{
		tools: make(map[string]Tool),
	}

	for _, tool := range tools {
		schema := tool.GetSchema()
		name := ""
		if schema != nil && schema.Title != "" {
			name = schema.Title
		}
		log.Printf("registered tool: %s", name)
		registry.tools[name] = tool
	}

	return registry
}

// Register adds a tool to the registry
func (r *ToolRegistry) Register(tool Tool) {
	schema := tool.GetSchema()
	name := ""
	if schema != nil && schema.Title != "" {
		name = schema.Title
	}
	log.Printf("registered tool: %s", name)
	r.tools[name] = tool
}

// Get retrieves a tool by name
func (r *ToolRegistry) Get(name string) (Tool, bool) {
	tool, ok := r.tools[name]
	return tool, ok
}

// AddTool adds a single tool to the registry
func (r *ToolRegistry) AddTool(tool Tool) {
	schema := tool.GetSchema()
	name := ""
	if schema != nil && schema.Title != "" {
		name = schema.Title
	}
	log.Printf("added tool: %s", name)
	r.tools[name] = tool
}

// RemoveTool removes a tool by name from the registry
func (r *ToolRegistry) RemoveTool(name string) {
	if _, ok := r.tools[name]; ok {
		delete(r.tools, name)
		log.Printf("removed tool: %s", name)
	}
}

// All returns all tools in the registry
func (r *ToolRegistry) All() []Tool {
	tools := make([]Tool, 0, len(r.tools))
	for _, tool := range r.tools {
		tools = append(tools, tool)
	}
	return tools
}

// GetSchemas returns all tool schemas
func (r *ToolRegistry) GetSchemas() []*jsonschema.Schema {
	schemas := make([]*jsonschema.Schema, 0, len(r.tools))
	for _, tool := range r.tools {
		schemas = append(schemas, tool.GetSchema())
	}
	return schemas
}

// ContextualTool is a tool that needs external context to execute
type ContextualTool interface {
	Tool
	SetContext(ctx any)
}
