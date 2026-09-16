package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestRunFailsWhenAChildKeepsTheOutputOpen: a descendant that outlives git
// while holding its stdout fails the command after the drain bound instead
// of blocking the manager until the descendant exits.
func TestRunFailsWhenAChildKeepsTheOutputOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	m, root := fixture(t)
	script := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 5 &\necho held\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := gitDrainTimeout
	gitDrainTimeout = 200 * time.Millisecond
	t.Cleanup(func() { gitDrainTimeout = old })
	m.Git = script
	started := time.Now()
	out, err := m.git(context.Background(), root, nil, nil, "rev-parse")
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("err = %v, want exec.ErrWaitDelay", err)
	}
	if elapsed := time.Since(started); elapsed >= 4*time.Second {
		t.Fatalf("waited %v on the held pipe", elapsed)
	}
	if strings.TrimSpace(string(out)) != "held" {
		t.Fatalf("output collected before the bound = %q", out)
	}
}
