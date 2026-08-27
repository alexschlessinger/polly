package sandbox

import (
	"runtime"
	"testing"
)

// skipIfWindows marks tests that exercise POSIX-only sandbox behavior, such as
// the trusted system Git route and guardrail path auditing.
func skipIfWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("sandboxing is unsupported on windows")
	}
}
