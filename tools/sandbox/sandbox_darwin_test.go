//go:build darwin

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
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func skipIfNoSandboxExec(t *testing.T) {
	t.Helper()
	if err := validateDarwinSandboxExecExecutable(darwinSandboxExecPath); err != nil {
		if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") == "1" {
			t.Fatalf("sandbox-exec is required in this environment: %v", err)
		}
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
	if !strings.Contains(profile, `(deny file-write-unlink (literal "/private/tmp"))`) {
		t.Fatalf("profile does not pin automatic temp grant:\n%s", profile)
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
	if !strings.Contains(profile, "(deny network-outbound (remote unix-socket))") {
		t.Fatalf("profile should still deny host Unix-domain sockets:\n%s", profile)
	}
	if !strings.Contains(profile, `(allow network-outbound (remote unix-socket (path-literal "/private/var/run/mDNSResponder")))`) {
		t.Fatalf("profile should re-allow only the system DNS Unix socket:\n%s", profile)
	}
}

func TestWrapCmd(t *testing.T) {
	skipIfNoSandboxExec(t)

	sb, err := New(Config{WritablePaths: []string{"/tmp"}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	cmd := exec.Command("bash", "-c", "echo hello")
	origPath, err := resolvedExecutablePath(cmd)
	if err != nil {
		t.Fatal(err)
	}
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	defer func() { _ = cleanup() }()

	if cmd.Path != "/usr/bin/sandbox-exec" {
		t.Fatalf("cmd.Path = %q, want /usr/bin/sandbox-exec", cmd.Path)
	}
	if len(cmd.Args) < 11 {
		t.Fatalf("cmd.Args too short: %v", cmd.Args)
	}
	if cmd.Args[0] != "sandbox-exec" || cmd.Args[1] != "-p" {
		t.Fatalf("cmd.Args prefix = %v, want [sandbox-exec -p ...]", cmd.Args[:2])
	}
	pipeCount, err := strconv.Atoi(cmd.Args[6])
	if err != nil || pipeCount < 1 || cmd.Args[7] != "3" {
		t.Fatalf("cmd.Args bootstrap = %v, want fixed Perl bootstrap and anonymous pipes from fd 3", cmd.Args[3:8])
	}
	if cmd.Env == nil || len(cmd.Env) != 0 {
		t.Fatalf("sandbox-exec Env = %v, want non-nil empty wrapper environment", cmd.Env)
	}
	if len(cmd.ExtraFiles) != pipeCount {
		t.Fatalf("cmd.ExtraFiles = %d, want %d environment pipe readers", len(cmd.ExtraFiles), pipeCount)
	}
	if tail := cmd.Args[len(cmd.Args)-4:]; tail[0] != origPath || tail[1] != "bash" || tail[2] != "-c" || tail[3] != "echo hello" {
		t.Fatalf("cmd.Args tail = %v, want [%s bash -c echo hello]", tail, origPath)
	}
}

func TestDarwinLegacyWrapFailsBeforeAllocatingDescriptors(t *testing.T) {
	skipIfNoSandboxExec(t)
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

func TestDarwinTargetEnvironmentIsOnlyInAnonymousPipes(t *testing.T) {
	skipIfNoSandboxExec(t)

	sb, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	callerFile, err := os.CreateTemp(t.TempDir(), "caller-extra-*")
	if err != nil {
		t.Fatal(err)
	}
	defer callerFile.Close()
	cmd := exec.Command("/usr/bin/true")
	cmd.Env = []string{"SAFE_VAR=ambient-visible-value"}
	cmd.ExtraFiles = []*os.File{callerFile}
	cleanup, err := WrapCmdWithEnvManaged(sb, cmd, map[string]string{
		"GITHUB_TOKEN": "explicit-wrapper-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	pipeCount, parseErr := strconv.Atoi(cmd.Args[6])
	if parseErr != nil || pipeCount < 1 || len(cmd.ExtraFiles) != 1+pipeCount || cmd.Args[3] != darwinEnvBootstrapPath || cmd.Args[4] != "-e" || cmd.Args[5] != darwinEnvBootstrapCode || cmd.Args[7] != "4" {
		t.Fatalf("caller/bootstrap descriptors = %d, argv = %v", len(cmd.ExtraFiles), cmd.Args)
	}
	bootstrapPipes := append([]*os.File(nil), cmd.ExtraFiles[1:]...)
	defer func() { _ = cleanup() }()

	if cmd.Env == nil || len(cmd.Env) != 0 {
		t.Fatalf("sandbox-exec Env = %v, want non-nil empty", cmd.Env)
	}
	joined := strings.Join(cmd.Args, "\x00")
	for _, value := range []string{"ambient-visible-value", "explicit-wrapper-secret"} {
		if strings.Contains(joined, value) {
			t.Fatalf("wrapper argv exposes target environment value %q: %v", value, cmd.Args)
		}
	}
	var data []byte
	for _, pipe := range bootstrapPipes {
		info, err := pipe.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeNamedPipe == 0 || info.Mode().IsRegular() {
			t.Fatalf("environment transport %q mode = %v, want anonymous pipe", pipe.Name(), info.Mode())
		}
		flags, err := unix.FcntlInt(pipe.Fd(), unix.F_GETFL, 0)
		if err != nil {
			t.Fatal(err)
		}
		if flags&unix.O_ACCMODE != unix.O_RDONLY {
			t.Fatalf("environment transport %q flags = %#x, want read-only", pipe.Name(), flags)
		}
		chunk, err := io.ReadAll(pipe)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, chunk...)
	}
	wantPayload, err := darwinEnvBootstrapPayload([]string{
		"SAFE_VAR=ambient-visible-value",
		"GITHUB_TOKEN=explicit-wrapper-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, wantPayload) {
		t.Fatalf("anonymous environment payload = %q, want %q", data, wantPayload)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	for _, pipe := range bootstrapPipes {
		if _, err := pipe.Stat(); err == nil {
			t.Fatal("managed cleanup left environment pipe open")
		}
	}
	if len(cmd.ExtraFiles) != 1 || cmd.ExtraFiles[0] != callerFile {
		t.Fatalf("managed cleanup removed caller descriptor: %v", cmd.ExtraFiles)
	}
	if _, err := callerFile.Stat(); err != nil {
		t.Fatalf("managed cleanup closed caller descriptor: %v", err)
	}
}

func TestDarwinEnvBootstrapPayloadRoundTrip(t *testing.T) {
	want := []string{
		"PLAIN=value",
		"NOT-POSIX=still valid to execve",
		"QUOTED=$(printf injected) `printf injected` $HOME; exit 91",
		"MULTILINE=line one\nline two",
	}
	payload, err := darwinEnvBootstrapPayload(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseDarwinEnvBootstrapPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("payload round trip = %q, want %q", got, want)
	}
}

func TestDarwinEnvBootstrapPayloadRejectsTruncation(t *testing.T) {
	payload, err := darwinEnvBootstrapPayload([]string{"FIRST=one", "SECOND=two"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseDarwinEnvBootstrapPayload(payload[:len(payload)-len("SECOND=two\x00")]); err == nil {
		t.Fatal("length-framed payload accepted entry-boundary truncation")
	}
}

func TestDarwinEnvBootstrapPayloadLimitLeavesCommandUntouched(t *testing.T) {
	callerFile, err := os.CreateTemp(t.TempDir(), "caller-extra-*")
	if err != nil {
		t.Fatal(err)
	}
	defer callerFile.Close()
	cmd := exec.Command("/usr/bin/true")
	cmd.ExtraFiles = []*os.File{callerFile}
	_, _, err = attachDarwinEnvBootstrap(cmd, []string{
		"SAFE_LARGE_ENV=" + strings.Repeat("x", darwinEnvBootstrapMaxPayload),
	})
	if err == nil {
		t.Fatal("oversized environment payload was accepted")
	}
	if len(cmd.ExtraFiles) != 1 || cmd.ExtraFiles[0] != callerFile {
		t.Fatalf("failed environment setup changed caller descriptors: %v", cmd.ExtraFiles)
	}
}

func TestDarwinEnvBootstrapExecutableIsTrusted(t *testing.T) {
	if err := validateDarwinSandboxExecExecutable(darwinSandboxExecPath); err != nil {
		t.Fatalf("system sandbox-exec rejected: %v", err)
	}
	if err := validateDarwinEnvBootstrapExecutable(darwinEnvBootstrapPath); err != nil {
		t.Fatalf("system bootstrap rejected: %v", err)
	}
	if err := validateDarwinEnvBootstrapExecutable(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing bootstrap executable was accepted")
	}
	notExecutable := filepath.Join(t.TempDir(), "bootstrap")
	if err := os.WriteFile(notExecutable, []byte("not executable"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateDarwinEnvBootstrapExecutable(notExecutable); err == nil {
		t.Fatal("non-executable bootstrap was accepted")
	}
}

func TestDarwinExplicitTargetEnvStartsInsideSandbox(t *testing.T) {
	skipIfNoSandboxExec(t)

	sb, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", `IFS= read -r line; fd=closed; test -e /dev/fd/3 && fd=open; printf '%s|%s|%s|%s|%s' "$GITHUB_TOKEN" "$SAFE_VAR" "$1" "$line" "$fd"`, "bash", "argument with spaces")
	cmd.Stdin = strings.NewReader("stdin delivery\n")
	cmd.Env = []string{"GITHUB_TOKEN=ambient-secret", "SAFE_VAR=kept"}
	cleanup, err := WrapCmdWithEnvManaged(sb, cmd, map[string]string{"GITHUB_TOKEN": "explicit-secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	if cmd.Env == nil || len(cmd.Env) != 0 {
		t.Fatalf("sandbox-exec Env = %v, want non-nil empty", cmd.Env)
	}
	joined := strings.Join(cmd.Args, " ")
	for _, value := range []string{"ambient-secret", "explicit-secret", "kept"} {
		if strings.Contains(joined, value) {
			t.Fatalf("wrapper argv exposes target environment value %q: %v", value, cmd.Args)
		}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandboxed target failed: %v (%s)", err, out)
	}
	if string(out) != "explicit-secret|kept|argument with spaces|stdin delivery|closed" {
		t.Fatalf("target environment/argv/stdin = %q", out)
	}
}

func TestDarwinLargeTargetEnvironmentUsesMultipleAnonymousPipes(t *testing.T) {
	skipIfNoSandboxExec(t)

	sb, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	const valueSize = 96 << 10
	value := strings.Repeat("x", valueSize)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/perl", "-e", `
my @open = grep { -e "/dev/fd/$_" } (3..1024);
die "environment descriptors survived exec: @open" if @open;
print length($ENV{"SAFE_LARGE_ENV"} // "");
`)
	cmd.Env = []string{"SAFE_LARGE_ENV=" + value}
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	pipeCount, err := strconv.Atoi(cmd.Args[6])
	if err != nil || pipeCount < 2 || len(cmd.ExtraFiles) != pipeCount {
		t.Fatalf("large environment used %d descriptors, argv = %v", len(cmd.ExtraFiles), cmd.Args[:8])
	}
	if strings.Contains(strings.Join(cmd.Args, "\x00"), value) {
		t.Fatal("large target environment leaked into wrapper argv")
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("large environment target failed: %v (%s)", err, out)
	}
	if string(out) != strconv.Itoa(valueSize) {
		t.Fatalf("large target environment length = %q, want %d", out, valueSize)
	}
}

func TestDarwinAnonymousEnvPipesManagedCleanupWithoutSuccessfulStart(t *testing.T) {
	skipIfNoSandboxExec(t)

	for _, tc := range []struct {
		name      string
		failStart bool
	}{
		{name: "without start"},
		{name: "failed start", failStart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb, err := New(Config{})
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/usr/bin/true")
			cmd.Env = []string{"SAFE_LARGE_ENV=" + strings.Repeat("x", 96<<10)}
			cleanup, err := WrapCmdManaged(sb, cmd)
			if err != nil {
				t.Fatal(err)
			}
			owned := append([]*os.File(nil), cmd.ExtraFiles...)
			if len(owned) < 2 {
				t.Fatalf("large environment used %d pipe shards, want multiple", len(owned))
			}
			if tc.failStart {
				cmd.Path = filepath.Join(t.TempDir(), "missing-sandbox-exec")
				if err := cmd.Start(); err == nil {
					t.Fatal("command unexpectedly started")
				}
			}
			if err := cleanup(); err != nil {
				t.Fatal(err)
			}
			if len(cmd.ExtraFiles) != 0 {
				t.Fatalf("managed cleanup retained descriptors: %v", cmd.ExtraFiles)
			}
			for _, pipe := range owned {
				if _, err := pipe.Stat(); err == nil {
					t.Fatalf("managed cleanup left pipe %q open", pipe.Name())
				}
			}
		})
	}
}

func TestDarwinTruncatedAnonymousEnvironmentNeverExecutesTarget(t *testing.T) {
	skipIfNoSandboxExec(t)

	sb, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "target-ran")
	cmd := exec.Command("/bin/sh", "-c", `printf ran > "$1"`, "sh", marker)
	cmd.Env = []string{"SAFE_VAR=value"}
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	one := []byte{0}
	if _, err := io.ReadFull(cmd.ExtraFiles[0], one); err != nil {
		t.Fatal(err)
	}
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Fatalf("bootstrap accepted truncated environment: %s", out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("target ran after truncated environment: %v", err)
	}
}

func TestDarwinStrictAllowEnvReachesTargetExactly(t *testing.T) {
	skipIfNoSandboxExec(t)

	sb, err := New(Config{AllowEnv: []string{"ONLY"}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/usr/bin/env")
	cmd.Env = []string{
		"ONLY=kept",
		"OMIT=blocked",
		"PWD=/must-not-reappear",
		"SHLVL=99",
	}
	cleanup, err := WrapCmdWithEnvManaged(sb, cmd, map[string]string{
		"EXPLICIT":  "given",
		"NOT-POSIX": "execve-valid",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandboxed env target failed: %v (%s)", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	want := map[string]bool{
		"ONLY=kept":              true,
		"EXPLICIT=given":         true,
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

func TestSandboxPreservesExecutableResolvedBeforeEnvFiltering(t *testing.T) {
	skipIfNoSandboxExec(t)

	dir := t.TempDir()
	toolPath := filepath.Join(dir, "p2-resolved-tool")
	if err := os.WriteFile(toolPath, []byte("#!/bin/sh\necho resolved\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd := exec.Command("p2-resolved-tool")
	resolvedPath, err := resolvedExecutablePath(cmd)
	if err != nil {
		t.Fatal(err)
	}

	sb, err := New(Config{AllowEnv: []string{"HOME"}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	foundResolved := false
	for _, arg := range cmd.Args[5:] {
		if arg == resolvedPath {
			foundResolved = true
			break
		}
	}
	if !foundResolved {
		t.Fatalf("wrapped args do not contain resolved executable %q: %v", resolvedPath, cmd.Args)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("resolved command failed without PATH in its environment: %v (%s)", err, out)
	}
	if strings.TrimSpace(string(out)) != "resolved" {
		t.Fatalf("output = %q, want resolved", strings.TrimSpace(string(out)))
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
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
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
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
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
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if err := cmd.Run(); err == nil {
		t.Fatal("expected network access to be blocked")
	}
}

func TestDarwinAllowNetworkBlocksHostUnixSocketsButAllowsTCP(t *testing.T) {
	skipIfNoSandboxExec(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home: %v", err)
	}
	hostDir, err := os.MkdirTemp(home, ".polly-unix-socket-")
	if err != nil {
		t.Fatalf("create host socket directory: %v", err)
	}
	defer os.RemoveAll(hostDir)

	socketPath := filepath.Join(hostDir, "listener.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatalf("listen on host Unix socket: %v", err)
	}
	defer listener.Close()
	if err := listener.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set listener deadline: %v", err)
	}
	accepted := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.AcceptUnix()
		if acceptErr == nil {
			_ = conn.Close()
		}
		accepted <- acceptErr
	}()

	sb, err := New(Config{AllowNetwork: true})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/nc", "-U", socketPath)
	cmd.Stdin = strings.NewReader("probe")
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	runErr := cmd.Run()
	if err := cleanup(); err != nil {
		t.Fatalf("close Unix client sandbox files: %v", err)
	}
	if runErr == nil {
		t.Fatal("allowNetwork sandbox connected to a host Unix-domain socket")
	}
	if acceptErr := <-accepted; acceptErr == nil {
		t.Fatal("host Unix-domain listener accepted a sandboxed connection")
	}

	tcpListener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen on host TCP socket: %v", err)
	}
	defer tcpListener.Close()
	if err := tcpListener.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set TCP listener deadline: %v", err)
	}
	tcpAccepted := make(chan error, 1)
	go func() {
		conn, acceptErr := tcpListener.AcceptTCP()
		if acceptErr == nil {
			_ = conn.Close()
		}
		tcpAccepted <- acceptErr
	}()

	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	port := fmt.Sprintf("%d", tcpListener.Addr().(*net.TCPAddr).Port)
	cmd = exec.CommandContext(ctx, "/usr/bin/nc", "127.0.0.1", port)
	cmd.Stdin = strings.NewReader("probe")
	cleanup, err = WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatalf("Wrap() TCP client error = %v", err)
	}
	runErr = cmd.Run()
	if err := cleanup(); err != nil {
		t.Fatalf("close TCP client sandbox files: %v", err)
	}
	if acceptErr := <-tcpAccepted; acceptErr != nil {
		t.Fatalf("host TCP listener did not accept sandboxed connection: %v", acceptErr)
	}
	if runErr != nil {
		t.Fatalf("allowNetwork sandbox could not connect over TCP: %v", runErr)
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
	if !strings.Contains(profile, "(deny file-read* (literal") || !strings.Contains(profile, "(deny file-write* (literal") {
		t.Fatal("profile missing denied-entry literal rules")
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
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
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
			if err := wrapCmdForTest(t, sb, cmd); err != nil {
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
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
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
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("write to base dir failed: %v", err)
	}

	// Write to extra dir should also work
	cmd = exec.CommandContext(context.Background(), "bash", "-c", "echo ok > "+filepath.Join(extraDir, "b.txt"))
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
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
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
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

func TestBuildProfileNarrowedReadAliasDoesNotAllowParent(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	alias := filepath.Join(root, "alias")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "allowed.txt"), []byte("allowed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareConfig(Config{ReadPaths: []string{alias}})
	if err != nil {
		t.Fatal(err)
	}
	allowedAlias := filepath.Join(alias, "allowed.txt")
	prepared.ReadPaths = []string{allowedAlias}
	narrowed, err := PrepareConfig(prepared)
	if err != nil {
		t.Fatal(err)
	}

	profile := buildProfile(narrowed)
	childRule := fmt.Sprintf("(allow file-read* (subpath %q))", allowedAlias)
	parentRule := fmt.Sprintf("(allow file-read* (subpath %q))", alias)
	if !strings.Contains(profile, childRule) {
		t.Fatalf("profile does not allow narrowed alias child %q:\n%s", allowedAlias, profile)
	}
	if strings.Contains(profile, parentRule) {
		t.Fatalf("profile retained broader alias allow %q:\n%s", alias, profile)
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
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
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

func TestDarwinReadPathAliasRemainsUsableAndRejectsRetarget(t *testing.T) {
	skipIfNoSandboxExec(t)
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
	if err := os.WriteFile(filepath.Join(first, "credentials"), []byte("first-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "credentials"), []byte("second-secret"), 0600); err != nil {
		t.Fatal(err)
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
	profile := buildProfileWithWritePaths(sb.(*darwinSandbox).cfg, sb.(*darwinSandbox).writePaths)
	if !strings.Contains(profile, fmt.Sprintf("(allow file-read* (subpath %q))", alias)) {
		t.Fatalf("profile omitted configured read alias %q:\n%s", alias, profile)
	}
	cmd := exec.Command("/bin/cat", filepath.Join(alias, "credentials"))
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatal(err)
	}
	out, runErr := cmd.CombinedOutput()
	_ = cleanup()
	if runErr != nil {
		t.Fatalf("read configured alias: %v (%s)", runErr, out)
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

func TestSandboxEnvFiltering(t *testing.T) {
	skipIfNoSandboxExec(t)

	sb, err := New(Config{AllowEnv: []string{"POLLY_TEST_KEEP"}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "bash", "-c", "echo keep=$POLLY_TEST_KEEP drop=$POLLY_TEST_DROP")
	cmd.Env = []string{"POLLY_TEST_KEEP=yes", "POLLY_TEST_DROP=no"}
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
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

func TestDarwinNewCopiesCallerAllowEnv(t *testing.T) {
	skipIfNoSandboxExec(t)
	allow := []string{"SAFE_NAME"}
	sb, err := New(Config{AllowEnv: allow})
	if err != nil {
		t.Fatal(err)
	}
	allow[0] = "AWS_SECRET_ACCESS_KEY"
	if got := sb.(*darwinSandbox).cfg.AllowEnv; len(got) != 1 || got[0] != "SAFE_NAME" {
		t.Fatalf("sandbox AllowEnv aliases caller slice: %v", got)
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
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
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
	if !strings.Contains(profile, `(deny network-outbound (remote unix-socket))`) {
		t.Fatalf("profile missing blanket Unix-socket deny:\n%s", profile)
	}
	if strings.Contains(profile, "mDNSResponder") {
		t.Fatalf("profile re-allows mDNSResponder despite DenyDNS:\n%s", profile)
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
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if err := cmd.Run(); err == nil {
		t.Fatal("expected DNS-based request to fail with DenyDNS")
	}

	// Direct IP TCP connection should still work (network is allowed, only DNS is blocked).
	// Use bash /dev/tcp to avoid file-write sandbox restrictions that affect curl.
	cmd2 := exec.CommandContext(context.Background(), "bash", "-c", "exec 3<>/dev/tcp/1.1.1.1/80 && echo ok")
	if err := wrapCmdForTest(t, sb, cmd2); err != nil {
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
	// The only file-write allows permitted under DenyWrite are the standard
	// character devices (/dev/null, /dev/zero, etc) — see
	// TestBuildProfileAllowsStandardDeviceFiles. Any other allow means a user
	// path has leaked through.
	for _, line := range strings.Split(profile, "\n") {
		if !strings.Contains(line, "(allow file-write*") {
			continue
		}
		if !strings.Contains(line, `"/dev/`) {
			t.Fatalf("unexpected file-write allow under DenyWrite: %s\nprofile:\n%s", line, profile)
		}
	}
}

func TestSandboxDenyWriteBlocksTemp(t *testing.T) {
	skipIfNoSandboxExec(t)

	sb, err := New(Config{DenyWrite: true})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "bash", "-c", "echo bad > /tmp/polly-deny-test")
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if err := cmd.Run(); err == nil {
		t.Fatal("expected write to /tmp to be blocked when DenyWrite is true")
		os.Remove("/tmp/polly-deny-test")
	}
}

func TestBuildProfileIncludesUserDenyPaths(t *testing.T) {
	dir := t.TempDir()
	profile := buildProfile(Config{DenyPaths: []string{dir}})
	if !strings.Contains(profile, fmt.Sprintf(`(deny file-read* (subpath %q))`, dir)) {
		t.Fatalf("profile missing deny for user denyPath %q:\n%s", dir, profile)
	}
}

// A writablePaths entry that is an ancestor of a denied credential path must
// not re-open write access to it: the deny file-write* rule has to appear
// after the writable allow so Seatbelt's last-match-wins blocks the write.
func TestBuildProfileDeniesWritesToCredentialPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// writablePaths deliberately includes an ancestor of the credential paths.
	profile := buildProfile(Config{WritablePaths: []string{home}})

	sshDir := filepath.Join(home, ".ssh")
	denyRule := fmt.Sprintf(`(deny file-write* (subpath %q))`, sshDir)
	denyIdx := strings.Index(profile, denyRule)
	if denyIdx < 0 {
		t.Fatalf("profile missing write deny for credential path %q under a writable ancestor:\n%s", sshDir, profile)
	}
	firstWriteAllow := strings.Index(profile, "(allow file-write* (subpath")
	if firstWriteAllow < 0 || denyIdx < firstWriteAllow {
		t.Fatalf("write deny for %q must come after the writable allows (last-match-wins):\n%s", sshDir, profile)
	}
}

// End-to-end: with home as a writablePath, a sandboxed process must still be
// unable to plant a file in ~/.ssh — reads are denied and so are writes.
func TestSandboxBlocksWriteToCredentialUnderWritablePath(t *testing.T) {
	skipIfNoSandboxExec(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatal(err)
	}

	sb, err := New(Config{WritablePaths: []string{home}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	victim := filepath.Join(sshDir, "authorized_keys")
	cmd := exec.CommandContext(context.Background(), "bash", "-c", "echo pwned > "+victim)
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if err := cmd.Run(); err == nil {
		os.Remove(victim)
		t.Fatal("expected write to ~/.ssh to be blocked even though home is a writablePath")
	}
	if _, err := os.Stat(victim); err == nil {
		os.Remove(victim)
		t.Fatal("credential file was created despite the sandbox")
	}
}

func TestBuildProfileDenyWritePaths(t *testing.T) {
	work := t.TempDir()
	hooks := filepath.Join(work, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0755); err != nil {
		t.Fatal(err)
	}

	profile := buildProfile(Config{WritablePaths: []string{work}, DenyWritePaths: []string{hooks}})

	literalRule := fmt.Sprintf(`(deny file-write* (literal %q))`, hooks)
	if !strings.Contains(profile, literalRule) {
		t.Fatalf("profile missing literal write deny for protected entry %q:\n%s", hooks, profile)
	}
	denyRule := fmt.Sprintf(`(deny file-write* (subpath %q))`, hooks)
	denyIdx := strings.Index(profile, denyRule)
	if denyIdx < 0 {
		t.Fatalf("profile missing write deny for %q:\n%s", hooks, profile)
	}
	firstWriteAllow := strings.Index(profile, "(allow file-write* (subpath")
	if firstWriteAllow < 0 || denyIdx < firstWriteAllow {
		t.Fatalf("denyWritePaths rule must come after the writable allows (last-match-wins):\n%s", profile)
	}
	// Reads must stay allowed: denyWritePaths must not emit a read deny.
	if strings.Contains(profile, fmt.Sprintf(`(deny file-read* (subpath %q))`, hooks)) {
		t.Fatalf("denyWritePaths must not deny reads:\n%s", profile)
	}
}

func TestBuildProfilePinsMutableDenyWriteAncestors(t *testing.T) {
	work := t.TempDir()
	protected := filepath.Join(work, "packages", "nested", ".git")
	if err := os.MkdirAll(protected, 0o755); err != nil {
		t.Fatal(err)
	}

	profile := buildProfile(Config{WritablePaths: []string{work}, DenyWritePaths: []string{protected}})
	realWork, err := filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatal(err)
	}
	for _, ancestor := range []string{
		realWork,
		filepath.Join(realWork, "packages"),
		filepath.Join(realWork, "packages", "nested"),
	} {
		want := fmt.Sprintf(`(deny file-write-unlink (literal %q))`, ancestor)
		if !strings.Contains(profile, want) {
			t.Errorf("profile missing mutable-ancestor pin %q:\n%s", want, profile)
		}
	}
}

func TestDenyWriteAncestorsMatchesCaseVariedWritableRoot(t *testing.T) {
	work, caseVariant := caseVariedWorkspace(t)
	protected := filepath.Join(work, "packages", "nested", ".git")
	if err := os.MkdirAll(protected, 0o755); err != nil {
		t.Fatal(err)
	}

	ancestors := denyWriteAncestors([]string{protected}, []string{caseVariant})
	for _, want := range []string{
		work,
		filepath.Join(work, "packages"),
		filepath.Join(work, "packages", "nested"),
	} {
		if !slices.Contains(ancestors, want) {
			t.Fatalf("denyWriteAncestors() = %v, want case-aliased ancestor %q", ancestors, want)
		}
	}
}

func TestDarwinNewRejectsMissingDenyWritePath(t *testing.T) {
	skipIfNoSandboxExec(t)

	work := t.TempDir()
	missing := filepath.Join(work, "reserved")
	if _, err := New(Config{WritablePaths: []string{work}, DenyWritePaths: []string{missing}}); err == nil {
		t.Fatal("New() error = nil, want missing denyWritePaths entry to fail closed")
	}
}

// End-to-end: with the workspace writable, a sandboxed process must still be
// unable to plant a git hook, but can read the hooks dir and write elsewhere
// in the workspace.
func TestSandboxDenyWritePathBlocksHookPlanting(t *testing.T) {
	skipIfNoSandboxExec(t)

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

	// The rest of the workspace stays writable and the hooks dir readable.
	cmd = exec.CommandContext(context.Background(), "bash", "-c",
		"ls "+hooks+" >/dev/null && echo ok > "+filepath.Join(work, "note.txt"))
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("workspace write/read alongside denyWritePaths failed: %v", err)
	}
}

func TestDarwinWrapRejectsDenyWritePathRemovedAfterConstruction(t *testing.T) {
	skipIfNoSandboxExec(t)

	work := t.TempDir()
	protected := filepath.Join(work, "protected")
	if err := os.MkdirAll(protected, 0o755); err != nil {
		t.Fatal(err)
	}
	sb, err := New(Config{WritablePaths: []string{work}, DenyWritePaths: []string{protected}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := os.RemoveAll(protected); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(context.Background(), "true")
	if err := wrapCmdForTest(t, sb, cmd); err == nil {
		t.Fatal("Wrap() error = nil after protected entry removal, want fail-closed error")
	}
}

func TestSandboxWorkspacePresetPinsGitRoutingAndAncestors(t *testing.T) {
	skipIfNoSandboxExec(t)

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
			cmd:  exec.CommandContext(context.Background(), "mv", rootGit, filepath.Join(work, ".git-old")),
		},
		{
			name: "create absent config.worktree",
			cmd: exec.CommandContext(context.Background(), "bash", "-c", `: > "$1"`, "bash",
				filepath.Join(rootGit, "config.worktree")),
		},
		{
			name: "relocate mutable nested ancestor",
			cmd: exec.CommandContext(context.Background(), "mv", filepath.Join(work, "packages"),
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
	if _, err := os.Stat(nestedGit); err != nil {
		t.Fatalf("nested Git routing entry moved or removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootGit, "config.worktree")); !os.IsNotExist(err) {
		t.Fatalf("config.worktree was created despite read-only gitdir: %v", err)
	}

	// Pinning ancestors is structural only; normal working-tree content remains
	// writable within the nested repository.
	note := filepath.Join(nestedRoot, "note.txt")
	cmd := exec.CommandContext(context.Background(), "bash", "-c", `echo ok > "$1"`, "bash", note)
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() safe workspace write error = %v", err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("safe nested working-tree write failed: %v (%s)", err, out)
	}
}

func TestBuildProfileResolvesSymlinkedDenyPaths(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real-creds")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".creds")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	profile := buildProfile(Config{DenyPaths: []string{link}})

	// Seatbelt matches resolved vnode paths, so the deny must name the real
	// target, not just the link.
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(profile, fmt.Sprintf(`(deny file-read* (subpath %q))`, resolved)) {
		t.Fatalf("profile missing deny for symlink target %q:\n%s", resolved, profile)
	}
	if !strings.Contains(profile, fmt.Sprintf(`(deny file-read* (subpath %q))`, link)) {
		t.Fatalf("profile missing deny for the link itself %q:\n%s", link, profile)
	}
}

func TestSandboxBlocksSymlinkedDenyPath(t *testing.T) {
	skipIfNoSandboxExec(t)

	dir := t.TempDir()
	target := filepath.Join(dir, "real-creds")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(target, "key")
	if err := os.WriteFile(secret, []byte("secret-value"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".creds")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	sb, err := New(Config{DenyPaths: []string{link}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Reading through the link and reading the target directly must both fail.
	for _, path := range []string{filepath.Join(link, "key"), secret} {
		cmd := exec.CommandContext(context.Background(), "cat", path)
		if err := wrapCmdForTest(t, sb, cmd); err != nil {
			t.Fatalf("Wrap() error = %v", err)
		}
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("expected read of %s to be blocked, got output: %s", path, string(out))
		}
	}
}

func TestWrapStripsSensitiveEnvByDefault(t *testing.T) {
	skipIfNoSandboxExec(t)

	sb, err := New(Config{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	cmd := exec.Command("bash", "-c", `printf '%s|%s|%s|%s' "$SSH_AUTH_SOCK" "$GITHUB_TOKEN" "$MY_API_KEY" "$OTHER_VAR"`)
	cmd.Env = []string{
		"SSH_AUTH_SOCK=/run/agent.sock",
		"GITHUB_TOKEN=ghp",
		"MY_API_KEY=sk",
		"OTHER_VAR=kept",
	}
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if cmd.Env == nil || len(cmd.Env) != 0 {
		t.Fatalf("sandbox-exec Env = %v, want non-nil empty wrapper environment", cmd.Env)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandboxed target failed: %v (%s)", err, out)
	}
	if string(out) != "|||kept" {
		t.Fatalf("target environment = %q, want only OTHER_VAR=kept", out)
	}
}

func TestBuildProfileDeniesSignal(t *testing.T) {
	profile := buildProfile(Config{})
	for _, rule := range []string{
		"(deny signal)",
		"(allow signal (target self))",
		"(allow signal (target same-sandbox))",
	} {
		if !strings.Contains(profile, rule) {
			t.Fatalf("profile missing signal rule %q:\n%s", rule, profile)
		}
	}
}

// A sandboxed script must be able to signal its own children even when it
// detaches them into a separate process group (job control / setsid workers).
// The (target same-sandbox) scope covers descendants regardless of pgroup,
// where a (target pgrp) scope would deny them with EPERM.
func TestSandboxAllowsSignalingOwnDetachedChild(t *testing.T) {
	skipIfNoSandboxExec(t)

	sb, err := New(Config{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	// `set -m` enables job control, putting the background job in its own
	// process group; the script then signals it by job spec.
	cmd := exec.CommandContext(context.Background(), "bash", "-c",
		"set -m; sleep 30 & kill %1")
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sandboxed script could not signal its own detached child: %v (%s)", err, out)
	}
}

func TestWrapSetsNewSession(t *testing.T) {
	skipIfNoSandboxExec(t)

	sb, err := New(Config{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	cmd := exec.Command("true")
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	// Setsid detaches the controlling terminal (macOS counterpart to bwrap's
	// --new-session) and gives the process its own group, which the profile's
	// (allow signal (target pgrp)) rule depends on.
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatalf("expected Wrap to set SysProcAttr.Setsid = true, got %+v", cmd.SysProcAttr)
	}
}

func TestSandboxAllowsSignalingOwnChild(t *testing.T) {
	skipIfNoSandboxExec(t)

	sb, err := New(Config{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	// A script must still be able to manage its own jobs (timeouts, background
	// workers). The child shares the sandbox's process group, so (target pgrp)
	// permits it.
	cmd := exec.CommandContext(context.Background(), "bash", "-c",
		"sleep 30 & c=$!; kill $c")
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sandboxed script could not signal its own child: %v (%s)", err, out)
	}
}

func TestSandboxBlocksSignalingUnrelatedProcess(t *testing.T) {
	skipIfNoSandboxExec(t)

	// An unrelated same-user process: spawned by the test, so it lives in the
	// test's session. The sandboxed process gets its own session via Setsid, so
	// this target is out of its process group and the signal must be denied.
	victim := exec.Command("sleep", "30")
	if err := victim.Start(); err != nil {
		t.Fatalf("starting victim: %v", err)
	}
	defer func() {
		_ = victim.Process.Kill()
		_ = victim.Wait()
	}()

	sb, err := New(Config{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	cmd := exec.CommandContext(context.Background(), "bash", "-c",
		fmt.Sprintf("kill -0 %d", victim.Process.Pid))
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("expected signaling an unrelated process to be blocked, got output: %s", out)
	}
}

// A missing writable path must not fail construction: it would otherwise brick
// session restore over a single stale path. The grant is dropped permanently,
// so a later creation cannot acquire authority.
func TestNewToleratesMissingWritablePath(t *testing.T) {
	skipIfNoSandboxExec(t)

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := New(Config{WritablePaths: []string{missing}}); err != nil {
		t.Fatalf("New() should tolerate a missing writable path, got: %v", err)
	}
	if _, err := New(Config{WritablePaths: []string{missing}, DenyWrite: true}); err != nil {
		t.Fatalf("New() with DenyWrite should also tolerate it, got: %v", err)
	}
}

func TestDarwinMissingAuthorityPathsCannotActivateLater(t *testing.T) {
	skipIfNoSandboxExec(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
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
	frozen := sb.(*darwinSandbox).cfg
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
		t.Fatalf("create authority-retarget links: %v (%s)", createErr, createOut)
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

func TestDarwinFrozenAuthorityRejectsCrossSandboxReplacement(t *testing.T) {
	skipIfNoSandboxExec(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
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
		t.Fatalf("replace authority path from broad sandbox: %v (%s)", runErr, out)
	}

	cmd := exec.Command("true")
	if err := wrapCmdForTest(t, narrow, cmd); err == nil {
		t.Fatal("narrow sandbox accepted a writable path replaced by another sandbox")
	}
	if len(cmd.ExtraFiles) != 0 {
		t.Fatalf("failed identity check leaked sandbox descriptors: %v", cmd.ExtraFiles)
	}
}

func TestDarwinWrappedAuthorityDoesNotFollowReplacementBeforeStart(t *testing.T) {
	skipIfNoSandboxExec(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp(home, ".polly-wrapped-authority-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	child := filepath.Join(base, "child")
	external := filepath.Join(base, "external")
	for _, path := range []string{child, external} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	sb, err := New(Config{WritablePaths: []string{child}})
	if err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(child, "marker")
	cmd := exec.Command("bash", "-c", `printf escaped > "$1"`, "bash", marker)
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	defer func() { _ = cleanup() }()

	if err := os.Rename(child, child+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, child); err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("profile compiled after replacement followed writable symlink: %s", out)
	}
	if _, err := os.Stat(filepath.Join(external, "marker")); !os.IsNotExist(err) {
		t.Fatalf("wrapped authority replacement wrote outside approved inode: %v", err)
	}
}

func TestDarwinAutomaticTempGrantIsPinnedAcrossSandboxes(t *testing.T) {
	skipIfNoSandboxExec(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp(home, ".polly-auto-temp-pin-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	tmpdir := filepath.Join(base, "tmp")
	laterTmpdir := filepath.Join(base, "later-tmp")
	external := filepath.Join(base, "external")
	for _, path := range []string{tmpdir, laterTmpdir, external} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TMPDIR", tmpdir)

	narrow, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", laterTmpdir)
	profileCmd := exec.Command("/usr/bin/true")
	profileCleanup, err := WrapCmdManaged(narrow, profileCmd)
	if err != nil {
		t.Fatal(err)
	}
	profile := profileCmd.Args[2]
	if err := profileCleanup(); err != nil {
		t.Fatal(err)
	}
	wantInitial := fmt.Sprintf(`(allow file-write* (subpath %q))`, tmpdir)
	wantLater := fmt.Sprintf(`(allow file-write* (subpath %q))`, laterTmpdir)
	if !strings.Contains(profile, wantInitial) {
		t.Fatalf("profile lost construction-time TMPDIR grant %q:\n%s", tmpdir, profile)
	}
	if strings.Contains(profile, wantLater) {
		t.Fatalf("profile acquired post-construction TMPDIR grant %q:\n%s", laterTmpdir, profile)
	}
	// Keep the existing cross-sandbox replacement proof focused on the same
	// automatic route captured by both sandboxes.
	t.Setenv("TMPDIR", tmpdir)
	broad, err := New(Config{WritablePaths: []string{base}})
	if err != nil {
		t.Fatal(err)
	}
	replace := exec.Command("bash", "-c", `mv "$1" "$1-old" && ln -s "$2" "$1"`, "bash", tmpdir, external)
	cleanup, err := WrapCmdManaged(broad, replace)
	if err != nil {
		t.Fatal(err)
	}
	out, runErr := replace.CombinedOutput()
	_ = cleanup()
	if runErr == nil {
		t.Fatalf("broader sandbox replaced automatic TMPDIR grant: %s", out)
	}
	if info, err := os.Lstat(tmpdir); err != nil || !info.IsDir() {
		t.Fatalf("automatic TMPDIR route changed after denied replacement: %v, %v", info, err)
	}

	if err := os.Rename(tmpdir, tmpdir+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, tmpdir); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("true")
	if err := wrapCmdForTest(t, narrow, cmd); err == nil {
		t.Fatal("sandbox accepted a replaced automatic TMPDIR grant")
	}
}

func TestDarwinDeniedPathCannotBeRelocatedUnderWritableParent(t *testing.T) {
	skipIfNoSandboxExec(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp(home, ".polly-denied-relocation-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	denied := filepath.Join(base, "secret")
	if err := os.Mkdir(denied, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(denied, "value"), []byte("host-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	sb, err := New(Config{WritablePaths: []string{base}, DenyPaths: []string{denied}})
	if err != nil {
		t.Fatal(err)
	}
	relocated := denied + "-old"
	cmd := exec.Command("bash", "-c", `mv "$1" "$2" && cat "$2/value"`, "bash", denied, relocated)
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatal(err)
	}
	out, runErr := cmd.CombinedOutput()
	_ = cleanup()
	if runErr == nil {
		t.Fatalf("sandbox relocated and read denied credentials: %s", out)
	}
	if _, err := os.Stat(denied); err != nil {
		t.Fatalf("denied entry was relocated despite route pin: %v", err)
	}
	if _, err := os.Stat(relocated); !os.IsNotExist(err) {
		t.Fatalf("relocated denied entry exists: %v", err)
	}
}

func TestDarwinWritablePathCannotHardlinkExternalFile(t *testing.T) {
	skipIfNoSandboxExec(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	externalDir, err := os.MkdirTemp(home, ".polly-hardlink-boundary-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(externalDir) })
	external := filepath.Join(externalDir, "external")
	if err := os.WriteFile(external, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(external)
	if err != nil {
		t.Fatal(err)
	}
	beforeStat := before.Sys()

	work := t.TempDir()
	alias := filepath.Join(work, "alias")
	sb, err := New(Config{WritablePaths: []string{work}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", `ln "$1" "$2" && printf escaped > "$2"`, "sh", external, alias)
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		t.Fatal(err)
	}
	out, runErr := cmd.CombinedOutput()
	_ = cleanup()
	if runErr == nil {
		t.Fatalf("sandbox created and overwrote an external hardlink: %s", out)
	}
	if data, err := os.ReadFile(external); err != nil || string(data) != "original" {
		t.Fatalf("external file changed through writable hardlink: %q, %v", data, err)
	}
	if _, err := os.Lstat(alias); !os.IsNotExist(err) {
		t.Fatalf("sandbox created external hardlink alias: %v", err)
	}
	after, err := os.Stat(external)
	if err != nil {
		t.Fatal(err)
	}
	if beforeStat != nil && after.Sys() != nil {
		// SameFile plus alias absence is the portable link-count invariant; the
		// external file must still be the original object.
		if !os.SameFile(before, after) {
			t.Fatal("external file identity changed during hardlink probe")
		}
	}
}

func TestDarwinPreservesCustomAndEmptyArgumentVectors(t *testing.T) {
	skipIfNoSandboxExec(t)
	sb, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}

	custom := exec.Command("/bin/sh", "-c", `printf '%s' "$0"`)
	custom.Args[0] = "custom-argv-zero"
	cleanup, err := WrapCmdManaged(sb, custom)
	if err != nil {
		t.Fatal(err)
	}
	out, runErr := custom.CombinedOutput()
	_ = cleanup()
	if runErr != nil {
		t.Fatalf("custom argv0 target failed: %v (%s)", runErr, out)
	}
	if string(out) != "custom-argv-zero" {
		t.Fatalf("target argv0 = %q, want custom-argv-zero", out)
	}

	empty := &exec.Cmd{Path: "/usr/bin/true"}
	cleanup, err = WrapCmdManaged(sb, empty)
	if err != nil {
		t.Fatal(err)
	}
	runErr = empty.Run()
	_ = cleanup()
	if runErr != nil {
		t.Fatalf("empty Args target failed: %v", runErr)
	}
}

func TestDarwinPreservesRelativeCommandDirectoryAndExecutable(t *testing.T) {
	skipIfNoSandboxExec(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
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
		t.Fatalf("relative target failed: %v (%s)", runErr, out)
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != realDir {
		t.Fatalf("relative command cwd = %q, want %q", got, realDir)
	}
}

func TestSandboxIgnoresPATHSandboxExec(t *testing.T) {
	skipIfNoSandboxExec(t)

	fakeDir := t.TempDir()
	fakeSandboxExec := filepath.Join(fakeDir, "sandbox-exec")
	marker := fakeSandboxExec + ".ran"
	if err := os.WriteFile(fakeSandboxExec, []byte("#!/bin/sh\n: > \"$0.ran\"\nexit 99\n"), 0700); err != nil {
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
	if cmd.Path != darwinSandboxExecPath {
		t.Fatalf("sandbox launcher = %q, want fixed %q", cmd.Path, darwinSandboxExecPath)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("PATH-selected fake sandbox-exec executed: %v", err)
	}
}
