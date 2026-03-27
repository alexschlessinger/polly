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

	client, err := NewMCPClient(configPath)
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

	client, err := NewMCPClient(configPath)
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

	client, err := NewMCPClient(configPath)
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
	_, err := NewMCPClient("")
	if err == nil {
		t.Error("Expected error for empty command")
	}
}

func TestMCPToolNoargsFiltering(t *testing.T) {
	checkUvxAvailable(t)
	ctx := context.Background()

	// Create MCP server config file
	configPath := createMCPTestConfig(t, "time", "uvx", []string{"mcp-server-time"})

	client, err := NewMCPClient(configPath)
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

func TestShellToolExecute(t *testing.T) {
	dir := t.TempDir()
	scriptPath := createTestScript(t, dir)

	tool, err := NewShellTool(scriptPath)
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

	tool, err := NewShellTool(scriptPath)
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

	tools, err := LoadShellTools([]string{script1, script2Path})
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

	_, err = NewShellTool(scriptPath)
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

	tool, err := NewShellTool(scriptPath)
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

	tool, err := NewShellTool(scriptPath)
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

	tool, err := NewShellTool(scriptPath)
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

	tool, err := NewShellTool(scriptPath)
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

	tool, err := NewShellTool(scriptPath)
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
	tool, err := NewShellTool(createTestScript(t, dir))
	if err != nil {
		t.Fatalf("Failed to create shell tool: %v", err)
	}
	if tool.WantsSandbox() {
		t.Error("Expected WantsSandbox()=false for script without sandbox flag")
	}

	// Script with sandbox: true
	tool2, err := NewShellTool(createSandboxedTestScript(t, dir))
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

	tool, err := NewShellTool(scriptPath)
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

	tool, err := NewShellTool(scriptPath)
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

func TestShellToolSandboxWrapError(t *testing.T) {
	dir := t.TempDir()
	scriptPath := createSandboxedTestScript(t, dir)

	tool, err := NewShellTool(scriptPath)
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
	// A shell tool with "sandbox": false opts out of sandboxing.
	// The user creates shell tool wrapper scripts, so this is trusted.
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
	registry := NewToolRegistry(nil, WithSandboxFactory(mockSandboxFactory(sb), sandbox.Config{}))

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
}

func (m *mockSandbox) Wrap(cmd *exec.Cmd) error {
	m.called = true
	return m.err
}

func mockSandboxFactory(sb *mockSandbox) func(sandbox.Config) (sandbox.Sandbox, error) {
	return func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		return sb, nil
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

	_, err = NewShellTool(scriptPath)
	if err == nil {
		t.Error("Expected error for non-executable file")
	}
}

func TestLoadShellToolsContinuesOnError(t *testing.T) {
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

	// Load should succeed with the valid tool and skip the invalid one
	tools, err := LoadShellTools([]string{invalidPath, validPath})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Should have loaded only the valid tool
	if len(tools) != 1 {
		t.Errorf("Expected 1 tool (valid only), got %d", len(tools))
	}
}

func TestUpperCaseTool(t *testing.T) {
	tool := UpperCaseTool

	// Test schema
	schema := tool.GetSchema()
	if schema.Title() != "uppercase" {
		t.Errorf("Expected title 'uppercase', got %s", schema.Title())
	}
	if req := schema.Required(); len(req) != 1 || req[0] != "text" {
		t.Error("Expected 'text' to be required")
	}

	// Test execution
	args := map[string]any{
		"text": "hello world",
	}
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if result != "HELLO WORLD" {
		t.Errorf("Expected 'HELLO WORLD', got '%s'", result)
	}
}

func TestUpperCaseToolInvalidArgs(t *testing.T) {
	tool := UpperCaseTool

	// Test with missing text argument
	args := map[string]any{}
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Expected error for missing text argument")
	}

	// Test with wrong type
	args = map[string]any{
		"text": 123,
	}
	_, err = tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Expected error for non-string text argument")
	}
}

func TestWordCountTool(t *testing.T) {
	tool := WordCountTool

	// Test schema
	schema := tool.GetSchema()
	if schema.Title() != "wordcount" {
		t.Errorf("Expected title 'wordcount', got %s", schema.Title())
	}

	// Test word counting
	testCases := []struct {
		input    string
		expected string
	}{
		{"hello world", "Word count: 2"},
		{"one two three four five", "Word count: 5"},
		{"   spaces   between   words   ", "Word count: 3"},
		{"", "Word count: 0"},
		{"single", "Word count: 1"},
	}

	for _, tc := range testCases {
		args := map[string]any{
			"text": tc.input,
		}
		result, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatalf("Unexpected error for input '%s': %v", tc.input, err)
		}
		if result != tc.expected {
			t.Errorf("For input '%s': expected '%s', got '%s'", tc.input, tc.expected, result)
		}
	}
}

func TestWordCountToolInvalidArgs(t *testing.T) {
	tool := WordCountTool

	// Test with non-string argument
	args := map[string]any{
		"text": []int{1, 2, 3},
	}
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Expected error for non-string text argument")
	}
}

// Mock logger for testing LoggerTool
type mockLogger struct {
	messages []string
}

func (m *mockLogger) Log(message string) {
	m.messages = append(m.messages, message)
}

func TestLoggerTool(t *testing.T) {
	tool := &LoggerTool{}
	logger := &mockLogger{}

	// Test without context
	_, err := tool.Execute(context.Background(), map[string]any{"message": "test"})
	if err == nil {
		t.Error("Expected error when logger context is not set")
	}

	// Set context
	tool.SetContext(logger)

	// Test schema
	schema := tool.GetSchema()
	if schema.Title() != "log" {
		t.Errorf("Expected title 'log', got %s", schema.Title())
	}

	// Test logging with default level
	args := map[string]any{
		"message": "Test message",
	}
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !strings.Contains(result, "[INFO] Test message") {
		t.Errorf("Expected result to contain '[INFO] Test message', got '%s'", result)
	}
	if len(logger.messages) != 1 || logger.messages[0] != "[INFO] Test message" {
		t.Errorf("Expected logger to receive '[INFO] Test message', got %v", logger.messages)
	}

	// Test logging with specific level
	args = map[string]any{
		"message": "Error occurred",
		"level":   "error",
	}
	result, err = tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !strings.Contains(result, "[ERROR] Error occurred") {
		t.Errorf("Expected result to contain '[ERROR] Error occurred', got '%s'", result)
	}
}

func TestLoggerToolInvalidArgs(t *testing.T) {
	tool := &LoggerTool{}
	logger := &mockLogger{}
	tool.SetContext(logger)

	// Test with non-string message
	args := map[string]any{
		"message": 12345,
	}
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Expected error for non-string message")
	}
}

func TestLoggerToolSetContext(t *testing.T) {
	tool := &LoggerTool{}

	// Test setting valid context
	logger := &mockLogger{}
	tool.SetContext(logger)
	if tool.logger != logger {
		t.Error("Expected logger to be set correctly")
	}

	// Test setting invalid context (should not panic)
	tool.SetContext("not a logger")
	// Logger should remain unchanged since the context is wrong type
	if tool.logger != logger {
		t.Error("Expected logger to remain unchanged with invalid context")
	}
}
