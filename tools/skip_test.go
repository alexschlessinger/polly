package tools

import (
	"os"
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

// skipUnlessSandboxTests marks tests that run commands under a real process
// sandbox: opt-in via POLLYTOOL_REQUIRE_SANDBOX_TESTS=1, macOS and Linux only.
func skipUnlessSandboxTests(t *testing.T) {
	t.Helper()
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" {
		t.Skip("opt-in process sandbox")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process sandbox needs macOS or Linux")
	}
}
