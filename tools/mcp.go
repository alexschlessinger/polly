package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPTool wraps an MCP tool to implement the Tool interface
type MCPTool struct {
	session      *mcp.ClientSession
	client       *MCPClient
	tool         *mcp.Tool
	Source       string             // Server spec that provided this tool
	cachedSchema *schema.ToolSchema // Cached converted schema
}

// Sandboxed reports whether the tool's server process runs inside a sandbox.
func (m *MCPTool) Sandboxed() bool {
	return m.client != nil && m.client.sandboxCapable && m.client.sandboxed
}

// SandboxDetails reports whether the owning MCP server process is sandboxed.
func (m *MCPTool) SandboxDetails() SandboxInfo {
	if m.client == nil || !m.client.sandboxCapable {
		return SandboxInfo{}
	}
	info := SandboxInfo{Capable: true}
	info.Active = m.client.sandboxed
	info.OptedOut = m.client.sandboxOptOut
	info.Config = copySandboxConfig(m.client.sandboxCfg)
	return info
}

// NewMCPTool creates a new MCP tool wrapper
func NewMCPTool(session *mcp.ClientSession, tool *mcp.Tool) *MCPTool {
	return &MCPTool{
		session: session,
		tool:    tool,
	}
}

// GetSchema returns the tool's schema (cached after first call)
func (m *MCPTool) GetSchema() *schema.ToolSchema {
	if m.cachedSchema != nil {
		return m.cachedSchema
	}

	var s *schema.ToolSchema
	if m.tool.InputSchema != nil {
		if data, err := json.Marshal(m.tool.InputSchema); err == nil {
			s = schema.ToolSchemaFromJSON(data)
		}
	}
	if s == nil {
		s = schema.Tool(m.tool.Name, m.tool.Description, nil)
	} else {
		if s.Title() == "" {
			s.SetTitle(m.tool.Name)
		}
		if s.Description() == "" {
			s.Raw["description"] = m.tool.Description
		}
	}

	m.cachedSchema = s
	return m.cachedSchema
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
	slog.Debug("mcp_tool_executing", "tool_name", m.tool.Name, "arguments", args)

	// Ensure args is not nil (some tools expect empty object instead of nil)
	if args == nil {
		args = make(map[string]any)
	}

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

	// Sandboxing (stdio only). true for defaults, or {"allowNetwork":true,"writablePaths":[...]}.
	Sandbox json.RawMessage `json:"sandbox,omitempty"`
}

// SandboxConfig returns the parsed sandbox config for merging overrides,
// or nil when the user didn't specify any sandbox overrides.
func (c *MCPConfig) SandboxConfig() (*sandbox.Config, error) {
	return sandbox.ParseConfig(c.Sandbox)
}

// SandboxOptOut reports whether this server requested sandbox:false. The
// registry honors that request only after an explicit WithUnsafeNoSandbox.
func (c *MCPConfig) SandboxOptOut() bool {
	return string(c.Sandbox) == "false"
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
	session        *mcp.ClientSession
	client         *mcp.Client
	serverSpec     string // The server spec (JSON file path) for this client
	sandboxCapable bool   // local stdio subprocess can be OS-sandboxed
	sandboxed      bool   // server process runs inside a sandbox (stdio only)
	sandboxCfg     *sandbox.Config
	sandboxOptOut  bool
}

// NewUnsafeMCPClient creates an unsandboxed MCP client from a server spec.
// Prefer ToolRegistry.LoadMCPServer, which applies the registry sandbox policy
// before starting stdio servers.
func NewUnsafeMCPClient(serverSpec string) (*MCPClient, error) {
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

	slog.Debug("mcp_config_loading", "config_file", jsonFile, "server_name", namespace)
	client, err := newMCPClientFromConfig(&config, nil)
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

// NewMCPClient creates an unsandboxed MCP client from a server spec.
//
// Deprecated: use NewUnsafeMCPClient, or ToolRegistry.LoadMCPServer to enforce
// sandbox policy for local stdio servers.
func NewMCPClient(serverSpec string) (*MCPClient, error) {
	return NewUnsafeMCPClient(serverSpec)
}

// newMCPClientFromConfig creates a client after the registry has decided and
// constructed the effective sandbox policy.
func newMCPClientFromConfig(config *MCPConfig, sb sandbox.Sandbox, effectiveCfg ...sandbox.Config) (*MCPClient, error) {
	ctx := context.Background()
	var cfg *sandbox.Config
	if len(effectiveCfg) > 0 {
		cfg = copySandboxConfig(&effectiveCfg[0])
	}

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
	var sandboxed bool

	switch config.Transport {
	case "sse":
		if config.URL == "" {
			return nil, fmt.Errorf("SSE transport requires a URL")
		}
		slog.Debug("mcp_sse_connecting", "url", config.URL)
		transport = &mcp.SSEClientTransport{
			Endpoint:   config.URL,
			HTTPClient: httpClientWithTimeout(config.Headers, timeout),
		}

	case "streamable":
		if config.URL == "" {
			return nil, fmt.Errorf("streamable transport requires a URL")
		}
		slog.Debug("mcp_http_connecting", "url", config.URL)
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

		// Set up stderr to see any error output from the server
		cmd.Stderr = os.Stderr

		// Treat config.Env as an explicit target-process grant. The sandbox
		// filters inherited variables, merges these configured values into the
		// target environment, and keeps them out of any pre-containment wrapper.
		closeSandboxFiles, err := sandbox.WrapCmdWithEnvManaged(sb, cmd, config.Env)
		if err != nil {
			return nil, fmt.Errorf("sandbox: %w", err)
		}
		defer func() { _ = closeSandboxFiles() }()
		sandboxed = sb != nil

		slog.Debug("mcp_stdio_connecting", "command", config.Command, "arguments", config.Args)
		transport = &mcp.CommandTransport{Command: cmd}

	default:
		return nil, fmt.Errorf("unknown transport type: %s (supported: stdio, sse, streamable)", config.Transport)
	}

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to MCP server: %v", err)
	}

	return &MCPClient{
		session:        session,
		client:         client,
		sandboxCapable: config.Transport == "" || config.Transport == "stdio",
		sandboxed:      sandboxed,
		sandboxCfg:     cfg,
		sandboxOptOut:  config.SandboxOptOut(),
		// serverSpec will be set by caller if needed
	}, nil
}

// NewMCPClientFromConfig creates an MCP client from a configuration. A non-nil
// sandbox contains local stdio transport; remote transports are never reported
// as sandbox-capable because their process runs outside this client.
//
// Deprecated: load servers through ToolRegistry so sandbox construction and
// opt-out policy are enforced centrally.
func NewMCPClientFromConfig(config *MCPConfig, sb sandbox.Sandbox) (*MCPClient, error) {
	return newMCPClientFromConfig(config, sb)
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
			slog.Debug("mcp_tool_loaded", "tool_name", tool.Name, "description", tool.Description)
			mcpTool := NewMCPTool(c.session, tool)
			mcpTool.client = c
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
