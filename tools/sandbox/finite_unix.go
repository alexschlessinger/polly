//go:build unix

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func configureFiniteCancellation(sb Sandbox, cmd *exec.Cmd) error {
	if finiteNamespaceCancellation(sb) {
		// The bwrap monitor owns PID-namespace teardown. Its PID is not the
		// target's process group; killing a guessed group could hit other work.
		cmd.Cancel = func() error { return cmd.Process.Kill() }
		return nil
	}
	attr := syscall.SysProcAttr{}
	if cmd.SysProcAttr != nil {
		attr = *cmd.SysProcAttr
	}
	if attr.Pgid != 0 || attr.Foreground {
		return fmt.Errorf("finite command must own a private process group")
	}
	// Seatbelt already requests a private session. Calling setpgid after
	// setsid would fail for its session leader. Preserve all other attributes.
	attr.Setpgid = !attr.Setsid
	cmd.SysProcAttr = &attr
	cmd.Cancel = func() error {
		// Check os.Process's synchronized foreground completion state before
		// using the private group. Already-reaped commands need no signal.
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			return err
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}
