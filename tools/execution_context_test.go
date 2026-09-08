package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestBoundNativeFilesAndReadOnlyPolicy(t *testing.T) {
	parent, child := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(parent, "file.txt"), []byte("parent"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "file.txt"), []byte("child"), 0600); err != nil {
		t.Fatal(err)
	}
	registry := NewToolRegistry(nil, WithUnsafeNoSandbox())
	defer registry.Close()
	for _, name := range []string{"read_file", "write_file"} {
		if _, err := registry.LoadToolAuto(name); err != nil {
			t.Fatal(err)
		}
	}
	ec, err := registry.ExecutionPolicy(child, true, []string{parent}, nil)
	if err != nil {
		t.Fatal(err)
	}
	bound, _, err := registry.BindExecutionContext(ec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	reader, _ := bound.Get("read_file")
	if out, err := reader.Execute(context.Background(), map[string]any{"path": "file.txt"}); err != nil || out == "parent" {
		t.Fatalf("read bound file: %s %v", out, err)
	}
	if _, err := reader.Execute(context.Background(), map[string]any{"path": filepath.Join(parent, "file.txt")}); err == nil {
		t.Fatal("read parent despite denial")
	}
	writer, _ := bound.Get("write_file")
	if _, err := writer.Execute(context.Background(), map[string]any{"path": "file.txt", "content": "bad"}); err == nil {
		t.Fatal("read-only native policy bypassed without process sandbox")
	}
}

type failBeforeTarget struct{}

func (failBeforeTarget) Wrap(cmd *exec.Cmd) error {
	cmd.Path = "/bin/sh"
	cmd.Args = []string{"sh", "-c", "exit 7"}
	return nil
}
func TestBashDistinguishesSandboxSetupFromCommandExit(t *testing.T) {
	skipIfWindows(t)
	bash := NewUnsafeBashTool(t.TempDir()).WithSandbox(failBeforeTarget{})
	_, err := bash.ExecuteOutput(context.Background(), map[string]any{"command": "exit 9"})
	var command *CommandError
	if err == nil || errors.As(err, &command) {
		t.Fatalf("setup became command exit: %v", err)
	}
	_, err = NewUnsafeBashTool(t.TempDir()).ExecuteOutput(context.Background(), map[string]any{"command": "exit 9"})
	if !errors.As(err, &command) || command.ExitCode != 9 {
		t.Fatalf("ordinary exit: %v", err)
	}
}
