package tools

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonschema"
)

// NamespacedTool wraps a tool to provide a namespaced schema
type NamespacedTool struct {
	Tool
	namespacedName string
}

// GetSchema returns a schema with the namespaced title
func (n *NamespacedTool) GetSchema() *jsonschema.Schema {
	schema := n.Tool.GetSchema()
	if schema != nil {
		// Create a copy to avoid modifying the original
		modifiedSchema := &jsonschema.Schema{
			Title:                schema.Title,
			Description:          schema.Description,
			Type:                 schema.Type,
			Properties:           schema.Properties,
			Required:             schema.Required,
			AdditionalProperties: schema.AdditionalProperties,
		}
		// Update the title to the namespaced name
		modifiedSchema.Title = n.namespacedName
		return modifiedSchema
	}
	return schema
}

// GetName returns the namespaced name
func (n *NamespacedTool) GetName() string {
	return n.namespacedName
}

// ToolRegistry manages available tools
type ToolRegistry struct {
	mu        sync.RWMutex
	tools     map[string]Tool

	// MCP tracking
	toolClients map[string]*MCPClient // toolName -> client
	serverTools map[string][]string   // serverSpec -> toolNames
}

// NewToolRegistry creates a new tool registry from a list of tools
func NewToolRegistry(tools []Tool) *ToolRegistry {
	registry := &ToolRegistry{
		tools:       make(map[string]Tool),
		toolClients: make(map[string]*MCPClient),
		serverTools: make(map[string][]string),
	}

	for _, tool := range tools {
		registry.Register(tool)
	}

	return registry
}


// Register adds a tool to the registry
func (r *ToolRegistry) Register(tool Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	name := tool.GetName()
	if name == "" {
		// Fallback to schema title if GetName() returns empty
		schema := tool.GetSchema()
		if schema != nil && schema.Title != "" {
			name = schema.Title
		}
	}

	if name != "" {
		log.Printf("registered tool: %s", name)
		r.tools[name] = tool
	}
}

// Get retrieves a tool by name
func (r *ToolRegistry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tool, ok := r.tools[name]
	return tool, ok
}


// Remove removes a tool by namespaced name from the registry
func (r *ToolRegistry) Remove(namespacedName string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	tool, exists := r.tools[namespacedName]
	if !exists {
		return
	}

	// Get MCP client if this is an MCP tool
	client := r.toolClients[namespacedName]

	// Remove from registry
	delete(r.tools, namespacedName)
	delete(r.toolClients, namespacedName)
	log.Printf("removed tool: %s", namespacedName)

	// Clean up MCP-specific tracking
	if client != nil {
		source := tool.GetSource()
		
		// Update serverTools list
		if tools := r.serverTools[source]; len(tools) > 0 {
			var remaining []string
			for _, name := range tools {
				if name != namespacedName {
					remaining = append(remaining, name)
				}
			}
			if len(remaining) > 0 {
				r.serverTools[source] = remaining
			} else {
				delete(r.serverTools, source)
			}
		}

		// Close client if no other tools use it
		stillInUse := false
		for _, c := range r.toolClients {
			if c == client {
				stillInUse = true
				break
			}
		}
		if !stillInUse {
			log.Printf("closing MCP client (no remaining tools)")
			client.Close()
		}
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

// LoadMCPServer connects to an MCP server and registers its tools with namespace
func (r *ToolRegistry) LoadMCPServer(serverSpec string) error {
	// Create client
	client, err := NewMCPClient(serverSpec)
	if err != nil {
		return err
	}

	// Extract namespace from file path
	namespace := extractNamespace(serverSpec)

	// Get tools
	tools, err := client.ListTools()
	if err != nil {
		client.Close()
		return err
	}

	// Register tools
	r.mu.Lock()
	defer r.mu.Unlock()

	var toolNames []string
	for _, tool := range tools {
		schema := tool.GetSchema()
		if schema != nil && schema.Title != "" {
			// Create namespaced name
			namespacedName := fmt.Sprintf("%s__%s", namespace, schema.Title)

			// Wrap the tool to provide namespaced schema
			wrappedTool := &NamespacedTool{
				Tool:           tool,
				namespacedName: namespacedName,
			}
			r.tools[namespacedName] = wrappedTool
			r.toolClients[namespacedName] = client
			toolNames = append(toolNames, namespacedName)
			log.Printf("registered MCP tool: %s", namespacedName)
		}
	}

	// Track which tools came from this server
	r.serverTools[serverSpec] = toolNames

	return nil
}

// UnloadMCPServer removes all tools from a server and closes it
func (r *ToolRegistry) UnloadMCPServer(serverSpec string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	toolNames, exists := r.serverTools[serverSpec]
	if !exists {
		return fmt.Errorf("MCP server not loaded: %s", GetMCPDisplayName(serverSpec))
	}

	// Get the client from first tool (all tools share same client)
	var client *MCPClient
	if len(toolNames) > 0 {
		client = r.toolClients[toolNames[0]]
	}

	// Remove all tools
	for _, name := range toolNames {
		delete(r.tools, name)
		delete(r.toolClients, name)
		log.Printf("removed MCP tool: %s", name)
	}

	// Close client
	if client != nil {
		client.Close()
		log.Printf("closed MCP server: %s", GetMCPDisplayName(serverSpec))
	}

	// Clean up tracking
	delete(r.serverTools, serverSpec)

	return nil
}

// LoadShellTool loads a single shell tool from a file path with namespace
func (r *ToolRegistry) LoadShellTool(path string) error {
	shellTool, err := NewShellTool(path)
	if err != nil {
		return fmt.Errorf("failed to load shell tool %s: %w", path, err)
	}

	// Get the tool's schema to find its name
	schema := shellTool.GetSchema()
	if schema == nil || schema.Title == "" {
		return fmt.Errorf("shell tool %s has no name in schema", path)
	}

	// Extract namespace from script filename
	namespace := extractNamespace(path)

	// Create namespaced name
	namespacedName := fmt.Sprintf("%s__%s", namespace, schema.Title)

	// Wrap the tool to provide namespaced schema
	wrappedTool := &NamespacedTool{
		Tool:           shellTool,
		namespacedName: namespacedName,
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.tools[namespacedName] = wrappedTool
	log.Printf("registered shell tool: %s", namespacedName)

	return nil
}

// LoadToolAuto attempts to load a tool, auto-detecting if it's a shell tool or MCP server
func (r *ToolRegistry) LoadToolAuto(pathOrServer string) (isShell bool, err error) {
	// First try as shell tool
	shellErr := r.LoadShellTool(pathOrServer)
	if shellErr == nil {
		return true, nil
	}

	// If shell tool failed, try as MCP server
	mcpErr := r.LoadMCPServer(pathOrServer)
	if mcpErr == nil {
		return false, nil
	}

	// Both failed, return combined error
	return false, fmt.Errorf("failed to load as shell tool (%v) or MCP server (%v)", shellErr, mcpErr)
}

// LoadMCPServers batch loads multiple servers
func (r *ToolRegistry) LoadMCPServers(serverSpecs []string) error {
	for _, spec := range serverSpecs {
		if err := r.LoadMCPServer(spec); err != nil {
			return fmt.Errorf("failed to load MCP server %s: %w", spec, err)
		}
	}
	return nil
}

// GetActiveToolLoaders returns loader information for all tools
// This returns one entry per tool to allow selective loading
func (r *ToolRegistry) GetActiveToolLoaders() []ToolLoaderInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var loaders []ToolLoaderInfo
	
	for name, tool := range r.tools {
		loaders = append(loaders, ToolLoaderInfo{
			Name:   name,
			Type:   tool.GetType(),
			Source: tool.GetSource(),
		})
	}
	
	return loaders
}


// LoadMCPServerWithFilter connects to an MCP server and only registers specified tools with namespace
func (r *ToolRegistry) LoadMCPServerWithFilter(serverSpec string, allowedTools []string) error {
	// Create client
	client, err := NewMCPClient(serverSpec)
	if err != nil {
		return err
	}

	// Extract namespace from file path
	namespace := extractNamespace(serverSpec)

	// Get all tools from server
	tools, err := client.ListTools()
	if err != nil {
		client.Close()
		return err
	}

	// Create a set of allowed tools for quick lookup
	// Note: allowedTools contains namespaced names like "perp__perplexity_search_web"
	allowed := make(map[string]bool)
	for _, name := range allowedTools {
		// Strip namespace prefix if present (format: namespace__toolname)
		if idx := strings.Index(name, "__"); idx != -1 {
			bareToolName := name[idx+2:]
			allowed[bareToolName] = true
		} else {
			allowed[name] = true
		}
	}

	// Register only allowed tools
	r.mu.Lock()
	defer r.mu.Unlock()

	var toolNames []string
	for _, tool := range tools {
		schema := tool.GetSchema()
		if schema != nil && schema.Title != "" {
			// Only register if this tool is in the allowed list
			if allowed[schema.Title] {
				// Create namespaced name
				namespacedName := fmt.Sprintf("%s__%s", namespace, schema.Title)

				// Wrap the tool to provide namespaced schema
				wrappedTool := &NamespacedTool{
					Tool:           tool,
					namespacedName: namespacedName,
				}
				r.tools[namespacedName] = wrappedTool
				r.toolClients[namespacedName] = client
				toolNames = append(toolNames, namespacedName)
				log.Printf("registered MCP tool: %s", namespacedName)
			}
		}
	}

	// Track which tools came from this server
	r.serverTools[serverSpec] = toolNames

	return nil
}

// GetLoadedMCPServers returns list of loaded server specs
func (r *ToolRegistry) GetLoadedMCPServers() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	servers := make([]string, 0, len(r.serverTools))
	for spec := range r.serverTools {
		servers = append(servers, spec)
	}
	return servers
}

// Close cleans up all resources
func (r *ToolRegistry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Close all unique MCP clients
	closed := make(map[*MCPClient]bool)
	for _, client := range r.toolClients {
		if !closed[client] {
			client.Close()
			closed[client] = true
		}
	}

	// Clear maps
	r.tools = make(map[string]Tool)
	r.toolClients = make(map[string]*MCPClient)
	r.serverTools = make(map[string][]string)

	return nil
}

// extractNamespace extracts a namespace from a file path
// e.g., "/path/to/filesystem.json" -> "filesystem"
func extractNamespace(filePath string) string {
	base := filepath.Base(filePath)
	// Remove extension
	namespace := strings.TrimSuffix(base, filepath.Ext(base))
	return namespace
}
