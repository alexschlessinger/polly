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
	// siblingLoaded reports whether a named dedicated tool is visible to the
	// model, so the schema can steer file work away from shell commands. It
	// takes the registry lock: GetSchema must not run under it (see
	// ToolRegistry.GetSchemas).
	siblingLoaded func(name string) bool
}

func newBashTool(workDir string) *BashTool {
	return &BashTool{workDir: workDir}
}

// NewBashTool creates an unsandboxed bash tool.
//
// Deprecated: use NewUnsafeBashTool to make the lack of containment explicit,
// or load "bash" through a ToolRegistry configured with WithSandboxFactory.
func NewBashTool(workDir string) *BashTool { return NewUnsafeBashTool(workDir) }

// NewUnsafeBashTool creates an unsandboxed bash tool. Prefer loading "bash"
// through a ToolRegistry configured with WithSandboxFactory. This constructor
// is intentionally explicit because executing model-authored commands without
// containment grants them the caller's ambient host access.
func NewUnsafeBashTool(workDir string) *BashTool { return newBashTool(workDir) }

// WithSandbox returns a copy with sandboxing enabled.
func (t *BashTool) WithSandbox(sb sandbox.Sandbox) *BashTool {
	return &BashTool{workDir: t.workDir, sandbox: sb, siblingLoaded: t.siblingLoaded}
}

func (t *BashTool) withSandboxConfig(sb sandbox.Sandbox, cfg sandbox.Config) *BashTool {
	out := t.WithSandbox(sb)
	out.sandboxCfg = copySandboxConfig(&cfg)
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

// bashAlternatives names the shell commands each dedicated tool replaces.
// Steering lives on bash — the tool the model is about to misuse — because
// that is the description it attends to when reaching for cat or grep.
var bashAlternatives = []struct{ name, replaces string }{
	{"read_file", "cat/head/tail"},
	{"search_files", "grep/rg"},
	{"list_dir", "ls/find"},
	{"write_file", "echo/tee redirection"},
	{"edit_file", "sed/awk in-place edits"},
}

func (t *BashTool) GetSchema() *schema.ToolSchema {
	// Mirror the shell-tool annotation so the model knows writes and network
	// may be restricted; call out a read-only .git specifically, since a
	// failing commit otherwise surfaces as an unexplained EPERM the model
	// will retry.
	description := "Execute a shell command and return its output"
	if t.sandbox != nil {
		switch {
		case t.sandboxCfg != nil && t.sandboxCfg.GitMetadataReadOnly():
			description += " [sandboxed: .git is read-only, git commit will fail]"
		default:
			description += " [sandboxed]"
		}
	}
	if t.siblingLoaded != nil {
		var prefer []string
		for _, alt := range bashAlternatives {
			if t.siblingLoaded(alt.name) {
				prefer = append(prefer, alt.name+" instead of "+alt.replaces)
			}
		}
		if len(prefer) > 0 {
			description += ". IMPORTANT: for file work, use the dedicated tools: " +
				strings.Join(prefer, ", ") +
				". Reserve bash for what only a shell can do (pipelines, git, builds, running programs)"
		}
	}
	return schema.Tool("bash", description,
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

	closeSandboxFiles, err := sandbox.WrapCmdManaged(t.sandbox, cmd)
	if err != nil {
		return "", fmt.Errorf("sandbox: %w", err)
	}
	defer func() { _ = closeSandboxFiles() }()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()

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
