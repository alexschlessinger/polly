//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFiniteCancellationLinuxNamespaceAndGroups(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "exit 0")
	if err := configureFiniteCancellation(&linuxSandbox{}, cmd); err != nil {
		t.Fatal(err)
	}
	if cmd.SysProcAttr != nil {
		t.Fatal("bubblewrap was assigned a host process group")
	}
	for _, session := range []bool{false, true} {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: session, Noctty: true}
		cleanup, err := WrapFiniteCmdManaged(stubSandbox{}, cmd)
		if err != nil {
			t.Fatal(err)
		}
		if err := cleanup(); err != nil {
			t.Fatal(err)
		}
		if cmd.SysProcAttr.Setsid != session || cmd.SysProcAttr.Setpgid == session || !cmd.SysProcAttr.Noctty {
			t.Fatalf("private group or unrelated attributes changed: %+v", cmd.SysProcAttr)
		}
	}
	for _, attr := range []*syscall.SysProcAttr{{Pgid: syscall.Getpgrp()}, {Foreground: true}} {
		cmd.SysProcAttr = attr
		if _, err := WrapFiniteCmdManaged(nil, cmd); err == nil {
			t.Fatalf("external process group accepted: %+v", attr)
		}
	}
}

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
	work := filepath.Join(home, "work")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}

	sb, err := New(Config{WritablePaths: []string{work}})
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

func TestLinuxSpecialMountRestrictionsFailClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	parentRoot := t.TempDir()
	parentCWD := filepath.Join(parentRoot, "one", "two", "three", "four")
	if err := os.MkdirAll(parentCWD, 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(parentCWD)
	procAlias := filepath.Join(home, "proc-alias")
	if err := os.Symlink("/proc", procAlias); err != nil {
		t.Fatal(err)
	}
	procCwdAlias := filepath.Join(home, "proc-cwd-alias")
	if err := os.Symlink("/proc/self/cwd", procCwdAlias); err != nil {
		t.Fatal(err)
	}
	procCwdDotDotAlias := filepath.Join(home, "proc-cwd-dot-dot-alias")
	if err := os.Symlink("/proc/self/cwd/../../../protected", procCwdDotDotAlias); err != nil {
		t.Fatal(err)
	}
	dotDotPath := filepath.Join(procCwdDotDotAlias, "missing")
	resolvedDotDotPath, err := resolveExistingPathPrefix(dotDotPath)
	if err != nil {
		t.Fatal(err)
	}
	wantDotDotPath := filepath.Join(parentRoot, "one", "protected", "missing")
	if resolvedDotDotPath != wantDotDotPath {
		t.Fatalf("dot-dot after proc cwd resolved to %q, want %q", resolvedDotDotPath, wantDotDotPath)
	}
	ordinaryProtected := filepath.Join(home, "ordinary-protected")
	if err := os.Mkdir(ordinaryProtected, 0700); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		cfg       Config
		wantField string
		wantRoot  string
	}{
		{
			name:      "proc deny path",
			cfg:       Config{DenyPaths: []string{"/proc/sys/kernel/hostname"}},
			wantField: "denyPaths",
			wantRoot:  "/proc",
		},
		{
			name:      "dev deny path",
			cfg:       Config{DenyPaths: []string{"/dev/null"}},
			wantField: "denyPaths",
			wantRoot:  "/dev",
		},
		{
			name:      "symlink into proc",
			cfg:       Config{DenyPaths: []string{filepath.Join(procAlias, "missing")}},
			wantField: "denyPaths",
			wantRoot:  "/proc",
		},
		{
			name:      "proc magic link out of proc",
			cfg:       Config{DenyPaths: []string{"/proc/self/cwd/missing"}},
			wantField: "denyPaths",
			wantRoot:  "/proc",
		},
		{
			name:      "outside alias through proc magic link",
			cfg:       Config{DenyPaths: []string{filepath.Join(procCwdAlias, "missing")}},
			wantField: "denyPaths",
			wantRoot:  "/proc",
		},
		{
			name:      "outside alias traverses proc before dot-dot",
			cfg:       Config{DenyPaths: []string{dotDotPath}},
			wantField: "denyPaths",
			wantRoot:  "/proc",
		},
		{
			name:      "ancestor of special mounts",
			cfg:       Config{DenyPaths: []string{"/"}},
			wantField: "denyPaths",
			wantRoot:  "/dev",
		},
		{
			name:      "proc deny-write path",
			cfg:       Config{DenyWritePaths: []string{"/proc/sys/kernel"}},
			wantField: "denyWritePaths",
			wantRoot:  "/proc",
		},
		{
			name: "ordinary restrictions",
			cfg: Config{
				DenyPaths:      []string{filepath.Join(home, "ordinary-denied")},
				DenyWritePaths: []string{ordinaryProtected},
			},
		},
		{
			name: "proc deny-write path under global deny-write",
			cfg: Config{
				DenyWrite:      true,
				DenyWritePaths: []string{"/proc/sys/kernel"},
			},
			wantField: "denyWritePaths",
			wantRoot:  "/proc",
		},
		{
			name: "device deny-write path under global deny-write",
			cfg: Config{
				DenyWrite:      true,
				DenyWritePaths: []string{"/dev/null"},
			},
			wantField: "denyWritePaths",
			wantRoot:  "/dev",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateLinuxSpecialMountRestrictions(test.cfg)
			if test.wantRoot == "" {
				if err != nil {
					t.Fatalf("validateLinuxSpecialMountRestrictions() error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("validateLinuxSpecialMountRestrictions() accepted an unenforceable restriction")
			}
			if !strings.Contains(err.Error(), test.wantField) || !strings.Contains(err.Error(), test.wantRoot) {
				t.Fatalf("validateLinuxSpecialMountRestrictions() error = %q, want field %q and root %q", err, test.wantField, test.wantRoot)
			}
		})
	}
}

func TestLinuxNewRejectsSpecialMountDenyPath(t *testing.T) {
	skipIfNoBwrap(t)
	t.Setenv("HOME", t.TempDir())
	if _, err := New(Config{DenyPaths: []string{"/proc/sys/kernel/hostname"}}); err == nil {
		t.Fatal("New() accepted a deny path that the final procfs mount would cover")
	} else if !strings.Contains(err.Error(), "Linux special mount \"/proc\"") {
		t.Fatalf("New() error = %q, want Linux special-mount refusal", err)
	}
}

func TestLinuxWrapRejectsDenyPathRetargetedIntoSpecialMount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	safeTarget := t.TempDir()
	alias := filepath.Join(home, "deny-route")
	if err := os.Symlink(safeTarget, alias); err != nil {
		t.Fatal(err)
	}
	cfg, err := normalizeConfigPaths(Config{DenyPaths: []string{filepath.Join(alias, "credential")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateLinuxSpecialMountRestrictions(cfg); err != nil {
		t.Fatalf("initial deny route was rejected: %v", err)
	}

	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/proc", alias); err != nil {
		t.Fatal(err)
	}
	sb := &linuxSandbox{cfg: cfg, bwrapPath: linuxBwrapPath, tempRoots: []string{"/tmp"}, runRoots: []string{"/run"}}
	cmd := exec.Command("/usr/bin/true")
	if err := sb.wrapManaged(cmd, nil); err == nil {
		t.Fatal("wrapManaged() accepted a deny route retargeted below /proc")
	} else if !strings.Contains(err.Error(), "Linux special mount \"/proc\"") {
		t.Fatalf("wrapManaged() error = %q, want Linux special-mount refusal", err)
	}
	if cmd.Path != "/usr/bin/true" || len(cmd.ExtraFiles) != 0 {
		t.Fatalf("failed wrap mutated command: Path=%q ExtraFiles=%d", cmd.Path, len(cmd.ExtraFiles))
	}
}

// buildBwrapArgs is the non-strict, unpinned argument builder the unit tests
// use to inspect mount ordering without opening descriptors.
func buildBwrapArgs(cfg Config, deniedPaths []DeniedPath, commandPaths ...string) []string {
	args, _ := buildBwrapArgsChecked(cfg, deniedPaths, commandPaths...)
	return args
}

// buildBwrapArgsChecked plans and renders the mounts the way wrapManaged does,
// with host paths as sources instead of pinned descriptors. deniedPaths
// extends cfg.DenyPaths for the call.
func buildBwrapArgsChecked(cfg Config, deniedPaths []DeniedPath, commandPaths ...string) ([]string, error) {
	for _, denied := range deniedPaths {
		cfg.DenyPaths = append(cfg.DenyPaths, denied.Path)
	}
	plan, err := planTestLinuxMounts(cfg, commandPaths...)
	if err != nil {
		return nil, err
	}
	return bwrapArgs(cfg, plan, nil)
}

func testLinuxPrivateRoots() linuxPrivateRootSet {
	tempRoots, runRoots := privateLinuxRoots()
	roots := linuxPrivateRootSet{temp: tempRoots, run: runRoots}
	if home := resolvedHomeDir(); home != "" {
		roots.home = []string{home}
	}
	return roots
}

func planTestLinuxMounts(cfg Config, commandPaths ...string) (linuxMountPlan, error) {
	roots := testLinuxPrivateRoots()
	grants := planLinuxGrants(cfg, roots.all())
	masks, islands, err := planLinuxMasks(cfg, grants, roots.all())
	if err != nil {
		return linuxMountPlan{}, err
	}
	denyWritePlan, err := planDenyWriteMounts(cfg, false)
	if err != nil {
		return linuxMountPlan{}, err
	}
	socketBinds := planLinuxUnixSocketBinds(effectiveUnixSocketGrants(cfg))
	return planLinuxMounts(cfg, roots, grants, masks, islands, denyWritePlan, socketBinds, commandPaths)
}

func TestLinuxBuildBwrapArgs(t *testing.T) {
	// Grants and denied paths must exist: missing grants are dropped and
	// missing denied paths need no mask. The denied entries sit inside the
	// writable project so the grant, not a private root, is what exposes them.
	project := t.TempDir()
	deniedDir := filepath.Join(project, ".ssh")
	deniedFile := filepath.Join(project, ".npmrc")
	if err := os.Mkdir(deniedDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deniedFile, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	args := buildBwrapArgs(Config{
		WritablePaths: []string{project},
	}, []DeniedPath{
		{Path: deniedDir, Kind: DeniedPathDir},
		{Path: deniedFile, Kind: DeniedPathFile},
	})

	joined := strings.Join(args, " ")

	if linuxMountIndex(args, "--ro-bind", "/") < 0 {
		t.Fatal("missing --ro-bind / /")
	}
	if linuxMountIndex(args, "--bind", project) < 0 {
		t.Fatalf("missing project writable bind:\n%s", joined)
	}
	if linuxMountIndex(args, "--tmpfs", "/tmp") < 0 {
		t.Fatal("missing private /tmp tmpfs")
	}
	if linuxMountIndex(args, "--bind", "/tmp") >= 0 {
		t.Fatal("host /tmp must not be bind-mounted")
	}
	if linuxMountIndex(args, "--tmpfs", "/run") < 0 || linuxMountIndex(args, "--remount-ro", "/run") < 0 {
		t.Fatal("missing private read-only /run")
	}
	if linuxMountIndex(args, "--tmpfs", deniedDir) < linuxMountIndex(args, "--bind", project) || linuxMountIndex(args, "--remount-ro", deniedDir) < 0 {
		t.Fatalf("missing read-only tmpfs overlay for the denied directory after its writable parent:\n%s", joined)
	}
	if i := linuxMountIndex(args, "--ro-bind", deniedFile); i < 0 || args[i+1] != "/dev/null" {
		t.Fatal("missing /dev/null bind for denied file")
	}
	if linuxMountIndex(args, "--tmpfs", deniedFile) >= 0 {
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
	cfg := Config{WritablePaths: []string{work}}
	roots := linuxPrivateRootSet{temp: []string{tempRoot}, run: []string{runRoot}}
	grants := planLinuxGrants(cfg, roots.all())
	plan, err := planLinuxMounts(cfg, roots, grants, nil, nil, denyWriteMountPlan{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	args, err := bwrapArgs(cfg, plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	workAt := linuxMountIndex(args, "--bind", work)
	tempAt := linuxMountIndex(args, "--tmpfs", tempRoot)
	runAt := linuxMountIndex(args, "--tmpfs", runRoot)
	if workAt < 0 || tempAt < workAt || runAt < workAt {
		t.Fatalf("writable ancestor must be installed before the private roots nested in it (work=%d temp=%d run=%d):\n%s", workAt, tempAt, runAt, strings.Join(args, " "))
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

	if linuxMountIndex(args, "--bind", expanded) < 0 {
		t.Fatalf("expected tilde-expanded writable bind of %q in:\n%s", expanded, strings.Join(args, " "))
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
	if linuxMountIndex(args, "--bind", "/tmp") >= 0 {
		t.Fatal("should not have writable /tmp bind when DenyWrite is true")
	}
	for _, root := range []string{"/tmp", "/run", resolvedHomeDir()} {
		if linuxMountIndex(args, "--tmpfs", root) < 0 || linuxMountIndex(args, "--remount-ro", root) < linuxMountIndex(args, "--tmpfs", root) {
			t.Fatalf("private root %s must be a tmpfs remounted read-only under DenyWrite:\n%s", root, joined)
		}
	}
	for _, root := range []string{"/dev", "/proc"} {
		mountIndex := linuxMountIndex(args, "--"+strings.TrimPrefix(root, "/"), root)
		readOnlyIndex := linuxMountIndex(args, "--remount-ro", root)
		if mountIndex < 0 || readOnlyIndex <= mountIndex {
			t.Fatalf("special mount %s must be remounted read-only under DenyWrite:\n%s", root, joined)
		}
	}
}

func TestLinuxSandboxDenyWriteProtectsSpecialMounts(t *testing.T) {
	skipIfNoBwrap(t)

	sb, err := New(Config{DenyWrite: true})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	cmd := exec.Command("/usr/bin/bash", "-c", `
if : 2>/dev/null > /dev/polly-deny-write-test; then exit 10; fi
if printf changed 2>/dev/null > /proc/self/comm; then exit 11; fi
printf ok > /dev/null
`)
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "Operation not permitted") || strings.Contains(string(out), "Permission denied") {
			skipOrFailBwrapUnavailable(t, err, out)
		}
		t.Fatalf("DenyWrite special-mount probe failed: %v (%s)", err, out)
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
	if bootstrapArg <= 0 || bootstrapArg+4 >= len(cmd.Args) {
		t.Fatalf("cmd.Args missing Linux environment bootstrap: %v", cmd.Args)
	}
	if cmd.Args[bootstrapArg-1] != "/proc/self/fd/"+strconv.Itoa(bootstrapFD) ||
		cmd.Args[bootstrapArg+1] != strconv.Itoa(bootstrapFD) ||
		cmd.Args[bootstrapArg+2] != strconv.Itoa(envFD) {
		t.Fatalf("cmd.Args bootstrap fd layout = %v, want bootstrap %d and env %d", cmd.Args, bootstrapFD, envFD)
	}
	authorityFDs, err := parseLinuxFDList(cmd.Args[bootstrapArg+3])
	if err != nil {
		t.Fatalf("parse authority fd list: %v", err)
	}
	wantAuthority := len(cmd.ExtraFiles) - callerFiles - 3
	if len(authorityFDs) != wantAuthority {
		t.Fatalf("authority fds = %v, want %d for ExtraFiles %v", authorityFDs, wantAuthority, cmd.ExtraFiles)
	}
	for i, fd := range authorityFDs {
		want := envFD + 1 + i
		if fd != want || fd >= seccompFD {
			t.Fatalf("authority fds = %v, want contiguous range starting at %d below seccomp fd %d", authorityFDs, envFD+1, seccompFD)
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
	args := buildBwrapArgs(Config{}, nil, commandPath)
	joined := strings.Join(args, " ")
	if linuxMountIndex(args, "--ro-bind", commandPath) < 0 {
		t.Fatalf("temporary command was not re-exposed read-only; want a bind of %q in:\n%s", commandPath, joined)
	}
	if linuxMountIndex(args, "--ro-bind", dir) >= 0 {
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

func linuxMountIndex(args []string, option, destination string) int {
	width := 2
	if option == "--bind" || option == "--ro-bind" || option == "--symlink" {
		width = 3
	}
	for i := 0; i+width-1 < len(args); i++ {
		if args[i] == option && args[i+width-1] == destination {
			return i
		}
	}
	return -1
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
	if linuxMountIndex(cmd.Args, "--tmpfs", hostTemp) < 0 || linuxMountIndex(cmd.Args, "--ro-bind", tool) < 0 {
		t.Fatalf("distinct temp/private command args missing:\n%s", strings.Join(cmd.Args, " "))
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
			if linuxMountIndex(cmd.Args, "--tmpfs", first) < 0 || linuxMountIndex(cmd.Args, "--tmpfs", second) >= 0 {
				_ = cleanup()
				t.Fatalf("Wrap recomputed private roots after TMPDIR changed:\n%s", strings.Join(cmd.Args, " "))
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
	frozen, err := prepareLinuxConfig(prepared, []string{"/tmp", root}, []string{"/run"}, nil)
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
	frozen, err := prepareLinuxConfig(prepared, tempRoots, runRoots, nil)
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
	packet, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("private sequenced-packet socketpair blocked: %v", err)
	}
	_ = unix.Close(packet[0])
	_ = unix.Close(packet[1])
	if fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0); err == nil {
		_ = unix.Close(fd)
		t.Fatal("sequenced-packet endpoint creation was allowed")
	} else if err != unix.EACCES {
		t.Fatalf("sequenced-packet socket error = %v, want EACCES", err)
	}
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
	args, err := buildBwrapArgsChecked(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if linuxMountIndex(args, "--bind", rootGit) >= 0 {
		t.Fatalf("nested protection reopened root .git:\n%s", joined)
	}
	rootRO := 0
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--ro-bind" && args[i+2] == rootGit {
			rootRO++
		}
	}
	if rootRO != 1 || linuxMountIndex(args, "--ro-bind", nested) >= 0 {
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
	workIdentity, err := captureAuthorityPathIdentities([]string{work})
	if err != nil {
		t.Fatal(err)
	}
	identities := append(cloneAuthorityPathIdentities(workIdentity), plan.ancestors...)
	identities = append(identities, plan.protected...)
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
	roots := testLinuxPrivateRoots()
	mounts, err := planLinuxMounts(cfg, roots, planLinuxGrants(cfg, roots.all()), nil, nil, plan, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	args, err := bwrapArgs(cfg, mounts, sources)
	if err != nil {
		t.Fatal(err)
	}
	if linuxMountIndex(args, "--bind", ancestor) < 0 || linuxMountIndex(args, "--ro-bind", protected) < 0 {
		t.Fatalf("deny-write plan missing its ancestor bind or protected leaf:\n%s", strings.Join(args, " "))
	}
	for i := 0; i+2 < len(args); i++ {
		if (args[i] == "--bind" || args[i] == "--ro-bind") && PathWithin(args[i+2], work) && !strings.HasPrefix(args[i+1], "/proc/self/fd/") {
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

	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	// Grant every credential's parent that is not the home directory itself,
	// so the masks inside those grants are exercised; entries directly under
	// the home directory are hidden by the private root and need no mount.
	var writable []string
	var script strings.Builder
	quote := func(path string) string { return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'" }
	for _, denied := range ExpandHome(DeniedPaths) {
		parent := filepath.Dir(denied.Path)
		switch denied.Kind {
		case DeniedPathFile:
			if err := os.MkdirAll(parent, 0755); err != nil {
				t.Fatalf("MkdirAll(%q) error = %v", parent, err)
			}
			if err := os.WriteFile(denied.Path, []byte("secret"), 0600); err != nil {
				t.Fatalf("WriteFile(%q) error = %v", denied.Path, err)
			}
		case DeniedPathDir:
			if err := os.MkdirAll(denied.Path, 0700); err != nil {
				t.Fatalf("MkdirAll(%q) error = %v", denied.Path, err)
			}
			if err := os.WriteFile(filepath.Join(denied.Path, "key"), []byte("secret"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if parent == home {
			script.WriteString("test ! -e " + quote(denied.Path) + " || exit 10\n")
			continue
		}
		if !slices.Contains(writable, parent) {
			writable = append(writable, parent)
		}
		if denied.Kind == DeniedPathFile {
			script.WriteString("test ! -s " + quote(denied.Path) + " || exit 11\n")
			script.WriteString("if (printf planted > " + quote(denied.Path) + ") 2>/dev/null; then exit 12; fi\n")
		} else {
			script.WriteString("test -z \"$(ls -A " + quote(denied.Path) + ")\" || exit 13\n")
			script.WriteString("if (touch " + quote(filepath.Join(denied.Path, "planted")) + ") 2>/dev/null; then exit 14; fi\n")
		}
	}
	script.WriteString("echo ok\n")

	sb, err := New(Config{WritablePaths: writable})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	cmd := exec.CommandContext(context.Background(), "bash", "-c", script.String())
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	for i, arg := range cmd.Args {
		if (arg == "--tmpfs" || arg == "--ro-bind") && i+2 < len(cmd.Args) && filepath.Dir(cmd.Args[i+2]) == home && cmd.Args[i+2] != home {
			if arg == "--tmpfs" || cmd.Args[i+1] == "/dev/null" {
				t.Fatalf("credential directly under the private home got its own mask: %v", cmd.Args[i:i+3])
			}
		}
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(output), "Operation not permitted") || strings.Contains(string(output), "Permission denied") {
			skipOrFailBwrapUnavailable(t, err, output)
		}
		t.Fatalf("sandboxed command failed with denied files present: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	if strings.TrimSpace(string(output)) != "ok" {
		t.Fatalf("sandboxed output = %q, want ok", output)
	}
	for _, denied := range ExpandHome(DeniedPaths) {
		if denied.Kind == DeniedPathFile {
			if data, err := os.ReadFile(denied.Path); err != nil || string(data) != "secret" {
				t.Fatalf("host credential %s changed: %q, %v", denied.Path, data, err)
			}
		}
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

// Linux twin of the Darwin whole-tree workspace E2E: without the git
// component the whole metadata tree stays unwritable, including the names
// that do not exist yet.
func TestLinuxSandboxWorkspacePresetPinsGitRoutingAndAncestors(t *testing.T) {
	skipIfNoBwrap(t)
	isolateGitConfig(t)

	work := t.TempDir()
	rootGit := filepath.Join(work, ".git")
	nestedRoot := filepath.Join(work, "packages", "nested")
	nestedGit := filepath.Join(nestedRoot, ".git")
	for _, dir := range []string{rootGit, nestedGit} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(work)
	cfg, err := ParsePreset("workspace")
	if err != nil {
		t.Fatalf("ParsePreset(workspace) error = %v", err)
	}
	sb, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	tests := []struct {
		name string
		cmd  *exec.Cmd
	}{
		{
			name: "relocate root routing directory",
			cmd:  exec.Command("mv", rootGit, filepath.Join(work, ".git-old")),
		},
		{
			name: "create absent config.worktree",
			cmd: exec.Command("bash", "-c", `: > "$1"`, "bash",
				filepath.Join(rootGit, "config.worktree")),
		},
		{
			name: "relocate mutable nested ancestor",
			cmd: exec.Command("mv", filepath.Join(work, "packages"),
				filepath.Join(work, "packages-old")),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := wrapCmdForTest(t, sb, tt.cmd); err != nil {
				t.Fatalf("Wrap() error = %v", err)
			}
			if out, err := tt.cmd.CombinedOutput(); err == nil {
				t.Fatalf("guarded mutation succeeded, output: %s", out)
			}
		})
	}
	if _, err := os.Stat(rootGit); err != nil {
		t.Fatalf("root Git routing entry moved or removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootGit, "config.worktree")); !os.IsNotExist(err) {
		t.Fatalf("config.worktree was created despite the whole-tree pin: %v", err)
	}

	note := filepath.Join(nestedRoot, "note.txt")
	cmd := exec.Command("bash", "-c", `echo ok > "$1"`, "bash", note)
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() safe workspace write error = %v", err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("safe nested working-tree write failed: %v (%s)", err, out)
	}
}

func TestLinuxSandboxWorkspaceGitPresetAllowsCommitBlocksConfig(t *testing.T) {
	skipIfNoBwrap(t)
	exerciseWorkspaceGitLeafSandbox(t)
}

func TestLinuxSandboxWorkspaceGitPresetWorktreeCommit(t *testing.T) {
	skipIfNoBwrap(t)
	exerciseWorkspaceGitWorktreeCommitSandbox(t)
}

func TestLinuxSandboxSSHAgentGrant(t *testing.T) {
	skipIfNoBwrap(t)
	exerciseSSHAgentGrantSandbox(t)
}

func TestLinuxSandboxAllowsGrantedUnixSocket(t *testing.T) {
	skipIfNoBwrap(t)

	// t.TempDir sits under os.TempDir, one of the private roots, so this
	// exercises the socket bind into the private tmpfs.
	dir := t.TempDir()
	granted := filepath.Join(dir, "granted.sock")
	sibling := filepath.Join(dir, "sibling.sock")
	for _, path := range []string{granted, sibling} {
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
	}

	sb, err := New(Config{AllowUnixSockets: []string{granted}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	dial := func(t *testing.T, target, expect string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxGrantedUnixSocketHelper$")
		cmd.Env = append(os.Environ(),
			"POLLY_TEST_GRANTED_SOCKET="+target,
			"POLLY_TEST_EXPECT="+expect)
		if err := wrapCmdForTest(t, sb, cmd); err != nil {
			t.Fatalf("Wrap() error = %v", err)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			if strings.Contains(string(out), "Operation not permitted") || strings.Contains(string(out), "Permission denied") {
				skipOrFailBwrapUnavailable(t, err, out)
			}
			t.Fatalf("socket grant probe failed: %v (%s)", err, out)
		}
	}
	t.Run("granted socket connects", func(t *testing.T) { dial(t, granted, "ok") })
	t.Run("sibling stays hidden", func(t *testing.T) { dial(t, sibling, "fail") })
}

func TestLinuxSandboxGrantedSocketOutsidePrivateRoots(t *testing.T) {
	skipIfNoBwrap(t)

	// A socket under $HOME is visible through the read-only root bind; only
	// the seccomp AF_UNIX allowance separates reachable from not.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home: %v", err)
	}
	dir, err := os.MkdirTemp(home, ".polly-unix-grant-")
	if err != nil {
		t.Fatalf("create host socket directory: %v", err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "granted.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	sb, err := New(Config{AllowUnixSockets: []string{sock}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxGrantedUnixSocketHelper$")
	cmd.Env = append(os.Environ(),
		"POLLY_TEST_GRANTED_SOCKET="+sock,
		"POLLY_TEST_EXPECT=ok")
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "Operation not permitted") || strings.Contains(string(out), "Permission denied") {
			skipOrFailBwrapUnavailable(t, err, out)
		}
		t.Fatalf("home-dir socket grant probe failed: %v (%s)", err, out)
	}
}

func TestLinuxSandboxDropsVanishedSocketGrant(t *testing.T) {
	skipIfNoBwrap(t)

	dir := t.TempDir()
	sock := filepath.Join(dir, "granted.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := New(Config{AllowUnixSockets: []string{sock}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	_ = listener.Close()
	if err := os.Remove(sock); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	// The command still runs; the dead grant is dropped, so the dial fails.
	cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxGrantedUnixSocketHelper$")
	cmd.Env = append(os.Environ(),
		"POLLY_TEST_GRANTED_SOCKET="+sock,
		"POLLY_TEST_EXPECT=fail")
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "Operation not permitted") || strings.Contains(string(out), "Permission denied") {
			skipOrFailBwrapUnavailable(t, err, out)
		}
		t.Fatalf("vanished-grant probe failed: %v (%s)", err, out)
	}
}

func TestLinuxGrantedUnixSocketHelper(t *testing.T) {
	path := os.Getenv("POLLY_TEST_GRANTED_SOCKET")
	if path == "" {
		return
	}
	conn, err := net.Dial("unix", path)
	switch os.Getenv("POLLY_TEST_EXPECT") {
	case "ok":
		if err != nil {
			t.Fatalf("dial granted socket: %v", err)
		}
		_ = conn.Close()
	case "fail":
		if err == nil {
			_ = conn.Close()
			t.Fatal("dialed a socket that should be unreachable")
		}
	}
}

func TestLinuxBuildBwrapArgsGrantedSocketBindOrdering(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "g.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	args := buildBwrapArgs(Config{AllowUnixSockets: []string{sock}}, nil)
	joined := strings.Join(args, "\x00")
	bindNeedle := strings.Join([]string{"--ro-bind", sock, sock}, "\x00")
	bindAt := strings.Index(joined, bindNeedle)
	if bindAt < 0 {
		t.Fatalf("bwrap args missing the socket bind: %v", args)
	}
	tempRoots, _ := privateLinuxRoots()
	coveringAt := -1
	for _, root := range tempRoots {
		if !PathWithin(sock, root) {
			continue
		}
		if at := strings.Index(joined, strings.Join([]string{"--tmpfs", root}, "\x00")); at >= 0 {
			coveringAt = at
		}
	}
	if coveringAt < 0 {
		t.Fatalf("no private root tmpfs covers %q in args %v", sock, args)
	}
	if bindAt < coveringAt {
		t.Fatalf("socket bind emitted before its covering private tmpfs (bind at %d, tmpfs at %d): %v", bindAt, coveringAt, args)
	}
}

func TestLinuxPolicyEnvAppliedAfterTempRewrite(t *testing.T) {
	skipIfNoBwrap(t)
	sb, err := New(Config{Env: map[string]string{"TMPDIR": "/work/scratch", "GOCACHE": "/work/scratch/go-build"}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("true")
	cmd.Env = []string{"TMPDIR=/host", "TMP=/host"}
	cleanup, err := WrapCmdWithEnvManaged(sb, cmd, map[string]string{"TMPDIR": "/explicit"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	env := strings.Join(readLinuxEnvPayload(t, cmd.ExtraFiles[1]), "\n")
	for _, want := range []string{"TMPDIR=/work/scratch", "TMP=/tmp", "GOCACHE=/work/scratch/go-build"} {
		if !strings.Contains(env, want) {
			t.Fatalf("target environment lacks %q:\n%s", want, env)
		}
	}
	for _, unwanted := range []string{"/explicit", "/host"} {
		if strings.Contains(env, unwanted) {
			t.Fatalf("policy env lost to an earlier layer (%q):\n%s", unwanted, env)
		}
	}
}

func TestLinuxBuildBwrapArgsDenyHostTempKeepsPrivateTmpfs(t *testing.T) {
	scratch := t.TempDir()
	args := buildBwrapArgs(Config{DenyHostTemp: true, WritablePaths: []string{scratch}}, nil)
	joined := strings.Join(args, " ")
	if linuxMountIndex(args, "--tmpfs", "/tmp") < 0 || linuxMountIndex(args, "--remount-ro", "/tmp") >= 0 {
		t.Fatalf("DenyHostTemp changed the private tmpfs:\n%s", joined)
	}
	if !slices.Contains(args, scratch) {
		t.Fatalf("scratch grant missing under DenyHostTemp:\n%s", joined)
	}
}

// homeFixture creates a directory directly under the real home directory, the
// one place the private root hides by default.
func homeFixture(t *testing.T, prefix string) string {
	t.Helper()
	home := resolvedHomeDir()
	if home == "" {
		t.Skip("no usable home directory")
	}
	dir, err := os.MkdirTemp(home, prefix)
	if err != nil {
		t.Skipf("cannot create a fixture under the home directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// hostVisibleTempDir creates a directory outside every private root, so the
// read-only root bind exposes it and explicit masks and grants are exercised.
func hostVisibleTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/var/tmp", "polly-sandbox-")
	if err != nil {
		if sandboxTestsRequired() {
			t.Fatalf("host-visible temp dir is required in this environment: %v", err)
		}
		t.Skipf("no host-visible temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	if isWithinAny(dir, allPrivateLinuxRoots()) {
		t.Skipf("%s lies inside a private root", dir)
	}
	return dir
}

func runSandboxedScript(t *testing.T, sb Sandbox, dir, script string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("/usr/bin/bash", append([]string{"-c", script, "bash"}, args...)...)
	cmd.Dir = dir
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatal(err)
	}
	out, runErr := cmd.CombinedOutput()
	_ = cleanup()
	return string(out), runErr
}

func TestLinuxHomeIsPrivateByDefault(t *testing.T) {
	skipIfNoBwrap(t)
	fixture := homeFixture(t, ".polly-home-private-")
	secret := filepath.Join(fixture, "secret")
	if err := os.WriteFile(secret, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	sb, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(resolvedHomeDir(), ".polly-created-"+filepath.Base(fixture))
	out, err := runSandboxedScript(t, sb, "/", `test ! -e "$1" && test ! -e "$2" && touch "$3" && test -e "$3" && test "$HOME" = "$4" && echo ok`, secret, fixture, marker, os.Getenv("HOME"))
	if err != nil {
		skipOrFailBwrapUnavailable(t, err, []byte(out))
	}
	if strings.TrimSpace(out) != "ok" {
		t.Fatalf("private home probe = %q", out)
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		_ = os.Remove(marker)
		t.Fatalf("a write into the private home reached the host: %v", err)
	}
}

func TestLinuxHomeGrantsAreReboundInsidePrivateHome(t *testing.T) {
	skipIfNoBwrap(t)
	fixture := homeFixture(t, ".polly-home-grants-")
	tree := filepath.Join(fixture, "tree")
	ro := filepath.Join(fixture, "ro")
	hidden := filepath.Join(fixture, "hidden")
	for _, dir := range []string{tree, ro, hidden} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ro, "file"), []byte("readable"), 0600); err != nil {
		t.Fatal(err)
	}
	sb, err := New(Config{WritablePaths: []string{tree}, ReadPaths: []string{ro}})
	if err != nil {
		t.Fatal(err)
	}
	script := `printf note > "$1/note" && test "$(cat "$2/file")" = readable && ! (touch "$2/x") 2>/dev/null && test ! -e "$3" && ls "$4"`
	out, err := runSandboxedScript(t, sb, "/", script, tree, ro, hidden, fixture)
	if err != nil {
		skipOrFailBwrapUnavailable(t, err, []byte(out))
	}
	if strings.Fields(out) == nil || strings.Join(strings.Fields(out), " ") != "ro tree" {
		t.Fatalf("fixture listing inside the private home = %q, want only the grants", out)
	}
	if data, err := os.ReadFile(filepath.Join(tree, "note")); err != nil || string(data) != "note" {
		t.Fatalf("writable grant write did not reach the host: %q, %v", data, err)
	}
}

// A member's slot lives under the private home beside its siblings, the
// runtime directory, and the parent checkout. Only its own tree and scratch
// plus the common Git directory are visible; the slot names other members
// deny add no mounts at all.
func TestLinuxSwarmMemberSlotsUnderPrivateHome(t *testing.T) {
	skipIfNoBwrap(t)
	fixture := homeFixture(t, ".polly-swarm-")
	project := filepath.Join(fixture, "project")
	gitDir := filepath.Join(project, ".git")
	view := filepath.Join(fixture, "worktrees", "view")
	own := filepath.Join(view, "slot-0000")
	ownTree := filepath.Join(own, "tree")
	ownScratch := filepath.Join(own, "scratch")
	sibling := filepath.Join(view, "slot-0001")
	liveScratch := filepath.Join(view, "scratch")
	for _, dir := range []string{filepath.Join(gitDir, "objects"), ownTree, ownScratch, filepath.Join(sibling, "tree"), liveScratch} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(project, "secret.txt"):     "parent secret",
		filepath.Join(gitDir, "HEAD"):            "ref: refs/heads/main\n",
		filepath.Join(gitDir, "config"):          "[core]\n",
		filepath.Join(ownTree, ".git"):           "gitdir: " + filepath.Join(gitDir, "worktrees", "slot-0000") + "\n",
		filepath.Join(sibling, "tree", "secret"): "sibling secret",
		filepath.Join(sibling, "owner"):          "other",
		filepath.Join(liveScratch, "live-notes"): "live secret",
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	denied := []string{project, liveScratch}
	for n := 0; n < 512; n++ {
		if slot := filepath.Join(view, fmt.Sprintf("slot-%04d", n)); slot != own {
			denied = append(denied, slot)
		}
	}
	cfg := Config{
		WritablePaths:  []string{ownTree, ownScratch},
		ReadPaths:      []string{gitDir},
		DenyPaths:      denied,
		DenyWritePaths: []string{gitDir, filepath.Join(ownTree, ".git")},
		Env:            map[string]string{"TMPDIR": ownScratch, "TMP": ownScratch, "TEMP": ownScratch},
	}
	sb, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	script := `test "$PWD" = "$1" && printf note > note.txt && printf scratch > "$TMPDIR/scratch-note" && ` +
		`test ! -e "$2" && test ! -e "$3" && test ! -e "$4" && ` +
		`test "$(cat "$5/HEAD")" = "ref: refs/heads/main" && ` +
		`! (printf x > "$5/planted") 2>/dev/null && ! (printf x > "$1/.git") 2>/dev/null && ` +
		`test "$HOME" = "$6" && echo ok`
	cmd := exec.Command("/usr/bin/bash", "-c", script, "bash", ownTree, sibling, liveScratch, filepath.Join(project, "secret.txt"), gitDir, os.Getenv("HOME"))
	cmd.Dir = ownTree
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string(nil), cmd.Args...)
	out, runErr := cmd.CombinedOutput()
	_ = cleanup()
	if runErr != nil {
		skipOrFailBwrapUnavailable(t, runErr, out)
	}
	if strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("member probe = %q", out)
	}
	for path, want := range map[string]string{filepath.Join(ownTree, "note.txt"): "note", filepath.Join(ownScratch, "scratch-note"): "scratch"} {
		if data, err := os.ReadFile(path); err != nil || string(data) != want {
			t.Fatalf("member write %s did not reach the host: %q, %v", path, data, err)
		}
	}
	home := resolvedHomeDir()
	if got, want := strings.Count(strings.Join(args, " "), "--tmpfs "), len(testLinuxPrivateRoots().all()); got > want {
		t.Fatalf("%d tmpfs mounts, want at most the %d private roots (slot denies must add none):\n%s", got, want, strings.Join(args, " "))
	}
	if linuxMountIndex(args, "--tmpfs", home) < 0 || linuxMountIndex(args, "--tmpfs", home) > linuxMountIndex(args, "--bind", ownTree) {
		t.Fatalf("home tmpfs must precede the member tree bind:\n%s", strings.Join(args, " "))
	}
	if linuxMountIndex(args, "--ro-bind", gitDir) < 0 {
		t.Fatalf("common git directory not re-bound read-only:\n%s", strings.Join(args, " "))
	}
	if linuxMountIndex(args, "--remount-ro", home) >= 0 {
		t.Fatalf("home tmpfs must stay writable without DenyWrite:\n%s", strings.Join(args, " "))
	}

	readOnly := Config{DenyWrite: true, DenyPaths: denied}
	readOnly, err = ExposeReadOnlyPaths(readOnly, ownTree)
	if err != nil {
		t.Fatal(err)
	}
	sb, err = New(readOnly)
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("/usr/bin/bash", "-c", `test "$(cat "$1/note.txt")" = note && ! (printf x > "$1/again") 2>/dev/null && test ! -e "$2" && echo ok`, "bash", ownTree, sibling)
	cmd.Dir = ownTree
	cleanup, err = WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatal(err)
	}
	args = append([]string(nil), cmd.Args...)
	out, runErr = cmd.CombinedOutput()
	_ = cleanup()
	if runErr != nil {
		skipOrFailBwrapUnavailable(t, runErr, out)
	}
	if strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("read-only member probe = %q", out)
	}
	if linuxMountIndex(args, "--remount-ro", home) < 0 {
		t.Fatalf("home tmpfs must be read-only under DenyWrite:\n%s", strings.Join(args, " "))
	}
}

func TestLinuxExplicitPrivateRootMasksDirectoryAndReboundsGrants(t *testing.T) {
	skipIfNoBwrap(t)
	dir := hostVisibleTempDir(t)
	pub := filepath.Join(dir, "pub")
	work := filepath.Join(dir, "work")
	for _, sub := range []string{pub, work} {
		if err := os.Mkdir(sub, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"secret": "secret", "pub/file": "public"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	sb, err := New(Config{DenyPaths: []string{dir}, ReadPaths: []string{pub}, WritablePaths: []string{work}})
	if err != nil {
		t.Fatal(err)
	}
	script := `test ! -e "$1/secret" && test "$(cat "$1/pub/file")" = public && ! (touch "$1/pub/x") 2>/dev/null && printf w > "$1/work/w" && ! (touch "$1/new") 2>/dev/null && ls "$1"`
	out, err := runSandboxedScript(t, sb, "/", script, dir)
	if err != nil {
		skipOrFailBwrapUnavailable(t, err, []byte(out))
	}
	if strings.Join(strings.Fields(out), " ") != "pub work" {
		t.Fatalf("explicit private root listing = %q, want only the grants", out)
	}
	if data, err := os.ReadFile(filepath.Join(work, "w")); err != nil || string(data) != "w" {
		t.Fatalf("writable grant inside the explicit private root did not reach the host: %q, %v", data, err)
	}
}

func TestLinuxFileMaskInsideGrantWins(t *testing.T) {
	skipIfNoBwrap(t)
	grant, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"token": "secret", "notes": "notes"} {
		if err := os.WriteFile(filepath.Join(grant, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	sb, err := New(Config{WritablePaths: []string{grant}, DenyPaths: []string{filepath.Join(grant, "token")}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := runSandboxedScript(t, sb, "/", `test ! -s "$1/token" && ! (printf x > "$1/token") 2>/dev/null && test "$(cat "$1/notes")" = notes && echo ok`, grant)
	if err != nil {
		skipOrFailBwrapUnavailable(t, err, []byte(out))
	}
	if strings.TrimSpace(out) != "ok" {
		t.Fatalf("file mask probe = %q", out)
	}
	if data, err := os.ReadFile(filepath.Join(grant, "token")); err != nil || string(data) != "secret" {
		t.Fatalf("masked file changed on the host: %q, %v", data, err)
	}
}

func TestLinuxPlanMasksClassifiesAndDrops(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Keep the built-in credential list out of the picture: none of its
	// entries exist under this home.
	t.Setenv("HOME", root)
	priv := filepath.Join(root, "priv")
	grant := filepath.Join(priv, "grant")
	grant2 := filepath.Join(root, "grant2")
	outside := filepath.Join(root, "outside")
	for _, dir := range []string{grant, grant2, filepath.Join(priv, "secret"), filepath.Join(grant, "secret"), filepath.Join(outside, "inner")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(grant, "token"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfg := Config{
		ReadPaths: []string{grant, grant2},
		DenyPaths: []string{
			filepath.Join(root, "missing"),
			filepath.Join(priv, "secret"),
			filepath.Join(grant, "secret"),
			filepath.Join(grant, "token"),
			outside,
			filepath.Join(root, "link"),
			filepath.Join(outside, "inner"),
			grant2,
		},
	}
	grants := planLinuxGrants(cfg, []string{priv})
	masks, islands, err := planLinuxMasks(cfg, grants, []string{priv})
	if err != nil {
		t.Fatal(err)
	}
	want := []linuxMask{{path: outside, dir: true}, {path: filepath.Join(grant, "secret"), dir: true}, {path: filepath.Join(grant, "token"), dir: false}}
	if !slices.Equal(masks, want) {
		t.Fatalf("masks = %+v, want %+v", masks, want)
	}
	if !slices.Equal(islands, []string{grant2}) {
		t.Fatalf("islands = %v, want the grant that tied with a deny", islands)
	}
	if os.Geteuid() != 0 {
		locked := filepath.Join(root, "locked")
		if err := os.Mkdir(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		if _, _, err := planLinuxMasks(Config{DenyPaths: []string{filepath.Join(locked, "secret")}}, nil, nil); err == nil {
			t.Fatal("an uninspectable denied path must fail closed")
		}
	}
}

func TestLinuxSymlinkedGrantUnderPrivateRootIsRecreated(t *testing.T) {
	skipIfNoBwrap(t)
	fixture := homeFixture(t, ".polly-symlink-")
	targets := hostVisibleTempDir(t)
	first := filepath.Join(targets, "first")
	second := filepath.Join(targets, "second")
	for _, dir := range []string{first, second} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "credentials"), []byte(filepath.Base(dir)+"-secret"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(fixture, "aws")
	if err := os.Symlink(first, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	sb, err := New(Config{ReadPaths: []string{link}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/usr/bin/bash", "-c", `cat "$1/credentials"`, "bash", link)
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if idx := linuxMountIndex(cmd.Args, "--symlink", link); idx < 0 || cmd.Args[idx+1] != first {
		_ = cleanup()
		t.Fatalf("granted symlink not recreated with its frozen target:\n%s", strings.Join(cmd.Args, " "))
	}
	out, runErr := cmd.CombinedOutput()
	_ = cleanup()
	if runErr != nil {
		skipOrFailBwrapUnavailable(t, runErr, out)
	}
	if string(out) != "first-secret" {
		t.Fatalf("read through the recreated symlink = %q, want first-secret", out)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, link); err != nil {
		t.Fatal(err)
	}
	again, err := runSandboxedScript(t, sb, "/", `cat "$1/credentials"`, link)
	if err != nil {
		skipOrFailBwrapUnavailable(t, err, []byte(again))
	}
	if again != "first-secret" {
		t.Fatalf("a host retarget changed the sandbox view: %q, want first-secret", again)
	}
}

func TestLinuxSymlinkComponentInsideGrantIsNotRecreated(t *testing.T) {
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(work, "real", "sub")
	if err := os.MkdirAll(real, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(work, "real"), filepath.Join(work, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfg, err := PrepareConfig(Config{WritablePaths: []string{work}, ReadPaths: []string{filepath.Join(work, "link", "sub")}})
	if err != nil {
		t.Fatal(err)
	}
	args := buildBwrapArgs(cfg, nil)
	if linuxMountIndex(args, "--symlink", filepath.Join(work, "link")) >= 0 {
		t.Fatalf("symlink inside a writable grant must not be recreated:\n%s", strings.Join(args, " "))
	}
	if linuxMountIndex(args, "--ro-bind", real) >= 0 {
		t.Fatalf("read grant inside a writable grant must not become a read-only island:\n%s", strings.Join(args, " "))
	}
}

func TestLinuxMountOrderIsDepthFirstAndDestinationsUnique(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	temp, run, home := filepath.Join(base, "tmp"), filepath.Join(base, "run"), filepath.Join(base, "home")
	proj := filepath.Join(home, "proj")
	secret := filepath.Join(proj, "secret")
	pub := filepath.Join(secret, "pub")
	rt := filepath.Join(home, ".rt")
	tree := filepath.Join(rt, "slot", "tree")
	scratch := filepath.Join(temp, "x")
	for _, dir := range []string{run, pub, tree, scratch} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{WritablePaths: []string{proj, pub, tree, scratch}, DenyPaths: []string{secret, rt}}
	roots := linuxPrivateRootSet{temp: []string{temp}, run: []string{run}, home: []string{home}}
	grants := planLinuxGrants(cfg, roots.all())
	masks, islands, err := planLinuxMasks(cfg, grants, roots.all())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planLinuxMounts(cfg, roots, grants, masks, islands, denyWriteMountPlan{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for i, op := range plan.ops {
		if seen[op.dest] {
			t.Fatalf("duplicate destination %q in %+v", op.dest, plan.ops)
		}
		seen[op.dest] = true
		for _, earlier := range plan.ops[:i] {
			if PathWithin(earlier.dest, op.dest) && earlier.dest != op.dest {
				t.Fatalf("descendant %q emitted before ancestor %q", earlier.dest, op.dest)
			}
		}
	}
	args, err := bwrapArgs(cfg, plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	lastOp := 0
	firstRemount := len(args)
	for i, arg := range args {
		if arg == "--tmpfs" || arg == "--bind" || arg == "--ro-bind" {
			lastOp = i
		}
		if arg == "--remount-ro" && i < firstRemount {
			firstRemount = i
		}
	}
	if firstRemount < lastOp {
		t.Fatalf("remounts must follow every mount:\n%s", strings.Join(args, " "))
	}
	for _, want := range [][2]string{{"--tmpfs", home}, {"--bind", proj}, {"--tmpfs", secret}, {"--bind", pub}, {"--tmpfs", temp}, {"--bind", scratch}, {"--bind", tree}, {"--remount-ro", secret}} {
		if linuxMountIndex(args, want[0], want[1]) < 0 {
			t.Fatalf("missing %s %s:\n%s", want[0], want[1], strings.Join(args, " "))
		}
	}
	if linuxMountIndex(args, "--tmpfs", rt) >= 0 {
		t.Fatalf("a denied directory inside the private home needs no mask:\n%s", strings.Join(args, " "))
	}
}

func TestLinuxDenyWriteLeafIsOnlyReinstalledInsideWritableRegions(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "home")
	hiddenLeaf := filepath.Join(home, "hidden", ".git")
	ro := filepath.Join(home, "ro")
	roLeaf := filepath.Join(ro, ".git")
	work := filepath.Join(home, "work")
	workLeaf := filepath.Join(work, "nested", ".git")
	for _, dir := range []string{hiddenLeaf, roLeaf, workLeaf} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{WritablePaths: []string{work}, ReadPaths: []string{ro}, DenyWritePaths: []string{hiddenLeaf, roLeaf, workLeaf}}
	roots := linuxPrivateRootSet{temp: []string{filepath.Join(base, "tmp")}, run: []string{filepath.Join(base, "run")}, home: []string{home}}
	for _, dir := range roots.all() {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	denyWrite, err := planDenyWriteMounts(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planLinuxMounts(cfg, roots, planLinuxGrants(cfg, roots.all()), nil, nil, denyWrite, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	args, err := bwrapArgs(cfg, plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	if linuxMountIndex(args, "--ro-bind", hiddenLeaf) >= 0 || linuxMountIndex(args, "--ro-bind", roLeaf) >= 0 {
		t.Fatalf("protected leaves inside hidden or read-only regions must not be re-exposed:\n%s", strings.Join(args, " "))
	}
	if linuxMountIndex(args, "--ro-bind", workLeaf) < linuxMountIndex(args, "--bind", work) || linuxMountIndex(args, "--bind", filepath.Join(work, "nested")) < 0 {
		t.Fatalf("protected leaf inside the writable grant must follow its pinned ancestors:\n%s", strings.Join(args, " "))
	}
}

func TestLinuxWorkingDirectoryInsidePrivateHomeResetsUnlessGranted(t *testing.T) {
	skipIfNoBwrap(t)
	fixture := homeFixture(t, ".polly-cwd-")
	for _, cfg := range []Config{DefaultConfig(), {WritablePaths: []string{fixture}}} {
		sb, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		out, err := runSandboxedScript(t, sb, fixture, "pwd")
		if err != nil {
			skipOrFailBwrapUnavailable(t, err, []byte(out))
		}
		want := "/"
		if slices.Contains(cfg.WritablePaths, fixture) {
			want = fixture
		}
		if strings.TrimSpace(out) != want {
			t.Fatalf("cwd with grants %v = %q, want %q", cfg.WritablePaths, out, want)
		}
	}
}

func TestLinuxDenyWriteRemountsHomeReadOnly(t *testing.T) {
	skipIfNoBwrap(t)
	sb, err := New(Config{DenyWrite: true})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/usr/bin/bash", "-c", `if (touch "$HOME/polly-deny-write") 2>/dev/null; then exit 20; fi; echo ok`)
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if linuxMountIndex(cmd.Args, "--remount-ro", resolvedHomeDir()) < 0 {
		_ = cleanup()
		t.Fatalf("home tmpfs not remounted read-only:\n%s", strings.Join(cmd.Args, " "))
	}
	out, runErr := cmd.CombinedOutput()
	_ = cleanup()
	if runErr != nil {
		skipOrFailBwrapUnavailable(t, runErr, out)
	}
	if strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("deny-write home probe = %q", out)
	}
}

func TestLinuxTMPDIRUnderHomeStaysPrivate(t *testing.T) {
	skipIfNoBwrap(t)
	tmp := homeFixture(t, ".polly-tmp-")
	t.Setenv("TMPDIR", tmp)
	if err := os.WriteFile(filepath.Join(tmp, "marker"), []byte("host"), 0600); err != nil {
		t.Fatal(err)
	}
	sb, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/usr/bin/bash", "-c", `test ! -e "$1/marker" && touch "$1/inside" && echo ok`, "bash", tmp)
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatal(err)
	}
	home := resolvedHomeDir()
	if linuxMountIndex(cmd.Args, "--tmpfs", tmp) < linuxMountIndex(cmd.Args, "--tmpfs", home) || linuxMountIndex(cmd.Args, "--bind", tmp) >= 0 {
		_ = cleanup()
		t.Fatalf("a TMPDIR under the home directory must be a nested private tmpfs, never a bind:\n%s", strings.Join(cmd.Args, " "))
	}
	out, runErr := cmd.CombinedOutput()
	_ = cleanup()
	if runErr != nil {
		skipOrFailBwrapUnavailable(t, runErr, out)
	}
	if strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("nested TMPDIR probe = %q", out)
	}
	if _, err := os.Lstat(filepath.Join(tmp, "inside")); !os.IsNotExist(err) {
		t.Fatalf("a write into the private TMPDIR reached the host: %v", err)
	}
}

func TestLinuxHomeExactGrantIsRejected(t *testing.T) {
	home := resolvedHomeDir()
	if home == "" {
		t.Skip("no usable home directory")
	}
	for _, cfg := range []Config{{WritablePaths: []string{home}}, {ReadPaths: []string{home}}} {
		if _, err := PrepareConfig(cfg); err == nil || !strings.Contains(err.Error(), "home directory") {
			t.Fatalf("PrepareConfig(%+v) error = %v, want the home directory rejected", cfg, err)
		}
	}
	if _, err := ExposeReadOnlyPaths(Config{}, home); err == nil || !strings.Contains(err.Error(), "home directory") {
		t.Fatalf("ExposeReadOnlyPaths(home) error = %v, want the home directory rejected", err)
	}
}

func TestLinuxNewRejectsMissingOrRootHome(t *testing.T) {
	skipIfNoBwrap(t)
	for _, home := range []string{"/nonexistent-polly-home", "/"} {
		t.Setenv("HOME", home)
		if _, err := New(DefaultConfig()); err == nil {
			t.Fatalf("New() with HOME=%s succeeded, want a private-root error", home)
		}
	}
}

func TestLinuxWritableGrantEqualToDenyBindsReadOnly(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	temp, run, home := filepath.Join(base, "tmp"), filepath.Join(base, "run"), filepath.Join(base, "home")
	shared := filepath.Join(home, "proj", "shared")
	for _, dir := range []string{temp, run, shared} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{WritablePaths: []string{shared}, DenyPaths: []string{shared}}
	roots := linuxPrivateRootSet{temp: []string{temp}, run: []string{run}, home: []string{home}}
	grants := planLinuxGrants(cfg, roots.all())
	masks, islands, err := planLinuxMasks(cfg, grants, roots.all())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planLinuxMounts(cfg, roots, grants, masks, islands, denyWriteMountPlan{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range plan.ops {
		if op.dest == shared && op.kind == linuxMountBind {
			t.Fatalf("a writable grant tying a denied path must not be bound read-write: %+v", plan.ops)
		}
	}
	found := false
	for _, op := range plan.ops {
		if op.dest == shared && op.kind == linuxMountROBind {
			found = true
		}
	}
	if !found {
		t.Fatalf("a writable grant tying a denied path must be bound read-only: %+v", plan.ops)
	}
}

func TestLinuxDenyEqualToPrivateRootNeedsNoMask(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run, home := filepath.Join(base, "run"), filepath.Join(base, "home")
	for _, dir := range []string{run, home} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	// TMPDIR is the home directory and the home is also denied explicitly.
	cfg := Config{DenyPaths: []string{home, home}}
	roots := linuxPrivateRootSet{temp: []string{home}, run: []string{run}, home: []string{home}}
	if got := roots.all(); len(got) != 2 {
		t.Fatalf("roots.all() = %v, want each root once", got)
	}
	grants := planLinuxGrants(cfg, roots.all())
	masks, islands, err := planLinuxMasks(cfg, grants, roots.all())
	if err != nil {
		t.Fatal(err)
	}
	if len(masks) != 0 || len(islands) != 0 {
		t.Fatalf("masks = %+v islands = %v, want none for a deny equal to a private root", masks, islands)
	}
	if _, err := planLinuxMounts(cfg, roots, grants, masks, islands, denyWriteMountPlan{}, nil, nil); err != nil {
		t.Fatalf("planLinuxMounts() = %v, want no conflicting mounts", err)
	}
}

func TestLinuxResolvConfReexposureHonorsDeny(t *testing.T) {
	real, err := filepath.EvalSymlinks("/etc/resolv.conf")
	if err != nil || !isWithinAny(real, []string{"/run"}) {
		t.Skipf("/etc/resolv.conf does not resolve into /run (%q, %v)", real, err)
	}
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	roots := linuxPrivateRootSet{temp: []string{"/tmp"}, run: []string{"/run"}, home: []string{home}}
	for _, denied := range []string{real, "/etc/resolv.conf"} {
		cfg := Config{AllowNetwork: true, DenyPaths: []string{denied}}
		grants := planLinuxGrants(cfg, roots.all())
		masks, islands, err := planLinuxMasks(cfg, grants, roots.all())
		if err != nil {
			t.Fatal(err)
		}
		plan, err := planLinuxMounts(cfg, roots, grants, masks, islands, denyWriteMountPlan{}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, op := range plan.ops {
			if op.dest != real {
				continue
			}
			found = true
			if op.kind != linuxMountROBind || op.source != "/dev/null" {
				t.Fatalf("denying %q must bind /dev/null over the re-exposed resolv.conf, got %+v", denied, op)
			}
		}
		if !found || !plan.resolvInPrivateRun {
			t.Fatalf("denying %q lost the resolv.conf mount entirely: %+v", denied, plan.ops)
		}
	}
}

func TestLinuxIslandReadGrantAndDenyWriteLeafShareOneBind(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	temp, run, home := filepath.Join(base, "tmp"), filepath.Join(base, "run"), filepath.Join(base, "home")
	work := filepath.Join(home, "proj")
	vendored := filepath.Join(work, "vendor")
	for _, dir := range []string{temp, run, vendored} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{WritablePaths: []string{work}, ReadPaths: []string{vendored}, DenyPaths: []string{vendored}, DenyWritePaths: []string{vendored}}
	roots := linuxPrivateRootSet{temp: []string{temp}, run: []string{run}, home: []string{home}}
	grants := planLinuxGrants(cfg, roots.all())
	masks, islands, err := planLinuxMasks(cfg, grants, roots.all())
	if err != nil {
		t.Fatal(err)
	}
	denyWritePlan, err := planDenyWriteMounts(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planLinuxMounts(cfg, roots, grants, masks, islands, denyWritePlan, nil, nil)
	if err != nil {
		t.Fatalf("a read grant tying a denied path and a deny-write leaf at the same path must plan: %v", err)
	}
	binds := 0
	for _, op := range plan.ops {
		if op.dest == vendored {
			binds++
			if op.kind != linuxMountROBind {
				t.Fatalf("the shared path must be bound read-only, got %+v", op)
			}
		}
	}
	if binds != 1 {
		t.Fatalf("want exactly one read-only bind at %s, got %d in %+v", vendored, binds, plan.ops)
	}
}
