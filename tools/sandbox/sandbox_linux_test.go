//go:build linux

package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func skipIfNoBwrap(t *testing.T) {
	t.Helper()
	skipIfBwrapUnavailable(t, linuxBwrapPath)
}

func skipIfBwrapUnavailable(t *testing.T, path string) {
	t.Helper()
	if err := validateLinuxBwrapExecutable(path); err != nil {
		if sandboxTestsRequired() {
			t.Fatalf("bwrap is required in this environment: %v", err)
		}
		t.Skip("bwrap not available")
	}
}

func skipOrFailBwrapUnavailable(t *testing.T, err error, output []byte) {
	t.Helper()
	if sandboxTestsRequired() {
		t.Fatalf("bwrap execution is required in this environment: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	t.Skipf("bwrap execution unavailable: %v (%s)", err, strings.TrimSpace(string(output)))
}

func sandboxTestsRequired() bool {
	return os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") == "1"
}

func skipOrFailSandboxPrerequisite(t *testing.T, format string, args ...any) {
	t.Helper()
	if sandboxTestsRequired() {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

func TestLinuxRequiredModeDoesNotSkipBwrapFailures(t *testing.T) {
	if mode := os.Getenv("POLLY_TEST_BWRAP_REQUIRED_HELPER"); mode != "" {
		switch mode {
		case "missing":
			skipIfBwrapUnavailable(t, "/definitely/missing/pollytool-bwrap")
		case "execution":
			skipOrFailBwrapUnavailable(t, fmt.Errorf("operation not permitted"), []byte("blocked"))
		default:
			t.Fatalf("unknown helper mode %q", mode)
		}
		return
	}

	for _, mode := range []string{"missing", "execution"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxRequiredModeDoesNotSkipBwrapFailures$")
			cmd.Env = []string{
				"POLLYTOOL_REQUIRE_SANDBOX_TESTS=1",
				"POLLY_TEST_BWRAP_REQUIRED_HELPER=" + mode,
			}
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("required-mode %s helper exited successfully:\n%s", mode, out)
			}
			if !strings.Contains(string(out), "required in this environment") {
				t.Fatalf("required-mode %s output did not report a hard failure:\n%s", mode, out)
			}
		})
	}
}

func readLinuxEnvPayload(t *testing.T, file *os.File) []string {
	t.Helper()
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	env, err := parseLinuxTargetEnvPayload(data)
	if err != nil {
		t.Fatal(err)
	}
	return env
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

// The deny list must reserve a missing route and re-evaluate it per Wrap: a
// credential dir created after construction stays masked.
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
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if !strings.Contains(strings.Join(cmd.Args, " "), "--tmpfs "+sshDir) {
		t.Fatalf("reservation mask for missing path %q was not installed", sshDir)
	}

	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", sshDir, err)
	}

	cmd = exec.Command("bash", "-c", "true")
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if !strings.Contains(strings.Join(cmd.Args, " "), "--tmpfs "+sshDir) {
		t.Fatalf("mask for %q missing after the directory was created mid-session:\n%s", sshDir, strings.Join(cmd.Args, " "))
	}
}

func TestLinuxLegacyWrapFailsBeforeAllocatingDescriptors(t *testing.T) {
	skipIfNoBwrap(t)
	sb, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("true")
	if err := WrapCmd(sb, cmd); err != ErrManagedWrapRequired {
		t.Fatalf("WrapCmd error = %v, want managed-cleanup requirement", err)
	}
	if len(cmd.ExtraFiles) != 0 {
		t.Fatalf("legacy WrapCmd allocated descriptors before failing: %v", cmd.ExtraFiles)
	}
	cmd = exec.Command("true")
	if err := WrapCmdWithEnv(sb, cmd, map[string]string{"EXPLICIT": "value"}); err != ErrManagedWrapRequired {
		t.Fatalf("WrapCmdWithEnv error = %v, want managed-cleanup requirement", err)
	}
	if len(cmd.ExtraFiles) != 0 {
		t.Fatalf("legacy WrapCmdWithEnv allocated descriptors before failing: %v", cmd.ExtraFiles)
	}
}

// A missing writable path must NOT fail construction (that would brick session
// restore over one stale path). It is dropped permanently at construction, so
// bwrap never sees a missing bind and a later creation cannot gain authority.
func TestLinuxMissingWritablePathIsSkippedNotRejected(t *testing.T) {
	skipIfNoBwrap(t)

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	sb, err := New(Config{WritablePaths: []string{missing}})
	if err != nil {
		t.Fatalf("New() should tolerate a missing writable path, got: %v", err)
	}

	cmd := exec.Command("true")
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if strings.Contains(strings.Join(cmd.Args, " "), missing) {
		t.Fatalf("missing writable path should be dropped, but a bind for it was emitted:\n%s", strings.Join(cmd.Args, " "))
	}

	// A path that exists is still bound.
	present := t.TempDir()
	sb2, err := New(Config{WritablePaths: []string{present}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	cmd2 := exec.Command("true")
	if err := wrapCmdForTest(t, sb2, cmd2); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	foundPinnedBind := false
	for i := 0; i+2 < len(cmd2.Args); i++ {
		if cmd2.Args[i] == "--bind" && strings.HasPrefix(cmd2.Args[i+1], "/proc/self/fd/") && cmd2.Args[i+2] == present {
			foundPinnedBind = true
			break
		}
	}
	if !foundPinnedBind {
		t.Fatalf("existing writable path should be bound:\n%s", strings.Join(cmd2.Args, " "))
	}
}

func TestLinuxMissingAuthorityPathsCannotActivateLater(t *testing.T) {
	skipIfNoBwrap(t)
	home, err := os.UserHomeDir()
	if err != nil {
		skipOrFailSandboxPrerequisite(t, "cannot resolve home: %v", err)
	}
	hostDir, err := os.MkdirTemp(home, ".polly-authority-freeze-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(hostDir) })
	if err := os.WriteFile(filepath.Join(hostDir, "secret"), []byte("host-secret"), 0600); err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	writableLink := filepath.Join(work, "out")
	readLink := filepath.Join(work, "readlink")
	sb, err := New(Config{
		WritablePaths: []string{work, writableLink},
		ReadPaths:     []string{readLink},
		DenyPaths:     []string{hostDir},
	})
	if err != nil {
		t.Fatal(err)
	}
	frozen := sb.(*linuxSandbox).cfg
	if len(frozen.WritablePaths) != 1 || len(frozen.ReadPaths) != 0 {
		t.Fatalf("missing authority survived construction: writable=%v read=%v", frozen.WritablePaths, frozen.ReadPaths)
	}

	create := exec.Command("bash", "-c", `ln -s "$1" "$3" && ln -s "$2" "$4"`, "bash", hostDir, hostDir, writableLink, readLink)
	cleanupCreate, err := WrapCmdManaged(sb, create)
	if err != nil {
		t.Fatal(err)
	}
	createOut, createErr := create.CombinedOutput()
	_ = cleanupCreate()
	if createErr != nil {
		skipOrFailBwrapUnavailable(t, createErr, createOut)
	}

	probe := exec.Command("bash", "-c", `
echo escaped > "$1/write-marker" 2>/dev/null || true
if test "$(cat "$2/secret" 2>/dev/null)" = host-secret; then exit 11; fi
`, "bash", writableLink, readLink)
	cleanupProbe, err := WrapCmdManaged(sb, probe)
	if err != nil {
		t.Fatal(err)
	}
	probeOut, probeErr := probe.CombinedOutput()
	_ = cleanupProbe()
	if probeErr != nil {
		t.Fatalf("frozen-authority probe failed: %v (%s)", probeErr, probeOut)
	}
	if _, err := os.Stat(filepath.Join(hostDir, "write-marker")); !os.IsNotExist(err) {
		t.Fatalf("later writable symlink escaped to host: %v", err)
	}
}

func TestLinuxFrozenAuthorityRejectsCrossSandboxReplacement(t *testing.T) {
	skipIfNoBwrap(t)
	home, err := os.UserHomeDir()
	if err != nil {
		skipOrFailSandboxPrerequisite(t, "cannot resolve home: %v", err)
	}
	hostDir, err := os.MkdirTemp(home, ".polly-cross-sandbox-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(hostDir) })

	work := t.TempDir()
	child := filepath.Join(work, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	narrow, err := New(Config{WritablePaths: []string{child}})
	if err != nil {
		t.Fatal(err)
	}
	broad, err := New(Config{WritablePaths: []string{work}})
	if err != nil {
		t.Fatal(err)
	}

	replace := exec.Command("bash", "-c", `mv "$1" "$1-old" && ln -s "$2" "$1"`, "bash", child, hostDir)
	cleanup, err := WrapCmdManaged(broad, replace)
	if err != nil {
		t.Fatal(err)
	}
	out, runErr := replace.CombinedOutput()
	_ = cleanup()
	if runErr != nil {
		skipOrFailBwrapUnavailable(t, runErr, out)
	}

	cmd := exec.Command("true")
	if err := wrapCmdForTest(t, narrow, cmd); err == nil {
		t.Fatal("narrow sandbox accepted a writable path replaced by another sandbox")
	}
	if len(cmd.ExtraFiles) != 0 {
		t.Fatalf("failed identity check leaked sandbox descriptors: %v", cmd.ExtraFiles)
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
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "Operation not permitted") || strings.Contains(string(out), "Permission denied") {
			skipOrFailBwrapUnavailable(t, err, out)
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

func TestLinuxBuildBwrapArgsDropsInheritedCapabilities(t *testing.T) {
	args := buildBwrapArgs(Config{}, nil)
	wantTail := []string{"--cap-drop", "ALL", "--die-with-parent", "--new-session"}
	if len(args) < len(wantTail) {
		t.Fatalf("bwrap args too short: %v", args)
	}
	gotTail := args[len(args)-len(wantTail):]
	if strings.Join(gotTail, "\x00") != strings.Join(wantTail, "\x00") {
		t.Fatalf("bwrap security tail = %v, want %v:\n%s", gotTail, wantTail, strings.Join(args, " "))
	}
}

func TestLinuxBuildBwrapArgsMountsNestedPrivateRootsAfterWritableAncestor(t *testing.T) {
	work := t.TempDir()
	tempRoot := filepath.Join(work, ".tmp")
	runRoot := filepath.Join(work, ".run")
	for _, root := range []string{tempRoot, runRoot} {
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}

	args, err := buildBwrapArgsInternalWithPlanAndRoots(
		Config{WritablePaths: []string{work}},
		nil,
		nil,
		true,
		nil,
		nil,
		nil,
		[]string{tempRoot},
		[]string{runRoot},
	)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := []string{
		"bwrap", "--ro-bind", "/", "/",
		"--bind", work, work,
		"--tmpfs", tempRoot,
		"--tmpfs", runRoot,
	}
	if len(args) < len(wantPrefix) || strings.Join(args[:len(wantPrefix)], "\x00") != strings.Join(wantPrefix, "\x00") {
		t.Fatalf("bwrap mount prefix must install writable ancestors before private roots; want %v:\n%s", wantPrefix, strings.Join(args, " "))
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
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
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
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("workspace write/read alongside denyWritePaths failed: %v", err)
	}
}

func TestLinuxBuildBwrapArgsReadPaths(t *testing.T) {
	home := t.TempDir()
	ssh := filepath.Join(home, ".ssh")
	aws := filepath.Join(home, ".aws")
	npmrc := filepath.Join(home, ".npmrc")
	for _, path := range []string{ssh, aws} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(npmrc, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	args := buildBwrapArgs(Config{
		ReadPaths: []string{ssh},
	}, []DeniedPath{
		{Path: ssh, Kind: DeniedPathDir},
		{Path: aws, Kind: DeniedPathDir},
		{Path: npmrc, Kind: DeniedPathFile},
	})

	joined := strings.Join(args, " ")

	// .ssh should NOT be overlaid because it's in ReadPaths
	if strings.Contains(joined, "--tmpfs "+ssh) {
		t.Fatal("denied path in ReadPaths should be skipped, but got --tmpfs for .ssh")
	}
	// .aws should still be overlaid
	if !strings.Contains(joined, "--tmpfs "+aws) {
		t.Fatal("denied path NOT in ReadPaths should still have --tmpfs")
	}
	// .npmrc should still be overlaid
	if !strings.Contains(joined, "--ro-bind /dev/null "+npmrc) {
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
	if !strings.Contains(joined, "--tmpfs "+deniedDir) {
		t.Fatalf("denied parent was not masked:\n%s", joined)
	}
	restore := "--ro-bind " + readable + " " + readable
	if !strings.Contains(joined, restore) {
		t.Fatalf("child exemption was not restored over its parent mask:\n%s", joined)
	}
	// bwrap resolves bind sources against its saved host root, so the original
	// path remains a valid source after the destination parent is masked.
	if strings.LastIndex(joined, restore) < strings.Index(joined, "--tmpfs "+deniedDir) {
		t.Fatalf("child exemption was restored before the parent mask:\n%s", joined)
	}
	if strings.Index(joined, "--remount-ro "+deniedDir) < strings.LastIndex(joined, restore) {
		t.Fatalf("denied parent became read-only before the exemption was restored:\n%s", joined)
	}
}

func TestLinuxSandboxAllowsChildReadPathExemption(t *testing.T) {
	skipIfNoBwrap(t)

	home, err := os.UserHomeDir()
	if err != nil {
		skipOrFailSandboxPrerequisite(t, "cannot resolve home: %v", err)
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
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
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

func TestLinuxReadPathDeniedIntersectionsStayReadOnly(t *testing.T) {
	skipIfNoBwrap(t)
	for _, kind := range []string{"file", "directory"} {
		t.Run(kind, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			secret := filepath.Join(root, "secret")
			readTarget := secret
			allowedDir := filepath.Join(root, "allowed")
			if err := os.Mkdir(allowedDir, 0700); err != nil {
				t.Fatal(err)
			}
			createTarget := filepath.Join(allowedDir, "written")
			if kind == "directory" {
				if err := os.Mkdir(secret, 0700); err != nil {
					t.Fatal(err)
				}
				readTarget = filepath.Join(secret, "value")
				createTarget = filepath.Join(secret, "planted")
			}
			if err := os.WriteFile(readTarget, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			sb, err := New(Config{
				WritablePaths: []string{root},
				DenyPaths:     []string{secret},
				ReadPaths:     []string{secret},
			})
			if err != nil {
				t.Fatal(err)
			}
			script := "cat \"$1\"; " +
				"if printf changed 2>/dev/null > \"$1\"; then exit 20; fi; "
			if kind == "directory" {
				script += "if printf planted 2>/dev/null > \"$2\"; then exit 21; fi"
			} else {
				script += "printf allowed > \"$2\""
			}
			cmd := exec.Command("/usr/bin/bash", "-c", script, "bash", readTarget, createTarget)
			cleanup, err := WrapCmdManaged(sb, cmd)
			if err != nil {
				t.Fatal(err)
			}
			out, runErr := cmd.CombinedOutput()
			_ = cleanup()
			if runErr != nil {
				skipOrFailBwrapUnavailable(t, runErr, out)
			}
			if string(out) != "original" {
				t.Fatalf("read-only exemption output = %q, want original", out)
			}
			if data, err := os.ReadFile(readTarget); err != nil || string(data) != "original" {
				t.Fatalf("read-only exemption was modified: %q, %v", data, err)
			}
			if kind == "directory" {
				if _, err := os.Lstat(createTarget); !os.IsNotExist(err) {
					t.Fatalf("read-only exemption allowed creation at %q: %v", createTarget, err)
				}
			} else if data, err := os.ReadFile(createTarget); err != nil || string(data) != "allowed" {
				t.Fatalf("unrelated sibling write failed: %q, %v", data, err)
			}
		})
	}
}

func TestLinuxMissingDeniedPathUnderReadGrantCannotBePlanted(t *testing.T) {
	skipIfNoBwrap(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "missing-secret")
	allowedDir := filepath.Join(root, "allowed")
	if err := os.Mkdir(allowedDir, 0700); err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join(allowedDir, "written")
	sb, err := New(Config{
		WritablePaths: []string{root},
		DenyPaths:     []string{missing},
		ReadPaths:     []string{root},
	})
	if err != nil {
		t.Fatal(err)
	}
	script := "test ! -s \"$1\"; " +
		"if printf planted 2>/dev/null > \"$1\"; then exit 20; fi; " +
		"printf allowed > \"$2\""
	cmd := exec.Command("/usr/bin/bash", "-c", script, "bash", missing, allowed)
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatal(err)
	}
	out, runErr := cmd.CombinedOutput()
	_ = cleanup()
	if runErr != nil {
		skipOrFailBwrapUnavailable(t, runErr, out)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("missing read-exempt credential was planted on host: %v", err)
	}
	if data, err := os.ReadFile(allowed); err != nil || string(data) != "allowed" {
		t.Fatalf("broad read grant incorrectly narrowed unrelated writes: %q, %v", data, err)
	}
}

func TestLinuxRootReadPathExemptsDescendants(t *testing.T) {
	if !isReadExempt("/home/user/.ssh", map[string]bool{"/": true}) {
		t.Fatal("filesystem root readPaths entry did not exempt a descendant")
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
	cfg, err := freezeAuthorityPaths(Config{
		ReadPaths: []string{link},
	})
	if err != nil {
		t.Fatal(err)
	}
	args := buildBwrapArgs(cfg, []DeniedPath{
		{Path: target, Kind: DeniedPathDir},
	})

	if joined := strings.Join(args, " "); strings.Contains(joined, "--tmpfs "+target) {
		t.Fatalf("readPaths exemption naming a symlink should exempt its resolved target, but it was masked:\n%s", joined)
	}
}

func TestLinuxReadPathAliasRemainsUsableAndRejectsRetarget(t *testing.T) {
	skipIfNoBwrap(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for path, value := range map[string]string{first: "first-secret", second: "second-secret"} {
		if err := os.WriteFile(filepath.Join(path, "credentials"), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(home, ".aws")
	if err := os.Symlink(first, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	sb, err := New(Config{WritablePaths: []string{home}, ReadPaths: []string{alias}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/usr/bin/cat", filepath.Join(alias, "credentials"))
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatal(err)
	}
	out, runErr := cmd.CombinedOutput()
	_ = cleanup()
	if runErr != nil {
		skipOrFailBwrapUnavailable(t, runErr, out)
	}
	if string(out) != "first-secret" {
		t.Fatalf("read through configured alias = %q, want original target", out)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, alias); err != nil {
		t.Fatal(err)
	}
	if err := wrapCmdForTest(t, sb, exec.Command("/usr/bin/true")); err == nil {
		t.Fatal("Wrap accepted a replaced and retargeted readPaths alias")
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
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	defer func() { _ = cleanup() }()

	if cmd.Env == nil || len(cmd.Env) != 0 {
		t.Fatalf("bwrap Env = %v, want non-nil empty", cmd.Env)
	}
	targetEnv := readLinuxEnvPayload(t, cmd.ExtraFiles[1])
	if strings.Join(targetEnv, " ") != "POLLY_KEEP=yes" {
		t.Fatalf("target environment = %q", targetEnv)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		skipOrFailBwrapUnavailable(t, err, out)
	}
	if strings.TrimSpace(string(out)) != "keep=yes drop=" {
		t.Fatalf("sandboxed target environment = %q", out)
	}
}

func TestLinuxNewCopiesCallerAllowEnv(t *testing.T) {
	skipIfNoBwrap(t)
	allow := []string{"SAFE_NAME"}
	sb, err := New(Config{AllowEnv: allow})
	if err != nil {
		t.Fatal(err)
	}
	allow[0] = "AWS_SECRET_ACCESS_KEY"
	if got := sb.(*linuxSandbox).cfg.AllowEnv; len(got) != 1 || got[0] != "SAFE_NAME" {
		t.Fatalf("sandbox AllowEnv aliases caller slice: %v", got)
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
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	defer func() { _ = cleanup() }()

	if cmd.Env == nil || len(cmd.Env) != 0 {
		t.Fatalf("bwrap Env = %v, want non-nil empty", cmd.Env)
	}
	targetEnv := strings.Join(readLinuxEnvPayload(t, cmd.ExtraFiles[1]), " ")
	if strings.Contains(targetEnv, "POLLYTOOL_") || !strings.Contains(targetEnv, "OTHER_VAR=kept") {
		t.Fatalf("target environment = %q", targetEnv)
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
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
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

	// The already-resolved executable is passed to the pinned post-containment
	// bootstrap, and the seccomp program has its own consumed descriptor.
	joined := strings.Join(cmd.Args, " ")
	if strings.Contains(joined, "--args") || strings.Contains(joined, "--setenv") {
		t.Fatalf("cmd.Args contain pre-containment env setup:\n%s", joined)
	}
	assertLinuxWrappedFDLayout(t, cmd, 0)
	wantSuffix := origPath + " bash -c echo hello"
	if !strings.HasSuffix(joined, wantSuffix) {
		t.Fatalf("cmd.Args missing original command suffix %q:\n%s", wantSuffix, joined)
	}
}

func assertLinuxWrappedFDLayout(t *testing.T, cmd *exec.Cmd, callerFiles int) {
	t.Helper()
	if len(cmd.ExtraFiles) < callerFiles+3 {
		t.Fatalf("cmd.ExtraFiles = %d, want at least caller files plus bootstrap, target-env, and seccomp fds", len(cmd.ExtraFiles))
	}
	bootstrapFD := 3 + callerFiles
	envFD := bootstrapFD + 1
	seccompFD := 3 + len(cmd.ExtraFiles) - 1
	seccompArg := -1
	bootstrapArg := -1
	for i, arg := range cmd.Args {
		if arg == "--seccomp" {
			seccompArg = i
		}
		if arg == linuxEnvBootstrapArg {
			bootstrapArg = i
		}
	}
	if seccompArg < 0 || seccompArg+1 >= len(cmd.Args) || cmd.Args[seccompArg+1] != strconv.Itoa(seccompFD) {
		t.Fatalf("cmd.Args seccomp fd = %v, want %d", cmd.Args, seccompFD)
	}
	if bootstrapArg <= 0 || bootstrapArg+5 >= len(cmd.Args) {
		t.Fatalf("cmd.Args missing Linux environment bootstrap: %v", cmd.Args)
	}
	if cmd.Args[bootstrapArg-1] != "/proc/self/fd/"+strconv.Itoa(bootstrapFD) ||
		cmd.Args[bootstrapArg+1] != strconv.Itoa(bootstrapFD) ||
		cmd.Args[bootstrapArg+2] != strconv.Itoa(envFD) {
		t.Fatalf("cmd.Args bootstrap fd layout = %v, want bootstrap %d and env %d", cmd.Args, bootstrapFD, envFD)
	}
	validationFD, err := parseLinuxOptionalFD(cmd.Args[bootstrapArg+3])
	if err != nil {
		t.Fatalf("parse reservation validation fd: %v", err)
	}
	authorityFDs, err := parseLinuxFDList(cmd.Args[bootstrapArg+4])
	if err != nil {
		t.Fatalf("parse authority fd list: %v", err)
	}
	wantAuthority := len(cmd.ExtraFiles) - callerFiles - 3
	wantFirstAuthority := envFD + 1
	if validationFD >= 0 {
		if validationFD != envFD+1 || validationFD >= seccompFD {
			t.Fatalf("reservation validation fd = %d, want %d below seccomp fd %d", validationFD, envFD+1, seccompFD)
		}
		wantAuthority--
		wantFirstAuthority++
	}
	if len(authorityFDs) != wantAuthority {
		t.Fatalf("authority fds = %v, want %d for ExtraFiles %v", authorityFDs, wantAuthority, cmd.ExtraFiles)
	}
	for i, fd := range authorityFDs {
		want := wantFirstAuthority + i
		if fd != want || fd >= seccompFD {
			t.Fatalf("authority fds = %v, want contiguous range starting at %d below seccomp fd %d", authorityFDs, wantFirstAuthority, seccompFD)
		}
	}
}

func TestLinuxManagedCleanupLeavesPreexistingExtraFile(t *testing.T) {
	skipIfNoBwrap(t)
	sb, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	callerFile, err := os.CreateTemp(t.TempDir(), "caller-extra-*")
	if err != nil {
		t.Fatal(err)
	}
	defer callerFile.Close()
	cmd := exec.Command("true")
	cmd.ExtraFiles = []*os.File{callerFile}
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatal(err)
	}
	assertLinuxWrappedFDLayout(t, cmd, 1)
	owned := append([]*os.File(nil), cmd.ExtraFiles[1:]...)
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if len(cmd.ExtraFiles) != 1 || cmd.ExtraFiles[0] != callerFile {
		t.Fatalf("ExtraFiles after close = %v, want caller file only", cmd.ExtraFiles)
	}
	if _, err := callerFile.Stat(); err != nil {
		t.Fatalf("caller descriptor closed: %v", err)
	}
	for _, file := range owned {
		if _, err := file.Stat(); err == nil {
			t.Fatalf("sandbox descriptor %v remains open", file)
		}
	}
}

func TestLinuxProbeDoesNotLeakSandboxFiles(t *testing.T) {
	skipIfNoBwrap(t)
	countFDs := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			skipOrFailSandboxPrerequisite(t, "cannot inspect process descriptors: %v", err)
		}
		return len(entries)
	}
	sb, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	before := countFDs()
	for i := 0; i < 25; i++ {
		if err := Probe(sb); err != nil {
			t.Fatal(err)
		}
	}
	after := countFDs()
	if after > before+1 {
		t.Fatalf("Probe leaked sandbox descriptors: before=%d after=%d", before, after)
	}
}

func TestLinuxBuildBwrapArgsReexposesTemporaryCommandReadOnly(t *testing.T) {
	dir := t.TempDir()
	commandPath := filepath.Join(dir, "tool")
	if err := os.WriteFile(commandPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(buildBwrapArgs(Config{}, nil, commandPath), " ")
	want := "--ro-bind " + commandPath + " " + commandPath
	if !strings.Contains(joined, want) {
		t.Fatalf("temporary command was not re-exposed read-only; want %q in:\n%s", want, joined)
	}
	if strings.Contains(joined, "--ro-bind "+dir+" "+dir) {
		t.Fatalf("temporary command leaked its containing directory:\n%s", joined)
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
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
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
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
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

func TestLinuxExplicitEnvIsTargetOnly(t *testing.T) {
	skipIfNoBwrap(t)
	sb, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("true")
	cmd.Env = []string{"LD_PRELOAD=/ambient.so", "SAFE=kept"}
	cleanup, err := WrapCmdWithEnvManaged(sb, cmd, map[string]string{"LD_PRELOAD": "/explicit.so"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	if cmd.Env == nil || len(cmd.Env) != 0 {
		t.Fatalf("bwrap Env = %v, want non-nil empty", cmd.Env)
	}
	if joined := strings.Join(cmd.Args, " "); strings.Contains(joined, "/ambient.so") || strings.Contains(joined, "/explicit.so") {
		t.Fatalf("target environment leaked into bwrap argv: %s", joined)
	}
	targetEnv := strings.Join(readLinuxEnvPayload(t, cmd.ExtraFiles[1]), " ")
	if strings.Contains(targetEnv, "/ambient.so") || !strings.Contains(targetEnv, "LD_PRELOAD=/explicit.so") {
		t.Fatalf("target environment = %q", targetEnv)
	}
}

func TestLinuxTargetEnvPayloadRoundTripAndDeduplicatesLast(t *testing.T) {
	payload, err := linuxTargetEnvPayload([]string{
		"PLAIN=value",
		"NOT-POSIX=execve-valid",
		"DUP=first",
		"DUP=last",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseLinuxTargetEnvPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"PLAIN=value", "NOT-POSIX=execve-valid", "DUP=last"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("payload round trip = %q, want %q", got, want)
	}
}

func TestLinuxReservationIdentityPayloadRoundTripAndBounds(t *testing.T) {
	want := []linuxReservationMountIdentity{{
		path: "/home/user/allowed", device: 42, inode: 99,
		fileType: unix.S_IFDIR, rdev: 0,
	}}
	payload, err := linuxReservationIdentityPayload(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseLinuxReservationIdentityPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("reservation identity round trip = %+v, want %+v", got, want)
	}

	malformed := []byte(linuxReservationIdentityMagic + "\x002\x00/home/user/allowed\x0042\x0099\x00" + strconv.FormatUint(unix.S_IFDIR, 10) + "\x000\x00")
	if _, err := parseLinuxReservationIdentityPayload(malformed); err == nil {
		t.Fatal("reservation identity payload accepted a mismatched entry count")
	}
	tooLarge := []linuxReservationMountIdentity{{
		path: "/" + strings.Repeat("x", linuxReservationIdentityPayloadLimit),
	}}
	if _, err := linuxReservationIdentityPayload(tooLarge); err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("oversized reservation identity payload error = %v, want bounded fail-closed error", err)
	}
}

func TestLinuxTargetEnvTransportIsSealedMemfd(t *testing.T) {
	cmd := exec.Command("true")
	if _, err := attachLinuxTargetEnvironment(cmd, []string{"SECRET=opaque"}); err != nil {
		t.Fatal(err)
	}
	if len(cmd.ExtraFiles) != 1 {
		t.Fatalf("ExtraFiles = %d, want one environment memfd", len(cmd.ExtraFiles))
	}
	f := cmd.ExtraFiles[0]
	defer f.Close()
	link, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(int(f.Fd())))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(link, "memfd:pollytool-target-env") {
		t.Fatalf("target environment descriptor route = %q, want anonymous memfd", link)
	}
	seals, err := unix.FcntlInt(f.Fd(), unix.F_GET_SEALS, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantSeals := unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL
	if seals&wantSeals != wantSeals {
		t.Fatalf("target environment descriptor seals = %#x, want %#x", seals, wantSeals)
	}
	if _, err := f.WriteAt([]byte("changed"), 0); err == nil {
		t.Fatal("sealed target environment descriptor remained writable")
	}
	if got := readLinuxEnvPayload(t, f); strings.Join(got, " ") != "SECRET=opaque" {
		t.Fatalf("sealed target environment = %q", got)
	}
}

func TestLinuxStrictAllowEnvReachesTargetExactly(t *testing.T) {
	skipIfNoBwrap(t)
	sb, err := New(Config{AllowEnv: []string{"ONLY", "DUP"}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/usr/bin/env")
	cmd.Env = []string{
		"ONLY=kept",
		"OMIT=blocked",
		"PWD=/must-not-reappear",
		"SHLVL=99",
		"DUP=first",
		"DUP=last",
	}
	cleanup, err := WrapCmdWithEnvManaged(sb, cmd, map[string]string{"NOT-POSIX": "execve-valid"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	out, err := cmd.CombinedOutput()
	if err != nil {
		skipOrFailBwrapUnavailable(t, err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	want := map[string]bool{
		"ONLY=kept":              true,
		"DUP=last":               true,
		"NOT-POSIX=execve-valid": true,
	}
	if len(lines) != len(want) {
		t.Fatalf("target environment = %q, want exactly %v", lines, want)
	}
	for _, line := range lines {
		if !want[line] {
			t.Fatalf("target environment contains unexpected entry %q: %q", line, lines)
		}
	}
}

func TestLinuxWrapWithEnvDirectlyRejectsInvalidKey(t *testing.T) {
	skipIfNoBwrap(t)
	sb, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	explicitSandbox := sb.(ExplicitEnvSandbox)
	cmd := exec.Command("true")
	if err := explicitSandbox.WrapWithEnv(cmd, map[string]string{"BAD=NAME": "value"}); err == nil {
		t.Fatal("direct WrapWithEnv accepted key containing equals")
	}
	if len(cmd.ExtraFiles) != 0 {
		t.Fatalf("invalid explicit environment allocated sandbox files: %v", cmd.ExtraFiles)
	}
}

func TestLinuxLoaderConstructorRunsOnlyAfterContainment(t *testing.T) {
	skipIfNoBwrap(t)
	cc, err := exec.LookPath("cc")
	if err != nil {
		skipOrFailSandboxPrerequisite(t, "C compiler is required for the loader-constructor containment test: %v", err)
	}
	buildDir := t.TempDir()
	source := filepath.Join(buildDir, "constructor.c")
	library := filepath.Join(buildDir, "constructor.so")
	code := `
#include <fcntl.h>
#include <stdlib.h>
#include <unistd.h>
__attribute__((constructor)) static void polly_constructor(void) {
    const char *marker = getenv("POLLY_TEST_HOST_MARKER");
    int fd = marker ? open(marker, O_CREAT | O_WRONLY, 0600) : -1;
    if (fd >= 0) { (void)write(fd, "escaped", 7); (void)close(fd); }
    (void)write(STDOUT_FILENO, "constructor-ran\n", 16);
}`
	if err := os.WriteFile(source, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	compile := exec.Command(cc, "-shared", "-fPIC", "-o", library, source)
	if out, err := compile.CombinedOutput(); err != nil {
		skipOrFailSandboxPrerequisite(t, "cannot build required constructor fixture: %v (%s)", err, out)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	markerDir, err := os.MkdirTemp(home, ".polly-loader-marker-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(markerDir) })
	hostMarker := filepath.Join(markerDir, "escaped")

	sb, err := New(Config{ReadPaths: []string{library}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("true")
	cleanup, err := WrapCmdWithEnvManaged(sb, cmd, map[string]string{
		"LD_PRELOAD":             library,
		"POLLY_TEST_HOST_MARKER": hostMarker,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	out, err := cmd.CombinedOutput()
	if cleanupErr := cleanup(); cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	if err != nil {
		skipOrFailBwrapUnavailable(t, err, out)
	}
	if !strings.Contains(string(out), "constructor-ran") {
		t.Fatalf("target loader constructor did not run: %q", out)
	}
	if _, err := os.Stat(hostMarker); !os.IsNotExist(err) {
		t.Fatalf("loader constructor ran before containment and created host marker: %v", err)
	}
}

func TestLinuxDeniedPathsStayReservedForLongLivedProcess(t *testing.T) {
	skipIfNoBwrap(t)
	hostHome, err := os.UserHomeDir()
	if err != nil {
		skipOrFailSandboxPrerequisite(t, "cannot resolve home: %v", err)
	}
	home, err := os.MkdirTemp(hostHome, ".polly-reservation-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)

	missingFile := filepath.Join(home, "later-secret")
	existingDir := filepath.Join(home, "existing-secret")
	if err := os.Mkdir(existingDir, 0700); err != nil {
		t.Fatal(err)
	}
	existingFile := filepath.Join(existingDir, "value")
	if err := os.WriteFile(existingFile, []byte("initial-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	target1 := filepath.Join(home, "target-one")
	target2 := filepath.Join(home, "target-two")
	for _, target := range []string{target1, target2} {
		if err := os.Mkdir(target, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, "value"), []byte("symlink-secret"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	deniedLink := filepath.Join(home, "denied-link")
	if err := os.Symlink(target1, deniedLink); err != nil {
		t.Fatal(err)
	}
	allowedDir := filepath.Join(home, "allowed")
	if err := os.Mkdir(allowedDir, 0700); err != nil {
		t.Fatal(err)
	}
	allowedFile := filepath.Join(allowedDir, "written")

	sb, err := New(Config{
		WritablePaths: []string{home},
		DenyPaths:     []string{missingFile, existingDir, deniedLink},
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxDeniedReservationHelper$")
	cmd.Env = append(os.Environ(),
		"POLLY_TEST_DENIED_FILES="+strings.Join([]string{missingFile, existingFile, filepath.Join(deniedLink, "value")}, "\n"),
		"POLLY_TEST_ALLOWED_FILE="+allowedFile)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	probe := func() string {
		t.Helper()
		if _, err := io.WriteString(stdin, "probe\n"); err != nil {
			t.Fatal(err)
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read sandboxed reservation probe: %v (%s)\nargs: %v", err, strings.TrimSpace(stderr.String()), cmd.Args)
		}
		return strings.TrimSpace(line)
	}
	if got := probe(); got != "hidden|hidden|hidden|write-ok" {
		t.Fatalf("initial probe = %q", got)
	}
	if err := os.WriteFile(missingFile, []byte("created-later"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(existingDir, existingDir+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(existingDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existingFile, []byte("replacement-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(deniedLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target2, deniedLink); err != nil {
		t.Fatal(err)
	}
	if got := probe(); got != "hidden|hidden|hidden|write-ok" {
		t.Fatalf("post-replacement probe = %q", got)
	}
	if data, err := os.ReadFile(allowedFile); err != nil || string(data) != "ok" {
		t.Fatalf("adjacent writable path was not preserved: %q, %v", data, err)
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxDeniedPathsStayReservedInHostBackedTempGrant(t *testing.T) {
	skipIfNoBwrap(t)
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	missingFile := filepath.Join(work, "later-secret")
	existingDir := filepath.Join(work, "existing-secret")
	if err := os.Mkdir(existingDir, 0700); err != nil {
		t.Fatal(err)
	}
	existingFile := filepath.Join(existingDir, "value")
	if err := os.WriteFile(existingFile, []byte("initial-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	allowedDir := filepath.Join(work, "allowed")
	if err := os.Mkdir(allowedDir, 0700); err != nil {
		t.Fatal(err)
	}
	allowedFile := filepath.Join(allowedDir, "written")

	sb, err := New(Config{
		WritablePaths: []string{work},
		DenyPaths:     []string{missingFile, existingDir},
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxDeniedReservationHelper$")
	cmd.Env = append(os.Environ(),
		"POLLY_TEST_DENIED_FILES="+strings.Join([]string{missingFile, existingFile}, "\n"),
		"POLLY_TEST_ALLOWED_FILE="+allowedFile)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		_ = cleanup()
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	probe := func() string {
		t.Helper()
		if _, err := io.WriteString(stdin, "probe\n"); err != nil {
			t.Fatal(err)
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(line)
	}
	if got := probe(); got != "hidden|hidden|write-ok" {
		t.Fatalf("initial probe = %q", got)
	}
	if err := os.WriteFile(missingFile, []byte("created-later"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(existingDir, existingDir+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(existingDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existingFile, []byte("replacement-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := probe(); got != "hidden|hidden|write-ok" {
		t.Fatalf("post-replacement probe = %q", got)
	}
	if data, err := os.ReadFile(allowedFile); err != nil || string(data) != "ok" {
		t.Fatalf("adjacent temp grant write was not preserved: %q, %v", data, err)
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxDeniedPathParentsCannotBeRelocated(t *testing.T) {
	skipIfNoBwrap(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	dockerDir := filepath.Join(home, ".docker")
	dockerConfig := filepath.Join(dockerDir, "config.json")
	configDir := filepath.Join(home, ".config")
	gcloudDir := filepath.Join(configDir, "gcloud")
	for _, path := range []string{dockerDir, gcloudDir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(dockerConfig, []byte("original-docker"), 0600); err != nil {
		t.Fatal(err)
	}
	gcloudMarker := filepath.Join(gcloudDir, "marker")
	if err := os.WriteFile(gcloudMarker, []byte("original-gcloud"), 0600); err != nil {
		t.Fatal(err)
	}
	sb, err := New(Config{
		WritablePaths: []string{root},
		DenyPaths: []string{
			dockerConfig,
			gcloudDir,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	script := "if mv \"$1\" \"$1-old\" 2>/dev/null; then exit 10; fi\n" +
		"mkdir -p \"$1/.docker\" \"$1/.config/gcloud\"\n" +
		"printf attacker > \"$1/.docker/config.json\" 2>/dev/null || true\n" +
		"printf attacker > \"$1/.config/gcloud/marker\" 2>/dev/null || true\n"
	cmd := exec.Command("/usr/bin/bash", "-c", script, "bash", home)
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatal(err)
	}
	out, runErr := cmd.CombinedOutput()
	_ = cleanup()
	if runErr != nil {
		skipOrFailBwrapUnavailable(t, runErr, out)
	}
	if data, err := os.ReadFile(dockerConfig); err != nil || string(data) != "original-docker" {
		t.Fatalf("host Docker credential changed after relocation attempt: %q, %v", data, err)
	}
	if data, err := os.ReadFile(gcloudMarker); err != nil || string(data) != "original-gcloud" {
		t.Fatalf("host gcloud credential changed after relocation attempt: %q, %v", data, err)
	}
	relocated := home + "-old"
	if _, err := os.Lstat(relocated); !os.IsNotExist(err) {
		t.Fatalf("denied route ancestor was relocated to %q: %v", relocated, err)
	}
}

func TestLinuxDenyWriteAncestorDoesNotReopenDeniedReservations(t *testing.T) {
	skipIfNoBwrap(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	dockerDir := filepath.Join(home, ".docker")
	if err := os.MkdirAll(dockerDir, 0700); err != nil {
		t.Fatal(err)
	}
	dockerConfig := filepath.Join(dockerDir, "config.json")
	awsAlias := filepath.Join(home, ".aws")
	if err := os.Symlink(filepath.Join(root, "missing-aws-target"), awsAlias); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "credentials"), []byte("symlink-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	allowedFile := filepath.Join(root, "allowed")
	t.Setenv("HOME", home)

	sb, err := New(Config{
		WritablePaths:  []string{root},
		DenyPaths:      []string{dockerConfig, awsAlias},
		DenyWritePaths: []string{home},
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxDeniedReservationHelper$")
	cmd.Env = append(os.Environ(),
		"POLLY_TEST_DENIED_FILES="+strings.Join([]string{dockerConfig, filepath.Join(awsAlias, "credentials")}, "\n"),
		"POLLY_TEST_ALLOWED_FILE="+allowedFile)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatal(err)
	}
	protectedIndex := linuxMountIndex(cmd.Args, "--ro-bind", home)
	reservationIndex := linuxMountIndex(cmd.Args, "--tmpfs", home)
	if protectedIndex < 0 || reservationIndex < 0 || protectedIndex > reservationIndex {
		_ = cleanup()
		t.Fatalf("protected ancestor must mount before reservation: protected=%d reservation=%d\n%v", protectedIndex, reservationIndex, cmd.Args)
	}
	if err := cmd.Start(); err != nil {
		_ = cleanup()
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	probe := func() string {
		t.Helper()
		if _, err := io.WriteString(stdin, "probe\n"); err != nil {
			t.Fatal(err)
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(line)
	}
	if got := probe(); got != "hidden|hidden|write-ok" {
		t.Fatalf("initial probe = %q", got)
	}
	if err := os.WriteFile(dockerConfig, []byte("created-later"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(awsAlias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, awsAlias); err != nil {
		t.Fatal(err)
	}
	if got := probe(); got != "hidden|hidden|write-ok" {
		t.Fatalf("post-host-creation probe = %q", got)
	}
	if data, err := os.ReadFile(dockerConfig); err != nil || string(data) != "created-later" {
		t.Fatalf("host setup changed unexpectedly: %q, %v", data, err)
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxNestedDenyWriteRoutingPrecedesMissingDeniedMask(t *testing.T) {
	skipIfNoBwrap(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	dockerDir := filepath.Join(home, ".docker")
	gitDir := filepath.Join(home, "repo", ".git")
	for _, path := range []string{dockerDir, gitDir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	missing := filepath.Join(dockerDir, "config.json")
	note := filepath.Join(home, "note")
	sb, err := New(Config{
		WritablePaths:  []string{root},
		DenyPaths:      []string{missing},
		DenyWritePaths: []string{gitDir},
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/usr/bin/bash", "-c", "printf planted > \"$1\" 2>/dev/null || true; printf allowed > \"$2\"", "bash", missing, note)
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatal(err)
	}
	routingIndex := linuxMountIndex(cmd.Args, "--bind", home)
	reservationIndex := linuxMountIndex(cmd.Args, "--tmpfs", dockerDir)
	protectedIndex := linuxMountIndex(cmd.Args, "--ro-bind", gitDir)
	if routingIndex < 0 || reservationIndex < 0 || protectedIndex < 0 || routingIndex > reservationIndex || protectedIndex < reservationIndex {
		_ = cleanup()
		t.Fatalf("mount phases are not routing -> reservation -> protected: routing=%d reservation=%d protected=%d\n%v", routingIndex, reservationIndex, protectedIndex, cmd.Args)
	}
	out, runErr := cmd.CombinedOutput()
	_ = cleanup()
	if runErr != nil {
		skipOrFailBwrapUnavailable(t, runErr, out)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("missing denied credential was planted on host: %v", err)
	}
	if data, err := os.ReadFile(note); err != nil || string(data) != "allowed" {
		t.Fatalf("adjacent writable path failed: %q, %v", data, err)
	}
}

func linuxMountIndex(args []string, option, destination string) int {
	width := 2
	if option == "--bind" || option == "--ro-bind" {
		width = 3
	}
	for i := 0; i+width-1 < len(args); i++ {
		if args[i] == option && args[i+width-1] == destination {
			return i
		}
	}
	return -1
}

func TestPlanDeniedReservationsOrdersEqualDepthRootsLexically(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"hotel", "golf", "foxtrot", "echo", "delta", "charlie", "bravo", "alpha"}
	denied := make([]DeniedPath, 0, len(names))
	for _, name := range names {
		root := filepath.Join(base, name)
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		denied = append(denied, DeniedPath{Path: filepath.Join(root, "missing-secret"), Kind: DeniedPathFile})
	}

	plans, err := planDeniedReservations(denied, nil, Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != len(names) {
		t.Fatalf("reservation count = %d, want %d", len(plans), len(names))
	}
	for i, name := range []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"} {
		if want := filepath.Join(base, name); plans[i].root != want {
			t.Fatalf("reservation %d root = %q, want %q", i, plans[i].root, want)
		}
	}
}

func TestLinuxReservationSiblingValidationRejectsSameTypeReplacement(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join(root, "allowed")
	if err := os.Mkdir(allowed, 0700); err != nil {
		t.Fatal(err)
	}
	denied := filepath.Join(root, "missing-secret")
	tempRoots, runRoots := privateLinuxRoots()
	privateRoots := append(append([]string{}, tempRoots...), runRoots...)
	plans, err := planDeniedReservations(
		[]DeniedPath{{Path: denied, Kind: DeniedPathFile}},
		nil,
		Config{WritablePaths: []string{root}},
		privateRoots,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 {
		t.Fatalf("host-backed temp deny reservation plans = %d, want 1", len(plans))
	}
	mountIdentities, err := linuxReservationMountIdentities(plans, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(allowed, allowed+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(allowed, 0700); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("true")
	sources, _, err := attachLinuxAuthorityPaths(cmd, reservationSourceIdentities(plans))
	if err != nil {
		t.Fatalf("reservation root pinning failed after a child replacement: %v", err)
	}
	if err := addLinuxReservationSources(plans, sources); err != nil {
		t.Fatal(err)
	}
	if source := sources[allowed]; !strings.HasPrefix(source, "/proc/self/fd/") || filepath.Base(source) != "allowed" {
		t.Fatalf("reservation sibling source = %q, want root-fd-relative route", source)
	}
	if err := validateLinuxReservationMountIdentities(mountIdentities); err == nil {
		t.Fatal("post-containment validation accepted a same-type sibling replacement")
	}
	for _, file := range cmd.ExtraFiles {
		_ = file.Close()
	}
}

func TestLinuxReservationSiblingReplacementAfterWrapFailsClosed(t *testing.T) {
	skipIfNoBwrap(t)
	hostHome, err := os.UserHomeDir()
	if err != nil {
		skipOrFailSandboxPrerequisite(t, "cannot resolve home: %v", err)
	}
	home, err := os.MkdirTemp(hostHome, ".polly-post-wrap-replacement-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	allowed := filepath.Join(home, "allowed")
	if err := os.Mkdir(allowed, 0700); err != nil {
		t.Fatal(err)
	}

	sb, err := New(Config{
		WritablePaths: []string{home},
		DenyPaths:     []string{filepath.Join(home, "missing-secret")},
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/usr/bin/true")
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	if err := os.Rename(allowed, allowed+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(allowed, 0700); err != nil {
		t.Fatal(err)
	}
	out, runErr := cmd.CombinedOutput()
	if cleanupErr := cleanup(); cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	if runErr == nil {
		t.Fatalf("sandbox target started after a restored sibling was replaced: %s", out)
	}
	if !strings.Contains(string(out), "denied reservation mount "+strconv.Quote(allowed)+" was replaced or changed") {
		t.Fatalf("post-containment replacement failure = %v (%s)", runErr, out)
	}
}

func TestLinuxReservationDescriptorsDoNotScaleWithSiblingCount(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const siblingCount = 512
	for i := 0; i < siblingCount; i++ {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("sibling-%04d", i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	plans, err := planDeniedReservations(
		[]DeniedPath{{Path: filepath.Join(root, "missing-secret"), Kind: DeniedPathFile}},
		nil,
		Config{},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || len(plans[0].entries) != siblingCount {
		t.Fatalf("reservation plan roots=%d entries=%d, want 1 root and %d entries", len(plans), len(plans[0].entries), siblingCount)
	}
	if got := len(reservationSourceIdentities(plans)); got != 1 {
		t.Fatalf("reservation authority identities = %d, want one root identity", got)
	}

	cmd := exec.Command("true")
	validationFD, err := attachLinuxReservationValidation(cmd, plans, nil)
	if err != nil {
		t.Fatal(err)
	}
	if validationFD < 3 {
		t.Fatalf("reservation validation fd = %d, want inherited descriptor", validationFD)
	}
	sources, authorityFDs, err := attachLinuxAuthorityPaths(cmd, reservationSourceIdentities(plans))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, file := range cmd.ExtraFiles {
			_ = file.Close()
		}
	}()
	if err := addLinuxReservationSources(plans, sources); err != nil {
		t.Fatal(err)
	}
	if len(cmd.ExtraFiles) != 2 || len(authorityFDs) != 1 {
		t.Fatalf("reservation descriptors = files:%d authority:%v, want one payload plus one root", len(cmd.ExtraFiles), authorityFDs)
	}
	for _, entry := range plans[0].entries {
		source := sources[filepath.Join(root, entry.name)]
		if !strings.HasPrefix(source, sources[root]+"/") {
			t.Fatalf("sibling %q source = %q, want below root source %q", entry.name, source, sources[root])
		}
	}
}

func TestLinuxReservationDescriptorsFitLowRlimit(t *testing.T) {
	if root := os.Getenv("POLLY_TEST_LOW_RLIMIT_RESERVATION_ROOT"); root != "" {
		var limit unix.Rlimit
		if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
			t.Fatal(err)
		}
		if limit.Max < 32 {
			t.Skipf("RLIMIT_NOFILE hard limit %d is already below test threshold", limit.Max)
		}
		limit.Cur = 32
		if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
			t.Fatal(err)
		}
		plans, err := planDeniedReservations(
			[]DeniedPath{{Path: filepath.Join(root, "missing-secret"), Kind: DeniedPathFile}}, nil, Config{}, nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("true")
		if _, err := attachLinuxReservationValidation(cmd, plans, nil); err != nil {
			t.Fatal(err)
		}
		if _, _, err := attachLinuxAuthorityPaths(cmd, reservationSourceIdentities(plans)); err != nil {
			t.Fatal(err)
		}
		if len(cmd.ExtraFiles) != 2 {
			t.Fatalf("reservation descriptors = %d, want payload plus root", len(cmd.ExtraFiles))
		}
		for _, file := range cmd.ExtraFiles {
			_ = file.Close()
		}
		return
	}

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 256; i++ {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("entry-%04d", i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxReservationDescriptorsFitLowRlimit$")
	cmd.Env = append(os.Environ(), "POLLY_TEST_LOW_RLIMIT_RESERVATION_ROOT="+root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("low-RLIMIT reservation helper failed: %v\n%s", err, out)
	}
}

func TestLinuxNestedReservationValidationUsesFinalVisibleMounts(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, ".config")
	configChild := filepath.Join(config, "allowed")
	outerChild := filepath.Join(home, "notes")
	for _, path := range []string{configChild, outerChild} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	plans, err := planDeniedReservations([]DeniedPath{
		{Path: filepath.Join(home, "missing-outer"), Kind: DeniedPathFile},
		{Path: filepath.Join(config, "missing-inner"), Kind: DeniedPathFile},
	}, nil, Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	identities, err := linuxReservationMountIdentities(plans, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(identities))
	for _, identity := range identities {
		got[identity.path] = true
	}
	if got[config] {
		t.Fatalf("outer restored entry %q is validated after an inner tmpfs intentionally replaces it", config)
	}
	for _, want := range []string{configChild, outerChild} {
		if !got[want] {
			t.Fatalf("final-visible restored entry %q is not validated: %v", want, got)
		}
	}
	if err := validateLinuxReservationMountIdentities(identities); err != nil {
		t.Fatalf("unchanged final-visible entries failed validation: %v", err)
	}
	overmountedIdentities, err := linuxReservationMountIdentities(plans, []string{outerChild})
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range overmountedIdentities {
		if identity.path == outerChild {
			t.Fatalf("later exact mount %q remained in final-visible validation set", outerChild)
		}
	}
}

func TestLinuxDeniedReservationHelper(t *testing.T) {
	encoded := os.Getenv("POLLY_TEST_DENIED_FILES")
	if encoded == "" {
		return
	}
	paths := strings.Split(encoded, "\n")
	allowed := os.Getenv("POLLY_TEST_ALLOWED_FILE")
	scanner := bufio.NewScanner(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	for scanner.Scan() {
		results := make([]string, 0, len(paths)+1)
		for _, path := range paths {
			if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
				results = append(results, "LEAK")
			} else {
				results = append(results, "hidden")
			}
		}
		if err := os.WriteFile(allowed, []byte("ok"), 0600); err != nil {
			results = append(results, "write-failed")
		} else {
			results = append(results, "write-ok")
		}
		_, _ = fmt.Fprintln(writer, strings.Join(results, "|"))
		_ = writer.Flush()
	}
}

func TestLinuxPrivateTempResetsOrRestoresWorkingDirectory(t *testing.T) {
	skipIfNoBwrap(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "host-marker")
	if err := os.WriteFile(marker, []byte("host"), 0600); err != nil {
		t.Fatal(err)
	}

	t.Run("hidden by default", func(t *testing.T) {
		sb, err := New(DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", "-c", "test \"$PWD\" = / && test ! -e host-marker")
		cmd.Dir = dir
		if err := wrapCmdForTest(t, sb, cmd); err != nil {
			t.Fatal(err)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			skipOrFailBwrapUnavailable(t, err, out)
		}
	})

	t.Run("explicit writable descendant", func(t *testing.T) {
		sb, err := New(Config{WritablePaths: []string{dir}})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", "-c", "test -e host-marker && echo ok > created")
		cmd.Dir = dir
		if err := wrapCmdForTest(t, sb, cmd); err != nil {
			t.Fatal(err)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			skipOrFailBwrapUnavailable(t, err, out)
		}
		if data, err := os.ReadFile(filepath.Join(dir, "created")); err != nil || strings.TrimSpace(string(data)) != "ok" {
			t.Fatalf("explicit temp workspace write missing: %q, %v", data, err)
		}
	})

	t.Run("relative command directory and executable", func(t *testing.T) {
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		relativeDir, err := filepath.Rel(cwd, dir)
		if err != nil {
			t.Fatal(err)
		}
		tool := filepath.Join(dir, "relative-tool.sh")
		if err := os.WriteFile(tool, []byte("#!/bin/sh\npwd\n"), 0700); err != nil {
			t.Fatal(err)
		}
		sb, err := New(Config{WritablePaths: []string{dir}})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("./relative-tool.sh")
		cmd.Dir = relativeDir
		cleanup, err := WrapCmdManaged(sb, cmd)
		if err != nil {
			t.Fatal(err)
		}
		out, runErr := cmd.CombinedOutput()
		_ = cleanup()
		if runErr != nil {
			skipOrFailBwrapUnavailable(t, runErr, out)
		}
		realDir, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(out)); got != realDir {
			t.Fatalf("relative command cwd = %q, want %q", got, realDir)
		}
	})
}

func TestLinuxDistinctTMPDIRIsPrivateButSelectedCommandRuns(t *testing.T) {
	skipIfNoBwrap(t)
	hostTemp, err := os.MkdirTemp("/var/tmp", "polly-distinct-tmp-")
	if err != nil {
		skipOrFailSandboxPrerequisite(t, "cannot create distinct host temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(hostTemp) })
	t.Setenv("TMPDIR", hostTemp)
	tool := filepath.Join(hostTemp, "tool.sh")
	marker := filepath.Join(hostTemp, "host-marker")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\ntest ! -e \"$1\" && printf ok\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("host"), 0600); err != nil {
		t.Fatal(err)
	}
	sb, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(tool, marker)
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "--tmpfs "+hostTemp) || !strings.Contains(joined, "--ro-bind "+tool+" "+tool) {
		t.Fatalf("distinct temp/private command args missing:\n%s", joined)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		skipOrFailBwrapUnavailable(t, err, out)
	}
	if strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("selected command output = %q", out)
	}
}

func TestLinuxWritableAncestorDoesNotExposeDistinctTMPDIR(t *testing.T) {
	skipIfNoBwrap(t)
	work, err := os.MkdirTemp("/var/tmp", "polly-private-tmp-parent-")
	if err != nil {
		skipOrFailSandboxPrerequisite(t, "cannot create workspace outside /tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(work) })
	hostTemp := filepath.Join(work, ".tmp")
	if err := os.Mkdir(hostTemp, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(hostTemp, "host-marker")
	if err := os.WriteFile(marker, []byte("host"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspaceOutput := filepath.Join(work, "sandbox-output")
	t.Setenv("TMPDIR", hostTemp)

	sb, err := New(Config{WritablePaths: []string{work}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", `test ! -e "$1" && printf ok > "$2"`, "sh", marker, workspaceOutput)
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		skipOrFailBwrapUnavailable(t, err, out)
	}
	got, err := os.ReadFile(workspaceOutput)
	if err != nil {
		t.Fatalf("sandbox did not retain the surrounding workspace write grant: %v", err)
	}
	if string(got) != "ok" {
		t.Fatalf("workspace output = %q, want ok", got)
	}
}

func TestLinuxPrivateRootsAreFrozenAtConstruction(t *testing.T) {
	skipIfNoBwrap(t)
	first, err := os.MkdirTemp("/var/tmp", "polly-frozen-tmp-a-")
	if err != nil {
		skipOrFailSandboxPrerequisite(t, "cannot create first distinct host temp: %v", err)
	}
	second, err := os.MkdirTemp("/var/tmp", "polly-frozen-tmp-b-")
	if err != nil {
		_ = os.RemoveAll(first)
		skipOrFailSandboxPrerequisite(t, "cannot create second distinct host temp: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(first)
		_ = os.RemoveAll(second)
	})
	if err := os.WriteFile(filepath.Join(first, "host-marker"), []byte("host"), 0600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		cfg  func() Config
	}{
		{name: "automatic private root", cfg: func() Config { return Config{} }},
		{name: "default exact private grant", cfg: DefaultConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TMPDIR", first)
			sb, err := New(tc.cfg())
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("TMPDIR", second)
			linuxSB := sb.(*linuxSandbox)
			if !pathEqualsAny(first, linuxSB.tempRoots) || pathEqualsAny(second, linuxSB.tempRoots) {
				t.Fatalf("frozen temp roots = %v, want first %q and not later %q", linuxSB.tempRoots, first, second)
			}

			created := filepath.Join(first, "sandbox-created")
			cmd := exec.Command("/usr/bin/bash", "-c", `test ! -e "$1/host-marker" && printf private > "$1/sandbox-created"`, "bash", first)
			cleanup, err := WrapCmdManaged(sb, cmd)
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(cmd.Args, " ")
			if !strings.Contains(joined, "--tmpfs "+first) || strings.Contains(joined, "--tmpfs "+second) {
				_ = cleanup()
				t.Fatalf("Wrap recomputed private roots after TMPDIR changed:\n%s", joined)
			}
			out, runErr := cmd.CombinedOutput()
			_ = cleanup()
			if runErr != nil {
				skipOrFailBwrapUnavailable(t, runErr, out)
			}
			if _, err := os.Stat(created); !os.IsNotExist(err) {
				t.Fatalf("private temp write escaped to host: %v", err)
			}
		})
	}
}

func TestLinuxPrepareConfigDefersPrivateRootMinimizationUntilNew(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareConfig(Config{WritablePaths: []string{root, nested}})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.WritablePaths) != 2 {
		t.Fatalf("generic PrepareConfig prematurely minimized writable roots: %v", prepared.WritablePaths)
	}
	frozen, err := prepareLinuxConfig(prepared, []string{"/tmp", root}, []string{"/run"})
	if err != nil {
		t.Fatal(err)
	}
	if len(frozen.WritablePaths) != 2 {
		t.Fatalf("construction-time private root removed its explicit descendant: %v", frozen.WritablePaths)
	}
}

func TestLinuxPrepareConfigKeepsDescendantOfSymlinkedPrivateRoot(t *testing.T) {
	realRoot, err := os.MkdirTemp("/var/tmp", "polly-private-real-")
	if err != nil {
		skipOrFailSandboxPrerequisite(t, "cannot create real private root: %v", err)
	}
	alias := realRoot + "-alias"
	t.Cleanup(func() {
		_ = os.Remove(alias)
		_ = os.RemoveAll(realRoot)
	})
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(realRoot, "explicit-child")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", alias)
	prepared, err := PrepareConfig(Config{WritablePaths: []string{alias, filepath.Join(alias, "explicit-child")}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{realRoot, nested}
	if strings.Join(prepared.WritablePaths, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("prepared symlinked private grants = %v, want %v", prepared.WritablePaths, want)
	}
	tempRoots, runRoots := privateLinuxRoots()
	frozen, err := prepareLinuxConfig(prepared, tempRoots, runRoots)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(frozen.WritablePaths, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("construction-time symlinked private grants = %v, want %v", frozen.WritablePaths, want)
	}
}

func TestLinuxSocketFilterPolicy(t *testing.T) {
	skipIfNoBwrap(t)
	for _, allowNetwork := range []bool{false, true} {
		t.Run(fmt.Sprintf("allowNetwork=%v", allowNetwork), func(t *testing.T) {
			sb, err := New(Config{AllowNetwork: allowNetwork})
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxSocketFilterHelper$")
			cmd.Env = append(os.Environ(), fmt.Sprintf("POLLY_TEST_ALLOW_NETWORK=%v", allowNetwork))
			if err := wrapCmdForTest(t, sb, cmd); err != nil {
				t.Fatal(err)
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				skipOrFailBwrapUnavailable(t, err, out)
			}
		})
	}
}

func TestLinuxSocketFilterHelper(t *testing.T) {
	mode := os.Getenv("POLLY_TEST_ALLOW_NETWORK")
	if mode == "" {
		return
	}
	stream, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("private stream socketpair blocked: %v", err)
	}
	_ = unix.Close(stream[0])
	_ = unix.Close(stream[1])
	if pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0); err == nil {
		_ = unix.Close(pair[0])
		_ = unix.Close(pair[1])
		t.Fatal("reconnectable datagram socketpair was allowed")
	} else if err != unix.EACCES {
		t.Fatalf("datagram socketpair error = %v, want EACCES", err)
	}
	if fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0); err == nil {
		_ = unix.Close(fd)
		t.Fatal("AF_VSOCK was allowed")
	} else if err != unix.EACCES {
		t.Fatalf("AF_VSOCK error = %v, want EACCES", err)
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if mode == "true" {
		if err != nil {
			t.Fatalf("AF_INET blocked with network enabled: %v", err)
		}
		_ = unix.Close(fd)
	} else if err != unix.EACCES {
		if err == nil {
			_ = unix.Close(fd)
		}
		t.Fatalf("AF_INET error = %v, want EACCES with network disabled", err)
	}
}

func TestLinuxDenyWritePathsFailClosedAndPinAncestors(t *testing.T) {
	skipIfNoBwrap(t)
	work := t.TempDir()
	missing := filepath.Join(work, "missing")
	if _, err := New(Config{WritablePaths: []string{work}, DenyWritePaths: []string{missing}}); err == nil {
		t.Fatal("missing denyWritePaths entry did not fail construction")
	}
	if _, err := New(Config{DenyWrite: true, DenyWritePaths: []string{missing}}); err != nil {
		t.Fatalf("redundant denyWritePaths rejected under DenyWrite: %v", err)
	}
	realRoute := filepath.Join(work, "real-route")
	if err := os.MkdirAll(filepath.Join(realRoute, "protected"), 0700); err != nil {
		t.Fatal(err)
	}
	symlinkRoute := filepath.Join(work, "symlink-route")
	if err := os.Symlink(realRoute, symlinkRoute); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{
		WritablePaths:  []string{work},
		DenyWritePaths: []string{filepath.Join(symlinkRoute, "protected")},
	}); err == nil {
		t.Fatal("symlink routing component inside writable path was accepted")
	}

	rootGit := filepath.Join(work, ".git")
	nested := filepath.Join(rootGit, "modules", "sub")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	packagesGit := filepath.Join(work, "packages", "repo", ".git")
	if err := os.MkdirAll(packagesGit, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := Config{WritablePaths: []string{work}, DenyWritePaths: []string{rootGit, nested, packagesGit}}
	args, err := buildBwrapArgsChecked(cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--bind "+rootGit+" "+rootGit) {
		t.Fatalf("nested protection reopened root .git:\n%s", joined)
	}
	rootRO := "--ro-bind " + rootGit + " " + rootGit
	if strings.Count(joined, rootRO) != 1 || strings.Contains(joined, "--ro-bind "+nested+" "+nested) {
		t.Fatalf("overlapping protected paths were not minimized:\n%s", joined)
	}
	for _, ancestor := range []string{filepath.Join(work, "packages"), filepath.Join(work, "packages", "repo")} {
		bind := "--bind " + ancestor + " " + ancestor
		if strings.Index(joined, bind) < 0 || strings.Index(joined, bind) > strings.Index(joined, "--ro-bind "+packagesGit+" "+packagesGit) {
			t.Fatalf("ancestor %q was not pinned before protected mount:\n%s", ancestor, joined)
		}
	}
}

func TestLinuxDenyWriteMountSourcesArePinnedAndRejectReplacement(t *testing.T) {
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ancestor := filepath.Join(work, "nested")
	protected := filepath.Join(ancestor, ".git")
	external := filepath.Join(work, "external")
	for _, path := range []string{protected, external} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{WritablePaths: []string{work}, DenyWritePaths: []string{protected}}
	plan, err := planDenyWriteMounts(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ancestors) == 0 || len(plan.protected) != 1 {
		t.Fatalf("deny-write mount plan = ancestors:%v protected:%v", plan.ancestors, plan.protected)
	}
	identities := append(cloneAuthorityPathIdentities(plan.ancestors), plan.protected...)
	cmd := exec.Command("true")
	sources, _, err := attachLinuxAuthorityPaths(cmd, identities)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, file := range cmd.ExtraFiles {
			_ = file.Close()
		}
	}()
	args, err := appendLinuxRoutingMounts(nil, Config{}, nil, nil, plan, sources)
	if err != nil {
		t.Fatal(err)
	}
	args, err = appendDenyWriteProtectedMounts(args, plan, nil, sources)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+2 < len(args); i += 3 {
		if !strings.HasPrefix(args[i+1], "/proc/self/fd/") {
			t.Fatalf("deny-write mount uses mutable source: %v", args[i:i+3])
		}
	}

	plan, err = planDenyWriteMounts(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(ancestor, ancestor+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, ancestor); err != nil {
		t.Fatal(err)
	}
	identities = append(cloneAuthorityPathIdentities(plan.ancestors), plan.protected...)
	replacedCmd := exec.Command("true")
	if _, _, err := attachLinuxAuthorityPaths(replacedCmd, identities); err == nil {
		t.Fatal("deny-write source pinning accepted an ancestor replaced after planning")
	}
	for _, file := range replacedCmd.ExtraFiles {
		_ = file.Close()
	}
}

func TestLinuxProtectedDescendantReinstallsAfterOuterReservation(t *testing.T) {
	home := "/work/home"
	protected := "/work/home/.config/subdir"
	reservations := []deniedReservation{
		{root: home, omitted: map[string]bool{".aws": true}},
		{root: filepath.Join(protected, "credentials"), omitted: map[string]bool{"token": true}},
	}
	schedule := planDenyWriteProtectedSchedule(denyWriteMountPlan{
		protected: []authorityPathIdentity{{path: protected}},
	}, reservations)
	if len(schedule.initial) != 0 || len(schedule.late) != 0 {
		t.Fatalf("protected schedule initial=%v late=%v, want only post-reservation mount", schedule.initial, schedule.late)
	}
	if got := schedule.after[home]; len(got) != 1 || got[0].path != protected {
		t.Fatalf("protected schedule after outer reservation = %v, want %q", got, protected)
	}
}

func TestLinuxDenyWritePathsBlockRoutingReplacementEndToEnd(t *testing.T) {
	skipIfNoBwrap(t)
	work := t.TempDir()
	rootGit := filepath.Join(work, ".git")
	packages := filepath.Join(work, "packages")
	repo := filepath.Join(packages, "repo")
	nestedGit := filepath.Join(repo, ".git")
	for _, path := range []string{rootGit, nestedGit} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	sb, err := New(Config{WritablePaths: []string{work}, DenyWritePaths: []string{rootGit, nestedGit}})
	if err != nil {
		t.Fatal(err)
	}
	script := `
if mv "$1" "$1-old" 2>/dev/null; then exit 10; fi
if mv "$2" "$2-old" 2>/dev/null; then exit 11; fi
if echo bad > "$3/config" 2>/dev/null; then exit 12; fi
echo ok > "$4/note"
`
	cmd := exec.Command("bash", "-c", script, "bash", rootGit, packages, nestedGit, repo)
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		skipOrFailBwrapUnavailable(t, err, out)
	}
	if data, err := os.ReadFile(filepath.Join(repo, "note")); err != nil || strings.TrimSpace(string(data)) != "ok" {
		t.Fatalf("safe nested workspace write failed: %q, %v", data, err)
	}
}

func TestLinuxBwrapExecutableIsTrusted(t *testing.T) {
	skipIfNoBwrap(t)
	if err := validateLinuxBwrapExecutable(linuxBwrapPath); err != nil {
		t.Fatalf("fixed system bwrap rejected: %v", err)
	}
	if err := validateLinuxBwrapExecutable(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing bwrap executable was accepted")
	}
	notExecutable := filepath.Join(t.TempDir(), "bwrap")
	if err := os.WriteFile(notExecutable, []byte("not executable"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateLinuxBwrapExecutable(notExecutable); err == nil {
		t.Fatal("non-executable bwrap was accepted")
	}
	symlink := filepath.Join(t.TempDir(), "bwrap")
	if err := os.Symlink(linuxBwrapPath, symlink); err != nil {
		t.Fatal(err)
	}
	if err := validateLinuxBwrapExecutable(symlink); err == nil {
		t.Fatal("symlinked bwrap executable was accepted")
	}
	special := filepath.Join(t.TempDir(), "bwrap-special")
	if err := os.WriteFile(special, []byte("not bwrap"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(special, os.ModeSetuid|0755); err != nil {
		t.Fatal(err)
	}
	if err := validateLinuxBwrapExecutable(special); err == nil || !strings.Contains(err.Error(), "must not be setuid or setgid") {
		t.Fatalf("setuid bwrap validation error = %v, want special-bit rejection", err)
	}
}

func TestLinuxSandboxIgnoresPATHBwrap(t *testing.T) {
	skipIfNoBwrap(t)

	fakeDir := t.TempDir()
	fakeBwrap := filepath.Join(fakeDir, "bwrap")
	marker := fakeBwrap + ".ran"
	if err := os.WriteFile(fakeBwrap, []byte("#!/bin/sh\n: > \"$0.ran\"\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	sb, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/usr/bin/true")
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	if cmd.Path != linuxBwrapPath {
		t.Fatalf("sandbox launcher = %q, want fixed %q", cmd.Path, linuxBwrapPath)
	}
	out, runErr := cmd.CombinedOutput()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("PATH-selected fake bwrap executed outside containment: %v", err)
	}
	if runErr != nil {
		skipOrFailBwrapUnavailable(t, runErr, out)
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

	cmd := exec.CommandContext(context.Background(), "bash", "-c", "test ! -s "+filepath.Join(home, ".npmrc"))
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(output), "Operation not permitted") || strings.Contains(string(output), "Permission denied") {
			skipOrFailBwrapUnavailable(t, err, output)
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

		var command string
		var args []string
		switch dp.Kind {
		case DeniedPathDir:
			command = "/usr/bin/ls"
			args = []string{"-A", "--", dp.Path} // -A: real entries only, excludes . and ..
		case DeniedPathFile:
			command = "/usr/bin/cat"
			args = []string{"--", dp.Path}
		}

		t.Run(dp.Path, func(t *testing.T) {
			cmd := exec.CommandContext(context.Background(), command, args...)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			cleanup, err := WrapCmdManaged(sb, cmd)
			if err != nil {
				t.Fatalf("Wrap() error = %v", err)
			}
			// The mask replaces the path with an empty tmpfs (dir) or empty
			// placeholder (file). The command may exit 0 with empty output OR
			// error (permission) — both hide the secret. Diagnostics are not
			// credential content, so inspect stdout separately while still surfacing
			// unexpected launcher failures.
			runErr := cmd.Run()
			if cleanupErr := cleanup(); cleanupErr != nil {
				t.Fatal(cleanupErr)
			}
			if stdout.Len() != 0 {
				t.Fatalf("expected %s to be masked (no contents), but read: %q", dp.Path, stdout.String())
			}
			if runErr != nil {
				diagnostic := strings.TrimSpace(stderr.String())
				commandDenied := strings.Contains(diagnostic, dp.Path) &&
					(strings.HasPrefix(diagnostic, command+":") || strings.HasPrefix(diagnostic, filepath.Base(command)+":"))
				if !commandDenied {
					skipOrFailBwrapUnavailable(t, runErr, stderr.Bytes())
				}
			}
		})
	}
	if tested == 0 {
		t.Skip("no denied credential paths exist on this machine")
	}
	t.Logf("tested %d/%d credential paths that exist on this machine", tested, len(denied))
}
