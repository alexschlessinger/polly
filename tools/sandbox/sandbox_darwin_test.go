//go:build darwin

package sandbox

import (
	"context"
	"fmt"
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

func TestBuildProfileWritePathsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot get home dir")
	}
	profile := buildProfile(Config{
		WritablePaths: []string{"~/output"},
	})
	if !strings.Contains(profile, fmt.Sprintf(`(allow file-write* (subpath %q))`, filepath.Join(home, "output"))) {
		t.Fatalf("profile did not expand ~ in writablePaths:\n%s", profile)
	}
}

func TestBuildProfileAllowsStandardDeviceFiles(t *testing.T) {
	// Standard character devices must be writable even under DenyWrite, matching
	// bwrap's --dev /dev on Linux. `echo foo > /dev/null` is too universal an
	// idiom to break at the sandbox layer.
	wantDevices := []string{
		"/dev/null",
		"/dev/zero",
		"/dev/random",
		"/dev/urandom",
		"/dev/stdout",
		"/dev/stderr",
	}
	for _, cfg := range []Config{{}, {DenyWrite: true}} {
		profile := buildProfile(cfg)
		for _, dev := range wantDevices {
			want := fmt.Sprintf(`(allow file-write* (literal %q))`, dev)
			if !strings.Contains(profile, want) {
				t.Errorf("DenyWrite=%v: profile missing %q\nprofile:\n%s", cfg.DenyWrite, want, profile)
			}
		}
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
	// Use a dir outside temp so it's not auto-allowed by the sandbox.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot get home dir")
	}
	blockedDir := filepath.Join(home, ".polly-sandbox-test-blocked")
	if err := os.MkdirAll(blockedDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	defer os.RemoveAll(blockedDir)

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
		os.Remove(target)
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

func TestMergeAllowsNetwork(t *testing.T) {
	skipIfNoSandboxExec(t)

	base := Config{WritablePaths: []string{t.TempDir()}}
	overlay := Config{AllowNetwork: true}
	merged := base.Merge(overlay)

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

func TestMergeAddsWritablePaths(t *testing.T) {
	skipIfNoSandboxExec(t)

	baseDir := t.TempDir()
	extraDir := t.TempDir()

	base := Config{WritablePaths: []string{baseDir}}
	overlay := Config{WritablePaths: []string{extraDir}}
	merged := base.Merge(overlay)

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

	// Write to a dir outside temp should be blocked
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot get home dir")
	}
	blockedDir := filepath.Join(home, ".polly-sandbox-test-merge")
	if mkErr := os.MkdirAll(blockedDir, 0755); mkErr != nil {
		t.Fatalf("MkdirAll: %v", mkErr)
	}
	defer os.RemoveAll(blockedDir)
	cmd = exec.CommandContext(context.Background(), "bash", "-c", "echo bad > "+filepath.Join(blockedDir, "c.txt"))
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if err := cmd.Run(); err == nil {
		t.Fatal("expected write to non-allowed dir to be blocked")
	}
}

func TestBuildProfileReadPaths(t *testing.T) {
	profile := buildProfile(Config{
		ReadPaths: []string{"~/.aws"},
	})
	// Should have the deny for .aws (from DeniedPaths)
	if !strings.Contains(profile, "(deny file-read*") {
		t.Fatal("profile missing file-read deny rules")
	}
	// Should have an allow after the deny for .aws
	if !strings.Contains(profile, "(allow file-read* (subpath") {
		t.Fatalf("profile missing file-read allow for ReadPaths:\n%s", profile)
	}
	// The allow should mention .aws
	if !strings.Contains(profile, ".aws") {
		t.Fatalf("profile ReadPaths allow does not include .aws:\n%s", profile)
	}
}

func TestSandboxAllowsReadOfExemptedPath(t *testing.T) {
	skipIfNoSandboxExec(t)

	// Create a temp dir to stand in for a credential path
	dir := t.TempDir()
	secret := filepath.Join(dir, "creds")
	if err := os.WriteFile(secret, []byte("secret-value"), 0600); err != nil {
		t.Fatal(err)
	}

	sb, err := New(Config{ReadPaths: []string{dir}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "cat", secret)
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected read of exempted path to succeed: %v (%s)", err, string(out))
	}
	if strings.TrimSpace(string(out)) != "secret-value" {
		t.Fatalf("unexpected output: %s", string(out))
	}
}

func TestSandboxEnvFiltering(t *testing.T) {
	skipIfNoSandboxExec(t)

	sb, err := New(Config{AllowEnv: []string{"POLLY_TEST_KEEP"}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "bash", "-c", "echo keep=$POLLY_TEST_KEEP drop=$POLLY_TEST_DROP")
	cmd.Env = []string{"POLLY_TEST_KEEP=yes", "POLLY_TEST_DROP=no"}
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("command failed: %v (%s)", err, string(out))
	}
	result := strings.TrimSpace(string(out))
	if !strings.Contains(result, "keep=yes") {
		t.Fatalf("expected POLLY_TEST_KEEP=yes in output, got: %s", result)
	}
	if strings.Contains(result, "drop=no") {
		t.Fatalf("expected POLLY_TEST_DROP to be filtered out, got: %s", result)
	}
}

func TestSandboxStripsPollytoolEnvByDefault(t *testing.T) {
	skipIfNoSandboxExec(t)

	sb, err := New(Config{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "bash", "-c", "echo key=$POLLYTOOL_OPENAIKEY other=$OTHER_VAR")
	cmd.Env = []string{"POLLYTOOL_OPENAIKEY=secret", "OTHER_VAR=kept"}
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("command failed: %v (%s)", err, string(out))
	}
	result := strings.TrimSpace(string(out))
	if strings.Contains(result, "key=secret") {
		t.Fatalf("expected POLLYTOOL_OPENAIKEY to be stripped, got: %s", result)
	}
	if !strings.Contains(result, "other=kept") {
		t.Fatalf("expected OTHER_VAR to be kept, got: %s", result)
	}
}

func TestBuildProfileDenyDNS(t *testing.T) {
	profile := buildProfile(Config{AllowNetwork: true, DenyDNS: true})
	if strings.Contains(profile, "(deny network*)") {
		t.Fatal("profile should not have blanket network deny when AllowNetwork is true")
	}
	if !strings.Contains(profile, `(deny network-outbound (to unix-socket (path-literal "/private/var/run/mDNSResponder")))`) {
		t.Fatalf("profile missing mDNSResponder socket deny:\n%s", profile)
	}
	if !strings.Contains(profile, `(deny network-outbound (remote udp "*:53"))`) {
		t.Fatalf("profile missing UDP port 53 deny:\n%s", profile)
	}
	if !strings.Contains(profile, `(deny network-outbound (remote tcp "*:53"))`) {
		t.Fatalf("profile missing TCP port 53 deny:\n%s", profile)
	}
}

func TestBuildProfileDenyDNSWithoutNetwork(t *testing.T) {
	profile := buildProfile(Config{AllowNetwork: false, DenyDNS: true})
	if !strings.Contains(profile, "(deny network*)") {
		t.Fatal("profile should have blanket network deny when AllowNetwork is false")
	}
	if strings.Contains(profile, "mDNSResponder") || strings.Contains(profile, "remote udp") || strings.Contains(profile, "remote tcp") {
		t.Fatalf("profile should not have DNS-specific rules when network is fully denied:\n%s", profile)
	}
}

func TestSandboxDenyDNSBlocksResolution(t *testing.T) {
	skipIfNoSandboxExec(t)

	sb, err := New(Config{AllowNetwork: true, DenyDNS: true})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Hostname resolution should fail
	cmd := exec.CommandContext(context.Background(), "bash", "-c", "curl -s --max-time 3 https://example.com")
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if err := cmd.Run(); err == nil {
		t.Fatal("expected DNS-based request to fail with DenyDNS")
	}

	// Direct IP TCP connection should still work (network is allowed, only DNS is blocked).
	// Use bash /dev/tcp to avoid file-write sandbox restrictions that affect curl.
	cmd2 := exec.CommandContext(context.Background(), "bash", "-c", "exec 3<>/dev/tcp/1.1.1.1/80 && echo ok")
	if err := sb.Wrap(cmd2); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if err := cmd2.Run(); err != nil {
		t.Fatalf("expected direct IP connection to succeed with DenyDNS, got: %v", err)
	}
}

func TestBuildProfileDenyWrite(t *testing.T) {
	profile := buildProfile(Config{DenyWrite: true})
	if !strings.Contains(profile, "(deny file-write*)") {
		t.Fatal("profile missing file-write deny")
	}
	if strings.Contains(profile, "(allow file-write*") {
		t.Fatalf("profile should not have any file-write allows when DenyWrite is true:\n%s", profile)
	}
}

func TestSandboxDenyWriteBlocksTemp(t *testing.T) {
	skipIfNoSandboxExec(t)

	sb, err := New(Config{DenyWrite: true})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "bash", "-c", "echo bad > /tmp/polly-deny-test")
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if err := cmd.Run(); err == nil {
		t.Fatal("expected write to /tmp to be blocked when DenyWrite is true")
		os.Remove("/tmp/polly-deny-test")
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
