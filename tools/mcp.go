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
	session *mcp.ClientSession
	tool    *mcp.Tool
}

// NewMCPTool creates a new MCP tool wrapper
func NewMCPTool(session *mcp.ClientSession, tool *mcp.Tool) *MCPTool {
	return &MCPTool{
		session: session,
		tool:    tool,
	}
}

// GetSchema returns the tool's schema
func (m *MCPTool) GetSchema() *jsonschema.Schema {
	// MCP tools already use jsonschema.Schema, so we can return it directly
	if m.tool.InputSchema != nil {
		// Set the title from the tool name if not already set
		if m.tool.InputSchema.Title == "" {
			m.tool.InputSchema.Title = m.tool.Name
		}
		// Set the description if not already set
		if m.tool.InputSchema.Description == "" {
			m.tool.InputSchema.Description = m.tool.Description
		}
		return m.tool.InputSchema
	}

	// If no input schema, create a basic one with no properties
	// Don't set Properties to empty map - leave it nil for tools with no inputs
	return &jsonschema.Schema{
		Title:       m.tool.Name,
		Description: m.tool.Description,
		Type:        "object",
	}
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

// MCPClient manages connection to an MCP server
type MCPClient struct {
	session *mcp.ClientSession
	client  *mcp.Client
}

// NewMCPClient creates a new MCP client connected to the given server command or JSON config file
func NewMCPClient(serverCommand string) (*MCPClient, error) {
	// Check if serverCommand is a JSON file path
	if strings.HasSuffix(serverCommand, ".json") {
		// Try to read and parse the JSON file
		data, err := os.ReadFile(serverCommand)
		if err != nil {
			return nil, fmt.Errorf("failed to read MCP config file %s: %v", serverCommand, err)
		}

		// Parse the JSON configuration as a single config
		var config MCPConfig
		if err := json.Unmarshal(data, &config); err != nil {
			return nil, fmt.Errorf("failed to parse MCP config: %v", err)
		}

		log.Printf("loading MCP config from %s", serverCommand)
		return NewMCPClientFromConfig(&config)
	}

	// Not a JSON file, treat as a command
	return NewMCPClientFromCommand(serverCommand)
}

// NewMCPClientFromCommand creates a new MCP client from a command string
func NewMCPClientFromCommand(serverCommand string) (*MCPClient, error) {
	ctx := context.Background()

	// Parse the command - it could have arguments
	parts := strings.Fields(serverCommand)
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty server command")
	}

	// Create the MCP client
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "pollytool",
		Version: "1.0.0",
	}, nil)

	// Create the command to run the server
	var cmd *exec.Cmd
	if len(parts) == 1 {
		cmd = exec.Command(parts[0])
	} else {
		cmd = exec.Command(parts[0], parts[1:]...)
	}

	// Set up stderr to see any error output from the server
	cmd.Stderr = log.Writer()

	// Connect to the server
	log.Printf("connecting to MCP server: %s", serverCommand)
	session, err := client.Connect(ctx, mcp.NewCommandTransport(cmd))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to MCP server: %v", err)
	}

	return &MCPClient{
		session: session,
		client:  client,
	}, nil
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
		session: session,
		client:  client,
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
			tools = append(tools, NewMCPTool(c.session, tool))
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
