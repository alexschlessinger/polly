package tools

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"github.com/google/jsonschema-go/jsonschema"
)

// BashTool executes shell commands via bash -c.
type BashTool struct {
	workDir string
	sandbox sandbox.Sandbox
}

// NewBashTool creates a bash tool that runs commands in the given working directory.
// If workDir is empty, commands run in the current process directory.
func NewBashTool(workDir string) *BashTool {
	return &BashTool{workDir: workDir}
}

// WithSandbox returns a copy with sandboxing enabled.
func (t *BashTool) WithSandbox(sb sandbox.Sandbox) *BashTool {
	return &BashTool{workDir: t.workDir, sandbox: sb}
}

func (t *BashTool) GetName() string   { return "bash" }
func (t *BashTool) GetType() string   { return "native" }
func (t *BashTool) GetSource() string { return "builtin" }

func (t *BashTool) GetSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Title:       "bash",
		Description: "Execute a shell command and return its output",
		Type:        "object",
		Properties: map[string]*jsonschema.Schema{
			"command": {
				Type:        "string",
				Description: "The shell command to execute",
			},
		},
		Required: []string{"command"},
	}
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
