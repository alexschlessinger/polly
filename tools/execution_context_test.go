package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
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

func TestExecutionPolicyRetainsDNSBlockAndMCPOverlaysKeepOnlyRestrictions(t *testing.T) {
	registry := NewToolRegistry(nil, WithSandboxFactory(func(cfg sandbox.Config) (sandbox.Sandbox, error) { return &mockSandbox{}, nil }, sandbox.Config{AllowNetwork: true, DenyDNS: true}))
	defer registry.Close()
	ec, err := registry.ExecutionPolicy(t.TempDir(), false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ec.Sandbox.AllowNetwork || !ec.Sandbox.DenyDNS {
		t.Fatalf("member policy widened the parent's network policy: %+v", ec.Sandbox)
	}
	home := t.TempDir()
	config, err := json.Marshal(sandbox.Config{
		AllowNetwork: true, WritablePaths: []string{home},
		DenyWrite: true, DenyDNS: true, DenyPaths: []string{filepath.Join(home, ".ssh")},
	})
	if err != nil {
		t.Fatal(err)
	}
	overlay := restrictiveSandboxOverlay(&MCPConfig{Sandbox: config})
	kept, err := sandbox.ParseConfig(overlay)
	if err != nil || kept == nil {
		t.Fatalf("overlay: %v %v", kept, err)
	}
	if kept.AllowNetwork || len(kept.WritablePaths) != 0 {
		t.Fatalf("server grants survived context binding: %+v", kept)
	}
	if !kept.DenyWrite || !kept.DenyDNS || len(kept.DenyPaths) != 1 {
		t.Fatalf("server restrictions dropped: %+v", kept)
	}
	if restrictiveSandboxOverlay(&MCPConfig{Sandbox: json.RawMessage(`false`)}) != nil || restrictiveSandboxOverlay(&MCPConfig{}) != nil {
		t.Fatal("opt-out or absent overlays must bind with the member policy alone")
	}
}

func TestBoundShellKeepsRestrictionsWithoutToolGrants(t *testing.T) {
	root, extra := t.TempDir(), t.TempDir()
	secret := filepath.Join(root, "secret")
	writeBlocked := filepath.Join(root, "protected")
	overlay := sandbox.Config{DenyPaths: []string{secret}, DenyWritePaths: []string{writeBlocked}, DenyWrite: true, DenyDNS: true, AllowNetwork: true, WritablePaths: []string{extra}}
	tool := &ShellTool{Command: "/bin/sh", schema: schema.ToolSchemaFromString(`{"title":"restricted","type":"object","properties":{}}`), sandboxCfg: &overlay}
	registry := NewToolRegistry([]Tool{tool}, WithSandboxFactory(func(sandbox.Config) (sandbox.Sandbox, error) { return &mockSandbox{}, nil }, sandbox.DefaultConfig()))
	defer registry.Close()
	ec, err := registry.ExecutionPolicy(root, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	bound, omitted, err := registry.BindExecutionContext(ec, []string{"restricted"})
	if err != nil {
		t.Fatalf("bind: %v; omitted %v", err, omitted)
	}
	defer bound.Close()
	got, _ := bound.Get("restricted")
	config := unwrapTool(got).(*ShellTool).SandboxDetails().Config
	if config == nil || !config.DenyWrite || !config.DenyDNS || config.AllowNetwork {
		t.Fatalf("bound shell policy: %+v", config)
	}
	if sandbox.ReadAllowed(*config, secret) == nil {
		t.Fatal("bound shell lost its read denial")
	}
	if len(config.DenyWritePaths) != 1 || config.DenyWritePaths[0] != writeBlocked {
		t.Fatalf("bound shell lost write island: %v", config.DenyWritePaths)
	}
	for _, path := range config.WritablePaths {
		if path == extra {
			t.Fatal("bound shell retained a tool-local write grant")
		}
	}
}
