package tools

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// BashTool executes shell commands via bash -c.
type BashTool struct {
	workDir    string
	sandbox    sandbox.Sandbox
	sandboxCfg *sandbox.Config
}

func newBashTool(workDir string) *BashTool {
	return &BashTool{workDir: workDir}
}

// NewUnsafeBashTool creates an unsandboxed bash tool. Prefer loading "bash"
// through a ToolRegistry configured with WithSandboxFactory. This constructor
// is intentionally explicit because executing model-authored commands without
// containment grants them the caller's ambient host access.
func NewUnsafeBashTool(workDir string) *BashTool { return newBashTool(workDir) }

// WithSandbox returns a copy with sandboxing enabled.
func (t *BashTool) WithSandbox(sb sandbox.Sandbox, cfg ...sandbox.Config) *BashTool {
	out := &BashTool{workDir: t.workDir, sandbox: sb, sandboxCfg: copySandboxConfig(t.sandboxCfg)}
	if len(cfg) > 0 {
		out.sandboxCfg = copySandboxConfig(&cfg[0])
	}
	return out
}

// Sandboxed reports whether commands run inside a sandbox.
func (t *BashTool) Sandboxed() bool { return t.sandbox != nil }

// SandboxDetails reports bash sandbox posture and the effective config if known.
func (t *BashTool) SandboxDetails() SandboxInfo {
	return SandboxInfo{
		Capable: true,
		Active:  t.sandbox != nil,
		Config:  copySandboxConfig(t.sandboxCfg),
	}
}

func (t *BashTool) GetName() string   { return "bash" }
func (t *BashTool) GetType() string   { return "native" }
func (t *BashTool) GetSource() string { return "builtin" }

func (t *BashTool) GetSchema() *schema.ToolSchema {
	return schema.Tool("bash", "Execute a shell command and return its output",
		schema.Params{"command": schema.S("The shell command to execute")},
		"command",
	)
}

func (t *BashTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	command, ok := args["command"].(string)
	if !ok || strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("command must be a non-empty string")
	}

	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	if t.workDir != "" {
		cmd.Dir = t.workDir
	}

	if err := sandbox.WrapCmd(t.sandbox, cmd); err != nil {
		return "", fmt.Errorf("sandbox: %w", err)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	result := stdout.String()
	if stderr.Len() > 0 {
		if result != "" && !strings.HasSuffix(result, "\n") {
			result += "\n"
		}
		result += stderr.String()
	}

	if err != nil {
		return strings.TrimSpace(result), fmt.Errorf("command failed: %w (output: %s)", err, strings.TrimSpace(result))
	}

	return strings.TrimSpace(result), nil
}
