package sandbox

import (
	"errors"
	"os/exec"
	"runtime"
	"slices"
	"testing"
)

func TestContainerSandboxIsGatedToHelperModeAndLayersEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("container sandbox is Unix only")
	}
	if !helperMode.Load() {
		if _, err := NewContainerSandboxFactory(nil); !errors.Is(err, ErrNotHelper) {
			t.Fatalf("factory outside helper mode = %v, want ErrNotHelper", err)
		}
	}
	EnterHelperMode()
	if _, err := NewContainerSandboxFactory([]string{"BROKEN"}); err == nil {
		t.Fatal("malformed sealed entry accepted")
	}
	factory, err := NewContainerSandboxFactory([]string{"SEALED=host", "SEALED_TOKEN=secret"})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := factory(Config{PassEnv: []string{"KEEP_TOKEN", "SEALED_TOKEN"}, Env: map[string]string{"TMPDIR": "/scratch"}, WritablePaths: []string{t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.Wrap(exec.Command("true")); !errors.Is(err, ErrManagedWrapRequired) {
		t.Fatalf("plain Wrap = %v, want ErrManagedWrapRequired", err)
	}
	cmd := exec.Command("sh", "-c", "true")
	cmd.Env = []string{"KEEP_TOKEN=k", "DROP_TOKEN=d", "SEALED=image", "PATH=/bin", "TMPDIR=/tmp"}
	cleanup, err := WrapCmdWithEnvManaged(sb, cmd, map[string]string{"EXPLICIT": "e"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for _, want := range []string{"SEALED=host", "SEALED_TOKEN=secret", "KEEP_TOKEN=k", "EXPLICIT=e", "TMPDIR=/scratch", "PATH=/bin"} {
		if !slices.Contains(cmd.Env, want) {
			t.Fatalf("env %v lacks %s", cmd.Env, want)
		}
	}
	for _, entry := range cmd.Env {
		if entry == "DROP_TOKEN=d" || entry == "SEALED=image" || entry == "TMPDIR=/tmp" {
			t.Fatalf("env %v kept %s", cmd.Env, entry)
		}
	}
	if cmd.Path == "" || cmd.Args[0] != "sh" || len(cmd.Args) != 3 {
		t.Fatalf("argv was rewritten: %v", cmd.Args)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatal("command does not start its own session")
	}
	if len(cmd.ExtraFiles) != 0 {
		t.Fatal("container sandbox lent descriptors")
	}
}
