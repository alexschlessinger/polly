package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBashToolSchema(t *testing.T) {
	tool := NewBashTool("")
	schema := tool.GetSchema()
	if schema.Title != "bash" {
		t.Fatalf("schema title = %q, want %q", schema.Title, "bash")
	}
	if schema.Properties["command"] == nil {
		t.Fatal("schema missing 'command' property")
	}
	if len(schema.Required) != 1 || schema.Required[0] != "command" {
		t.Fatalf("schema required = %v, want [command]", schema.Required)
	}
}

func TestBashToolMetadata(t *testing.T) {
	tool := NewBashTool("")
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
	tool := NewBashTool("")
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

func TestBashToolWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	tool := NewBashTool(dir)
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
	tool := NewBashTool("")
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
	tool := NewBashTool("")
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
	tool := NewBashTool("")
	_, err := tool.Execute(context.Background(), map[string]any{
		"command": "",
	})
	if err == nil {
		t.Fatal("Execute() expected error for empty command")
	}
}

func TestBashToolRejectsMissingCommand(t *testing.T) {
	tool := NewBashTool("")
	_, err := tool.Execute(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("Execute() expected error for missing command")
	}
}

func TestBashToolRegisteredAsNativeFactory(t *testing.T) {
	registry := NewToolRegistry(nil)
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
	dir := t.TempDir()
	script := filepath.Join(dir, "greet.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"hello $1\"\n"), 0755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	tool := NewBashTool("")
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
