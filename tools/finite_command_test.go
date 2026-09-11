package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func commandFixtureQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func waitCommandFixture(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !ready() {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("command fixture did not become ready")
		}
	}
}

func commandFixtureProcess(t *testing.T, path string) *os.Process {
	t.Helper()
	var pid int
	waitCommandFixture(t, func() bool {
		data, err := os.ReadFile(path)
		if err != nil || !strings.HasSuffix(string(data), "\n") {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil && pid > 0
	})
	p, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if commandFixtureAlive(p) {
			_ = p.Kill()
		}
		_ = p.Release()
	})
	return p
}

func commandFixtureAlive(p *os.Process) bool {
	if p.Signal(syscall.Signal(0)) != nil {
		return false
	}
	// Orphans may remain zombies until the host's init reaps them. They no
	// longer execute or retain descriptors, and cannot be killed again.
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", p.Pid))
		if err == nil {
			_, tail, _ := strings.Cut(string(data), ") ")
			if strings.HasPrefix(tail, "Z ") {
				return false
			}
		}
	}
	return true
}

func heldOutputCommand(pidFile, redirect, ending string) string {
	return "printf 'stdout-prefix\\n'; printf 'stderr-prefix\\n' >&2; " +
		"sleep 30 " + redirect + " & printf '%s\\n' \"$!\" > " + commandFixtureQuote(pidFile) + "; " + ending
}

type finiteToolResult struct {
	text string
	err  error
}

func receiveFiniteTool(t *testing.T, done <-chan finiteToolResult) finiteToolResult {
	t.Helper()
	select {
	case got := <-done:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("finite command did not return within the drain bound")
		return finiteToolResult{}
	}
}

func TestFiniteCommandCancellationStopsOwnedDescendants(t *testing.T) {
	skipIfWindows(t)
	for _, stream := range []struct{ name, redirect string }{
		{"stdout", "2>/dev/null"}, {"stderr", ">/dev/null"}, {"both", ""},
	} {
		t.Run(stream.name, func(t *testing.T) {
			// This separate command must survive cancellation of the tool group.
			unrelated := exec.Command("sleep", "30")
			cleanup, err := sandbox.WrapCmdManaged(nil, unrelated)
			if err != nil {
				t.Fatal(err)
			}
			if err := unrelated.Start(); err != nil {
				t.Fatal(err)
			}
			_ = cleanup()
			t.Cleanup(func() { _ = unrelated.Process.Kill(); _ = unrelated.Wait() })
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "child.pid")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan finiteToolResult, 1)
			go func() {
				text, err := NewUnsafeBashTool(dir).Execute(ctx, map[string]any{"command": heldOutputCommand(pidFile, stream.redirect, "wait")})
				done <- finiteToolResult{text, err}
			}()
			child := commandFixtureProcess(t, pidFile)
			cancel()
			got := receiveFiniteTool(t, done)
			if !errors.Is(got.err, context.Canceled) || !strings.Contains(got.text, "stdout-prefix") || !strings.Contains(got.text, "stderr-prefix") {
				t.Fatalf("partial cancellation result: %+v", got)
			}
			waitCommandFixture(t, func() bool { return !commandFixtureAlive(child) })
			if !commandFixtureAlive(unrelated.Process) {
				t.Fatal("cancellation touched an unrelated command")
			}
		})
	}
}

func TestFiniteCommandExitWithInheritedPipes(t *testing.T) {
	skipIfWindows(t)
	for _, path := range []string{"bash", "shell", "schema", "search"} {
		for _, code := range []int{0, 7} {
			t.Run(fmt.Sprintf("%s/%d", path, code), func(t *testing.T) {
				dir := t.TempDir()
				pidFile := filepath.Join(dir, "child.pid")
				command := heldOutputCommand(pidFile, "", fmt.Sprintf("exit %d", code))
				script := filepath.Join(dir, "held-output.sh")
				if err := os.WriteFile(script, []byte("#!/bin/sh\n"+command+"\n"), 0700); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan finiteToolResult, 1)
				started := time.Now()
				go func() {
					var text string
					var err error
					switch path {
					case "bash":
						text, err = NewUnsafeBashTool(dir).Execute(ctx, map[string]any{"command": command})
					case "shell":
						text, err = (&ShellTool{Command: script, workDir: dir}).Execute(ctx, map[string]any{})
					case "schema":
						text, err = (&ShellTool{Command: script}).runCommand("--schema", nil)
					case "search":
						var truncated bool
						text, _, truncated, err = runIndexedSearchCommand(ctx, nil, script, dir, nil)
						if truncated {
							err = fmt.Errorf("drain timeout confused with output size limit: %v", err)
						}
					}
					done <- finiteToolResult{text, err}
				}()
				child := commandFixtureProcess(t, pidFile)
				got := receiveFiniteTool(t, done)
				var commandErr *CommandError
				if !errors.Is(got.err, ErrCommandOutputIncomplete) || errors.As(got.err, &commandErr) || !strings.Contains(got.text, "stdout-prefix") {
					t.Fatalf("incomplete capture classified as exit: %+v", got)
				}
				if time.Since(started) < commandDrainTimeout {
					t.Fatal("did not allow the output drain interval")
				}
				cancel() // No delayed signal may be sent to the exited shell's group.
				if !commandFixtureAlive(child) {
					t.Fatal("post-exit drain terminated the background command")
				}
			})
		}
	}
}

func TestFiniteCommandShellCancelAfterStartup(t *testing.T) {
	skipIfWindows(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "tool.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+heldOutputCommand(pidFile, "", "wait")), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan finiteToolResult, 1)
	go func() {
		text, err := (&ShellTool{Command: script}).Execute(ctx, map[string]any{})
		done <- finiteToolResult{text, err}
	}()
	child := commandFixtureProcess(t, pidFile)
	cancel()
	got := receiveFiniteTool(t, done)
	if !errors.Is(got.err, context.Canceled) || !strings.Contains(got.text, "stdout-prefix") || !strings.Contains(got.text, "stderr-prefix") {
		t.Fatalf("shell cancellation identity or combined output lost: %+v", got)
	}
	waitCommandFixture(t, func() bool { return !commandFixtureAlive(child) })
}

func TestFiniteCommandRedirectedBackgroundOutput(t *testing.T) {
	skipIfWindows(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	done := make(chan finiteToolResult, 1)
	go func() {
		text, err := NewUnsafeBashTool(dir).Execute(context.Background(), map[string]any{"command": heldOutputCommand(pidFile, ">/dev/null 2>&1", "exit 0")})
		done <- finiteToolResult{text, err}
	}()
	child := commandFixtureProcess(t, pidFile)
	got := receiveFiniteTool(t, done)
	if got.err != nil || !strings.Contains(got.text, "stdout-prefix") || !commandFixtureAlive(child) {
		t.Fatalf("redirected background behavior changed: %+v", got)
	}
}

func TestFiniteCommandCancelDuringPostExitDrain(t *testing.T) {
	skipIfWindows(t)
	dir := t.TempDir()
	parentFile, childFile := filepath.Join(dir, "parent.pid"), filepath.Join(dir, "child.pid")
	command := "printf '%s\\n' \"$$\" > " + commandFixtureQuote(parentFile) + "; " + heldOutputCommand(childFile, "", "exit 0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan finiteToolResult, 1)
	go func() {
		text, err := NewUnsafeBashTool(dir).Execute(ctx, map[string]any{"command": command})
		done <- finiteToolResult{text, err}
	}()
	parent := commandFixtureProcess(t, parentFile)
	child := commandFixtureProcess(t, childFile)
	waitCommandFixture(t, func() bool { return !commandFixtureAlive(parent) })
	cancel()
	got := receiveFiniteTool(t, done)
	if !errors.Is(got.err, context.Canceled) || !strings.Contains(got.text, "stdout-prefix") || !commandFixtureAlive(child) {
		t.Fatalf("post-exit cancellation changed background lifetime: %+v", got)
	}
}

func TestFiniteCommandCombinedOutputAndSizeLimit(t *testing.T) {
	skipIfWindows(t)
	for _, limit := range []int{1024, 5} {
		output := newBoundedBuffer(limit)
		_, err := runFiniteCommand(context.Background(), nil, finiteCommand{
			name: "bash", args: []string{"-c", "printf out; printf err >&2; printf end"},
			stdout: output, stderr: output,
		})
		if err != nil {
			t.Fatal(err)
		}
		if limit == 1024 && output.String() != "outerrend" {
			t.Fatalf("combined stream lost write order: %q", output.String())
		}
		if limit == 5 && (!output.Truncated() || !strings.HasPrefix(output.String(), "outer\n[output truncated:")) {
			t.Fatalf("size truncation changed: %q", output.String())
		}
	}
}

func TestFiniteCommandSchemaLoaderRejectsIncompleteJSON(t *testing.T) {
	skipIfWindows(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "schema.sh")
	body := "#!/bin/sh\nprintf '%s\\n' '{\"title\":\"partial\",\"type\":\"object\"}'; sleep 30 & printf '%s\\n' \"$!\" > " + commandFixtureQuote(pidFile) + "; exit 0"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	done := make(chan finiteToolResult, 1)
	go func() {
		tool, err := newShellTool(script)
		if tool != nil {
			err = fmt.Errorf("incomplete schema registered a tool")
		}
		done <- finiteToolResult{err: err}
	}()
	commandFixtureProcess(t, pidFile)
	got := receiveFiniteTool(t, done)
	if !errors.Is(got.err, ErrCommandOutputIncomplete) {
		t.Fatalf("schema discovery lost capture error identity: %v", got.err)
	}
}

func TestFiniteCommandDetachedDescendantBoundsCapture(t *testing.T) {
	skipIfWindows(t)
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("detached-session fixture needs perl POSIX")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "detached.pid")
	// A readiness file written after setsid proves the child escaped the
	// command group. The test explicitly owns teardown of that escaped child.
	program := `POSIX::setsid() >= 0 or die $!; open(my $f, ">", $ARGV[0]) or die $!; print $f "$$\n"; close($f); sleep 30;`
	command := "printf 'prefix\\n'; perl -MPOSIX -e " + commandFixtureQuote(program) + " " + commandFixtureQuote(pidFile) + " & wait"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan finiteToolResult, 1)
	go func() {
		text, err := NewUnsafeBashTool(dir).Execute(ctx, map[string]any{"command": command})
		done <- finiteToolResult{text, err}
	}()
	child := commandFixtureProcess(t, pidFile)
	cancel()
	got := receiveFiniteTool(t, done)
	if !errors.Is(got.err, context.Canceled) || !strings.Contains(got.text, "prefix") {
		t.Fatalf("detached capture: %+v", got)
	}
	if !commandFixtureAlive(child) {
		t.Fatal("fixture did not escape the process group")
	}
}

type commandLauncherFixture struct{ command string }

func (s commandLauncherFixture) Wrap(cmd *exec.Cmd) error {
	cmd.Path = "/bin/sh"
	cmd.Args = []string{"sh", "-c", s.command}
	return nil
}

func TestFiniteCommandStartupAndAcknowledgment(t *testing.T) {
	skipIfWindows(t)
	for _, kind := range []string{"canceled", "wrap", "start", "launcher", "missing-ack", "lost-ack-success"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "child.pid")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var sb sandbox.Sandbox
			name, args := "bash", []string{"-c", "true"}
			switch kind {
			case "canceled":
				cancel()
			case "wrap":
				sb = &mockSandbox{err: errors.New("wrap failure")}
			case "start":
				name = filepath.Join(dir, "absent")
			case "launcher":
				sb = commandLauncherFixture{"exit 7"}
			case "missing-ack":
				// Only the startup descriptor remains inherited by the child.
				sb = commandLauncherFixture{heldOutputCommand(pidFile, ">/dev/null 2>&1", "exit 7")}
			case "lost-ack-success":
				sb = commandLauncherFixture{"exit 0"}
			}
			stdout, stderr := newBoundedBuffer(1024), newBoundedBuffer(1024)
			done := make(chan finiteToolResult, 1)
			go func() {
				_, err := runFiniteCommand(ctx, sb, finiteCommand{name: name, args: args, stdout: stdout, stderr: stderr, acknowledge: sb != nil})
				done <- finiteToolResult{err: err}
			}()
			if kind == "missing-ack" {
				commandFixtureProcess(t, pidFile)
			}
			got := receiveFiniteTool(t, done)
			if kind == "lost-ack-success" {
				if got.err != nil {
					t.Fatal(got.err)
				}
				return
			}
			if got.err == nil {
				t.Fatal("expected failure")
			}
			if _, ordinary := got.err.(*exec.ExitError); ordinary {
				t.Fatalf("setup error became ordinary exit: %v", got.err)
			}
			if kind == "canceled" && !errors.Is(got.err, context.Canceled) {
				t.Fatal(got.err)
			}
			if kind == "missing-ack" && (!errors.Is(got.err, ErrCommandOutputIncomplete) || !strings.Contains(got.err.Error(), "target did not start")) {
				t.Fatal(got.err)
			}
		})
	}
}

func TestFiniteCommandDescriptorCleanupFailure(t *testing.T) {
	skipIfWindows(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", "sleep 30")
	cmd.WaitDelay = commandDrainTimeout
	output := newBoundedBuffer(1024)
	capture, err := newCommandCapture(cmd, output, output, false)
	if err != nil {
		t.Fatal(err)
	}
	defer capture.close()
	cleanup, err := sandbox.WrapFiniteCmdManaged(nil, cmd)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("descriptor cleanup failed")
	// Keep the caller context independent: internal cleanup cancellation must
	// preserve its original failure, not replace it with context.Canceled.
	err = capture.run(context.Background(), cmd, cancel, func() error {
		return errors.Join(cleanup(), failure)
	})
	if !errors.Is(err, failure) || errors.Is(err, context.Canceled) || cmd.ProcessState == nil {
		t.Fatalf("cleanup result: %v, state %v", err, cmd.ProcessState)
	}
	for _, p := range capture.pipes {
		select {
		case <-p.done:
		default:
			t.Fatal("capture reader leaked")
		}
		if _, err := p.writer.Stat(); err == nil {
			t.Fatal("parent writer leaked")
		}
	}
}

func TestFiniteCommandNativeSandboxCancellation(t *testing.T) {
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" || (runtime.GOOS != "darwin" && runtime.GOOS != "linux") {
		t.Skip("opt-in native sandbox")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sb, err := sandbox.New(sandbox.Config{WritablePaths: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(dir, "ready")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", heldOutputCommand(pidFile, "", "wait"))
	cmd.Dir = dir
	cmd.WaitDelay = commandDrainTimeout
	stdout, stderr := newBoundedBuffer(1024), newBoundedBuffer(1024)
	capture, err := newCommandCapture(cmd, stdout, stderr, true)
	if err != nil {
		t.Fatal(err)
	}
	defer capture.close()
	cleanup, err := sandbox.WrapFiniteCmdManaged(sb, cmd)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan finiteToolResult, 1)
	go func() { done <- finiteToolResult{err: capture.run(ctx, cmd, cancel, cleanup)} }()
	waitCommandFixture(t, func() bool {
		data, err := os.ReadFile(pidFile)
		return err == nil && strings.HasSuffix(string(data), "\n")
	})
	cancel()
	got := receiveFiniteTool(t, done)
	if !errors.Is(got.err, context.Canceled) || !strings.Contains(stdout.String(), "stdout-prefix") || !strings.Contains(stderr.String(), "stderr-prefix") {
		t.Fatalf("native cancellation: %v / %q / %q", got.err, stdout.String(), stderr.String())
	}
	for _, p := range capture.pipes {
		if p.err != nil {
			t.Fatalf("native descendant retained %s after cancellation: %v", p.name, p.err)
		}
	}
	if len(cmd.ExtraFiles) != 1 { // Only our closed startup writer remains.
		t.Fatalf("managed descriptors retained after Start: %d", len(cmd.ExtraFiles))
	}
}
