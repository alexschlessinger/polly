package main

import (
	"runtime"
	"testing"
)

// skipIfWindows marks tests that depend on POSIX-only sandbox presets or
// shell tool execution.
func skipIfWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell tooling or sandboxing")
	}
}
