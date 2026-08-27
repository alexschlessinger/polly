package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func TestBashToolSchema(t *testing.T) {
	tool := NewBashTool("")
	s := tool.GetSchema()
	if s.Title() != "bash" {
		t.Fatalf("schema title = %q, want %q", s.Title(), "bash")
	}
	if s.Properties()["command"] == nil {
		t.Fatal("schema missing 'command' property")
	}
	if req := s.Required(); len(req) != 1 || req[0] != "command" {
		t.Fatalf("schema required = %v, want [command]", req)
	}
}

func TestBashToolSchemaAnnotatesSandboxPosture(t *testing.T) {
	skipIfWindows(t)
	unsandboxed := newBashTool("")
	if desc := unsandboxed.GetSchema().Description(); strings.Contains(desc, "[sandboxed") {
		t.Fatalf("unsandboxed description = %q, must not claim sandboxing", desc)
	}

	sandboxed := newBashTool("").withSandboxConfig(&mockSandbox{}, sandbox.Config{})
	if desc := sandboxed.GetSchema().Description(); !strings.HasSuffix(desc, "[sandboxed]") {
		t.Fatalf("sandboxed description = %q, want the [sandboxed] suffix", desc)
	}

	// A whole-tree git policy (workspace preset without the git component)
	// must warn the model that commits will fail.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	t.Chdir(root)
	cfg, err := sandbox.ParsePreset("workspace")
	if err != nil {
		t.Fatalf("ParsePreset(workspace) error = %v", err)
	}
	wholeTree := newBashTool("").withSandboxConfig(&mockSandbox{}, cfg)
	if desc := wholeTree.GetSchema().Description(); !strings.Contains(desc, ".git is read-only") {
		t.Fatalf("whole-tree description = %q, want the read-only .git warning", desc)
	}

	leaf, err := sandbox.ParsePreset("workspace+git")
	if err != nil {
		t.Fatalf("ParsePreset(workspace+git) error = %v", err)
	}
	leafMode := newBashTool("").withSandboxConfig(&mockSandbox{}, leaf)
	if desc := leafMode.GetSchema().Description(); strings.Contains(desc, ".git is read-only") {
		t.Fatalf("leaf-mode description = %q, must not warn when commits work", desc)
	}
}

func TestBashToolMetadata(t *testing.T) {
	tool := newBashTool("")
	if tool.GetName() != "bash" {
		t.Fatalf("GetName() = %q, want %q", tool.GetName(), "bash")
	}
	if tool.GetType() != "native" {
		t.Fatalf("GetType() = %q, want %q", tool.GetType(), "native")
	}
	if tool.GetSource() != "builtin" {
		t.Fatalf("GetSource() = %q, want %q", tool.GetSource(), "builtin")
	}
}

func TestBashToolExecutesCommand(t *testing.T) {
	tool := newBashTool("")
	result, err := tool.Execute(context.Background(), map[string]any{
		"command": "echo hello",
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if strings.TrimSpace(result) != "hello" {
		t.Fatalf("Execute() result = %q, want %q", result, "hello")
	}
}

func TestBashToolLeavesLegacySandboxFilesOpenAfterExecution(t *testing.T) {
	skipIfWindows(t)
	file, err := os.CreateTemp(t.TempDir(), "bash-sandbox-extra-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	tool := newBashTool("").WithSandbox(&mockSandbox{file: file})
	if _, err := tool.Execute(context.Background(), map[string]any{"command": "true"}); err != nil {
		t.Fatal(err)
	}
	// Legacy Sandbox implementations retain ownership of descriptors they
	// place in ExtraFiles; execution must not close them.
	if _, err := file.Stat(); err != nil {
		t.Fatalf("legacy sandbox descriptor was closed after bash execution: %v", err)
	}
}

func TestBashToolWorkingDirectory(t *testing.T) {
	skipIfWindows(t)
	dir := t.TempDir()
	tool := newBashTool(dir)
	result, err := tool.Execute(context.Background(), map[string]any{
		"command": "pwd",
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	resolved, _ := filepath.EvalSymlinks(dir)
	resultResolved, _ := filepath.EvalSymlinks(strings.TrimSpace(result))
	if resultResolved != resolved {
		t.Fatalf("Execute() pwd = %q (resolved %q), want %q", strings.TrimSpace(result), resultResolved, resolved)
	}
}

func TestBashToolReturnsStderr(t *testing.T) {
	tool := newBashTool("")
	result, err := tool.Execute(context.Background(), map[string]any{
		"command": "echo out && echo err >&2",
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(result, "out") {
		t.Fatalf("Execute() result missing stdout: %q", result)
	}
	if !strings.Contains(result, "err") {
		t.Fatalf("Execute() result missing stderr: %q", result)
	}
}

func TestBashToolReturnsErrorOnFailure(t *testing.T) {
	tool := newBashTool("")
	_, err := tool.Execute(context.Background(), map[string]any{
		"command": "exit 42",
	})
	if err == nil {
		t.Fatal("Execute() expected error for non-zero exit")
	}
	if !strings.Contains(err.Error(), "exit") {
		t.Fatalf("Execute() error = %v, want exit code info", err)
	}
}

func TestBashToolRejectsEmptyCommand(t *testing.T) {
	tool := newBashTool("")
	_, err := tool.Execute(context.Background(), map[string]any{
		"command": "",
	})
	if err == nil {
		t.Fatal("Execute() expected error for empty command")
	}
}

func TestBashToolRejectsMissingCommand(t *testing.T) {
	tool := newBashTool("")
	_, err := tool.Execute(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("Execute() expected error for missing command")
	}
}

func TestBashToolRegisteredAsNativeFactory(t *testing.T) {
	registry := NewToolRegistry(nil, WithUnsafeNoSandbox())
	result, err := registry.LoadToolAuto("bash")
	if err != nil {
		t.Fatalf("LoadToolAuto('bash') error = %v", err)
	}
	if result.Type != "native" {
		t.Fatalf("LoadToolAuto('bash') type = %q, want %q", result.Type, "native")
	}
	tool, ok := registry.Get("bash")
	if !ok {
		t.Fatal("expected bash tool to be registered")
	}
	if tool.GetName() != "bash" {
		t.Fatalf("tool name = %q, want %q", tool.GetName(), "bash")
	}
}

func TestBashToolRunsScriptByAbsolutePath(t *testing.T) {
	skipIfWindows(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "greet.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"hello $1\"\n"), 0755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	tool := newBashTool("")
	result, err := tool.Execute(context.Background(), map[string]any{
		"command": script + " world",
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if strings.TrimSpace(result) != "hello world" {
		t.Fatalf("Execute() result = %q, want %q", strings.TrimSpace(result), "hello world")
	}
}
