package tools

import (
	"context"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// Tool is the generic interface for all tools
type Tool interface {
	// Execution methods
	GetSchema() *schema.ToolSchema
	Execute(ctx context.Context, args map[string]any) (string, error)

	// Metadata methods
	GetName() string   // Returns the namespaced name (e.g., "script__toolname")
	GetType() string   // Returns the tool type: "shell", "mcp", or "native"
	GetSource() string // Returns the source path/spec (e.g., "/path/to/script.sh")
}

// ToolOutput preserves typed media without forcing it through a base64 JSON
// string. Agent callers use OutputTool when available; ordinary Tool callers
// remain source-compatible with Execute's string result.
type ToolOutput struct {
	Text  string
	Media []ToolMedia
}

type ToolMedia struct {
	Data      []byte
	MIMEType  string
	Name      string
	Reference string
}

// OutputTool is the optional rich-result extension implemented by tools such
// as MCP. Tool itself intentionally remains unchanged.
type OutputTool interface {
	Tool
	ExecuteOutput(ctx context.Context, args map[string]any) (ToolOutput, error)
}

// sandboxedTool is implemented by tool types whose commands can run sandboxed.
type sandboxedTool interface {
	Sandboxed() bool
}

// SandboxInfo describes whether a tool can be sandboxed, whether sandboxing is
// currently active, and the effective sandbox config when it is known.
type SandboxInfo struct {
	Capable  bool
	Active   bool
	OptedOut bool
	Config   *sandbox.Config
}

type sandboxDetailsTool interface {
	SandboxDetails() SandboxInfo
}

func copySandboxConfig(cfg *sandbox.Config) *sandbox.Config {
	if cfg == nil {
		return nil
	}
	c := *cfg
	c.WritablePaths = append([]string(nil), cfg.WritablePaths...)
	c.ReadPaths = append([]string(nil), cfg.ReadPaths...)
	c.DenyPaths = append([]string(nil), cfg.DenyPaths...)
	c.DenyWritePaths = append([]string(nil), cfg.DenyWritePaths...)
	c.AllowEnv = append([]string(nil), cfg.AllowEnv...)
	c.PassEnv = append([]string(nil), cfg.PassEnv...)
	c.AllowUnixSockets = append([]string(nil), cfg.AllowUnixSockets...)
	return &c
}

func unwrapTool(t Tool) Tool {
	for {
		nt, ok := t.(*NamespacedTool)
		if !ok || nt.Tool == nil {
			return t
		}
		t = nt.Tool
	}
}

// SandboxDetails reports sandbox capability, active state, opt-out state, and
// the effective config when the tool recorded it at sandbox construction time.
func SandboxDetails(t Tool) SandboxInfo {
	if t == nil {
		return SandboxInfo{}
	}
	t = unwrapTool(t)
	if dt, ok := t.(sandboxDetailsTool); ok {
		info := dt.SandboxDetails()
		info.Config = copySandboxConfig(info.Config)
		return info
	}
	st, ok := t.(sandboxedTool)
	if !ok {
		return SandboxInfo{}
	}
	return SandboxInfo{Capable: true, Active: st.Sandboxed()}
}

// SandboxState reports whether t supports sandboxing and whether it is active.
// Namespaced wrappers are unwrapped first: NamespacedTool embeds the Tool
// interface, so methods outside it don't promote.
func SandboxState(t Tool) (capable, active bool) {
	info := SandboxDetails(t)
	return info.Capable, info.Active
}

// ToolCall represents a request to execute a tool
type ToolCall struct {
	ID   string         // Provider-specific ID (if any)
	Name string         // Tool name
	Args map[string]any // Parsed arguments
}

// ToolLoaderInfo stores information needed to reload a specific tool
type ToolLoaderInfo struct {
	Name   string `json:"name"`   // Full namespaced tool name
	Type   string `json:"type"`   // "shell", "mcp", or "native"
	Source string `json:"source"` // Path for shell, server spec for MCP, "builtin" for native
}
