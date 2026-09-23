//go:build !unix

package sandbox

import "os/exec"

func configureFiniteCancellation(_ Sandbox, cmd *exec.Cmd) error {
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	return nil
}

// FiniteProcessGroup reports no group: without process groups only the
// direct child of a cancelled call is killed, and nothing is reaped later.
func FiniteProcessGroup(Sandbox, *exec.Cmd) (int, bool) { return 0, false }

// ProcessGroupAlive is always false where there are no process groups.
func ProcessGroupAlive(int) bool { return false }

// KillProcessGroup does nothing where there are no process groups.
func KillProcessGroup(int) error { return nil }
