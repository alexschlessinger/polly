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

// FiniteProcessGroup reports the private process group a finite command ran
// in, for reaping survivors after Wait. The leader is already reaped, so only
// the group ID identifies them. Linux's built-in sandbox has none to report:
// its PID namespace dies with bwrap.
func FiniteProcessGroup(sb Sandbox, cmd *exec.Cmd) (int, bool) {
	if cmd == nil || cmd.Process == nil || finiteNamespaceCancellation(sb) {
		return 0, false
	}
	attr := cmd.SysProcAttr
	if attr == nil || !(attr.Setpgid || attr.Setsid) {
		return 0, false
	}
	return cmd.Process.Pid, true
}

// ProcessGroupAlive reports whether any process remains in the group.
func ProcessGroupAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// KillProcessGroup sends SIGKILL to every process in the group; a group that
// is already gone is not an error.
func KillProcessGroup(pgid int) error {
	if pgid <= 0 {
		return nil
	}
	err := syscall.Kill(-pgid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
