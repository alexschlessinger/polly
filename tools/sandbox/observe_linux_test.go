//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type foreignSandbox struct{}

func (foreignSandbox) Wrap(*exec.Cmd) error { return nil }

func TestDenialObserverRefusesAnotherBackend(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	observer, err := NewDenialObserver()
	if err != nil {
		t.Fatal(err)
	}
	if err := observer.Start(context.Background(), foreignSandbox{}); !errors.Is(err, ErrDenialsUnobservable) {
		t.Fatalf("Start on another backend = %v, want ErrDenialsUnobservable", err)
	}
}

// The trial script runs the command with its exit status and without the
// report descriptor or the variables that carried it in, and reports the
// writes the command left in the private home.
func TestLinuxTrialScriptReportsHomeWrites(t *testing.T) {
	skipIfNoBwrap(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	observer, err := NewDenialObserver()
	if err != nil {
		t.Fatal(err)
	}
	sb, err := New(observer.Config(Config{PrivateHome: true}))
	if err != nil {
		t.Fatal(err)
	}
	if err := observer.Start(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	command := `echo forged >&3 || echo "no descriptor"; echo "vars:${POLLY_TRIAL_COMMAND:-}${POLLY_TRIAL_HOME:-}."; mkdir -p "$HOME/.local/state/tool"; exit 7`
	shell := observer.Shell(command, 3)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	cmd := exec.Command("bash", "-c", shell.Script)
	cmd.ExtraFiles = []*os.File{writer}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cleanup, err := WrapCmdWithEnvManaged(sb, cmd, shell.Env)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	report, readErr := io.ReadAll(reader)
	waitErr := cmd.Wait()
	if readErr != nil {
		t.Fatal(readErr)
	}
	var exit *exec.ExitError
	if !errors.As(waitErr, &exit) || exit.ExitCode() != 7 {
		if strings.Contains(output.String(), "Operation not permitted") {
			skipOrFailBwrapUnavailable(t, waitErr, output.Bytes())
		}
		t.Fatalf("trial script = %v (%s); want exit 7", waitErr, output.String())
	}
	if out := output.String(); !strings.Contains(out, "no descriptor") || !strings.Contains(out, "vars:.") {
		t.Fatalf("the command saw the report descriptor or its variables: %q", out)
	}
	if bytes.Contains(report, []byte("forged")) {
		t.Fatalf("the command wrote into the report: %q", report)
	}
	obs, err := observer.Finish(context.Background(), sb, report)
	if err != nil {
		t.Fatal(err)
	}
	want := Denial{Access: AccessWrite, Path: filepath.Join(home, ".local", "state", "tool"), Operation: "write", Count: 3, Discarded: true, Directory: true}
	if obs.Incomplete != "" || len(obs.Denials) != 1 || obs.Denials[0] != want {
		t.Fatalf("observation = %+v; want only %+v", obs, want)
	}
}

func TestLinuxReadableHomeTrialDoesNotScanHome(t *testing.T) {
	observer := &DenialObserver{home: "/home/fixture"}
	if err := observer.Start(context.Background(), &linuxSandbox{cfg: Config{}}); err != nil {
		t.Fatal(err)
	}
	command := "echo trial"
	shell := observer.Shell(command, 3)
	if shell.Script != command || shell.Report || len(shell.Env) != 0 {
		t.Fatalf("readable home was wrapped in a scan: %+v", shell)
	}
	obs, err := observer.Finish(context.Background(), nil, nil)
	if err != nil || obs.Limit == "" || len(obs.Denials) != 0 {
		t.Fatalf("missing observation limitation: %+v %v", obs, err)
	}
	if err := observer.Start(context.Background(), &linuxSandbox{cfg: Config{PrivateHome: true}}); err != nil {
		t.Fatal(err)
	}
	if shell := observer.Shell(command, 3); !shell.Report {
		t.Fatal("private home lost write reporting")
	}
}
