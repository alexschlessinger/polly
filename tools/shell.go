package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonschema"
)

// ShellTool wraps external commands/scripts as tools
type ShellTool struct {
	Command string
	schema  *jsonschema.Schema
}

// NewShellTool creates a new shell tool from a command
func NewShellTool(command string) (*ShellTool, error) {
	tool := &ShellTool{Command: command}

	// Load schema from the tool
	schemaJSON, err := tool.runCommand("--schema")
	if err != nil {
		return nil, fmt.Errorf("failed to get schema from %s: %v", command, err)
	}

	// Parse the schema directly - it should unmarshal properly
	tool.schema = &jsonschema.Schema{}
	err = json.Unmarshal([]byte(schemaJSON), tool.schema)
	if err != nil {
		return nil, fmt.Errorf("failed to parse schema from %s: %v", command, err)
	}

	return tool, nil
}

// GetSchema returns the tool's schema
func (s *ShellTool) GetSchema() *jsonschema.Schema {
	return s.schema
}

// Execute runs the tool with the given arguments
func (s *ShellTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	// Convert args to JSON
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("failed to marshal arguments: %v", err)
	}

	// Run command with --execute using context for timeout
	cmd := exec.CommandContext(ctx, s.Command, "--execute", string(argsJSON))
	output, err := cmd.CombinedOutput()

	// Log execution details
	if cmd.ProcessState != nil {
		name := ""
		if s.schema != nil && s.schema.Title != "" {
			name = s.schema.Title
		}
		log.Printf("shelltool %s: usr=%v sys=%v rc=%d",
			name,
			cmd.ProcessState.UserTime(),
			cmd.ProcessState.SystemTime(),
			cmd.ProcessState.ExitCode())
	}

	result := strings.TrimSpace(string(output))
	if err != nil {
		return result, fmt.Errorf("tool execution failed: %v (output: %s)", err, result)
	}

	return result, nil
}

// runCommand executes the shell tool with a single argument
func (s *ShellTool) runCommand(arg string) (string, error) {
	cmd := exec.Command(s.Command, arg)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// LoadShellTools loads shell tools from the given file paths
func LoadShellTools(paths []string) ([]Tool, error) {
	var tools []Tool

	for _, path := range paths {
		log.Printf("loading tool from: %s", path)
		shellTool, err := NewShellTool(path)
		if err != nil {
			log.Printf("failed to load tool %s: %v", path, err)
			// Continue loading other tools even if one fails
			continue
		}
		tools = append(tools, shellTool)
	}

	return tools, nil
}
