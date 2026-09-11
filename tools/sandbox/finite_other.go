//go:build !unix

package sandbox

import "os/exec"

func configureFiniteCancellation(_ Sandbox, cmd *exec.Cmd) error {
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	return nil
}
