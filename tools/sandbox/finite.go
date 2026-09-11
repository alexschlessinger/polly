package sandbox

import (
	"errors"
	"fmt"
	"os/exec"
)

// WrapFiniteCmdManaged wraps a context-bound finite command and owns its
// cancellation policy. Cleanup has the same descriptor ownership contract as
// WrapCmdManaged and must be called immediately after Start, including failure.
// On Unix, cancellation kills the private command group, except for Linux's
// built-in sandbox, which uses bubblewrap's PID-namespace teardown. Other
// platforms kill the direct process. Detached sessions are outside the group.
// Use the existing wrapping APIs for long-lived transports such as stdio MCP.
func WrapFiniteCmdManaged(sb Sandbox, cmd *exec.Cmd) (func() error, error) {
	if cmd.Cancel == nil {
		return noSandboxFileCleanup, fmt.Errorf("finite command requires exec.CommandContext")
	}
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		return cleanup, err
	}
	if err := configureFiniteCancellation(sb, cmd); err != nil {
		return noSandboxFileCleanup, errors.Join(err, cleanup())
	}
	return cleanup, nil
}
