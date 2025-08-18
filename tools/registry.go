package tools

import (
	"log"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonschema"
)

// ToolRegistry manages available tools
type ToolRegistry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewToolRegistry creates a new tool registry from a list of tools
func NewToolRegistry(tools []Tool) *ToolRegistry {
	registry := &ToolRegistry{
		tools: make(map[string]Tool),
	}

	for _, tool := range tools {
		registry.Register(tool)
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
	
	r.mu.Lock()
	defer r.mu.Unlock()
	
	log.Printf("registered tool: %s", name)
	r.tools[name] = tool
}

// Get retrieves a tool by name
func (r *ToolRegistry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	
	tool, ok := r.tools[name]
	return tool, ok
}

// Remove removes a tool by name from the registry
func (r *ToolRegistry) Remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	
	if _, ok := r.tools[name]; ok {
		delete(r.tools, name)
		log.Printf("removed tool: %s", name)
	}
}

// All returns all tools in the registry
func (r *ToolRegistry) All() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	
	tools := make([]Tool, 0, len(r.tools))
	for _, tool := range r.tools {
		tools = append(tools, tool)
	}
	return tools
}

// GetSchemas returns all tool schemas
func (r *ToolRegistry) GetSchemas() []*jsonschema.Schema {
	r.mu.RLock()
	defer r.mu.RUnlock()
	
	schemas := make([]*jsonschema.Schema, 0, len(r.tools))
	for _, tool := range r.tools {
		schemas = append(schemas, tool.GetSchema())
	}
	return schemas
}