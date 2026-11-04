package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAgentSandboxNativePolicy(t *testing.T) {
	skipUnlessSandboxTests(t)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	for _, tc := range []struct {
		name                             string
		specs                            []string
		rootWrite, scratchWrite, network bool
	}{
		{name: "workspace", specs: []string{"workspace"}, rootWrite: true, scratchWrite: true},
		{name: "workspace net", specs: []string{"workspace+net"}, rootWrite: true, scratchWrite: true, network: true},
		{name: "base", specs: []string{"base"}, scratchWrite: true},
		{name: "readonly", specs: []string{"readonly"}},
		{name: "floor", specs: []string{"base", "workspace+net+sshkeys"}, scratchWrite: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, scratch := realTempDir(t), realTempDir(t)
			secret, protected := filepath.Join(root, "secret"), filepath.Join(root, "protected")
			for _, path := range []string{secret, protected} {
				if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			parent := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
			defer parent.Close()
			for _, name := range []string{"bash", "read_file", "write_file"} {
				if _, err := parent.LoadToolAuto(name); err != nil {
					t.Fatal(err)
				}
			}
			ec, err := parent.ExecutionPolicy(root, ExecutionGrant{Scratch: scratch, SandboxPresets: tc.specs, DeniedReads: []string{secret}, DeniedWrites: []string{protected}})
			if err != nil {
				t.Fatal(err)
			}
			bound, _, err := parent.BindExecutionContext(ec, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer bound.Close()
			if !bound.HasSandbox() || bound.UnsafeNoSandbox() {
				t.Fatal("child was not sandboxed")
			}
			bash, _ := bound.Get("bash")
			run := func(command string, want bool) {
				t.Helper()
				out, err := bash.Execute(context.Background(), map[string]any{"command": command})
				if (err == nil) != want {
					t.Fatalf("%s: %q %v; want success %v", command, out, err, want)
				}
			}
			run("printf probe > child-file", tc.rootWrite)
			run(`printf probe > "$TMPDIR/scratch-file"`, tc.scratchWrite)
			run("cat secret", false)
			run("printf probe > protected", false)
			run(fmt.Sprintf("printf probe > /dev/tcp/127.0.0.1/%d", port), tc.network)
			writer, _ := bound.Get("write_file")
			for path, want := range map[string]bool{"native-file": tc.rootWrite, filepath.Join(scratch, "native-file"): tc.scratchWrite, protected: false} {
				_, err := writer.Execute(context.Background(), map[string]any{"path": path, "content": "probe"})
				if (err == nil) != want {
					t.Fatalf("native write %s: %v; want success %v", path, err, want)
				}
			}
			reader, _ := bound.Get("read_file")
			if _, err := reader.Execute(context.Background(), map[string]any{"path": secret}); err == nil {
				t.Fatal("native secret read allowed")
			}
			// Parent process posture is unchanged by the child's explicit policy.
			original, _ := parent.Get("bash")
			if SandboxDetails(original).Active || parent.HasSandbox() {
				t.Fatal("child sandbox changed the parent")
			}
		})
	}
}

func TestAgentSandboxNativeInheritedDenials(t *testing.T) {
	skipUnlessSandboxTests(t)
	root, scratch := realTempDir(t), realTempDir(t)
	secret := filepath.Join(root, "secret")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	parent := NewToolRegistry(nil, WithNativeTools(), WithSandboxFactory(sandbox.New, sandbox.Config{WritablePaths: []string{root}, DenyPaths: []string{secret}, DenyWrite: true}))
	defer parent.Close()
	if _, err := parent.LoadToolAuto("bash"); err != nil {
		t.Fatal(err)
	}
	ec, err := parent.ExecutionPolicy(root, ExecutionGrant{SourceRoot: root, Scratch: scratch, SandboxPresets: []string{"workspace+net+sshkeys"}})
	if err != nil {
		t.Fatal(err)
	}
	bound, _, err := parent.BindExecutionContext(ec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	bash, _ := bound.Get("bash")
	for _, command := range []string{"cat secret", "printf bad > root-file", "printf bad > '" + scratch + "/scratch-file'"} {
		if out, err := bash.Execute(context.Background(), map[string]any{"command": command}); err == nil {
			t.Fatalf("inherited deny lost: %s: %s", command, out)
		}
	}
	if ec.Sandbox.AllowNetwork {
		t.Fatal("inherited network denial lost")
	}
}

func TestAgentSandboxNativeShellOptOut(t *testing.T) {
	skipUnlessSandboxTests(t)
	root := realTempDir(t)
	script := filepath.Join(root, "opt-out.sh")
	body := "#!/bin/sh\nif [ \"$1\" = --schema ]; then\nprintf '%s\\n' '{\"title\":\"opt-out\",\"description\":\"fixture\",\"type\":\"object\",\"properties\":{},\"sandbox\":false}'\nelse\nprintf escaped > escaped\nfi\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	parent := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
	defer parent.Close()
	if _, err := parent.LoadShellToolWithNamespace(script, "fixture"); err != nil {
		t.Fatal(err)
	}
	ec, err := parent.ExecutionPolicy(root, ExecutionGrant{SandboxPresets: []string{"readonly"}})
	if err != nil {
		t.Fatal(err)
	}
	bound, omitted, err := parent.BindExecutionContext(ec, []string{"fixture__opt-out"})
	if err != nil {
		t.Fatalf("bind: %v, omitted %v", err, omitted)
	}
	defer bound.Close()
	tool, ok := bound.Get("fixture__opt-out")
	if !ok {
		t.Fatal("rebound shell missing")
	}
	if out, err := tool.Execute(context.Background(), map[string]any{}); err == nil {
		t.Fatalf("shell opt-out escaped readonly: %s", out)
	}
	if _, err := os.Stat(filepath.Join(root, "escaped")); !os.IsNotExist(err) {
		t.Fatalf("shell wrote through sandbox: %v", err)
	}
}

func TestAgentSandboxNativeMCPOptOut(t *testing.T) {
	skipUnlessSandboxTests(t)
	root := realTempDir(t)
	command, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config := MCPServersConfig{MCPServers: map[string]MCPConfig{"probe": {Command: command, Args: []string{"-test.run=^TestAgentSandboxMCPFixture$", "--", "agent-sandbox-mcp"}, Sandbox: sandboxDeclaration(t, "false")}}}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "mcp.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	parent := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
	defer parent.Close()
	if _, err := parent.LoadMCPServer(path); err != nil {
		t.Fatal(err)
	}
	ec, err := parent.ExecutionPolicy(root, ExecutionGrant{SandboxPresets: []string{"readonly"}})
	if err != nil {
		t.Fatal(err)
	}
	bound, omitted, err := parent.BindExecutionContext(ec, []string{"probe__write_probe"})
	if err != nil {
		t.Fatalf("bind: %v; omissions %v", err, omitted)
	}
	defer bound.Close()
	tool, ok := bound.Get("probe__write_probe")
	if !ok {
		t.Fatal("rebound MCP missing")
	}
	out, err := tool.Execute(context.Background(), map[string]any{"path": filepath.Join(root, "escaped")})
	if err == nil && !strings.Contains(out, "denied") {
		t.Fatalf("MCP opt-out escaped readonly: %s %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(root, "escaped")); !os.IsNotExist(err) {
		t.Fatalf("MCP wrote through sandbox: %v", err)
	}
}

func TestAgentSandboxMCPFixture(t *testing.T) {
	if os.Args[len(os.Args)-1] != "agent-sandbox-mcp" {
		return
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "agent-sandbox", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "write_probe", Description: "Write a sandbox fixture", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"}}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		if err := os.WriteFile(args.Path, []byte("escaped"), 0o600); err != nil {
			return nil, fmt.Errorf("write denied: %w", err)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "written"}}}, nil
	})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
