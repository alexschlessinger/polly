//go:build linux

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func skipIfNoBwrap(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not available")
	}
}

func TestLinuxBuildBwrapArgs(t *testing.T) {
	args := buildBwrapArgs(Config{
		WritablePaths: []string{"/home/user/project"},
	}, []DeniedPath{
		{Path: "/home/user/.ssh", Kind: DeniedPathDir},
		{Path: "/home/user/.npmrc", Kind: DeniedPathFile},
	}, "/tmp/pollytool-sandbox-empty")

	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "--ro-bind / /") {
		t.Fatal("missing --ro-bind / /")
	}
	if !strings.Contains(joined, "--bind /home/user/project /home/user/project") {
		t.Fatalf("missing project writable bind:\n%s", joined)
	}
	if !strings.Contains(joined, "--bind /tmp /tmp") {
		t.Fatal("missing /tmp writable bind")
	}
	if !strings.Contains(joined, "--tmpfs /home/user/.ssh") {
		t.Fatal("missing tmpfs overlay for denied directory")
	}
	if !strings.Contains(joined, "--ro-bind /tmp/pollytool-sandbox-empty /home/user/.npmrc") {
		t.Fatal("missing placeholder bind for denied file")
	}
	if strings.Contains(joined, "--tmpfs /home/user/.npmrc") {
		t.Fatal("denied file should not be mounted with tmpfs")
	}
	if !strings.Contains(joined, "--unshare-net") {
		t.Fatal("missing --unshare-net (network should be denied by default)")
	}
	if !strings.Contains(joined, "--die-with-parent") {
		t.Fatal("missing --die-with-parent")
	}
	if !strings.Contains(joined, "--dev /dev") {
		t.Fatal("missing --dev /dev")
	}
	if !strings.Contains(joined, "--proc /proc") {
		t.Fatal("missing --proc /proc")
	}
}

func TestLinuxBuildBwrapArgsWritePathsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot get home dir")
	}
	expanded := filepath.Join(home, "output")
	args := buildBwrapArgs(Config{
		WritablePaths: []string{"~/output"},
	}, nil, "/tmp/pollytool-sandbox-empty")

	joined := strings.Join(args, " ")
	expected := "--bind " + expanded + " " + expanded
	if !strings.Contains(joined, expected) {
		t.Fatalf("expected tilde-expanded writable bind %q in:\n%s", expected, joined)
	}
}

func TestLinuxBuildBwrapArgsReadPaths(t *testing.T) {
	args := buildBwrapArgs(Config{
		ReadPaths: []string{"/home/user/.ssh"},
	}, []DeniedPath{
		{Path: "/home/user/.ssh", Kind: DeniedPathDir},
		{Path: "/home/user/.aws", Kind: DeniedPathDir},
		{Path: "/home/user/.npmrc", Kind: DeniedPathFile},
	}, "/tmp/pollytool-sandbox-empty")

	joined := strings.Join(args, " ")

	// .ssh should NOT be overlaid because it's in ReadPaths
	if strings.Contains(joined, "--tmpfs /home/user/.ssh") {
		t.Fatal("denied path in ReadPaths should be skipped, but got --tmpfs for .ssh")
	}
	// .aws should still be overlaid
	if !strings.Contains(joined, "--tmpfs /home/user/.aws") {
		t.Fatal("denied path NOT in ReadPaths should still have --tmpfs")
	}
	// .npmrc should still be overlaid
	if !strings.Contains(joined, "--ro-bind /tmp/pollytool-sandbox-empty /home/user/.npmrc") {
		t.Fatal("denied file NOT in ReadPaths should still have placeholder bind")
	}
}

func TestLinuxSandboxEnvFiltering(t *testing.T) {
	skipIfNoBwrap(t)

	sb, err := New(Config{AllowEnv: []string{"POLLY_KEEP"}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	cmd := exec.Command("bash", "-c", "echo keep=$POLLY_KEEP drop=$POLLY_DROP")
	cmd.Env = []string{"POLLY_KEEP=yes", "POLLY_DROP=no"}
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}

	// Verify env was filtered before bwrap args are applied
	foundKeep := false
	foundDrop := false
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, "POLLY_KEEP=") {
			foundKeep = true
		}
		if strings.HasPrefix(e, "POLLY_DROP=") {
			foundDrop = true
		}
	}
	if !foundKeep {
		t.Fatal("expected POLLY_KEEP to remain in cmd.Env")
	}
	if foundDrop {
		t.Fatal("expected POLLY_DROP to be filtered from cmd.Env")
	}
}

func TestLinuxSandboxStripsPollytoolEnvByDefault(t *testing.T) {
	skipIfNoBwrap(t)

	sb, err := New(Config{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	cmd := exec.Command("bash", "-c", "echo test")
	cmd.Env = []string{"POLLYTOOL_OPENAIKEY=secret", "OTHER_VAR=kept"}
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}

	foundPollytool := false
	foundOther := false
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, "POLLYTOOL_") {
			foundPollytool = true
		}
		if strings.HasPrefix(e, "OTHER_VAR=") {
			foundOther = true
		}
	}
	if foundPollytool {
		t.Fatal("expected POLLYTOOL_* vars to be stripped by default")
	}
	if !foundOther {
		t.Fatal("expected non-POLLYTOOL vars to be kept")
	}
}

func TestLinuxBuildBwrapArgsDenyWrite(t *testing.T) {
	args := buildBwrapArgs(Config{DenyWrite: true}, nil, "/tmp/pollytool-sandbox-empty")
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--bind /tmp /tmp") {
		t.Fatal("should not have writable /tmp bind when DenyWrite is true")
	}
}

func TestLinuxBuildBwrapArgsAllowsNetwork(t *testing.T) {
	args := buildBwrapArgs(Config{AllowNetwork: true}, nil, "/tmp/pollytool-sandbox-empty")
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--unshare-net") {
		t.Fatal("should not have --unshare-net when AllowNetwork is true")
	}
}

func TestEnsurePlaceholderFile(t *testing.T) {
	path, err := ensurePlaceholderFile()
	if err != nil {
		t.Fatalf("ensurePlaceholderFile() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	if info.Size() != 0 {
		t.Fatalf("placeholder file size = %d, want 0", info.Size())
	}
	if !strings.HasPrefix(path, os.TempDir()) {
		t.Fatalf("placeholder path = %q, want prefix %q", path, os.TempDir())
	}
}

func TestLinuxWrapCmd(t *testing.T) {
	skipIfNoBwrap(t)

	sb, err := New(Config{WritablePaths: []string{"/tmp"}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	cmd := exec.Command("bash", "-c", "echo hello")
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}

	// Should have bwrap as the binary
	if !strings.HasSuffix(cmd.Path, "bwrap") {
		t.Fatalf("cmd.Path = %q, want bwrap", cmd.Path)
	}

	// Original args should appear after --
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "-- bash -c echo hello") {
		t.Fatalf("cmd.Args missing original command after --:\n%s", joined)
	}
}

func TestLinuxGracefulFallback(t *testing.T) {
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", "/nonexistent")
	defer os.Setenv("PATH", origPath)

	sb, err := New(Config{})
	if err == nil {
		t.Fatal("expected New() to return an error when bwrap is not in PATH")
	}
	if sb != nil {
		t.Fatal("expected New() to return nil sandbox when bwrap is not in PATH")
	}
}

func TestLinuxSandboxHandlesDeniedFiles(t *testing.T) {
	skipIfNoBwrap(t)

	home := t.TempDir()
	t.Setenv("HOME", home)

	for _, denied := range ExpandHome(DeniedPaths) {
		switch denied.Kind {
		case DeniedPathFile:
			if err := os.MkdirAll(filepath.Dir(denied.Path), 0755); err != nil {
				t.Fatalf("MkdirAll(%q) error = %v", filepath.Dir(denied.Path), err)
			}
			if err := os.WriteFile(denied.Path, []byte("secret"), 0600); err != nil {
				t.Fatalf("WriteFile(%q) error = %v", denied.Path, err)
			}
		case DeniedPathDir:
			if err := os.MkdirAll(denied.Path, 0700); err != nil {
				t.Fatalf("MkdirAll(%q) error = %v", denied.Path, err)
			}
		}
	}

	sb, err := New(Config{WritablePaths: []string{home}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "bash", "-c", "test -f "+filepath.Join(home, ".npmrc"))
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(output), "Operation not permitted") || strings.Contains(string(output), "Permission denied") {
			t.Skipf("bwrap execution unavailable in this environment: %v (%s)", err, strings.TrimSpace(string(output)))
		}
		t.Fatalf("sandboxed command failed with denied file present: %v (%s)", err, strings.TrimSpace(string(output)))
	}
}

func TestLinuxSandboxBlocksAllExistingCredentialPaths(t *testing.T) {
	skipIfNoBwrap(t)

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
