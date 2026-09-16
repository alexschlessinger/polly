package tools

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// BashTool executes shell commands via bash -c, or bash -o pipefail -c under
// a context from WithPipefail.
type BashTool struct {
	workDir    string
	sandbox    sandbox.Sandbox
	sandboxCfg *sandbox.Config
	// siblingLoaded reports whether a named dedicated tool is visible to the
	// model, so the schema can steer file work away from shell commands. It
	// takes the registry lock: GetSchema must not run under it (see
	// ToolRegistry.GetSchemas).
	siblingLoaded func(name string) bool
	// tracker resolves the registry's ChangeTracker at execution time, so a
	// tracker installed after the tool loaded still observes its commands.
	tracker func() ChangeTracker
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
func (t *BashTool) WithSandbox(sb sandbox.Sandbox) *BashTool {
	return &BashTool{workDir: t.workDir, sandbox: sb, siblingLoaded: t.siblingLoaded, tracker: t.tracker}
}

func (t *BashTool) withSandboxConfig(sb sandbox.Sandbox, cfg sandbox.Config) *BashTool {
	out := t.WithSandbox(sb)
	out.sandboxCfg = copySandboxConfig(&cfg)
	return out
}

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
	{"list_dir", "ls/find"},
	{"write_file", "echo/tee redirection"},
	{"edit_file", "sed/awk in-place edits"},
}

func (t *BashTool) GetSchema() *schema.ToolSchema {
	// Mirror the shell-tool annotation so the model knows writes and network
	// may be restricted; call out a read-only .git specifically, since a
	// failing commit otherwise surfaces as an unexplained EPERM the model
	// will retry.
	description := "Run bash -c on " + runtime.GOOS + "; return output. Fresh shell per call: cd, exports, variables, and options do not persist. Repeat setup or source a setup file each call. Put caches and disposable build output in supplied writable scratch/temp paths. Sandbox denials are environment limits; do not bypass by changing ownership, persistent user configuration, or project code. Tool success means final exit 0; earlier commands and pipeline stages may fail. Run required checks separately or explicitly propagate their status. Opt into strict execution with set -e -o pipefail; handle expected nonzero exits and early-closing pipelines. Use portable flags; do not assume GNU utilities"
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
	out, err := t.ExecuteOutput(ctx, args)
	return out.Text, err
}

// CommandResult records the process outcome without interpreting stderr.
// Changes is what the command changed in its workspace when the registry has
// a ChangeTracker, and nil otherwise.
type CommandResult struct {
	ExitCode int          `json:"exitCode"`
	Changes  *FileChanges `json:"changes,omitempty"`
}

// changeSnapshotTimeout bounds each workspace snapshot around a command. The
// after-snapshot runs even when the command's own context has ended, since
// the command may have written files before it was stopped, but a slow
// tracker must not extend the call indefinitely.
const changeSnapshotTimeout = 5 * time.Second

func snapshotContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), changeSnapshotTimeout)
}

// commandChanges observes the workspace around one command through the
// registry's tracker. Tracking never fails the command: an unobservable
// workspace or a tracker error reports Tracked=false with a reason.
type commandChanges struct {
	tracker ChangeTracker
	dir     string
	token   string
	result  *FileChanges
}

func (t *BashTool) beginChanges(ctx context.Context) *commandChanges {
	if t.tracker == nil {
		return nil
	}
	tracker := t.tracker()
	if tracker == nil {
		return nil
	}
	c := &commandChanges{tracker: tracker, dir: t.workDir, result: &FileChanges{}}
	sctx, cancel := snapshotContext(ctx)
	defer cancel()
	token, ok, reason, err := tracker.Snapshot(sctx, t.workDir)
	switch {
	case err != nil:
		c.result.Reason = err.Error()
	case !ok:
		c.result.Reason = reason
	default:
		c.token = token
	}
	return c
}

func (c *commandChanges) finish(ctx context.Context) *FileChanges {
	if c == nil {
		return nil
	}
	if c.token == "" {
		return c.result
	}
	sctx, cancel := snapshotContext(ctx)
	defer cancel()
	changes, err := c.tracker.Changes(sctx, c.dir, c.token)
	if err != nil {
		return &FileChanges{Reason: err.Error()}
	}
	return &changes
}

// CommandError means a command was launched and exited unsuccessfully. Setup,
// cancellation, timeout and incomplete-capture errors do not have this type.
type CommandError struct {
	ExitCode int
	Cause    error
}

func (e *CommandError) Error() string { return fmt.Sprintf("command failed: %v", e.Cause) }
func (e *CommandError) Unwrap() error { return e.Cause }

type pipefailKey struct{}

// WithPipefail makes bash commands executed under ctx run with pipefail, so a
// pipeline fails when any stage fails rather than only its last. Workflow exec
// uses it because scripts gate on exit codes without reading the output.
func WithPipefail(ctx context.Context) context.Context {
	return context.WithValue(ctx, pipefailKey{}, true)
}

func (t *BashTool) ExecuteOutput(ctx context.Context, args map[string]any) (ToolOutput, error) {
	command, ok := args["command"].(string)
	if !ok || strings.TrimSpace(command) == "" {
		return ToolOutput{}, fmt.Errorf("command must be a non-empty string")
	}

	shellArgs := []string{"-c", command}
	if pipefail, _ := ctx.Value(pipefailKey{}).(bool); pipefail {
		shellArgs = []string{"-o", "pipefail", "-c", command}
	}
	stdout := newBoundedBuffer(capturedOutputLimit)
	stderr := newBoundedBuffer(capturedOutputLimit)
	tracking := t.beginChanges(ctx)
	_, err := runFiniteCommand(ctx, t.sandbox, finiteCommand{
		name: "bash", args: shellArgs, dir: t.workDir,
		stdout: stdout, stderr: stderr, acknowledge: t.sandbox != nil,
	})
	changes := tracking.finish(ctx)

	result := stdout.String()
	if stderr.Len() > 0 || stderr.Truncated() {
		if result != "" && !strings.HasSuffix(result, "\n") {
			result += "\n"
		}
		result += stderr.String()
	}

	out := ToolOutput{Text: strings.TrimSpace(result)}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	if err != nil {
		// The runner returns a bare ExitError only for a complete command
		// result. Wrapped launcher/capture errors must remain unrecoverable.
		if exit, ok := err.(*exec.ExitError); ok {
			code := exit.ExitCode()
			if code < 0 {
				// Killed by a signal: report it the way shells do, so the
				// caller still sees a command result rather than a launch failure.
				if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
					code = 128 + int(status.Signal())
				}
			}
			if code >= 0 {
				out.Data = CommandResult{ExitCode: code, Changes: changes}
				return out, &CommandError{ExitCode: code, Cause: err}
			}
		}
		return out, err
	}
	out.Data = CommandResult{ExitCode: 0, Changes: changes}
	return out, nil
}
