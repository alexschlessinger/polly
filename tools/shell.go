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
	effectiveCfg  *sandbox.Config // merged config used for the sandbox, when known
	sandboxOptOut bool            // user set "sandbox": false
}

// SandboxConfig returns sandbox override config parsed from the script's schema,
// or nil if the tool didn't declare any overrides.
func (s *ShellTool) SandboxConfig() *sandbox.Config { return s.sandboxCfg }

// SandboxOptOut reports whether the script requested sandbox:false. The
// registry honors that request only after an explicit WithUnsafeNoSandbox.
func (s *ShellTool) SandboxOptOut() bool { return s.sandboxOptOut }

// WantsSandbox reports whether the script's schema declared sandbox overrides.
func (s *ShellTool) WantsSandbox() bool { return s.sandboxCfg != nil }

// Sandboxed reports whether commands run inside a sandbox.
func (s *ShellTool) Sandboxed() bool { return s.sandbox != nil }

// WithSandbox returns a copy with sandboxing enabled.
func (s *ShellTool) WithSandbox(sb sandbox.Sandbox, cfg ...sandbox.Config) *ShellTool {
	out := &ShellTool{
		Command:       s.Command,
		schema:        s.schema,
		sandbox:       sb,
		sandboxCfg:    copySandboxConfig(s.sandboxCfg),
		effectiveCfg:  copySandboxConfig(s.effectiveCfg),
		sandboxOptOut: s.sandboxOptOut,
	}
	if len(cfg) > 0 {
		out.effectiveCfg = copySandboxConfig(&cfg[0])
	}
	return out
}

// SandboxDetails reports shell tool sandbox posture and the effective config if known.
func (s *ShellTool) SandboxDetails() SandboxInfo {
	return SandboxInfo{
		Capable:  true,
		Active:   s.sandbox != nil,
		OptedOut: s.sandboxOptOut,
		Config:   copySandboxConfig(s.effectiveCfg),
	}
}

// newShellTool creates a shell tool and optionally contains its schema command.
// Public callers should load process-backed tools through ToolRegistry, which
// enforces either a sandbox factory or an explicit unsafe opt-out.
func newShellTool(command string, schemaSandbox ...sandbox.Sandbox) (*ShellTool, error) {
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

// NewShellTool loads a shell tool and optionally contains its --schema command.
// Executions remain unsandboxed unless the returned tool is given a sandbox.
//
// Deprecated: use NewUnsafeShellTool for an explicitly unsandboxed tool, or
// ToolRegistry.LoadShellTool to enforce the registry's sandbox policy.
func NewShellTool(command string, schemaSandbox ...sandbox.Sandbox) (*ShellTool, error) {
	return newShellTool(command, schemaSandbox...)
}

// NewUnsafeShellTool loads a shell tool without containing its --schema
// command or future executions. Prefer ToolRegistry.LoadShellTool.
func NewUnsafeShellTool(command string) (*ShellTool, error) {
	return newShellTool(command)
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

	closeSandboxFiles, err := sandbox.WrapCmdManaged(s.sandbox, cmd)
	if err != nil {
		return "", fmt.Errorf("sandbox: %w", err)
	}
	defer func() { _ = closeSandboxFiles() }()

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
	closeSandboxFiles, err := sandbox.WrapCmdManaged(sb, cmd)
	if err != nil {
		return "", fmt.Errorf("sandbox: %w", err)
	}
	defer func() { _ = closeSandboxFiles() }()
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// LoadShellTools loads unsandboxed shell tools using the legacy best-effort
// behavior: invalid paths are skipped and successfully loaded tools are
// returned with a nil error. Prefer LoadShellToolsWithRegistry so discovery and
// later executions are contained and the batch leaves no partial registry state.
//
// Deprecated: use LoadShellToolsWithRegistry.
func LoadShellTools(paths []string) ([]Tool, error) {
	loaded := make([]Tool, 0, len(paths))
	for _, path := range paths {
		slog.Debug("tool_loading", "path", path)
		tool, err := NewUnsafeShellTool(path)
		if err != nil {
			slog.Debug("tool_load_failed", "path", path, "error", err)
			continue
		}
		loaded = append(loaded, tool)
	}
	return loaded, nil
}

// LoadShellToolsWithRegistry prepares every shell tool under registry policy,
// then registers the whole batch under one lock. A preparation failure leaves
// the registry unchanged.
func LoadShellToolsWithRegistry(registry *ToolRegistry, paths []string) ([]Tool, error) {
	if registry == nil {
		return nil, fmt.Errorf("tool registry is nil")
	}
	records := make([]stagedToolRecord, 0, len(paths))
	for _, path := range paths {
		prepared, _, err := registry.prepareShellToolWithNamespace(path, extractNamespace(path))
		if err != nil {
			return nil, err
		}
		records = append(records, prepared...)
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()

	loaded := make([]Tool, 0, len(records))
	for _, record := range records {
		registry.tools[record.name] = record.tool
		loaded = append(loaded, record.tool)
		slog.Debug("shell_tool_registered", "tool_name", record.name)
	}
	return loaded, nil
}
