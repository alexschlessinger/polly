package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// Preserve the pre-hardening public call signatures while callers migrate to
// the explicit unsafe constructors and registry-backed batch loader.
var (
	_ func(string) *BashTool                                = NewBashTool
	_ func(string, ...sandbox.Sandbox) (*ShellTool, error)  = NewShellTool
	_ func(string) (*MCPClient, error)                      = NewMCPClient
	_ func(*MCPConfig, sandbox.Sandbox) (*MCPClient, error) = NewMCPClientFromConfig
	_ func([]string) ([]Tool, error)                        = LoadShellTools
)

// checkUvxAvailable checks if uvx is available on the system
func checkUvxAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("uvx"); err != nil {
		t.Skip("uvx is not installed, skipping MCP tests")
	}
}

// createMCPTestConfig creates a temporary MCP config file for testing
func createMCPTestConfig(t *testing.T, serverName, command string, args []string) string {
	t.Helper()

	config := MCPServersConfig{
		MCPServers: map[string]MCPConfig{
			serverName: {
				Command: command,
				Args:    args,
			},
		},
	}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("Failed to marshal test config: %v", err)
	}

	// Create temp file
	f, err := os.CreateTemp(t.TempDir(), "mcp-test-*.json")
	if err != nil {
		t.Fatalf("Failed to create temp config file: %v", err)
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		t.Fatalf("Failed to write temp config: %v", err)
	}

	return f.Name()
}

func TestMCPClient(t *testing.T) {
	checkUvxAvailable(t)

	// Create MCP server config file
	configPath := createMCPTestConfig(t, "time", "uvx", []string{"mcp-server-time"})

	client, err := NewUnsafeMCPClient(configPath)
	if err != nil {
		t.Skipf("Could not start MCP server: %v", err)
	}
	defer client.Close()

	// Give the server a moment to initialize
	time.Sleep(100 * time.Millisecond)

	// Test listing tools
	tools, err := client.ListTools()
	if err != nil {
		t.Fatalf("Failed to list tools: %v", err)
	}

	if len(tools) == 0 {
		t.Error("Expected at least one tool from mcp-server-time")
	}

	// Verify we got expected tools
	var foundTimeTools bool
	for _, tool := range tools {
		s := tool.GetSchema()
		if s != nil && strings.Contains(s.Title(), "time") {
			foundTimeTools = true
			break
		}
	}

	if !foundTimeTools {
		t.Error("Expected to find time-related tools")
	}
}

func TestMCPToolExecution(t *testing.T) {
	checkUvxAvailable(t)
	ctx := context.Background()

	// Create MCP server config file
	configPath := createMCPTestConfig(t, "time", "uvx", []string{"mcp-server-time"})

	client, err := NewUnsafeMCPClient(configPath)
	if err != nil {
		t.Skipf("Could not start MCP server: %v", err)
	}
	defer client.Close()

	// Give the server a moment to initialize
	time.Sleep(100 * time.Millisecond)

	// Get tools
	tools, err := client.ListTools()
	if err != nil {
		t.Fatalf("Failed to list tools: %v", err)
	}

	if len(tools) == 0 {
		t.Fatal("No tools available from server")
	}

	// Find the get_current_time tool and test it
	for _, tool := range tools {
		s := tool.GetSchema()
		if s.Title() == "get_current_time" {
			t.Logf("Testing tool: %s", s.Title())

			// Test with valid timezone
			args := map[string]any{
				"timezone": "America/New_York",
			}

			result, err := tool.Execute(ctx, args)
			if err != nil {
				t.Errorf("Failed to execute tool with valid args: %v", err)
			} else {
				t.Logf("Tool result: %s", result)
				// Verify we got some result
				if result == "" {
					t.Error("Expected non-empty result from tool execution")
				}
				// Result should contain time information
				if !strings.Contains(result, "time") && !strings.Contains(result, "Time") {
					t.Error("Expected result to contain time information")
				}
			}
			return
		}
	}

	t.Error("Could not find get_current_time tool")
}

func TestMCPToolSchema(t *testing.T) {
	checkUvxAvailable(t)

	// Create MCP server config file
	configPath := createMCPTestConfig(t, "time", "uvx", []string{"mcp-server-time"})

	client, err := NewUnsafeMCPClient(configPath)
	if err != nil {
		t.Skipf("Could not start MCP server: %v", err)
	}
	defer client.Close()

	// Give the server a moment to initialize
	time.Sleep(100 * time.Millisecond)

	// Get tools
	tools, err := client.ListTools()
	if err != nil {
		t.Fatalf("Failed to list tools: %v", err)
	}

	// Test schema generation for each tool
	for _, tool := range tools {
		s := tool.GetSchema()

		if s == nil {
			t.Error("Expected non-nil schema")
			continue
		}

		// Verify basic schema properties
		if s.Title() == "" {
			t.Error("Expected schema to have a title")
		}

		if typ, _ := s.Raw["type"].(string); typ != "object" {
			t.Errorf("Expected schema type to be 'object', got %s", typ)
		}

		t.Logf("Tool schema - Title: %s, Description: %s", s.Title(), s.Description())
	}
}

func TestMCPClientInvalidCommand(t *testing.T) {
	// Test with a non-existent command
	_, err := NewMCPClient("this-command-does-not-exist")
	if err == nil {
		t.Error("Expected error for non-existent command")
	}
}

func TestMCPClientEmptyCommand(t *testing.T) {
	// Test with empty command
	_, err := NewUnsafeMCPClient("")
	if err == nil {
		t.Error("Expected error for empty command")
	}
}

func TestMCPToolNoargsFiltering(t *testing.T) {
	checkUvxAvailable(t)
	ctx := context.Background()

	// Create MCP server config file
	configPath := createMCPTestConfig(t, "time", "uvx", []string{"mcp-server-time"})

	client, err := NewUnsafeMCPClient(configPath)
	if err != nil {
		t.Skipf("Could not start MCP server: %v", err)
	}
	defer client.Close()

	// Give the server a moment to initialize
	time.Sleep(100 * time.Millisecond)

	// Get tools
	tools, err := client.ListTools()
	if err != nil {
		t.Fatalf("Failed to list tools: %v", err)
	}

	if len(tools) == 0 {
		t.Fatal("No tools available")
	}

	tool := tools[0]

	args := map[string]any{
		"timezone": "America/New_York",
	}

	_, err = tool.Execute(ctx, args)
	// We don't check the error because the tool might still fail for other reasons
	// (network issues, invalid timezone format, etc.)
	t.Logf("Execution completed (error ok): %v", err)
}

func createTestScript(t *testing.T, dir string) string {
	script := `#!/bin/bash
if [ "$1" = "--schema" ]; then
	echo '{
		"title": "test-tool",
		"description": "A test tool",
		"type": "object",
		"properties": {
			"message": {
				"type": "string",
				"description": "A test message"
			}
		},
		"required": ["message"]
	}'
elif [ "$1" = "--execute" ]; then
	# Parse JSON argument
	MESSAGE=$(echo "$2" | sed -n 's/.*"message":[[:space:]]*"\([^"]*\)".*/\1/p')
	echo "Received: $MESSAGE"
else
	echo "Unknown argument: $1"
	exit 1
fi
`
	scriptPath := filepath.Join(dir, "test-tool.sh")
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	if err != nil {
		t.Fatalf("Failed to create test script: %v", err)
	}
	return scriptPath
}

func TestNewShellTool(t *testing.T) {
	dir := t.TempDir()
	scriptPath := createTestScript(t, dir)

	tool, err := NewShellTool(scriptPath)
	if err != nil {
		t.Fatalf("Failed to create shell tool: %v", err)
	}

	schema := tool.GetSchema()
	if schema == nil {
		t.Fatal("Expected schema to be non-nil")
	}

	if schema.Title() != "test-tool" {
		t.Errorf("Expected title 'test-tool', got %s", schema.Title())
	}

	if schema.Description() != "A test tool" {
		t.Errorf("Expected description 'A test tool', got %s", schema.Description())
	}
}

func TestNewShellToolParsesStrictMetadata(t *testing.T) {
	script := `#!/bin/bash
if [ "$1" = "--schema" ]; then
	echo '{
		"title": "strict-tool",
		"description": "A strict test tool",
		"type": "object",
		"strict": true,
		"properties": {
			"message": {
				"type": "string"
			}
		}
	}'
elif [ "$1" = "--execute" ]; then
	echo "ok"
fi
`
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "strict-tool.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("Failed to create strict test script: %v", err)
	}

	tool, err := newShellTool(scriptPath)
	if err != nil {
		t.Fatalf("Failed to create shell tool: %v", err)
	}

	schema := tool.GetSchema()
	if schema == nil {
		t.Fatal("Expected schema to be non-nil")
	}
	if !schema.Strict {
		t.Fatal("expected strict metadata to set ToolSchema.Strict")
	}
	if _, ok := schema.Raw["strict"]; ok {
		t.Fatalf("expected strict metadata to be removed from Raw, got %#v", schema.Raw["strict"])
	}
}

func TestShellToolExecute(t *testing.T) {
	dir := t.TempDir()
	scriptPath := createTestScript(t, dir)

	tool, err := newShellTool(scriptPath)
	if err != nil {
		t.Fatalf("Failed to create shell tool: %v", err)
	}

	args := map[string]any{
		"message": "Hello, World!",
	}

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Failed to execute tool: %v", err)
	}

	expected := "Received: Hello, World!"
	if result != expected {
		t.Errorf("Expected result '%s', got '%s'", expected, result)
	}
}

func TestShellToolExecuteWithCancel(t *testing.T) {
	// Create a script that sleeps to test cancellation
	script := `#!/bin/bash
if [ "$1" = "--schema" ]; then
	echo '{"title": "slow-tool", "type": "object"}'
elif [ "$1" = "--execute" ]; then
	sleep 10
	echo "Should not reach here"
fi
`
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "slow-tool.sh")
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	if err != nil {
		t.Fatalf("Failed to create test script: %v", err)
	}

	tool, err := newShellTool(scriptPath)
	if err != nil {
		t.Fatalf("Failed to create shell tool: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err = tool.Execute(ctx, map[string]any{})
	if err == nil {
		t.Error("Expected error due to context cancellation")
	}
}

func TestLoadShellTools(t *testing.T) {
	dir := t.TempDir()

	// Create multiple test scripts
	script1 := createTestScript(t, dir)

	// Create a second test script
	script2 := `#!/bin/bash
if [ "$1" = "--schema" ]; then
	echo '{"title": "tool2", "type": "object"}'
elif [ "$1" = "--execute" ]; then
	echo "Tool 2 executed"
fi
`
	script2Path := filepath.Join(dir, "tool2.sh")
	err := os.WriteFile(script2Path, []byte(script2), 0755)
	if err != nil {
		t.Fatalf("Failed to create second test script: %v", err)
	}

	registry := NewToolRegistry(nil, WithSandboxFactory(mockSandboxFactory(&mockSandbox{}), sandbox.Config{}))
	tools, err := LoadShellToolsWithRegistry(registry, []string{script1, script2Path})
	if err != nil {
		t.Fatalf("Failed to load shell tools: %v", err)
	}

	if len(tools) != 2 {
		t.Errorf("Expected 2 tools, got %d", len(tools))
	}
}

func TestShellToolInvalidJSON(t *testing.T) {
	// Create a script that returns invalid JSON for schema
	script := `#!/bin/bash
if [ "$1" = "--schema" ]; then
	echo 'not valid json'
fi
`
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "invalid.sh")
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	if err != nil {
		t.Fatalf("Failed to create test script: %v", err)
	}

	_, err = newShellTool(scriptPath)
	if err == nil {
		t.Error("Expected error for invalid JSON schema")
	}
}

func TestShellToolExecuteError(t *testing.T) {
	// Create a script that exits with error during execution
	script := `#!/bin/bash
if [ "$1" = "--schema" ]; then
	echo '{"title": "error-tool", "type": "object"}'
elif [ "$1" = "--execute" ]; then
	echo "Error occurred" >&2
	exit 1
fi
`
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "error.sh")
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	if err != nil {
		t.Fatalf("Failed to create test script: %v", err)
	}

	tool, err := newShellTool(scriptPath)
	if err != nil {
		t.Fatalf("Failed to create shell tool: %v", err)
	}

	_, err = tool.Execute(context.Background(), map[string]any{})
	if err == nil {
		t.Error("Expected error from tool execution")
	}
}

func TestShellToolMarshalArgsError(t *testing.T) {
	dir := t.TempDir()
	scriptPath := createTestScript(t, dir)

	tool, err := newShellTool(scriptPath)
	if err != nil {
		t.Fatalf("Failed to create shell tool: %v", err)
	}

	// Create args that can't be marshaled to JSON
	args := map[string]any{
		"invalid": make(chan int), // channels can't be marshaled to JSON
	}

	_, err = tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Expected error when marshaling invalid arguments")
	}
}

func TestShellToolComplexArgs(t *testing.T) {
	// Create a script that handles complex JSON arguments
	script := `#!/bin/bash
if [ "$1" = "--schema" ]; then
	echo '{
		"title": "complex-tool",
		"type": "object",
		"properties": {
			"count": {"type": "integer"},
			"values": {"type": "array", "items": {"type": "string"}}
		}
	}'
elif [ "$1" = "--execute" ]; then
	# Just echo back the JSON to verify it was received
	echo "$2"
fi
`
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "complex.sh")
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	if err != nil {
		t.Fatalf("Failed to create test script: %v", err)
	}

	tool, err := newShellTool(scriptPath)
	if err != nil {
		t.Fatalf("Failed to create shell tool: %v", err)
	}

	args := map[string]any{
		"count":  42,
		"values": []string{"a", "b", "c"},
	}

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Failed to execute tool: %v", err)
	}

	// Verify the result is valid JSON containing our args
	var resultMap map[string]any
	err = json.Unmarshal([]byte(result), &resultMap)
	if err != nil {
		t.Fatalf("Result is not valid JSON: %v", err)
	}

	if int(resultMap["count"].(float64)) != 42 {
		t.Errorf("Expected count 42, got %v", resultMap["count"])
	}
}

func createSandboxedTestScript(t *testing.T, dir string) string {
	t.Helper()
	script := `#!/bin/bash
if [ "$1" = "--schema" ]; then
	echo '{
		"title": "sandboxed-tool",
		"description": "A sandboxed test tool",
		"type": "object",
		"sandbox": true,
		"properties": {
			"message": {
				"type": "string",
				"description": "A test message"
			}
		},
		"required": ["message"]
	}'
elif [ "$1" = "--execute" ]; then
	MESSAGE=$(echo "$2" | sed -n 's/.*"message":[[:space:]]*"\([^"]*\)".*/\1/p')
	echo "Received: $MESSAGE"
fi
`
	scriptPath := filepath.Join(dir, "sandboxed-tool.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("Failed to create sandboxed test script: %v", err)
	}
	return scriptPath
}

func createSandboxedTestScriptWithSpec(t *testing.T, dir string) string {
	t.Helper()
	script := `#!/bin/bash
if [ "$1" = "--schema" ]; then
	echo '{
		"title": "sandboxed-spec-tool",
		"description": "A sandboxed test tool with spec overrides",
		"type": "object",
		"sandbox": {"allowNetwork": true, "writablePaths": ["/tmp/extra"]},
		"properties": {
			"message": {
				"type": "string",
				"description": "A test message"
			}
		},
		"required": ["message"]
	}'
elif [ "$1" = "--execute" ]; then
	MESSAGE=$(echo "$2" | sed -n 's/.*"message":[[:space:]]*"\([^"]*\)".*/\1/p')
	echo "Received: $MESSAGE"
fi
`
	scriptPath := filepath.Join(dir, "sandboxed-spec-tool.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("Failed to create sandboxed spec test script: %v", err)
	}
	return scriptPath
}

func TestShellToolSandboxConfigObject(t *testing.T) {
	dir := t.TempDir()
	scriptPath := createSandboxedTestScriptWithSpec(t, dir)

	tool, err := newShellTool(scriptPath)
	if err != nil {
		t.Fatalf("Failed to create shell tool: %v", err)
	}

	if !tool.WantsSandbox() {
		t.Fatal("Expected WantsSandbox()=true for script with sandbox object")
	}

	sbCfg := tool.SandboxConfig()
	if sbCfg == nil {
		t.Fatal("Expected non-nil SandboxConfig")
	}
	if !sbCfg.AllowNetwork {
		t.Error("Expected AllowNetwork=true from config")
	}
	if len(sbCfg.WritablePaths) != 1 || sbCfg.WritablePaths[0] != "/tmp/extra" {
		t.Errorf("Expected WritablePaths=[/tmp/extra], got %v", sbCfg.WritablePaths)
	}
}

func createSandboxedTestScriptWithFullSpec(t *testing.T, dir string) string {
	t.Helper()
	script := `#!/bin/bash
if [ "$1" = "--schema" ]; then
	echo '{
		"title": "full-spec-tool",
		"description": "A tool with full sandbox spec",
		"type": "object",
		"sandbox": {
			"allowNetwork": true,
			"writablePaths": ["/tmp/deploy"],
			"readPaths": ["~/.aws"],
			"allowEnv": ["AWS_PROFILE", "AWS_REGION", "HOME", "PATH"]
		},
		"properties": {
			"cmd": {"type": "string"}
		}
	}'
elif [ "$1" = "--execute" ]; then
	echo "ok"
fi
`
	scriptPath := filepath.Join(dir, "full-spec-tool.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("Failed to create full spec test script: %v", err)
	}
	return scriptPath
}

func TestShellToolSandboxConfigWithReadPathsAndEnv(t *testing.T) {
	dir := t.TempDir()
	scriptPath := createSandboxedTestScriptWithFullSpec(t, dir)

	tool, err := newShellTool(scriptPath)
	if err != nil {
		t.Fatalf("Failed to create shell tool: %v", err)
	}

	if !tool.WantsSandbox() {
		t.Fatal("Expected WantsSandbox()=true")
	}

	sbCfg := tool.SandboxConfig()
	if sbCfg == nil {
		t.Fatal("Expected non-nil SandboxConfig")
	}
	if !sbCfg.AllowNetwork {
		t.Error("Expected AllowNetwork=true")
	}
	if len(sbCfg.WritablePaths) != 1 || sbCfg.WritablePaths[0] != "/tmp/deploy" {
		t.Errorf("WritablePaths = %v, want [/tmp/deploy]", sbCfg.WritablePaths)
	}
	if len(sbCfg.ReadPaths) != 1 || sbCfg.ReadPaths[0] != "~/.aws" {
		t.Errorf("ReadPaths = %v, want [~/.aws]", sbCfg.ReadPaths)
	}
	if len(sbCfg.AllowEnv) != 4 {
		t.Errorf("AllowEnv = %v, want 4 entries", sbCfg.AllowEnv)
	}
	expected := []string{"AWS_PROFILE", "AWS_REGION", "HOME", "PATH"}
	for i, want := range expected {
		if i >= len(sbCfg.AllowEnv) || sbCfg.AllowEnv[i] != want {
			t.Errorf("AllowEnv[%d] = %q, want %q", i, sbCfg.AllowEnv[i], want)
		}
	}
}

func TestShellToolWantsSandbox(t *testing.T) {
	dir := t.TempDir()

	// Script without sandbox flag
	tool, err := newShellTool(createTestScript(t, dir))
	if err != nil {
		t.Fatalf("Failed to create shell tool: %v", err)
	}
	if tool.WantsSandbox() {
		t.Error("Expected WantsSandbox()=false for script without sandbox flag")
	}

	// Script with sandbox: true
	tool2, err := newShellTool(createSandboxedTestScript(t, dir))
	if err != nil {
		t.Fatalf("Failed to create sandboxed shell tool: %v", err)
	}
	if !tool2.WantsSandbox() {
		t.Error("Expected WantsSandbox()=true for script with sandbox flag")
	}
}

func TestShellToolWithSandbox(t *testing.T) {
	dir := t.TempDir()
	scriptPath := createSandboxedTestScript(t, dir)

	tool, err := newShellTool(scriptPath)
	if err != nil {
		t.Fatalf("Failed to create shell tool: %v", err)
	}

	// Without sandbox applied, description should not contain [sandboxed]
	schema := tool.GetSchema()
	if strings.Contains(schema.Description(), "[sandboxed]") {
		t.Error("Expected no [sandboxed] hint without sandbox applied")
	}

	// With sandbox applied, description should contain [sandboxed]
	sandboxed := tool.WithSandbox(&mockSandbox{})
	schema = sandboxed.GetSchema()
	if !strings.Contains(schema.Description(), "[sandboxed]") {
		t.Errorf("Expected [sandboxed] hint in description, got %q", schema.Description())
	}

	// WithSandbox should preserve command, schema, and wantsSandbox
	if sandboxed.Command != tool.Command {
		t.Error("WithSandbox should preserve Command")
	}
	if sandboxed.GetName() != tool.GetName() {
		t.Error("WithSandbox should preserve name")
	}
	if !sandboxed.WantsSandbox() {
		t.Error("WithSandbox should preserve wantsSandbox")
	}
}

func TestShellToolSandboxExecution(t *testing.T) {
	dir := t.TempDir()
	scriptPath := createSandboxedTestScript(t, dir)

	tool, err := newShellTool(scriptPath)
	if err != nil {
		t.Fatalf("Failed to create shell tool: %v", err)
	}

	sb := &mockSandbox{}
	sandboxed := tool.WithSandbox(sb)

	_, _ = sandboxed.Execute(context.Background(), map[string]any{"message": "test"})
	if !sb.called {
		t.Error("Expected sandbox.Wrap to be called during execution")
	}
}

func TestShellToolClosesSandboxFilesAfterExecution(t *testing.T) {
	dir := t.TempDir()
	tool, err := newShellTool(createSandboxedTestScript(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(t.TempDir(), "shell-sandbox-extra-*")
	if err != nil {
		t.Fatal(err)
	}
	sandboxed := tool.WithSandbox(&mockSandbox{file: file})
	if _, err := sandboxed.Execute(context.Background(), map[string]any{"message": "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Stat(); err == nil {
		t.Fatal("sandbox-added descriptor remains open after shell execution")
	}
}

func TestShellToolSandboxWrapError(t *testing.T) {
	dir := t.TempDir()
	scriptPath := createSandboxedTestScript(t, dir)

	tool, err := newShellTool(scriptPath)
	if err != nil {
		t.Fatalf("Failed to create shell tool: %v", err)
	}

	sb := &mockSandbox{err: fmt.Errorf("sandbox unavailable")}
	sandboxed := tool.WithSandbox(sb)

	_, err = sandboxed.Execute(context.Background(), map[string]any{"message": "test"})
	if err == nil {
		t.Fatal("Expected error when sandbox.Wrap fails")
	}
	if !strings.Contains(err.Error(), "sandbox") {
		t.Errorf("Expected sandbox error, got: %v", err)
	}
}

func TestMCPConfigSandboxConfig(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		isNil   bool
		wantErr bool
		net     bool
	}{
		{"absent", `{"command":"echo"}`, true, false, false},
		{"true", `{"command":"echo","sandbox":true}`, false, false, false},
		{"false", `{"command":"echo","sandbox":false}`, true, false, false},
		{"object", `{"command":"echo","sandbox":{"allowNetwork":true}}`, false, false, true},
		{"invalid string", `{"command":"echo","sandbox":"yes"}`, false, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg MCPConfig
			if err := json.Unmarshal([]byte(tt.json), &cfg); err != nil {
				t.Fatalf("Unmarshal error: %v", err)
			}
			sbCfg, err := cfg.SandboxConfig()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got config %+v", sbCfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.isNil {
				if sbCfg != nil {
					t.Fatalf("expected nil config, got %+v", sbCfg)
				}
				return
			}
			if sbCfg == nil {
				t.Fatal("expected non-nil config")
			}
			if sbCfg.AllowNetwork != tt.net {
				t.Fatalf("AllowNetwork = %v, want %v", sbCfg.AllowNetwork, tt.net)
			}
		})
	}
}

func TestRegistryAppliesSandboxToOptInShellTools(t *testing.T) {
	dir := t.TempDir()
	sandboxedScript := createSandboxedTestScript(t, dir)

	sb := &mockSandbox{}
	registry := NewToolRegistry(nil, WithSandboxFactory(mockSandboxFactory(sb), sandbox.Config{}))

	_, err := registry.LoadShellTool(sandboxedScript)
	if err != nil {
		t.Fatalf("Failed to load shell tool: %v", err)
	}

	// Tool that opted in should have the [sandboxed] hint
	for _, tool := range registry.All() {
		schema := tool.GetSchema()
		if schema != nil && strings.Contains(schema.Description(), "[sandboxed]") {
			return
		}
	}
	t.Error("Expected opt-in shell tool to have [sandboxed] hint when registry has sandbox")
}

func TestRegistrySandboxesNonOptInShellTools(t *testing.T) {
	dir := t.TempDir()
	scriptPath := createTestScript(t, dir)

	sb := &mockSandbox{}
	registry := NewToolRegistry(nil, WithSandboxFactory(mockSandboxFactory(sb), sandbox.Config{}))

	_, err := registry.LoadShellTool(scriptPath)
	if err != nil {
		t.Fatalf("Failed to load shell tool: %v", err)
	}

	// Shell tools are always sandboxed when factory exists, even without opt-in
	found := false
	for _, tool := range registry.All() {
		schema := tool.GetSchema()
		if schema != nil && strings.Contains(schema.Description(), "[sandboxed]") {
			found = true
		}
	}
	if !found {
		t.Error("Expected non-opt-in shell tool to be sandboxed when factory exists")
	}
}

func TestShellToolSandboxFalseOptOut(t *testing.T) {
	// A shell tool may opt out only when the registry owner also made the
	// explicit unsafe choice; tool-controlled schema metadata is not authority
	// to silently disable containment by itself.
	script := `#!/bin/bash
if [ "$1" = "--schema" ]; then
	echo '{
		"title": "unsandboxed-tool",
		"description": "Opts out of sandbox",
		"type": "object",
		"sandbox": false,
		"properties": {"msg": {"type": "string"}}
	}'
elif [ "$1" = "--execute" ]; then
	echo "ok"
fi
`
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "unsandboxed.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	sb := &mockSandbox{}
	registry := NewToolRegistry(nil,
		WithSandboxFactory(mockSandboxFactory(sb), sandbox.Config{}),
		WithUnsafeNoSandbox())

	_, err := registry.LoadShellTool(scriptPath)
	if err != nil {
		t.Fatalf("Failed to load shell tool: %v", err)
	}

	for _, tool := range registry.All() {
		schema := tool.GetSchema()
		if schema != nil && strings.Contains(schema.Description(), "[sandboxed]") {
			t.Error("Expected shell tool with sandbox:false to NOT be sandboxed")
		}
	}
}

func TestShellToolSandboxFalseRequiresUnsafeRegistryOptOut(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "unsandboxed.sh")
	script := `#!/bin/bash
if [ "$1" = "--schema" ]; then
  echo '{"title":"unsandboxed-tool","type":"object","sandbox":false}'
fi
`
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	registry := NewToolRegistry(nil, WithSandboxFactory(mockSandboxFactory(&mockSandbox{}), sandbox.Config{}))
	if _, err := registry.LoadShellTool(scriptPath); err == nil {
		t.Fatal("expected sandbox:false to be refused without WithUnsafeNoSandbox")
	}
}

func TestMCPConfigSandboxOptOut(t *testing.T) {
	tests := []struct {
		name   string
		json   string
		optOut bool
	}{
		{"absent", `{"command":"echo"}`, false},
		{"true", `{"command":"echo","sandbox":true}`, false},
		{"false", `{"command":"echo","sandbox":false}`, true},
		{"object", `{"command":"echo","sandbox":{"allowNetwork":true}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg MCPConfig
			if err := json.Unmarshal([]byte(tt.json), &cfg); err != nil {
				t.Fatalf("Unmarshal error: %v", err)
			}
			if got := cfg.SandboxOptOut(); got != tt.optOut {
				t.Fatalf("SandboxOptOut() = %v, want %v", got, tt.optOut)
			}
		})
	}
}

// mockSandbox implements sandbox.Sandbox for testing
type mockSandbox struct {
	called bool
	err    error
	file   *os.File
}

func (m *mockSandbox) Wrap(cmd *exec.Cmd) error {
	m.called = true
	if m.file != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, m.file)
	}
	return m.err
}

type explicitEnvCaptureSandbox struct {
	explicit map[string]string
}

func (s *explicitEnvCaptureSandbox) Wrap(*exec.Cmd) error {
	return fmt.Errorf("plain Wrap called instead of WrapWithEnv")
}

func (s *explicitEnvCaptureSandbox) WrapWithEnv(_ *exec.Cmd, explicit map[string]string) error {
	s.explicit = make(map[string]string, len(explicit))
	for key, value := range explicit {
		s.explicit[key] = value
	}
	return fmt.Errorf("stop after environment capture")
}

type mcpExtraFileSandbox struct {
	file *os.File
}

func (s *mcpExtraFileSandbox) Wrap(cmd *exec.Cmd) error {
	cmd.ExtraFiles = append(cmd.ExtraFiles, s.file)
	return nil
}

func (s *mcpExtraFileSandbox) WrapWithEnv(cmd *exec.Cmd, _ map[string]string) error {
	return s.Wrap(cmd)
}

func mockSandboxFactory(sb *mockSandbox) func(sandbox.Config) (sandbox.Sandbox, error) {
	return func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		return sb, nil
	}
}

func failingSandboxFactory() func(sandbox.Config) (sandbox.Sandbox, error) {
	return func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		return nil, fmt.Errorf("backend broken")
	}
}

func TestRegistryShellToolSchemaSandboxFailureFailsClosed(t *testing.T) {
	dir := t.TempDir()
	scriptPath := createTestScript(t, dir)

	registry := NewToolRegistry(nil, WithSandboxFactory(failingSandboxFactory(), sandbox.Config{}))

	if _, err := registry.LoadShellTool(scriptPath); err == nil {
		t.Fatal("expected load to fail when the schema sandbox can't be constructed")
	}
	if len(registry.All()) != 0 {
		t.Fatalf("registry should be empty after a failed load, has %d tools", len(registry.All()))
	}
}

func TestRegistryShellSchemaUsesStrictDiscoveryConfig(t *testing.T) {
	dir := t.TempDir()
	scriptPath := createTestScript(t, dir)
	base := sandbox.Config{
		AllowNetwork:   true,
		WritablePaths:  []string{dir},
		ReadPaths:      []string{"/read-exception"},
		DenyPaths:      []string{"/blocked-secret"},
		DenyWritePaths: []string{"/protected-write"},
		AllowEnv:       []string{"SECRET_TOKEN"},
	}
	var configs []sandbox.Config
	factory := func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		configs = append(configs, cfg)
		return &mockSandbox{}, nil
	}
	registry := NewToolRegistry(nil, WithSandboxFactory(factory, base))

	if _, err := registry.LoadShellTool(scriptPath); err != nil {
		t.Fatalf("LoadShellTool() error = %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("sandbox factory calls = %d, want schema and execution", len(configs))
	}
	discovery := configs[0]
	realTemp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realTemp = filepath.Clean(realTemp)
	if discovery.AllowNetwork {
		t.Fatal("schema discovery inherited network access")
	}
	if len(discovery.WritablePaths) != 1 || discovery.WritablePaths[0] != realTemp {
		t.Fatalf("schema discovery writable paths = %v, want only %q", discovery.WritablePaths, realTemp)
	}
	if len(discovery.ReadPaths) != 0 || len(discovery.AllowEnv) != 0 {
		t.Fatalf("schema discovery inherited allowances: read=%v env=%v", discovery.ReadPaths, discovery.AllowEnv)
	}
	if len(discovery.DenyPaths) != 1 || discovery.DenyPaths[0] != "/blocked-secret" {
		t.Fatalf("schema discovery deny paths = %v", discovery.DenyPaths)
	}
	if len(discovery.DenyWritePaths) != 1 || discovery.DenyWritePaths[0] != "/protected-write" {
		t.Fatalf("schema discovery deny-write paths = %v", discovery.DenyWritePaths)
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !configs[1].AllowNetwork || len(configs[1].WritablePaths) != 1 || configs[1].WritablePaths[0] != realDir {
		t.Fatalf("execution config = %+v, want base execution allowances", configs[1])
	}
}

func TestRegistryPreparedBaseSurvivesLaterSandboxConstruction(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "approved-child")
	external := filepath.Join(parent, "external")
	for _, path := range []string{child, external} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	realChild, err := filepath.EvalSymlinks(child)
	if err != nil {
		t.Fatal(err)
	}
	route := filepath.Join(parent, "approved-route")
	if err := os.Symlink(child, route); err != nil {
		t.Fatal(err)
	}

	var configs []sandbox.Config
	factory := func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		configs = append(configs, cfg)
		return &mockSandbox{}, nil
	}
	registry := NewToolRegistry(nil, WithSandboxFactory(factory, sandbox.Config{
		WritablePaths: []string{route},
	}))
	if _, err := registry.LoadToolAuto("bash"); err != nil {
		t.Fatal(err)
	}
	bash, ok := registry.Get("bash")
	if !ok {
		t.Fatal("bash not registered")
	}
	details := SandboxDetails(bash)
	if details.Config == nil || len(details.Config.WritablePaths) != 1 || details.Config.WritablePaths[0] != realChild {
		t.Fatalf("SandboxDetails config = %+v, want canonical writable path %q", details.Config, realChild)
	}

	// A later tool may legitimately add the parent as its own overlay. The
	// inherited child identity must survive minimization even though the parent
	// makes the child's public path redundant for that one effective config.
	if _, err := registry.NewSandbox(&sandbox.Config{WritablePaths: []string{parent}}); err != nil {
		t.Fatalf("construct parent-writable sandbox: %v", err)
	}
	if err := os.Rename(child, child+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, child); err != nil {
		t.Fatal(err)
	}

	callsBefore := len(configs)
	if _, err := registry.NewSandbox(nil); err == nil {
		t.Fatal("later NewSandbox accepted a replaced prepared base authority")
	}
	if len(configs) != callsBefore {
		t.Fatalf("sandbox factory called after prepared authority identity failure: %d -> %d", callsBefore, len(configs))
	}
}

func TestRegistryPreparedBaseDoesNotActivateMissingGrantLater(t *testing.T) {
	parent := t.TempDir()
	missing := filepath.Join(parent, "missing-at-approval")
	var configs []sandbox.Config
	factory := func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		configs = append(configs, cfg)
		return &mockSandbox{}, nil
	}
	registry := NewToolRegistry(nil, WithSandboxFactory(factory, sandbox.Config{
		WritablePaths: []string{missing},
	}))
	if _, err := registry.NewSandbox(&sandbox.Config{WritablePaths: []string{parent}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(missing, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.NewSandbox(nil); err != nil {
		t.Fatal(err)
	}
	last := configs[len(configs)-1]
	if len(last.WritablePaths) != 0 {
		t.Fatalf("missing base grant activated after later creation: %v", last.WritablePaths)
	}
}

func TestWithSandboxFactorySnapshotsBaseConfig(t *testing.T) {
	approved := t.TempDir()
	mutated := t.TempDir()
	base := sandbox.Config{WritablePaths: []string{approved}}
	var captured sandbox.Config
	option := WithSandboxFactory(func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		captured = cfg
		return &mockSandbox{}, nil
	}, base)
	base.WritablePaths[0] = mutated

	registry := NewToolRegistry(nil, option)
	if _, err := registry.NewSandbox(nil); err != nil {
		t.Fatal(err)
	}
	realApproved, err := filepath.EvalSymlinks(approved)
	if err != nil {
		t.Fatal(err)
	}
	if len(captured.WritablePaths) != 1 || captured.WritablePaths[0] != realApproved {
		t.Fatalf("factory received caller-mutated base config: %v, want %q", captured.WritablePaths, realApproved)
	}
}

func TestWithSandboxFactorySnapshotsBaseIdentityAtOptionCreation(t *testing.T) {
	parent := t.TempDir()
	approved := filepath.Join(parent, "approved")
	external := filepath.Join(parent, "external")
	for _, path := range []string{approved, external} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	called := false
	option := WithSandboxFactory(func(sandbox.Config) (sandbox.Sandbox, error) {
		called = true
		return &mockSandbox{}, nil
	}, sandbox.Config{WritablePaths: []string{approved}})
	if err := os.Rename(approved, approved+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, approved); err != nil {
		t.Fatal(err)
	}

	registry := NewToolRegistry(nil, option)
	if _, err := registry.NewSandbox(nil); err == nil {
		t.Fatal("NewSandbox accepted base authority replaced after option creation")
	}
	if called {
		t.Fatal("sandbox factory called after prepared base identity failure")
	}
}

func TestWithSandboxFactoryDefersBasePreparationError(t *testing.T) {
	called := false
	option := WithSandboxFactory(func(sandbox.Config) (sandbox.Sandbox, error) {
		called = true
		return &mockSandbox{}, nil
	}, sandbox.Config{WritablePaths: []string{""}})
	registry := NewToolRegistry(nil, option)
	if _, err := registry.NewSandbox(nil); err == nil || !strings.Contains(err.Error(), "empty path") {
		t.Fatalf("NewSandbox() error = %v, want deferred preparation failure", err)
	}
	if called {
		t.Fatal("sandbox factory called after base preparation failure")
	}
}

func TestRegistryRejectsNilSuccessfulSandbox(t *testing.T) {
	factory := func(sandbox.Config) (sandbox.Sandbox, error) { return nil, nil }
	registry := NewToolRegistry(nil, WithSandboxFactory(factory, sandbox.Config{}))

	if _, err := registry.NewSandbox(nil); err == nil || !strings.Contains(err.Error(), "returned no sandbox") {
		t.Fatalf("NewSandbox() error = %v, want nil-result rejection", err)
	}
	if _, err := registry.NewSandboxDirect(sandbox.Config{}); err == nil || !strings.Contains(err.Error(), "returned no sandbox") {
		t.Fatalf("NewSandboxDirect() error = %v, want nil-result rejection", err)
	}
	if _, err := registry.LoadToolAuto("bash"); err == nil || !strings.Contains(err.Error(), "returned no sandbox") {
		t.Fatalf("LoadToolAuto(bash) error = %v, want nil-result rejection", err)
	}
	if _, ok := registry.Get("bash"); ok {
		t.Fatal("bash registered after its sandbox factory returned nil")
	}
	if _, err := registry.LoadShellTool(createTestScript(t, t.TempDir())); err == nil || !strings.Contains(err.Error(), "schema sandbox") {
		t.Fatalf("LoadShellTool() error = %v, want schema sandbox rejection", err)
	}
	mcpConfig := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(mcpConfig, []byte(`{"mcpServers":{"local":{"command":"true"}}}`), 0600); err != nil {
		t.Fatalf("write MCP config: %v", err)
	}
	if _, err := registry.LoadMCPServer(mcpConfig); err == nil || !strings.Contains(err.Error(), "returned no sandbox") {
		t.Fatalf("LoadMCPServer() error = %v, want nil-result rejection", err)
	}
	if len(registry.All()) != 0 {
		t.Fatalf("registry should stay empty, has %d tools", len(registry.All()))
	}
}

func TestMCPConfiguredEnvUsesExplicitTargetChannel(t *testing.T) {
	sb := &explicitEnvCaptureSandbox{}
	config := &MCPConfig{
		Command: "/bin/true",
		Env: map[string]string{
			"GITHUB_TOKEN":   "configured-secret",
			"ORDINARY_VALUE": "configured-value",
		},
	}

	if _, err := NewMCPClientFromConfig(config, sb); err == nil || !strings.Contains(err.Error(), "environment capture") {
		t.Fatalf("NewMCPClientFromConfig() error = %v, want capture sentinel", err)
	}
	if len(sb.explicit) != len(config.Env) {
		t.Fatalf("explicit env = %v, want %v", sb.explicit, config.Env)
	}
	for key, value := range config.Env {
		if sb.explicit[key] != value {
			t.Fatalf("explicit env[%q] = %q, want %q", key, sb.explicit[key], value)
		}
	}
}

func TestMCPConnectFailureClosesSandboxFiles(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "mcp-sandbox-extra-*")
	if err != nil {
		t.Fatal(err)
	}
	config := &MCPConfig{
		Command: "/bin/true",
		Env:     map[string]string{"TARGET_ONLY": "value"},
	}
	if _, err := NewMCPClientFromConfig(config, &mcpExtraFileSandbox{file: file}); err == nil {
		t.Fatal("MCP connection unexpectedly succeeded")
	}
	if _, err := file.Stat(); err == nil {
		t.Fatal("sandbox-added descriptor remains open after MCP Connect failure")
	}
}

func TestRegistryRefusesShellWithoutSandboxBeforeSchemaExecution(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "schema-ran")
	scriptPath := filepath.Join(dir, "tool.sh")
	script := fmt.Sprintf(`#!/bin/bash
if [ "$1" = "--schema" ]; then
  touch %q
  echo '{"title":"unsafe","type":"object"}'
fi
`, marker)
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	registry := NewToolRegistry(nil)
	if _, err := registry.LoadShellTool(scriptPath); err == nil {
		t.Fatal("expected a registry without sandbox policy to reject shell loading")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("schema command ran before sandbox policy rejection: %v", err)
	}
}

func TestRegistryRefusesStdioMCPWithoutSandbox(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "mcp.json")
	config := `{"mcpServers":{"unsafe":{"command":"definitely-not-started"}}}`
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}

	registry := NewToolRegistry(nil)
	if _, err := registry.LoadMCPServer(configPath); err == nil || !strings.Contains(err.Error(), "requires sandboxing") {
		t.Fatalf("LoadMCPServer() error = %v, want secure-default refusal", err)
	}
}

func TestRegistryShellToolSandboxFailureFailsClosed(t *testing.T) {
	dir := t.TempDir()
	scriptPath := createTestScript(t, dir)

	// First construction (schema-load sandbox) succeeds, second (execution
	// sandbox) fails — the tool must not load and run unsandboxed.
	calls := 0
	factory := func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		calls++
		if calls > 1 {
			return nil, fmt.Errorf("backend broken")
		}
		return &mockSandbox{}, nil
	}
	registry := NewToolRegistry(nil, WithSandboxFactory(factory, sandbox.Config{}))

	_, err := registry.LoadShellTool(scriptPath)
	if err == nil {
		t.Fatal("expected load to fail when the execution sandbox can't be constructed")
	}
	if !strings.Contains(err.Error(), "sandbox for shell tool") {
		t.Fatalf("error = %q, want it to name the sandbox failure", err)
	}
	if len(registry.All()) != 0 {
		t.Fatalf("registry should be empty after a failed load, has %d tools", len(registry.All()))
	}
}

func TestLoadToolAutoBashSandboxFailureFailsClosed(t *testing.T) {
	registry := NewToolRegistry(nil, WithSandboxFactory(failingSandboxFactory(), sandbox.Config{}))

	_, err := registry.LoadToolAuto("bash")
	if err == nil {
		t.Fatal("expected bash load to fail when its sandbox can't be constructed")
	}
	if !strings.Contains(err.Error(), "sandbox for bash") {
		t.Fatalf("error = %q, want it to name the sandbox failure", err)
	}
	if _, ok := registry.Get("bash"); ok {
		t.Fatal("bash should not be registered after a failed load")
	}
}

func TestSandboxState(t *testing.T) {
	dir := t.TempDir()
	shell, err := newShellTool(createTestScript(t, dir))
	if err != nil {
		t.Fatalf("NewShellTool error = %v", err)
	}
	sandboxedBash := newBashTool("").WithSandbox(&mockSandbox{})

	tests := []struct {
		name    string
		tool    Tool
		capable bool
		active  bool
	}{
		{"bash plain", newBashTool(""), true, false},
		{"bash sandboxed", sandboxedBash, true, true},
		{"shell plain", shell, true, false},
		{"shell sandboxed", shell.WithSandbox(&mockSandbox{}), true, true},
		{"mcp sandboxed", &MCPTool{client: &MCPClient{sandboxCapable: true, sandboxed: true}}, true, true},
		{"mcp remote", &MCPTool{client: &MCPClient{sandboxCapable: false, sandboxed: true}}, false, false},
		{"func not capable", &Func{Name: "f"}, false, false},
		{"namespaced unwraps", &NamespacedTool{Tool: sandboxedBash, namespacedName: "ns__bash"}, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capable, active := SandboxState(tt.tool)
			if capable != tt.capable || active != tt.active {
				t.Fatalf("SandboxState() = (%v, %v), want (%v, %v)", capable, active, tt.capable, tt.active)
			}
		})
	}
}

func TestSandboxDetailsIncludesEffectiveConfigAndOptOut(t *testing.T) {
	cfg := sandbox.Config{
		AllowNetwork:   true,
		WritablePaths:  []string{"/tmp/work"},
		DenyWritePaths: []string{"/tmp/work/.git"},
		AllowEnv:       []string{"PATH"},
	}
	bash := newBashTool("").WithSandbox(&mockSandbox{}, cfg)
	info := SandboxDetails(bash)
	if !info.Capable || !info.Active {
		t.Fatalf("SandboxDetails(bash) = %+v, want capable active", info)
	}
	if info.Config == nil || !info.Config.AllowNetwork || len(info.Config.WritablePaths) != 1 || info.Config.WritablePaths[0] != "/tmp/work" {
		t.Fatalf("SandboxDetails(bash).Config = %+v, want copied effective config", info.Config)
	}
	cfg.WritablePaths[0] = "/mutated"
	cfg.DenyWritePaths[0] = "/mutated-deny"
	if info.Config.WritablePaths[0] != "/tmp/work" {
		t.Fatalf("SandboxDetails exposed mutable config: %+v", info.Config.WritablePaths)
	}
	if info.Config.DenyWritePaths[0] != "/tmp/work/.git" {
		t.Fatalf("SandboxDetails exposed mutable deny-write config: %+v", info.Config.DenyWritePaths)
	}
	info.Config.DenyWritePaths[0] = "/mutated-return"
	if got := SandboxDetails(bash).Config.DenyWritePaths[0]; got != "/tmp/work/.git" {
		t.Fatalf("SandboxDetails retained caller mutation: %q", got)
	}

	dir := t.TempDir()
	script := `#!/bin/bash
if [ "$1" = "--schema" ]; then
	echo '{"title":"unsandboxed-tool","description":"Opts out","type":"object","sandbox":false,"properties":{}}'
elif [ "$1" = "--execute" ]; then
	echo "ok"
fi
`
	scriptPath := filepath.Join(dir, "unsandboxed.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	shell, err := newShellTool(scriptPath)
	if err != nil {
		t.Fatalf("newShellTool() error = %v", err)
	}
	info = SandboxDetails(shell)
	if !info.Capable || info.Active || !info.OptedOut {
		t.Fatalf("SandboxDetails(opt-out shell) = %+v, want capable inactive opted out", info)
	}
}

func TestRegisterNativeFactoryNilTool(t *testing.T) {
	registry := NewToolRegistry(nil)
	registry.RegisterNative("broken", func() Tool { return nil })

	_, err := registry.LoadToolAuto("broken")
	if err == nil {
		t.Fatal("expected an error, not a panic, for a native factory returning nil")
	}
	if _, ok := registry.Get("broken"); ok {
		t.Fatal("nil tool should not be registered")
	}
}

func TestShellToolNonExecutable(t *testing.T) {
	// Test with a non-executable file
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "not-executable.txt")
	err := os.WriteFile(scriptPath, []byte("not a script"), 0644) // Note: not executable
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	_, err = newShellTool(scriptPath)
	if err == nil {
		t.Error("Expected error for non-executable file")
	}
}

func TestLoadShellToolsFailsClosedOnError(t *testing.T) {
	dir := t.TempDir()

	// Create one valid tool
	validScript := `#!/bin/bash
if [ "$1" = "--schema" ]; then
	echo '{"title": "valid-tool", "type": "object"}'
fi
`
	validPath := filepath.Join(dir, "valid.sh")
	err := os.WriteFile(validPath, []byte(validScript), 0755)
	if err != nil {
		t.Fatalf("Failed to create valid script: %v", err)
	}

	// Create one invalid tool (non-executable)
	invalidPath := filepath.Join(dir, "invalid.txt")
	err = os.WriteFile(invalidPath, []byte("not executable"), 0644)
	if err != nil {
		t.Fatalf("Failed to create invalid file: %v", err)
	}

	registry := NewToolRegistry(nil, WithSandboxFactory(mockSandboxFactory(&mockSandbox{}), sandbox.Config{}))
	tools, err := LoadShellToolsWithRegistry(registry, []string{validPath, invalidPath})
	if err == nil {
		t.Fatal("expected invalid tool to abort secure loading")
	}
	if len(tools) != 0 {
		t.Fatalf("LoadShellToolsWithRegistry() returned partial tools after failure: %d", len(tools))
	}
	if len(registry.All()) != 0 {
		t.Fatalf("registry mutated after batch failure: %d tools", len(registry.All()))
	}
}

func TestLoadShellToolsLegacyBestEffortCompatibility(t *testing.T) {
	dir := t.TempDir()
	validPath := createTestScript(t, dir)
	invalidPath := filepath.Join(dir, "not-executable")
	if err := os.WriteFile(invalidPath, []byte("not executable"), 0644); err != nil {
		t.Fatalf("write invalid tool: %v", err)
	}

	loaded, err := LoadShellTools([]string{invalidPath, validPath})
	if err != nil {
		t.Fatalf("legacy LoadShellTools() error = %v, want nil", err)
	}
	if len(loaded) != 1 || loaded[0].GetName() != "test-tool" {
		t.Fatalf("legacy LoadShellTools() = %v, want the one valid tool", loaded)
	}
}
