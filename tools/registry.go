package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"go.uber.org/zap"
)

// LoadResult contains information about tools that were loaded
type LoadResult struct {
	Type    string         // "native", "shell", "mcp"
	Servers []ServerResult // For MCP, one per server loaded; for shell/native, single entry
}

// ServerResult contains information about tools loaded from a single source
type ServerResult struct {
	Name      string   // Server/tool name (e.g., "git", "filesystem", "datetime")
	ToolNames []string // Fully namespaced tool names loaded
}

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

	// Native tool factories
	nativeTools map[string]func() Tool // toolName -> factory

	// MCP tracking
	toolClients map[string]*MCPClient // toolName -> client
	serverTools map[string][]string   // serverSpec -> toolNames
}

// NewToolRegistry creates a new tool registry from a list of tools
func NewToolRegistry(tools []Tool) *ToolRegistry {
	registry := &ToolRegistry{
		tools:       make(map[string]Tool),
		nativeTools: make(map[string]func() Tool),
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
		zap.S().Debugw("tool_registered", "tool_name", name)
		r.tools[name] = tool
	}
}

// RegisterNative registers a native tool factory
func (r *ToolRegistry) RegisterNative(name string, factory func() Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.nativeTools[name] = factory
	zap.S().Debugw("native_factory_registered", "factory_name", name)
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
	zap.S().Debugw("tool_removed", "tool_name", namespacedName)

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
			zap.S().Debugw("mcp_client_closed", "reason", "no_remaining_tools")
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
// For multi-server configs (mcpServers format), loads ALL servers
func (r *ToolRegistry) LoadMCPServer(serverSpec string) (LoadResult, error) {
	jsonFile, serverName := ParseServerSpec(serverSpec)

	// Load config file
	configs, err := LoadMCPConfigFile(jsonFile)
	if err != nil {
		return LoadResult{}, err
	}

	result := LoadResult{Type: "mcp"}

	// If specific server requested, only load that one
	if serverName != "" {
		config, ok := configs[serverName]
		if !ok {
			var available []string
			for name := range configs {
				available = append(available, name)
			}
			return LoadResult{}, fmt.Errorf("server %q not found in config (available: %v)", serverName, available)
		}
		toolNames, err := r.loadSingleMCPServer(jsonFile, serverName, &config)
		if err != nil {
			return LoadResult{}, err
		}
		result.Servers = append(result.Servers, ServerResult{
			Name:      serverName,
			ToolNames: toolNames,
		})
		return result, nil
	}

	// Load all servers from config
	for name, config := range configs {
		cfg := config // avoid loop variable capture
		toolNames, err := r.loadSingleMCPServer(jsonFile, name, &cfg)
		if err != nil {
			return LoadResult{}, fmt.Errorf("server %s: %w", name, err)
		}
		result.Servers = append(result.Servers, ServerResult{
			Name:      name,
			ToolNames: toolNames,
		})
	}

	return result, nil
}

// loadSingleMCPServer loads a single server and registers its tools
// Returns the list of registered tool names
func (r *ToolRegistry) loadSingleMCPServer(jsonFile, serverName string, config *MCPConfig) ([]string, error) {
	client, err := NewMCPClientFromConfig(config)
	if err != nil {
		return nil, err
	}

	// Build the server spec for persistence
	serverSpec := fmt.Sprintf("%s#%s", jsonFile, serverName)
	client.serverSpec = serverSpec

	// Get tools
	tools, err := client.ListTools()
	if err != nil {
		client.Close()
		return nil, err
	}

	// Register tools
	r.mu.Lock()
	defer r.mu.Unlock()

	var toolNames []string
	for _, tool := range tools {
		schema := tool.GetSchema()
		if schema != nil && schema.Title != "" {
			// Create namespaced name using server name
			namespacedName := fmt.Sprintf("%s__%s", serverName, schema.Title)

			// Set source on the tool for persistence
			if mcpTool, ok := tool.(*MCPTool); ok {
				mcpTool.Source = serverSpec
			}

			// Wrap the tool to provide namespaced schema
			wrappedTool := &NamespacedTool{
				Tool:           tool,
				namespacedName: namespacedName,
			}
			r.tools[namespacedName] = wrappedTool
			r.toolClients[namespacedName] = client
			toolNames = append(toolNames, namespacedName)
			zap.S().Debugw("mcp_tool_registered", "tool_name", namespacedName)
		}
	}

	// Track which tools came from this server
	r.serverTools[serverSpec] = toolNames

	return toolNames, nil
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
		zap.S().Debugw("mcp_tool_removed", "tool_name", name)
	}

	// Close client
	if client != nil {
		client.Close()
		zap.S().Debugw("mcp_server_closed", "server_name", GetMCPDisplayName(serverSpec))
	}

	// Clean up tracking
	delete(r.serverTools, serverSpec)

	return nil
}

// LoadShellTool loads a single shell tool from a file path with namespace
func (r *ToolRegistry) LoadShellTool(path string) (LoadResult, error) {
	shellTool, err := NewShellTool(path)
	if err != nil {
		return LoadResult{}, fmt.Errorf("failed to load shell tool %s: %w", path, err)
	}

	// Get the tool's schema to find its name
	schema := shellTool.GetSchema()
	if schema == nil || schema.Title == "" {
		return LoadResult{}, fmt.Errorf("shell tool %s has no name in schema", path)
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
	zap.S().Debugw("shell_tool_registered", "tool_name", namespacedName)

	return LoadResult{
		Type: "shell",
		Servers: []ServerResult{{
			Name:      namespace,
			ToolNames: []string{namespacedName},
		}},
	}, nil
}

// LoadToolAuto attempts to load a tool, auto-detecting if it's native, shell tool, or MCP server
func (r *ToolRegistry) LoadToolAuto(pathOrServer string) (LoadResult, error) {
	// First check if it's a registered native tool
	r.mu.RLock()
	factory, isNative := r.nativeTools[pathOrServer]
	r.mu.RUnlock()

	if isNative {
		tool := factory()
		r.Register(tool)
		return LoadResult{
			Type: "native",
			Servers: []ServerResult{{
				Name:      "native",
				ToolNames: []string{pathOrServer},
			}},
		}, nil
	}

	// Check if file exists
	info, err := os.Stat(pathOrServer)
	if os.IsNotExist(err) {
		return LoadResult{}, fmt.Errorf("file not found: %s", pathOrServer)
	}
	if err != nil {
		return LoadResult{}, fmt.Errorf("cannot access %s: %v", pathOrServer, err)
	}

	// Determine type based on extension and try appropriate loader
	isJSON := strings.HasSuffix(strings.ToLower(pathOrServer), ".json")

	if isJSON {
		// Try MCP for JSON files
		mcpResult, mcpErr := r.LoadMCPServer(pathOrServer)
		if mcpErr == nil {
			return mcpResult, nil
		}
		return LoadResult{}, mcpErr
	}

	// For non-JSON, try as shell tool
	// Check if executable
	if info.Mode()&0111 == 0 {
		return LoadResult{}, fmt.Errorf("%s is not executable (for shell tools, run: chmod +x %s)", pathOrServer, pathOrServer)
	}

	shellResult, shellErr := r.LoadShellTool(pathOrServer)
	if shellErr == nil {
		return shellResult, nil
	}
	return LoadResult{}, shellErr
}

// LoadMCPServers batch loads multiple servers
func (r *ToolRegistry) LoadMCPServers(serverSpecs []string) error {
	for _, spec := range serverSpecs {
		if _, err := r.LoadMCPServer(spec); err != nil {
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


// LoadMCPServerWithFilter connects to an MCP server and only registers specified tools
// serverSpec format: "path/to/config.json#servername"
func (r *ToolRegistry) LoadMCPServerWithFilter(serverSpec string, allowedTools []string) error {
	jsonFile, serverName := ParseServerSpec(serverSpec)

	// Load config file
	configs, err := LoadMCPConfigFile(jsonFile)
	if err != nil {
		return err
	}

	// Find the right config
	var config MCPConfig
	var namespace string

	if serverName != "" {
		cfg, ok := configs[serverName]
		if !ok {
			return fmt.Errorf("server %q not found in config", serverName)
		}
		config = cfg
		namespace = serverName
	} else if len(configs) == 1 {
		for name, cfg := range configs {
			config = cfg
			namespace = name
			break
		}
	} else {
		return fmt.Errorf("config has multiple servers, need specific server in spec")
	}

	// Create client
	client, err := NewMCPClientFromConfig(&config)
	if err != nil {
		return err
	}
	client.serverSpec = serverSpec

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

				// Set source on the tool for persistence
				if mcpTool, ok := tool.(*MCPTool); ok {
					mcpTool.Source = serverSpec
				}

				// Wrap the tool to provide namespaced schema
				wrappedTool := &NamespacedTool{
					Tool:           tool,
					namespacedName: namespacedName,
				}
				r.tools[namespacedName] = wrappedTool
				r.toolClients[namespacedName] = client
				toolNames = append(toolNames, namespacedName)
				zap.S().Debugw("mcp_tool_registered", "tool_name", namespacedName)
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

// extractNamespace extracts a namespace from a server spec
// e.g., "/path/to/filesystem.json" -> "filesystem"
// e.g., "/path/to/mcp.json#myserver" -> "myserver"
func extractNamespace(serverSpec string) string {
	jsonFile, serverName := ParseServerSpec(serverSpec)
	if serverName != "" {
		return serverName
	}
	base := filepath.Base(jsonFile)
	// Remove extension
	namespace := strings.TrimSuffix(base, filepath.Ext(base))
	return namespace
}
