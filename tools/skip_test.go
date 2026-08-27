package tools

import (
	"runtime"
	"testing"
)

// skipIfWindows marks tests that depend on POSIX-only behavior: executing
// shell scripts by shebang, bash path handling, or sandbox presets.
func skipIfWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell tooling or sandboxing")
	}
}
