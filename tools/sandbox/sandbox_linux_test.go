//go:build linux

package sandbox

import (
	"context"
	"net"
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

func skipOrFailBwrapUnavailable(t *testing.T, err error, output []byte) {
	t.Helper()
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") == "1" {
		t.Fatalf("bwrap execution is required in this environment: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	t.Skipf("bwrap execution unavailable: %v (%s)", err, strings.TrimSpace(string(output)))
}

// When a deny entry's parent is a regular file (e.g. ~/.docker is a file, so
// ~/.docker/config.json can't exist), EvalSymlinks returns ENOTDIR, not ENOENT.
// existingDeniedPaths must still drop it — keeping it would make bwrap try to
// create a mountpoint under a non-directory and abort every command.
func TestLinuxExistingDeniedPathsDropsENOTDIR(t *testing.T) {
	dir := t.TempDir()
	fileParent := filepath.Join(dir, "dockerfile-not-dir")
	if err := os.WriteFile(fileParent, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	underFile := filepath.Join(fileParent, "config.json") // parent is a file → ENOTDIR

	kept := existingDeniedPaths([]DeniedPath{
		{Path: underFile, Kind: DeniedPathFile},
	})
	if len(kept) != 0 {
		t.Fatalf("expected the ENOTDIR path to be dropped, got %v", kept)
	}
}

func TestLinuxExistingDeniedPaths(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir()) // resolve once so comparisons are stable
	if err != nil {
		t.Fatalf("EvalSymlinks(tempdir) error = %v", err)
	}

	present := filepath.Join(dir, ".ssh")
	if err := os.MkdirAll(present, 0700); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", present, err)
	}

	missing := filepath.Join(dir, ".gnupg") // deliberately not created

	// A symlinked deny-path (like WSL's ~/.aws) should resolve to its target.
	target := filepath.Join(dir, "real-aws")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", target, err)
	}
	link := filepath.Join(dir, ".aws")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink error = %v", err)
	}

	// A dangling symlink reads as ENOENT through the link: nothing to mask.
	dangling := filepath.Join(dir, ".azure")
	if err := os.Symlink(filepath.Join(dir, "gone"), dangling); err != nil {
		t.Fatalf("Symlink error = %v", err)
	}

	got := existingDeniedPaths([]DeniedPath{
		{Path: present, Kind: DeniedPathDir},
		{Path: missing, Kind: DeniedPathDir},
		{Path: link, Kind: DeniedPathDir},
		{Path: dangling, Kind: DeniedPathDir},
	})

	want := map[string]bool{present: true, target: true}
	if len(got) != len(want) {
		t.Fatalf("existingDeniedPaths = %v, want paths %v", got, want)
	}
	for _, p := range got {
		if !want[p.Path] {
			t.Fatalf("unexpected path %q in result %v (missing should be dropped, symlink resolved to %q)", p.Path, got, target)
		}
	}
}

// An EvalSymlinks failure that is NOT "does not exist" (here: an unreadable
// parent directory) must keep the original path — dropping the mask on an
// unexpected error would silently leave a real path readable.
func TestLinuxExistingDeniedPathsFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions are not enforced")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	secret := filepath.Join(locked, ".secret")
	if err := os.MkdirAll(secret, 0700); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", secret, err)
	}
	if err := os.Chmod(locked, 0000); err != nil {
		t.Fatalf("Chmod error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0700) })

	got := existingDeniedPaths([]DeniedPath{{Path: secret, Kind: DeniedPathDir}})
	if len(got) != 1 || got[0].Path != secret {
		t.Fatalf("existingDeniedPaths = %v, want the unresolvable path %q kept (fail closed)", got, secret)
	}
}

// The deny list must be re-evaluated per Wrap: a credential dir created after
// the sandbox was constructed still has to be masked.
func TestLinuxWrapReevaluatesDeniedPaths(t *testing.T) {
	skipIfNoBwrap(t)

	home := t.TempDir()
	t.Setenv("HOME", home)

	sb, err := New(Config{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	sshDir := filepath.Join(home, ".ssh")

	cmd := exec.Command("bash", "-c", "true")
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if strings.Contains(strings.Join(cmd.Args, " "), "--tmpfs "+sshDir) {
		t.Fatalf("mask for %q present before the directory exists", sshDir)
	}

	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", sshDir, err)
	}

	cmd = exec.Command("bash", "-c", "true")
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if !strings.Contains(strings.Join(cmd.Args, " "), "--tmpfs "+sshDir) {
		t.Fatalf("mask for %q missing after the directory was created mid-session:\n%s", sshDir, strings.Join(cmd.Args, " "))
	}
}

// A missing writable path must NOT fail construction (that would brick session
// restore over one stale path). It is skipped per-command instead, so bwrap
// never sees a missing bind source and aborts.
func TestLinuxMissingWritablePathIsSkippedNotRejected(t *testing.T) {
	skipIfNoBwrap(t)

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	sb, err := New(Config{WritablePaths: []string{missing}})
	if err != nil {
		t.Fatalf("New() should tolerate a missing writable path, got: %v", err)
	}

	cmd := exec.Command("true")
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if strings.Contains(strings.Join(cmd.Args, " "), missing) {
		t.Fatalf("missing writable path should be skipped, but a bind for it was emitted:\n%s", strings.Join(cmd.Args, " "))
	}

	// A path that exists is still bound.
	present := t.TempDir()
	sb2, err := New(Config{WritablePaths: []string{present}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	cmd2 := exec.Command("true")
	if err := sb2.Wrap(cmd2); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if !strings.Contains(strings.Join(cmd2.Args, " "), "--bind "+present+" "+present) {
		t.Fatalf("existing writable path should be bound:\n%s", strings.Join(cmd2.Args, " "))
	}
}

// Reproduces the original bug: when a credential dir (e.g. ~/.gnupg) is absent,
// bwrap used to abort trying to mkdir a mountpoint for it under the read-only
// root bind ("Can't mkdir ...: Read-only file system"), killing every command.
func TestLinuxSandboxRunsWhenCredentialPathsMissing(t *testing.T) {
	skipIfNoBwrap(t)

	home := t.TempDir() // empty: none of the denied credential paths exist
	t.Setenv("HOME", home)

	sb, err := New(Config{WritablePaths: []string{home}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "bash", "-c", "echo ok")
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "Operation not permitted") || strings.Contains(string(out), "Permission denied") {
			t.Skipf("bwrap execution unavailable in this environment: %v (%s)", err, strings.TrimSpace(string(out)))
		}
		t.Fatalf("sandbox failed when credential paths are missing: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	if strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("sandboxed output = %q, want %q", strings.TrimSpace(string(out)), "ok")
	}
}

func TestLinuxBuildBwrapArgs(t *testing.T) {
	// The writable path must exist: buildBwrapArgs skips missing bind sources
	// (bwrap would abort on them). Denied paths need no existence check here —
	// existingDeniedPaths has already filtered them by the time they arrive.
	project := t.TempDir()
	args := buildBwrapArgs(Config{
		WritablePaths: []string{project},
	}, []DeniedPath{
		{Path: "/home/user/.ssh", Kind: DeniedPathDir},
		{Path: "/home/user/.npmrc", Kind: DeniedPathFile},
	})

	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "--ro-bind / /") {
		t.Fatal("missing --ro-bind / /")
	}
	if !strings.Contains(joined, "--bind "+project+" "+project) {
		t.Fatalf("missing project writable bind:\n%s", joined)
	}
	if !strings.Contains(joined, "--tmpfs /tmp") {
		t.Fatal("missing private /tmp tmpfs")
	}
	if strings.Contains(joined, "--bind /tmp /tmp") {
		t.Fatal("host /tmp must not be bind-mounted")
	}
	if !strings.Contains(joined, "--tmpfs /run") || !strings.Contains(joined, "--remount-ro /run") {
		t.Fatal("missing private read-only /run")
	}
	if !strings.Contains(joined, "--tmpfs /home/user/.ssh") {
		t.Fatal("missing tmpfs overlay for denied directory")
	}
	if !strings.Contains(joined, "--ro-bind /dev/null /home/user/.npmrc") {
		t.Fatal("missing /dev/null bind for denied file")
	}
	if strings.Contains(joined, "--tmpfs /home/user/.npmrc") {
		t.Fatal("denied file should not be mounted with tmpfs")
	}
	if !strings.Contains(joined, "--unshare-net") {
		t.Fatal("missing --unshare-net (network should be denied by default)")
	}
	if !strings.Contains(joined, "--unshare-pid") {
		t.Fatal("missing --unshare-pid (host /proc leaks same-UID process environs)")
	}
	if !strings.Contains(joined, "--unshare-ipc") {
		t.Fatal("missing --unshare-ipc")
	}
	if !strings.Contains(joined, "--new-session") {
		t.Fatal("missing --new-session (controlling tty allows TIOCSTI keystroke injection)")
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
	home := t.TempDir()
	t.Setenv("HOME", home)
	expanded := filepath.Join(home, "output")
	// Must exist, or the skip-missing-bind-source logic drops it.
	if err := os.MkdirAll(expanded, 0700); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", expanded, err)
	}
	args := buildBwrapArgs(Config{
		WritablePaths: []string{"~/output"},
	}, nil)

	joined := strings.Join(args, " ")
	expected := "--bind " + expanded + " " + expanded
	if !strings.Contains(joined, expected) {
		t.Fatalf("expected tilde-expanded writable bind %q in:\n%s", expected, joined)
	}
}

func TestLinuxBuildBwrapArgsDenyWritePaths(t *testing.T) {
	work := t.TempDir()
	hooks := filepath.Join(work, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0755); err != nil {
		t.Fatal(err)
	}

	args := buildBwrapArgs(Config{
		WritablePaths:  []string{work},
		DenyWritePaths: []string{hooks, filepath.Join(work, ".git", "missing")},
	}, nil)
	joined := strings.Join(args, " ")

	roBind := "--ro-bind " + hooks + " " + hooks
	roIdx := strings.Index(joined, roBind)
	if roIdx < 0 {
		t.Fatalf("missing read-only shadow bind for denyWritePaths entry:\n%s", joined)
	}
	// The shadow must be mounted after the writable parent bind so it wins.
	writableIdx := strings.Index(joined, "--bind "+work+" "+work)
	if writableIdx < 0 || roIdx < writableIdx {
		t.Fatalf("denyWritePaths ro-bind must come after the writable bind:\n%s", joined)
	}
	// The missing entry is skipped — bwrap aborts on absent bind sources.
	if strings.Contains(joined, filepath.Join(work, ".git", "missing")) {
		t.Fatalf("missing denyWritePaths entry should be skipped:\n%s", joined)
	}
}

// End-to-end: with the workspace writable, a sandboxed process must still be
// unable to plant a git hook, but can read the hooks dir and write elsewhere
// in the workspace.
func TestLinuxSandboxDenyWritePathBlocksHookPlanting(t *testing.T) {
	skipIfNoBwrap(t)

	work := t.TempDir()
	hooks := filepath.Join(work, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0755); err != nil {
		t.Fatal(err)
	}

	sb, err := New(Config{WritablePaths: []string{work}, DenyWritePaths: []string{hooks}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	victim := filepath.Join(hooks, "pre-commit")
	cmd := exec.CommandContext(context.Background(), "bash", "-c", "echo pwned > "+victim)
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if err := cmd.Run(); err == nil {
		os.Remove(victim)
		t.Fatal("expected hook write to be blocked despite the writable workspace")
	}
	if _, err := os.Stat(victim); err == nil {
		os.Remove(victim)
		t.Fatal("hook file was created despite the sandbox")
	}

	cmd = exec.CommandContext(context.Background(), "bash", "-c",
		"ls "+hooks+" >/dev/null && echo ok > "+filepath.Join(work, "note.txt"))
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("workspace write/read alongside denyWritePaths failed: %v", err)
	}
}

func TestLinuxBuildBwrapArgsReadPaths(t *testing.T) {
	args := buildBwrapArgs(Config{
		ReadPaths: []string{"/home/user/.ssh"},
	}, []DeniedPath{
		{Path: "/home/user/.ssh", Kind: DeniedPathDir},
		{Path: "/home/user/.aws", Kind: DeniedPathDir},
		{Path: "/home/user/.npmrc", Kind: DeniedPathFile},
	})

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
	if !strings.Contains(joined, "--ro-bind /dev/null /home/user/.npmrc") {
		t.Fatal("denied file NOT in ReadPaths should still have /dev/null bind")
	}
}

func TestLinuxBuildBwrapArgsRestoresChildReadPath(t *testing.T) {
	deniedDir := t.TempDir()
	readable := filepath.Join(deniedDir, "config")
	if err := os.WriteFile(readable, []byte("allowed"), 0600); err != nil {
		t.Fatal(err)
	}

	args := buildBwrapArgs(Config{ReadPaths: []string{readable}, DenyWrite: true}, []DeniedPath{{
		Path: deniedDir,
		Kind: DeniedPathDir,
	}})
	joined := strings.Join(args, " ")
	stage := "/run/.pollytool-readpaths/0"
	if !strings.Contains(joined, "--ro-bind "+readable+" "+stage) {
		t.Fatalf("child exemption was not staged before its parent mask:\n%s", joined)
	}
	if !strings.Contains(joined, "--tmpfs "+deniedDir) {
		t.Fatalf("denied parent was not masked:\n%s", joined)
	}
	if !strings.Contains(joined, "--ro-bind "+stage+" "+readable) {
		t.Fatalf("child exemption was not restored over its parent mask:\n%s", joined)
	}
	if strings.Index(joined, "--ro-bind "+readable+" "+stage) > strings.Index(joined, "--tmpfs "+deniedDir) {
		t.Fatalf("child exemption source was staged after it became hidden:\n%s", joined)
	}
	if strings.Index(joined, "--ro-bind "+stage+" "+readable) < strings.Index(joined, "--tmpfs "+deniedDir) {
		t.Fatalf("child exemption was restored before the parent mask:\n%s", joined)
	}
	if strings.Index(joined, "--remount-ro "+deniedDir) < strings.Index(joined, "--ro-bind "+stage+" "+readable) {
		t.Fatalf("denied parent became read-only before the exemption was restored:\n%s", joined)
	}
}

func TestLinuxSandboxAllowsChildReadPathExemption(t *testing.T) {
	skipIfNoBwrap(t)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot resolve home: %v", err)
	}
	deniedDir, err := os.MkdirTemp(home, ".polly-sandbox-readpath-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(deniedDir) })
	readable := filepath.Join(deniedDir, "allowed")
	hidden := filepath.Join(deniedDir, "hidden")
	if err := os.WriteFile(readable, []byte("allowed-value"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hidden, []byte("hidden-value"), 0600); err != nil {
		t.Fatal(err)
	}

	sb, err := New(Config{DenyPaths: []string{deniedDir}, ReadPaths: []string{readable}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	cmd := exec.CommandContext(context.Background(), "bash", "-c", "cat \"$1\"; test ! -e \"$2\"", "bash", readable, hidden)
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		skipOrFailBwrapUnavailable(t, err, out)
	}
	if strings.TrimSpace(string(out)) != "allowed-value" {
		t.Fatalf("child read exemption output = %q, want allowed-value", strings.TrimSpace(string(out)))
	}
}

// A readPaths exemption that names a symlink (e.g. WSL's ~/.aws -> /mnt/c/...)
// must still exempt the resolved target: denied paths arrive already
// symlink-resolved, so an unresolved exemption would never match and the path
// would stay masked despite the user opting to read it.
func TestLinuxReadPathsResolvesSymlinkExemption(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(tempdir) error = %v", err)
	}
	target := filepath.Join(dir, "real-aws")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", target, err)
	}
	link := filepath.Join(dir, ".aws")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink error = %v", err)
	}

	// The denied path is the resolved target (what existingDeniedPaths yields);
	// the readPaths exemption names the link.
	args := buildBwrapArgs(Config{
		ReadPaths: []string{link},
	}, []DeniedPath{
		{Path: target, Kind: DeniedPathDir},
	})

	if joined := strings.Join(args, " "); strings.Contains(joined, "--tmpfs "+target) {
		t.Fatalf("readPaths exemption naming a symlink should exempt its resolved target, but it was masked:\n%s", joined)
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

func TestLinuxBuildBwrapArgsDenyDNS(t *testing.T) {
	args := buildBwrapArgs(Config{AllowNetwork: true, DenyDNS: true}, nil)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--unshare-net") {
		t.Fatal("should not have --unshare-net when AllowNetwork is true")
	}
	if !strings.Contains(joined, "--ro-bind /dev/null ") {
		t.Fatalf("missing resolv.conf masking for DenyDNS:\n%s", joined)
	}
}

func TestLinuxBuildBwrapArgsDenyDNSWithoutNetwork(t *testing.T) {
	args := buildBwrapArgs(Config{AllowNetwork: false, DenyDNS: true}, nil)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--unshare-net") {
		t.Fatal("should have --unshare-net when AllowNetwork is false")
	}
	if strings.Contains(joined, "/etc/resolv.conf") {
		t.Fatal("should not mask resolv.conf when network is fully denied")
	}
}

func TestLinuxBuildBwrapArgsDenyWrite(t *testing.T) {
	args := buildBwrapArgs(Config{DenyWrite: true}, nil)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--bind /tmp /tmp") {
		t.Fatal("should not have writable /tmp bind when DenyWrite is true")
	}
	if !strings.Contains(joined, "--tmpfs /tmp --tmpfs /run") || !strings.Contains(joined, "--remount-ro /tmp") {
		t.Fatalf("private temp must be remounted read-only under DenyWrite:\n%s", joined)
	}
}

func TestLinuxBuildBwrapArgsAllowsNetwork(t *testing.T) {
	args := buildBwrapArgs(Config{AllowNetwork: true}, nil)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--unshare-net") {
		t.Fatal("should not have --unshare-net when AllowNetwork is true")
	}
}

func TestLinuxWrapCmd(t *testing.T) {
	skipIfNoBwrap(t)

	sb, err := New(Config{WritablePaths: []string{"/tmp"}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	cmd := exec.Command("bash", "-c", "echo hello")
	origPath, err := resolvedExecutablePath(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.Wrap(cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}

	// Should have bwrap as the binary
	if !strings.HasSuffix(cmd.Path, "bwrap") {
		t.Fatalf("cmd.Path = %q, want bwrap", cmd.Path)
	}

	defer func() {
		for _, f := range cmd.ExtraFiles {
			_ = f.Close()
		}
	}()

	// The already-resolved executable should appear after --, and the seccomp
	// program must be passed as an extra file descriptor.
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "--seccomp 3") {
		t.Fatalf("cmd.Args missing seccomp fd:\n%s", joined)
	}
	if !strings.Contains(joined, "-- "+origPath+" -c echo hello") {
		t.Fatalf("cmd.Args missing original command after --:\n%s", joined)
	}
	if len(cmd.ExtraFiles) != 1 {
		t.Fatalf("cmd.ExtraFiles = %d, want seccomp filter fd", len(cmd.ExtraFiles))
	}
}

func TestLinuxBuildBwrapArgsReexposesTemporaryCommandReadOnly(t *testing.T) {
	dir := t.TempDir()
	commandPath := filepath.Join(dir, "tool")
	if err := os.WriteFile(commandPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(buildBwrapArgs(Config{}, nil, commandPath), " ")
	want := "--ro-bind " + dir + " " + dir
	if !strings.Contains(joined, want) {
		t.Fatalf("temporary command directory was not re-exposed read-only; want %q in:\n%s", want, joined)
	}
	if strings.Contains(joined, "--bind "+dir+" "+dir) {
		t.Fatalf("temporary command directory became writable:\n%s", joined)
	}
}

func TestLinuxSandboxUsesPrivateTmp(t *testing.T) {
	skipIfNoBwrap(t)

	hostDir := t.TempDir()
	hostMarker := filepath.Join(hostDir, "host-only")
	if err := os.WriteFile(hostMarker, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	createdOnHost := filepath.Join("/tmp", filepath.Base(hostDir)+"-sandbox-created")
	defer os.Remove(createdOnHost)

	sb, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", "test ! -e "+hostMarker+" && echo private > "+createdOnHost)
	if err := sb.Wrap(cmd); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	for _, f := range cmd.ExtraFiles {
		_ = f.Close()
	}
	if err != nil {
		if strings.Contains(string(out), "Operation not permitted") || strings.Contains(string(out), "Permission denied") {
			skipOrFailBwrapUnavailable(t, err, out)
		}
		t.Fatalf("sandboxed private-temp probe failed: %v (%s)", err, out)
	}
	if _, err := os.Stat(createdOnHost); !os.IsNotExist(err) {
		t.Fatalf("sandbox temp write escaped to host: %v", err)
	}
}

func TestLinuxSandboxBlocksUnixSocketsEvenWithNetwork(t *testing.T) {
	skipIfNoBwrap(t)

	dir := t.TempDir()
	socketPath := filepath.Join(dir, "host.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	sb, err := New(Config{AllowNetwork: true, WritablePaths: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxUnixSocketClientHelper$")
	cmd.Env = append(os.Environ(), "POLLY_TEST_UNIX_SOCKET="+socketPath)
	if err := sb.Wrap(cmd); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	for _, f := range cmd.ExtraFiles {
		_ = f.Close()
	}
	if err != nil {
		if strings.Contains(string(out), "Operation not permitted") || strings.Contains(string(out), "Permission denied") {
			skipOrFailBwrapUnavailable(t, err, out)
		}
		t.Fatalf("Unix-socket escape probe failed: %v (%s)", err, out)
	}
}

func TestLinuxUnixSocketClientHelper(t *testing.T) {
	path := os.Getenv("POLLY_TEST_UNIX_SOCKET")
	if path == "" {
		return
	}
	conn, err := net.Dial("unix", path)
	if err == nil {
		_ = conn.Close()
		t.Fatal("connected to host Unix socket from sandbox")
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
			shellCmd = "ls -A " + dp.Path // -A: real entries only, excludes . and ..
		case DeniedPathFile:
			shellCmd = "cat " + dp.Path
		}

		t.Run(dp.Path, func(t *testing.T) {
			cmd := exec.CommandContext(context.Background(), "bash", "-c", shellCmd)
			if err := sb.Wrap(cmd); err != nil {
				t.Fatalf("Wrap() error = %v", err)
			}
			// The mask replaces the path with an empty tmpfs (dir) or empty
			// placeholder (file). The command may exit 0 with empty output OR
			// error (permission) — both hide the secret. What must never happen
			// is real credential content coming back.
			out, _ := cmd.CombinedOutput()
			if strings.TrimSpace(string(out)) != "" {
				t.Fatalf("expected %s to be masked (no contents), but read: %q", dp.Path, string(out))
			}
		})
	}
	if tested == 0 {
		t.Skip("no denied credential paths exist on this machine")
	}
	t.Logf("tested %d/%d credential paths that exist on this machine", tested, len(denied))
}
