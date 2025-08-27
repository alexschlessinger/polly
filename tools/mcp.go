package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPTool wraps an MCP tool to implement the Tool interface
type MCPTool struct {
	session   *mcp.ClientSession
	tool      *mcp.Tool
	Namespace string // Namespace prefix for the tool
	Source    string // Server spec that provided this tool
}

// NewMCPTool creates a new MCP tool wrapper
func NewMCPTool(session *mcp.ClientSession, tool *mcp.Tool) *MCPTool {
	return &MCPTool{
		session: session,
		tool:    tool,
	}
}

// GetSchema returns the tool's schema with namespaced title
func (m *MCPTool) GetSchema() *jsonschema.Schema {
	var schema *jsonschema.Schema
	
	// Get base schema
	if m.tool.InputSchema != nil {
		// Create a copy to avoid modifying the original
		schema = &jsonschema.Schema{
			Title:                m.tool.InputSchema.Title,
			Description:          m.tool.InputSchema.Description,
			Type:                 m.tool.InputSchema.Type,
			Properties:           m.tool.InputSchema.Properties,
			Required:             m.tool.InputSchema.Required,
			AdditionalProperties: m.tool.InputSchema.AdditionalProperties,
		}
		// Set the title from the tool name if not already set
		if schema.Title == "" {
			schema.Title = m.tool.Name
		}
		// Set the description if not already set
		if schema.Description == "" {
			schema.Description = m.tool.Description
		}
	} else {
		// If no input schema, create a basic one with no properties
		schema = &jsonschema.Schema{
			Title:       m.tool.Name,
			Description: m.tool.Description,
			Type:        "object",
		}
	}
	
	// Add namespace to title if present
	if m.Namespace != "" && schema.Title != "" {
		schema.Title = fmt.Sprintf("%s__%s", m.Namespace, schema.Title)
	}
	
	return schema
}

// GetName returns the namespaced name of the tool
func (m *MCPTool) GetName() string {
	name := m.tool.Name
	if m.Namespace != "" {
		return fmt.Sprintf("%s__%s", m.Namespace, name)
	}
	return name
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

	// Filter out any arguments not present in the tool's input schema.
	// This is important for no-arg tools where we injected a placeholder
	// property for OpenAI schema compliance.
	if m.tool != nil {
		allowed := map[string]struct{}{}
		if m.tool.InputSchema != nil && len(m.tool.InputSchema.Properties) > 0 {
			for k := range m.tool.InputSchema.Properties {
				allowed[k] = struct{}{}
			}
		}
		// Build a filtered args map with only allowed keys
		if len(allowed) == 0 {
			// No args allowed; drop everything
			args = map[string]any{}
		} else {
			filtered := make(map[string]any)
			for k, v := range args {
				if _, ok := allowed[k]; ok {
					filtered[k] = v
				}
			}
			args = filtered
		}
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
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// GetMCPDisplayName returns a display-friendly name for an MCP server spec
// For JSON files, it shows: "filename.json → command args"
// For direct commands, it returns them as-is
func GetMCPDisplayName(serverSpec string) string {
	// Check if it's a JSON file
	if !strings.HasSuffix(serverSpec, ".json") {
		return serverSpec
	}

	// Try to read and parse the JSON file
	data, err := os.ReadFile(serverSpec)
	if err != nil {
		// If we can't read the file, just return the spec as-is
		return serverSpec
	}

	// Parse the JSON configuration
	var config MCPConfig
	if err := json.Unmarshal(data, &config); err != nil {
		// If we can't parse it, return the spec as-is
		return serverSpec
	}

	// Build the display name from the command and args
	displayParts := []string{config.Command}
	displayParts = append(displayParts, config.Args...)
	displayName := strings.Join(displayParts, " ")

	// Return in the format: "file.json → command args"
	return fmt.Sprintf("%s → %s", serverSpec, displayName)
}

// FormatMCPServersForDisplay formats a list of MCP server specs for display
func FormatMCPServersForDisplay(servers []string) []string {
	formatted := make([]string, len(servers))
	for i, server := range servers {
		formatted[i] = GetMCPDisplayName(server)
	}
	return formatted
}

// MCPClient manages connection to an MCP server
type MCPClient struct {
	session    *mcp.ClientSession
	client     *mcp.Client
	serverSpec string // The server spec (JSON file path) for this client
}

// NewMCPClient creates a new MCP client from a JSON config file
func NewMCPClient(jsonFile string) (*MCPClient, error) {
	// Require JSON file
	if !strings.HasSuffix(jsonFile, ".json") {
		return nil, fmt.Errorf("MCP servers must be defined in JSON files (got %s)", jsonFile)
	}

	// Try to read and parse the JSON file
	data, err := os.ReadFile(jsonFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read MCP config file %s: %v", jsonFile, err)
	}

	// Parse the JSON configuration as a single config
	var config MCPConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse MCP config: %v", err)
	}

	log.Printf("loading MCP config from %s", jsonFile)
	client, err := NewMCPClientFromConfig(&config)
	if err != nil {
		return nil, err
	}
	// Set the serverSpec for persistence
	client.serverSpec = jsonFile
	return client, nil
}


// NewMCPClientFromConfig creates a new MCP client from a JSON configuration
func NewMCPClientFromConfig(config *MCPConfig) (*MCPClient, error) {
	ctx := context.Background()

	if config.Command == "" {
		return nil, fmt.Errorf("empty command in MCP config")
	}

	// Create the MCP client
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "pollytool",
		Version: "1.0.0",
	}, nil)

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

	// Connect to the server
	log.Printf("connecting to MCP server: %s %v", config.Command, config.Args)
	session, err := client.Connect(ctx, mcp.NewCommandTransport(cmd))
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
