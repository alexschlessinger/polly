//go:build !windows

package sessions

import (
	"syscall"
	"testing"
)

func setPermissiveTestUmask(t *testing.T) {
	t.Helper()
	previous := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(previous) })
}
