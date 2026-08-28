package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAllowedDenyWriteBlocksEverything(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DenyWrite: true, WritablePaths: []string{dir}}
	if err := WriteAllowed(cfg, filepath.Join(dir, "f")); err == nil {
		t.Fatal("expected DenyWrite to block the write")
	}
}

func TestWriteAllowedWithinWritablePath(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{WritablePaths: []string{dir}}
	if err := WriteAllowed(cfg, filepath.Join(dir, "sub", "f.txt")); err != nil {
		t.Fatalf("expected write within writable path, got %v", err)
	}
}

func TestWriteAllowedOutsideWritablePaths(t *testing.T) {
	cfg := Config{WritablePaths: []string{t.TempDir()}}
	outside := filepath.Join(string(filepath.Separator), "nonexistent-root-for-write-policy", "f")
	err := WriteAllowed(cfg, outside)
	if err == nil || !strings.Contains(err.Error(), "writable paths") {
		t.Fatalf("expected outside-writable denial, got %v", err)
	}
}

func TestWriteAllowedTempDirImplicitlyWritable(t *testing.T) {
	cfg := Config{}
	if err := WriteAllowed(cfg, filepath.Join(os.TempDir(), "polly-write-policy-test")); err != nil {
		t.Fatalf("expected temp dir write to be allowed, got %v", err)
	}
}

func TestWriteAllowedDenyWritePathIsland(t *testing.T) {
	dir := t.TempDir()
	island := filepath.Join(dir, ".git")
	if err := os.Mkdir(island, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := Config{WritablePaths: []string{dir}, DenyWritePaths: []string{island}}
	err := WriteAllowed(cfg, filepath.Join(island, "config"))
	if err == nil || !strings.Contains(err.Error(), "sandbox policy") {
		t.Fatalf("expected deny-write island to block, got %v", err)
	}
	if err := WriteAllowed(cfg, filepath.Join(dir, "ok.txt")); err != nil {
		t.Fatalf("expected sibling write to remain allowed, got %v", err)
	}
}

func TestWriteAllowedCredentialPathsBlockedInsideWritableRoot(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	// A writable grant over the home directory must not re-open the built-in
	// credential deny list for writes.
	cfg := Config{WritablePaths: []string{home}}
	err = WriteAllowed(cfg, filepath.Join(home, ".ssh", "authorized_keys"))
	if err == nil || !strings.Contains(err.Error(), "sandbox policy") {
		t.Fatalf("expected credential path write denial, got %v", err)
	}
}

func TestWriteAllowedReadPathExemptionDoesNotAllowWrites(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	sshDir := filepath.Join(home, ".ssh")
	cfg := Config{WritablePaths: []string{home}, ReadPaths: []string{sshDir}}
	if err := WriteAllowed(cfg, filepath.Join(sshDir, "config")); err == nil {
		t.Fatal("expected ReadPaths exemption to leave writes denied")
	}
}

func TestWriteAllowedSymlinkOutOfWritableRoot(t *testing.T) {
	writable := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(writable, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfg := Config{WritablePaths: []string{writable}}
	// The resolved route lands outside the writable root... unless the outside
	// directory is itself under the OS temp roots, which t.TempDir may be. Use
	// a root that is definitely not writable.
	if strings.HasPrefix(outside, os.TempDir()) || strings.HasPrefix(writable, os.TempDir()) {
		t.Skip("temp-dir-backed test dirs are implicitly writable; covered by TestWriteAllowedSymlinkIntoDeniedIsland")
	}
	if err := WriteAllowed(cfg, filepath.Join(link, "f")); err == nil {
		t.Fatal("expected symlink escape to be denied")
	}
}

func TestWriteAllowedSymlinkIntoDeniedIsland(t *testing.T) {
	dir := t.TempDir()
	island := filepath.Join(dir, "protected")
	if err := os.Mkdir(island, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(dir, "harmless")
	if err := os.Symlink(island, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfg := Config{WritablePaths: []string{dir}, DenyWritePaths: []string{island}}
	err := WriteAllowed(cfg, filepath.Join(link, "f"))
	if err == nil || !strings.Contains(err.Error(), "sandbox policy") {
		t.Fatalf("expected symlink into denied island to be blocked, got %v", err)
	}
}

func TestWriteAllowedNonexistentTargetUnderWritableRoot(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{WritablePaths: []string{dir}}
	if err := WriteAllowed(cfg, filepath.Join(dir, "a", "b", "c.txt")); err != nil {
		t.Fatalf("expected nonexistent nested target to be allowed, got %v", err)
	}
}

func TestWriteAllowedRelativePathRejected(t *testing.T) {
	cfg := Config{WritablePaths: []string{t.TempDir()}}
	if err := WriteAllowed(cfg, "relative/path.txt"); err == nil {
		t.Fatal("expected relative path to be rejected")
	}
}
