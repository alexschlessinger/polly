package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// ShellTool wraps external commands/scripts as tools
type ShellTool struct {
	Command       string
	schema        *schema.ToolSchema
	sandbox       sandbox.Sandbox
	sandboxCfg    *sandbox.Config // parsed from the script's schema "sandbox" field
	sandboxOptOut bool            // user set "sandbox": false
}

// SandboxConfig returns sandbox override config parsed from the script's schema,
// or nil if the tool didn't declare any overrides.
func (s *ShellTool) SandboxConfig() *sandbox.Config { return s.sandboxCfg }

// SandboxOptOut reports whether the script explicitly disabled sandboxing
// by setting "sandbox": false in its schema.
func (s *ShellTool) SandboxOptOut() bool { return s.sandboxOptOut }

// WantsSandbox reports whether the script's schema declared sandbox overrides.
func (s *ShellTool) WantsSandbox() bool { return s.sandboxCfg != nil }

// WithSandbox returns a copy with sandboxing enabled.
func (s *ShellTool) WithSandbox(sb sandbox.Sandbox) *ShellTool {
	return &ShellTool{Command: s.Command, schema: s.schema, sandbox: sb, sandboxCfg: s.sandboxCfg, sandboxOptOut: s.sandboxOptOut}
}

// NewShellTool creates a new shell tool from a command.
// An optional sandbox.Sandbox can be provided to run the --schema
// invocation inside a sandbox, preventing untrusted scripts from
// executing side effects during schema loading.
func NewShellTool(command string, schemaSandbox ...sandbox.Sandbox) (*ShellTool, error) {
	tool := &ShellTool{Command: command}

	// Load schema from the tool, sandboxed if a sandbox is provided.
	var schemaJSON string
	var err error
	if len(schemaSandbox) > 0 && schemaSandbox[0] != nil {
		schemaJSON, err = tool.runCommandSandboxed("--schema", schemaSandbox[0])
	} else {
		schemaJSON, err = tool.runCommand("--schema")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get schema from %s: %v", command, err)
	}

	// Extract the sandbox spec before parsing the standard schema.
	var meta struct {
		Sandbox json.RawMessage `json:"sandbox"`
	}
	_ = json.Unmarshal([]byte(schemaJSON), &meta)
	tool.sandboxOptOut = string(meta.Sandbox) == "false"
	tool.sandboxCfg, err = sandbox.ParseConfig(meta.Sandbox)
	if err != nil {
		return nil, fmt.Errorf("invalid sandbox config in %s: %w", command, err)
	}

	tool.schema = schema.ToolSchemaFromString(schemaJSON)
	if tool.schema == nil {
		return nil, fmt.Errorf("failed to parse schema from %s", command)
	}

	return tool, nil
}

// GetSchema returns the tool's schema, annotated with [sandboxed] if applicable
func (s *ShellTool) GetSchema() *schema.ToolSchema {
	c := s.schema.Copy()
	if c == nil {
		return nil
	}
	if s.sandbox != nil {
		c.Raw["description"] = c.Description() + " [sandboxed]"
	}
	return c
}

// GetName returns the name of the tool
func (s *ShellTool) GetName() string {
	if s.schema != nil {
		return s.schema.Title()
	}
	return ""
}

// GetType returns "shell" for shell tools
func (s *ShellTool) GetType() string {
	return "shell"
}

// GetSource returns the command/script path
func (s *ShellTool) GetSource() string {
	return s.Command
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

	if err := sandbox.WrapCmd(s.sandbox, cmd); err != nil {
		return "", fmt.Errorf("sandbox: %w", err)
	}

	output, err := cmd.CombinedOutput()

	// Log execution details
	if cmd.ProcessState != nil {
		name := ""
		if s.schema != nil {
			name = s.schema.Title()
		}
		slog.Debug("shell_tool_completed",
			"tool_name", name,
			"user_time", cmd.ProcessState.UserTime(),
			"system_time", cmd.ProcessState.SystemTime(),
			"exit_code", cmd.ProcessState.ExitCode())
	}

	result := strings.TrimSpace(string(output))
	if err != nil {
		return result, fmt.Errorf("tool execution failed: %v (output: %s)", err, result)
	}

	return result, nil
}

// runCommand executes the shell tool with a single argument.
func (s *ShellTool) runCommand(arg string) (string, error) {
	cmd := exec.Command(s.Command, arg)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// runCommandSandboxed executes the shell tool inside a sandbox.
func (s *ShellTool) runCommandSandboxed(arg string, sb sandbox.Sandbox) (string, error) {
	cmd := exec.Command(s.Command, arg)
	if err := sandbox.WrapCmd(sb, cmd); err != nil {
		return "", fmt.Errorf("sandbox: %w", err)
	}
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
		slog.Debug("tool_loading", "path", path)
		shellTool, err := NewShellTool(path)
		if err != nil {
			slog.Debug("tool_load_failed", "path", path, "error", err)
			// Continue loading other tools even if one fails
			continue
		}
		tools = append(tools, shellTool)
	}

	return tools, nil
}
