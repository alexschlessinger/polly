package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func agentToolPolicyMCPConfig(t *testing.T, root, command string, local sandbox.Config) string {
	t.Helper()
	config := MCPServersConfig{MCPServers: map[string]MCPConfig{"probe": {
		Command: command, Args: []string{"-test.run=^TestAgentToolPolicyMCPFixture$", "--", "agent-tool-policy-mcp"},
		Env: map[string]string{"MCP_EXPLICIT": "configured"}, Sandbox: sandbox.DeclareConfig(local),
	}}}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return agentToolPolicyFixture(t, root, "mcp.json", string(data))
}

func TestAgentToolNativeHiddenExecutables(t *testing.T) {
	skipUnlessSandboxTests(t)
	home, root := realTempDir(t), realTempDir(t)
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "hidden")
	secret := agentToolPolicyFixture(t, dir, "secret", "private-sibling")
	script := agentToolPolicyFixture(t, dir, "probe.sh", "#!/bin/sh\nif [ \"$1\" = --schema ]; then\nprintf '%s\\n' '{\"title\":\"probe\",\"type\":\"object\",\"properties\":{}}'\nelse\n/bin/cat '"+secret+"'\nfi\n")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	server := agentToolPolicyFixture(t, dir, "server.sh", "#!/bin/sh\nexec '"+binary+"' \"$@\"\n")
	config := agentToolPolicyMCPConfig(t, root, server, sandbox.Config{})
	parent := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
	defer parent.Close()
	if _, err := parent.LoadShellToolWithNamespace(script, "shell"); err != nil {
		t.Fatal(err)
	}
	if _, err := parent.LoadMCPServer(config); err != nil {
		t.Fatal(err)
	}
	ec, err := parent.ExecutionPolicy(root, ExecutionGrant{SandboxPresets: []string{"workspace+private-home"}})
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.ReadAllowed(ec.Sandbox, secret) == nil {
		t.Fatal("fixture sibling was not private")
	}
	bound, omitted, err := parent.BindExecutionContext(ec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	for _, name := range []string{"shell__probe", "probe__inspect"} {
		if !slices.Contains(omitted, name) {
			t.Fatalf("hidden tool %s not omitted: %v", name, omitted)
		}
		if err := bound.ValidateToolSelection([]string{name}, nil); err == nil {
			t.Fatalf("required hidden tool %s accepted", name)
		}
	}
	for _, r := range []*ToolRegistry{bound, bound.Derive()} {
		defer r.Close()
		if _, err := r.LoadShellTool(script); err == nil || !strings.Contains(err.Error(), "agent sandbox policy") {
			t.Fatalf("hidden schema executable admitted: %v", err)
		}
		if _, err := r.LoadMCPServer(config); err == nil || !strings.Contains(err.Error(), "agent sandbox policy") {
			t.Fatalf("hidden MCP executable admitted: %v", err)
		}
	}
}

func TestAgentToolNativeFileOnlyExecutable(t *testing.T) {
	skipUnlessSandboxTests(t)
	home, root := realTempDir(t), realTempDir(t)
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "hidden")
	secret := agentToolPolicyFixture(t, dir, "secret", "private-sibling")
	script := agentToolPolicyFixture(t, dir, "probe.sh", "#!/bin/sh\nif [ \"$1\" = --schema ]; then\nprintf '%s\\n' '{\"title\":\"probe\",\"type\":\"object\",\"properties\":{}}'\nelse\n/bin/cat '"+secret+"'\nfi\n")
	// Model a resolved child policy that authorizes just the executable. The
	// registry's convenience exposure must not turn it into a directory grant.
	r := NewToolRegistry(nil, WithSandboxFactory(sandbox.New, sandbox.Config{PrivateHome: true, ReadPaths: []string{script}, WritablePaths: []string{root}}))
	defer r.Close()
	r.executionSandboxPresets = []string{"workspace+private-home"}
	for _, registry := range []*ToolRegistry{r, r.Derive()} {
		defer registry.Close()
		if _, err := registry.LoadShellToolWithNamespace(script, "shell"); err != nil {
			t.Fatal(err)
		}
		tool, ok := registry.Get("shell__probe")
		if !ok {
			t.Fatal("authorized executable missing")
		}
		if sandbox.ReadAllowed(*SandboxDetails(tool).Config, secret) == nil {
			t.Fatal("file grant exposed sibling")
		}
		if out, err := tool.Execute(context.Background(), nil); err == nil || strings.Contains(out, "private-sibling") {
			t.Fatalf("native sibling read: %q, %v", out, err)
		}
	}
}

func TestAgentToolNativeEnvironmentCeiling(t *testing.T) {
	skipUnlessSandboxTests(t)
	t.Setenv("REVIEW_INTERNAL_CONTEXT", "ambient-context")
	t.Setenv("REVIEW_API_TOKEN", "ambient-secret")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, strictParent := range []bool{false, true} {
		t.Run(fmt.Sprintf("strict-parent=%v", strictParent), func(t *testing.T) {
			root := realTempDir(t)
			base := sandbox.Config{WritablePaths: []string{root}, Env: map[string]string{"POLICY_ONLY": "configured"}}
			local := sandbox.Config{AllowEnv: []string{"PATH"}, Env: map[string]string{"TOOL_ADDED": "bad"}}
			if strictParent {
				base.AllowEnv = []string{"PATH"}
				local.AllowEnv = []string{"PATH", "REVIEW_INTERNAL_CONTEXT", "REVIEW_API_TOKEN"}
			}
			meta, err := json.Marshal(map[string]any{"title": "probe", "type": "object", "properties": map[string]any{}, "sandbox": local})
			if err != nil {
				t.Fatal(err)
			}
			script := agentToolPolicyFixture(t, root, "probe.sh", "#!/bin/sh\nif [ \"$1\" = --schema ]; then\nprintf '%s\\n' '"+string(meta)+"'\nelse\nprintf '%s|%s|%s|%s' \"${REVIEW_INTERNAL_CONTEXT-unset}\" \"${REVIEW_API_TOKEN-unset}\" \"${POLICY_ONLY-unset}\" \"${TOOL_ADDED-unset}\"\nfi\n")
			config := agentToolPolicyMCPConfig(t, root, binary, local)
			parent := NewToolRegistry(nil, WithNativeTools(), WithSandboxFactory(sandbox.New, base))
			defer parent.Close()
			if _, err := parent.LoadShellToolWithNamespace(script, "shell"); err != nil {
				t.Fatal(err)
			}
			if _, err := parent.LoadMCPServer(config); err != nil {
				t.Fatal(err)
			}
			ec, err := parent.ExecutionPolicy(root, ExecutionGrant{SourceRoot: root, SandboxPresets: []string{"workspace"}})
			if err != nil {
				t.Fatal(err)
			}
			bound, omitted, err := parent.BindExecutionContext(ec, nil)
			if err != nil || len(omitted) != 0 {
				t.Fatalf("bind: %v, omissions %v", err, omitted)
			}
			defer bound.Close()
			for _, mode := range []string{"inherited", "late", "derived"} {
				t.Run(mode, func(t *testing.T) {
					r := bound
					if mode == "derived" {
						r = bound.Derive()
						defer r.Close()
					}
					if mode != "inherited" {
						if _, err := r.LoadShellToolWithNamespace(script, "shell"); err != nil {
							t.Fatal(err)
						}
						if _, err := r.LoadMCPServer(config); err != nil {
							t.Fatal(err)
						}
					}
					for _, name := range []string{"shell__probe", "probe__inspect"} {
						tool, ok := r.Get(name)
						if !ok {
							t.Fatalf("missing %s", name)
						}
						out, err := tool.Execute(context.Background(), map[string]any{})
						if err != nil {
							t.Fatal(err)
						}
						if name == "shell__probe" {
							if out != "unset|unset|configured|unset" {
								t.Fatalf("shell environment: %q", out)
							}
						} else if strings.Contains(out, "ambient-") || !strings.Contains(out, "unset|unset|configured|unset|configured") {
							t.Fatalf("MCP environment: %q", out)
						}
					}
				})
			}
		})
	}
}

func TestAgentToolNativeSchemaEnvironmentCeiling(t *testing.T) {
	skipUnlessSandboxTests(t)
	t.Setenv("REVIEW_INTERNAL_CONTEXT", "must-not-reach-discovery")
	root := realTempDir(t)
	script := agentToolPolicyFixture(t, root, "probe.sh", "#!/bin/sh\n[ -z \"${REVIEW_INTERNAL_CONTEXT+x}\" ] || exit 19\nprintf '%s\\n' '{\"title\":\"probe\",\"type\":\"object\",\"properties\":{}}'\n")
	r := NewToolRegistry(nil, WithSandboxFactory(sandbox.New, sandbox.Config{WritablePaths: []string{root}, AllowEnv: []string{"PATH"}}))
	defer r.Close()
	r.executionSandboxPresets = []string{"workspace"}
	derived := r.Derive()
	defer derived.Close()
	if _, err := derived.LoadShellTool(script); err != nil {
		t.Fatalf("schema discovery restored ambient environment: %v", err)
	}
}

func TestAgentToolNativeIncompatibleEnvironment(t *testing.T) {
	skipUnlessSandboxTests(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		base, local []string
	}{
		{name: "disjoint", base: []string{"PATH"}, local: []string{"REVIEW_INTERNAL_CONTEXT"}},
		{name: "sensitive only", local: []string{"Review_Api_Token"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := realTempDir(t)
			local := sandbox.Config{AllowEnv: tc.local}
			meta, err := json.Marshal(map[string]any{"title": "probe", "type": "object", "properties": map[string]any{}, "sandbox": local})
			if err != nil {
				t.Fatal(err)
			}
			script := agentToolPolicyFixture(t, root, "probe.sh", "#!/bin/sh\nprintf '%s\\n' '"+string(meta)+"'\n")
			config := agentToolPolicyMCPConfig(t, root, binary, local)
			parent := NewToolRegistry(nil, WithNativeTools(), WithSandboxFactory(sandbox.New, sandbox.Config{WritablePaths: []string{root}, AllowEnv: tc.base}))
			defer parent.Close()
			if _, err := parent.LoadShellToolWithNamespace(script, "shell"); err != nil {
				t.Fatal(err)
			}
			if _, err := parent.LoadMCPServer(config); err != nil {
				t.Fatal(err)
			}
			ec, err := parent.ExecutionPolicy(root, ExecutionGrant{SourceRoot: root, SandboxPresets: []string{"workspace"}})
			if err != nil {
				t.Fatal(err)
			}
			bound, omitted, err := parent.BindExecutionContext(ec, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer bound.Close()
			for _, name := range []string{"shell__probe", "probe__inspect"} {
				if !slices.Contains(omitted, name) || bound.ValidateToolSelection([]string{name}, nil) == nil {
					t.Fatalf("incompatible tool %s was not rejected: %v", name, omitted)
				}
			}
			for _, r := range []*ToolRegistry{bound, bound.Derive()} {
				defer r.Close()
				if _, err := r.LoadShellTool(script); err == nil || !strings.Contains(err.Error(), "allowEnv") {
					t.Fatalf("incompatible late shell accepted: %v", err)
				}
				if _, err := r.LoadMCPServer(config); err == nil || !strings.Contains(err.Error(), "allowEnv") {
					t.Fatalf("incompatible late MCP accepted: %v", err)
				}
			}
		})
	}
}

func TestAgentToolPolicyMCPFixture(t *testing.T) {
	if os.Args[len(os.Args)-1] != "agent-tool-policy-mcp" {
		return
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "agent-tool-policy", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "inspect", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var values []string
		for _, name := range []string{"REVIEW_INTERNAL_CONTEXT", "REVIEW_API_TOKEN", "POLICY_ONLY", "TOOL_ADDED", "MCP_EXPLICIT"} {
			value, ok := os.LookupEnv(name)
			if !ok {
				value = "unset"
			}
			values = append(values, value)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Join(values, "|")}}}, nil
	})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
