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
	"maps"
	"slices"
	"strings"
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
	ec, err := registry.ExecutionPolicy(child, ExecutionGrant{ReadOnly: true, DeniedReads: []string{parent}})
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
	ec, err := registry.ExecutionPolicy(t.TempDir(), ExecutionGrant{})
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
	ec, err := registry.ExecutionPolicy(root, ExecutionGrant{})
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

func TestExecutionPolicyReadOnlyScratch(t *testing.T) {
	canonical := func(path string) string {
		t.Helper()
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}
	registry := NewToolRegistry(nil, WithUnsafeNoSandbox())
	defer registry.Close()
	if _, err := registry.LoadToolAuto("write_file"); err != nil {
		t.Fatal(err)
	}
	root, scratch := canonical(t.TempDir()), canonical(t.TempDir())
	ec, err := registry.ExecutionPolicy(root, ExecutionGrant{ReadOnly: true, Scratch: scratch})
	if err != nil {
		t.Fatal(err)
	}
	if !ec.ReadOnly || ec.Scratch != scratch || ec.Sandbox.DenyWrite || !ec.Sandbox.DenyHostTemp || !slices.Equal(ec.Sandbox.WritablePaths, []string{scratch}) || !slices.Contains(ec.Sandbox.DenyWritePaths, root) {
		t.Fatalf("read-only scratch policy = %+v", ec)
	}
	wantEnv := map[string]string{"TMPDIR": scratch, "TMP": scratch, "TEMP": scratch, "GOTMPDIR": scratch, "GOCACHE": filepath.Join(scratch, "go-build"), "GOPROXY": "off"}
	if !maps.Equal(ec.Sandbox.Env, wantEnv) {
		t.Fatalf("scratch env = %v, want %v", ec.Sandbox.Env, wantEnv)
	}
	if err := sandbox.WriteAllowed(ec.Sandbox, filepath.Join(root, "f")); err == nil {
		t.Fatal("read-only root writable in-process")
	}
	if err := sandbox.WriteAllowed(ec.Sandbox, filepath.Join(scratch, "f")); err != nil {
		t.Fatalf("scratch write refused: %v", err)
	}
	if err := sandbox.WriteAllowed(ec.Sandbox, filepath.Join(os.TempDir(), "polly-scratch-probe")); err == nil {
		t.Fatal("host temp writable for a read-only context")
	}
	bound, _, err := registry.BindExecutionContext(ec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	writer, _ := bound.Get("write_file")
	if _, err := writer.Execute(context.Background(), map[string]any{"path": "file.txt", "content": "bad"}); err == nil {
		t.Fatal("bound write into the read-only root succeeded")
	}
	if _, err := writer.Execute(context.Background(), map[string]any{"path": filepath.Join(scratch, "note.txt"), "content": "ok"}); err != nil {
		t.Fatalf("bound write into the scratch: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(scratch, "note.txt")); err != nil || string(data) != "ok" {
		t.Fatalf("scratch file = %q %v", data, err)
	}
	if _, err := registry.ExecutionPolicy(root, ExecutionGrant{ReadOnly: true, Scratch: filepath.Join(scratch, "missing")}); err == nil || !strings.Contains(err.Error(), "must be an existing directory") {
		t.Fatalf("missing scratch accepted: %v", err)
	}
	nested := filepath.Join(root, "scratch")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ExecutionPolicy(root, ExecutionGrant{ReadOnly: true, Scratch: nested}); err == nil || !strings.Contains(err.Error(), "outside the execution root") {
		t.Fatalf("nested scratch accepted: %v", err)
	}
	editing, err := registry.ExecutionPolicy(root, ExecutionGrant{Scratch: scratch})
	if err != nil || editing.ReadOnly || editing.Sandbox.DenyHostTemp || editing.Sandbox.DenyWrite || !slices.Equal(editing.Sandbox.WritablePaths, []string{root, scratch}) || !maps.Equal(editing.Sandbox.Env, wantEnv) {
		t.Fatalf("editing scratch policy = %+v %v", editing, err)
	}
}

// An operator's readonly preset keeps denying every write, scratch included.
func TestDenyWritePresetStillDeniesScratch(t *testing.T) {
	registry := NewToolRegistry(nil, WithSandboxFactory(mockSandboxFactory(&mockSandbox{}), sandbox.Config{DenyWrite: true}))
	defer registry.Close()
	scratch := t.TempDir()
	for _, readOnly := range []bool{true, false} {
		ec, err := registry.ExecutionPolicy(t.TempDir(), ExecutionGrant{ReadOnly: readOnly, Scratch: scratch})
		if err != nil {
			t.Fatal(err)
		}
		if !ec.Sandbox.DenyWrite || !ec.ReadOnly || ec.Sandbox.Env != nil {
			t.Fatalf("readOnly=%v: operator denyWrite weakened: %+v", readOnly, ec.Sandbox)
		}
		if err := sandbox.WriteAllowed(ec.Sandbox, filepath.Join(scratch, "f")); err == nil || !strings.Contains(err.Error(), "denies all file writes") {
			t.Fatalf("readOnly=%v: scratch writable under denyWrite: %v", readOnly, err)
		}
	}
}
