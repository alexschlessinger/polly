//go:build darwin

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func skipIfNoSandboxExec(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not available")
	}
}

func TestBuildProfileWritePaths(t *testing.T) {
	profile := buildProfile(Config{
		WritablePaths: []string{"/Users/test/project"},
	})
	if !strings.Contains(profile, "(deny file-write*)") {
		t.Fatal("profile missing file-write deny")
	}
	if !strings.Contains(profile, `(allow file-write* (subpath "/private/tmp"))`) {
		t.Fatal("profile missing /private/tmp allow")
	}
	if !strings.Contains(profile, `(allow file-write* (subpath "/Users/test/project"))`) {
		t.Fatalf("profile missing project path allow:\n%s", profile)
	}
}

func TestBuildProfileNetworkDeny(t *testing.T) {
	profile := buildProfile(Config{})
	if !strings.Contains(profile, "(deny network*)") {
		t.Fatal("profile should deny network by default")
	}
}

func TestBuildProfileNetworkAllow(t *testing.T) {
	profile := buildProfile(Config{AllowNetwork: true})
	if strings.Contains(profile, "(deny network*)") {
		t.Fatal("profile should not deny network when AllowNetwork is true")
	}
}

func TestWrapCmd(t *testing.T) {
	skipIfNoSandboxExec(t)

	sb, err := New(Config{WritablePaths: []string{"/tmp"}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	cmd := exec.Command("bash", "-c", "echo hello")
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}

	if cmd.Path != "/usr/bin/sandbox-exec" {
		t.Fatalf("cmd.Path = %q, want /usr/bin/sandbox-exec", cmd.Path)
	}
	if len(cmd.Args) < 4 {
		t.Fatalf("cmd.Args too short: %v", cmd.Args)
	}
	if cmd.Args[0] != "sandbox-exec" || cmd.Args[1] != "-p" {
		t.Fatalf("cmd.Args prefix = %v, want [sandbox-exec -p ...]", cmd.Args[:2])
	}
	// Original args should be at the end
	tail := cmd.Args[3:]
	if len(tail) != 3 || tail[0] != "bash" || tail[1] != "-c" || tail[2] != "echo hello" {
		t.Fatalf("cmd.Args tail = %v, want [bash -c echo hello]", tail)
	}
}

func TestSandboxAllowsWriteInAllowedPath(t *testing.T) {
	skipIfNoSandboxExec(t)

	dir := t.TempDir()
	sb, err := New(Config{WritablePaths: []string{dir}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	target := filepath.Join(dir, "test.txt")
	cmd := exec.CommandContext(context.Background(), "bash", "-c", "echo ok > "+target)
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("sandboxed write to allowed path failed: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("file not created: %v", err)
	}
}

func TestSandboxBlocksWriteOutsideAllowedPath(t *testing.T) {
	skipIfNoSandboxExec(t)

	allowedDir := t.TempDir()
	blockedDir := t.TempDir()

	sb, err := New(Config{WritablePaths: []string{allowedDir}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	target := filepath.Join(blockedDir, "test.txt")
	cmd := exec.CommandContext(context.Background(), "bash", "-c", "echo bad > "+target)
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if err := cmd.Run(); err == nil {
		t.Fatal("expected sandboxed write outside allowed path to fail")
	}
}

func TestSandboxBlocksNetwork(t *testing.T) {
	skipIfNoSandboxExec(t)

	sb, err := New(Config{AllowNetwork: false})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "bash", "-c", "curl -s --max-time 2 https://example.com")
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if err := cmd.Run(); err == nil {
		t.Fatal("expected network access to be blocked")
	}
}

func TestBuildProfileDeniesCredentialPaths(t *testing.T) {
	profile := buildProfile(Config{})
	// Check that at least some key credential paths are denied
	for _, suffix := range []string{".ssh", ".aws", ".gnupg"} {
		if !strings.Contains(profile, suffix) {
			t.Fatalf("profile missing deny for %s:\n%s", suffix, profile)
		}
	}
	// Verify they use file-read* deny
	if !strings.Contains(profile, "(deny file-read* (subpath") {
		t.Fatal("profile missing file-read deny rules")
	}
}

func TestSandboxBlocksCredentialRead(t *testing.T) {
	skipIfNoSandboxExec(t)

	// Only test if ~/.ssh actually exists
	home, _ := os.UserHomeDir()
	sshDir := home + "/.ssh"
	if _, err := os.Stat(sshDir); os.IsNotExist(err) {
		t.Skip("~/.ssh does not exist")
	}

	sb, err := New(Config{AllowNetwork: false})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "bash", "-c", "ls "+sshDir)
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if err := cmd.Run(); err == nil {
		t.Fatal("expected reading ~/.ssh to be blocked by sandbox")
	}
}

func TestSandboxBlocksAllExistingCredentialPaths(t *testing.T) {
	skipIfNoSandboxExec(t)

	sb, err := New(Config{AllowNetwork: false})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	denied := ExpandHome(DeniedPaths)
	tested := 0
	for _, dp := range denied {
		if _, err := os.Stat(dp.Path); os.IsNotExist(err) {
			continue
		}
		tested++

		var shellCmd string
		switch dp.Kind {
		case DeniedPathDir:
			shellCmd = "ls " + dp.Path
		case DeniedPathFile:
			shellCmd = "cat " + dp.Path
		}

		t.Run(dp.Path, func(t *testing.T) {
			cmd := exec.CommandContext(context.Background(), "bash", "-c", shellCmd)
			if err := sb.Wrap(cmd); err != nil {
				t.Fatalf("Wrap() error = %v", err)
			}
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("expected read of %s to be blocked, got output: %s", dp.Path, string(out))
			}
		})
	}
	if tested == 0 {
		t.Skip("no denied credential paths exist on this machine")
	}
	t.Logf("tested %d/%d credential paths that exist on this machine", tested, len(denied))
}

func TestSpecMergeIntoAllowsNetwork(t *testing.T) {
	skipIfNoSandboxExec(t)

	base := Config{WritablePaths: []string{t.TempDir()}}
	spec := &Spec{AllowNetwork: true}
	merged := spec.MergeInto(base)

	sb, err := New(merged)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "bash", "-c", "curl -s --max-time 3 https://example.com")
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("expected network to be allowed with spec override, got: %v", err)
	}
}

func TestSpecMergeIntoAddsWritablePaths(t *testing.T) {
	skipIfNoSandboxExec(t)

	baseDir := t.TempDir()
	extraDir := t.TempDir()

	base := Config{WritablePaths: []string{baseDir}}
	spec := &Spec{WritablePaths: []string{extraDir}}
	merged := spec.MergeInto(base)

	sb, err := New(merged)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Write to base dir should work
	cmd := exec.CommandContext(context.Background(), "bash", "-c", "echo ok > "+filepath.Join(baseDir, "a.txt"))
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("write to base dir failed: %v", err)
	}

	// Write to extra dir should also work
	cmd = exec.CommandContext(context.Background(), "bash", "-c", "echo ok > "+filepath.Join(extraDir, "b.txt"))
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("write to extra dir failed: %v", err)
	}

	// Write to a third dir should be blocked
	blockedDir := t.TempDir()
	cmd = exec.CommandContext(context.Background(), "bash", "-c", "echo bad > "+filepath.Join(blockedDir, "c.txt"))
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if err := cmd.Run(); err == nil {
		t.Fatal("expected write to non-allowed dir to be blocked")
	}
}

func TestSandboxGracefulFallback(t *testing.T) {
	// Override PATH to exclude sandbox-exec
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", "/nonexistent")
	defer os.Setenv("PATH", origPath)

	sb, err := New(Config{})
	if err == nil {
		t.Fatal("expected New() to return an error when sandbox-exec is not in PATH")
	}
	if sb != nil {
		t.Fatal("expected New() to return nil sandbox when sandbox-exec is not in PATH")
	}
}
