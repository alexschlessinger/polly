package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPTool wraps an MCP tool to implement the Tool interface
type MCPTool struct {
	session      *mcp.ClientSession
	tool         *mcp.Tool
	Source       string             // Server spec that provided this tool
	cachedSchema *jsonschema.Schema // Cached converted schema
}

// NewMCPTool creates a new MCP tool wrapper
func NewMCPTool(session *mcp.ClientSession, tool *mcp.Tool) *MCPTool {
	return &MCPTool{
		session: session,
		tool:    tool,
	}
}

// GetSchema returns the tool's schema (cached after first call)
func (m *MCPTool) GetSchema() *jsonschema.Schema {
	// Return cached schema if available
	if m.cachedSchema != nil {
		return m.cachedSchema
	}

	var schema *jsonschema.Schema

	// Get base schema
	if m.tool.InputSchema != nil {
		// InputSchema is now 'any' in v0.8.0, try to convert it
		// First, try to unmarshal it to jsonschema.Schema
		schemaBytes, err := json.Marshal(m.tool.InputSchema)
		if err == nil {
			schema = &jsonschema.Schema{}
			if err := json.Unmarshal(schemaBytes, schema); err != nil {
				schema = nil
			}
		}

		// If we got a schema, set defaults
		if schema != nil {
			// Set the title from the tool name if not already set
			if schema.Title == "" {
				schema.Title = m.tool.Name
			}
			// Set the description if not already set
			if schema.Description == "" {
				schema.Description = m.tool.Description
			}
		} else {
			// If conversion failed, create a basic schema
			schema = &jsonschema.Schema{
				Title:       m.tool.Name,
				Description: m.tool.Description,
				Type:        "object",
			}
		}
	} else {
		// If no input schema, create a basic one with no properties
		schema = &jsonschema.Schema{
			Title:       m.tool.Name,
			Description: m.tool.Description,
			Type:        "object",
		}
	}

	m.cachedSchema = schema
	return schema
}

// GetName returns the name of the tool
func (m *MCPTool) GetName() string {
	return m.tool.Name
}

// GetType returns "mcp" for MCP tools
func (m *MCPTool) GetType() string {
	return "mcp"
}

// GetSource returns the server spec that provided this tool
func (m *MCPTool) GetSource() string {
	return m.Source
}

// Execute runs the MCP tool with the given arguments
func (m *MCPTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	// Log the tool execution for debugging
	log.Printf("%s %v", m.tool.Name, args)

	// Ensure args is not nil (some tools expect empty object instead of nil)
	if args == nil {
		args = make(map[string]any)
	}

	// Remove OpenAI compatibility placeholder if present.
	// OpenAI requires at least one property in function schemas, so we inject
	// "__noargs" for no-arg tools (see llm/openai.go ConvertToolToOpenAI).
	// Remove it before calling the actual MCP server.
	delete(args, "__noargs")

	// Create the call parameters
	params := &mcp.CallToolParams{
		Name:      m.tool.Name,
		Arguments: args,
	}

	// Call the tool via MCP
	result, err := m.session.CallTool(ctx, params)
	if err != nil {
		return "", fmt.Errorf("MCP tool execution failed: %v", err)
	}

	// Handle the result
	if result.IsError {
		// If it's an error, return the error content
		if len(result.Content) > 0 {
			content, _ := json.Marshal(result.Content)
			return "", fmt.Errorf("tool returned error: %s", string(content))
		}
		return "", fmt.Errorf("tool returned error without content")
	}

	// Convert the result content to a string
	if len(result.Content) == 0 {
		return "", nil
	}

	// Marshal the content as JSON for consistent output
	output, err := json.Marshal(result.Content)
	if err != nil {
		return "", fmt.Errorf("failed to marshal tool result: %v", err)
	}

	return string(output), nil
}

// MCPConfig represents the JSON configuration for an MCP server
type MCPConfig struct {
	// Local/stdio transport fields
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`

	// Remote transport fields
	URL       string            `json:"url,omitempty"`       // Remote server URL
	Transport string            `json:"transport,omitempty"` // "stdio" | "sse" | "streamable"
	Headers   map[string]string `json:"headers,omitempty"`   // Auth headers, API keys
	Timeout   string            `json:"timeout,omitempty"`   // Connection timeout (e.g., "30s")
}

// MCPServersConfig represents the Claude Desktop format with multiple servers
type MCPServersConfig struct {
	MCPServers map[string]MCPConfig `json:"mcpServers"`
}

// ParseServerSpec splits a server spec into file path and server name
// Format: "path/to/config.json" or "path/to/config.json#servername"
func ParseServerSpec(spec string) (jsonFile string, serverName string) {
	// Handle #servername suffix
	if idx := strings.LastIndex(spec, "#"); idx != -1 {
		// Make sure # is after the .json extension
		if strings.HasSuffix(spec[:idx], ".json") {
			return spec[:idx], spec[idx+1:]
		}
	}
	return spec, ""
}

// LoadMCPConfigFile parses a config file and returns server configs
// Requires mcpServers format: {"mcpServers": {"name": {...}}}
func LoadMCPConfigFile(jsonFile string) (map[string]MCPConfig, error) {
	data, err := os.ReadFile(jsonFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read MCP config file %s: %v", jsonFile, err)
	}

	var multiConfig MCPServersConfig
	if err := json.Unmarshal(data, &multiConfig); err != nil {
		return nil, fmt.Errorf("failed to parse MCP config: %v", err)
	}

	if len(multiConfig.MCPServers) == 0 {
		return nil, fmt.Errorf("no servers defined in mcpServers (use format: {\"mcpServers\": {\"name\": {...}}})")
	}

	return multiConfig.MCPServers, nil
}

// formatConfigDisplay formats a config for display
func formatConfigDisplay(config MCPConfig) string {
	switch config.Transport {
	case "sse", "streamable":
		return fmt.Sprintf("%s (%s)", config.URL, config.Transport)
	default:
		displayParts := []string{config.Command}
		displayParts = append(displayParts, config.Args...)
		return strings.Join(displayParts, " ")
	}
}

// GetMCPDisplayName returns a display-friendly name for an MCP server spec
// Formats: "file.json → command args" or "file.json#server → command args"
func GetMCPDisplayName(serverSpec string) string {
	jsonFile, serverName := ParseServerSpec(serverSpec)

	// Check if it's a JSON file
	if !strings.HasSuffix(jsonFile, ".json") {
		return serverSpec
	}

	configs, err := LoadMCPConfigFile(jsonFile)
	if err != nil {
		return serverSpec
	}

	// If server name specified, show that specific config
	if serverName != "" {
		if config, ok := configs[serverName]; ok {
			return fmt.Sprintf("%s → %s", serverSpec, formatConfigDisplay(config))
		}
		return serverSpec
	}

	// Single-server file or show first config
	if len(configs) == 1 {
		for _, config := range configs {
			return fmt.Sprintf("%s → %s", serverSpec, formatConfigDisplay(config))
		}
	}

	// Multi-server file without specific server - list server names
	var names []string
	for name := range configs {
		names = append(names, name)
	}
	return fmt.Sprintf("%s → [%s]", jsonFile, strings.Join(names, ", "))
}

// FormatMCPServersForDisplay formats a list of MCP server specs for display
func FormatMCPServersForDisplay(servers []string) []string {
	formatted := make([]string, len(servers))
	for i, server := range servers {
		formatted[i] = GetMCPDisplayName(server)
	}
	return formatted
}

// headerRoundTripper wraps an http.RoundTripper to inject custom headers
type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return h.base.RoundTrip(req)
}

// httpClientWithTimeout creates an HTTP client with custom headers and timeout
func httpClientWithTimeout(headers map[string]string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &headerRoundTripper{
			base:    http.DefaultTransport,
			headers: headers,
		},
	}
}

// MCPClient manages connection to an MCP server
type MCPClient struct {
	session    *mcp.ClientSession
	client     *mcp.Client
	serverSpec string // The server spec (JSON file path) for this client
}

// NewMCPClient creates a new MCP client from a server spec
// Format: "path/to/config.json" or "path/to/config.json#servername"
func NewMCPClient(serverSpec string) (*MCPClient, error) {
	jsonFile, serverName := ParseServerSpec(serverSpec)

	// Require JSON file
	if !strings.HasSuffix(jsonFile, ".json") {
		return nil, fmt.Errorf("MCP servers must be defined in JSON files (got %s)", jsonFile)
	}

	configs, err := LoadMCPConfigFile(jsonFile)
	if err != nil {
		return nil, err
	}

	var config MCPConfig
	var namespace string

	if serverName != "" {
		// Specific server requested
		cfg, ok := configs[serverName]
		if !ok {
			var available []string
			for name := range configs {
				available = append(available, name)
			}
			return nil, fmt.Errorf("server %q not found in config (available: %v)", serverName, available)
		}
		config = cfg
		namespace = serverName
	} else if len(configs) == 1 {
		// Single server in file
		for name, cfg := range configs {
			config = cfg
			namespace = name
			break
		}
	} else {
		// Multiple servers, none specified - error
		var available []string
		for name := range configs {
			available = append(available, name)
		}
		return nil, fmt.Errorf("config has multiple servers, specify one: %s#<servername> (available: %v)", jsonFile, available)
	}

	log.Printf("loading MCP config %s (server: %s)", jsonFile, namespace)
	client, err := NewMCPClientFromConfig(&config)
	if err != nil {
		return nil, err
	}

	// Set the serverSpec for persistence (include #servername for multi-server)
	if serverName != "" {
		client.serverSpec = serverSpec
	} else {
		client.serverSpec = jsonFile
	}

	return client, nil
}


// NewMCPClientFromConfig creates a new MCP client from a JSON configuration
func NewMCPClientFromConfig(config *MCPConfig) (*MCPClient, error) {
	ctx := context.Background()

	// Create the MCP client
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "pollytool",
		Version: "1.0.0",
	}, nil)

	// Parse timeout (default 30s for remote transports)
	timeout := 30 * time.Second
	if config.Timeout != "" {
		if t, err := time.ParseDuration(config.Timeout); err == nil {
			timeout = t
		}
	}

	var transport mcp.Transport

	switch config.Transport {
	case "sse":
		if config.URL == "" {
			return nil, fmt.Errorf("SSE transport requires a URL")
		}
		log.Printf("connecting to MCP server via SSE: %s", config.URL)
		transport = &mcp.SSEClientTransport{
			Endpoint:   config.URL,
			HTTPClient: httpClientWithTimeout(config.Headers, timeout),
		}

	case "streamable":
		if config.URL == "" {
			return nil, fmt.Errorf("streamable transport requires a URL")
		}
		log.Printf("connecting to MCP server via streamable HTTP: %s", config.URL)
		transport = &mcp.StreamableClientTransport{
			Endpoint:   config.URL,
			HTTPClient: httpClientWithTimeout(config.Headers, timeout),
		}

	case "stdio", "":
		// Default: local subprocess via stdio
		if config.Command == "" {
			return nil, fmt.Errorf("stdio transport requires a command")
		}

		// Create the command with arguments
		cmd := exec.Command(config.Command, config.Args...)

		// Set environment variables if provided
		if len(config.Env) > 0 {
			// Start with current environment
			cmd.Env = os.Environ()
			// Add/override with config environment variables
			for key, value := range config.Env {
				cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", key, value))
			}
		}

		// Set up stderr to see any error output from the server
		cmd.Stderr = log.Writer()

		log.Printf("connecting to MCP server: %s %v", config.Command, config.Args)
		transport = &mcp.CommandTransport{Command: cmd}

	default:
		return nil, fmt.Errorf("unknown transport type: %s (supported: stdio, sse, streamable)", config.Transport)
	}

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to MCP server: %v", err)
	}

	return &MCPClient{
		session:    session,
		client:     client,
		// serverSpec will be set by caller if needed
	}, nil
}

// ListTools returns all tools available from the MCP server
func (c *MCPClient) ListTools() ([]Tool, error) {
	ctx := context.Background()

	// List available tools using the Tools iterator
	var tools []Tool
	for tool, err := range c.session.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("error listing tools: %v", err)
		}
		if tool != nil {
			log.Printf("loaded MCP tool: %s - %s", tool.Name, tool.Description)
			mcpTool := NewMCPTool(c.session, tool)
			// Set the source to the server spec so it can be persisted
			mcpTool.Source = c.serverSpec
			tools = append(tools, mcpTool)
		}
	}

	return tools, nil
}

// Close closes the MCP client connection
func (c *MCPClient) Close() error {
	if c.session != nil {
		return c.session.Close()
	}
	return nil
}
