package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// rewriteRegistryMCPConfig replaces the config at path with one server "srv".
func rewriteRegistryMCPConfig(t *testing.T, path string, config MCPConfig) {
	t.Helper()
	data, err := json.Marshal(MCPServersConfig{MCPServers: map[string]MCPConfig{"srv": config}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRestartMCPServerKeepsItsNamespaceAndTools(t *testing.T) {
	url, closed := registryMCPServer(t, "alpha", "beta", "gamma")
	path := writeRegistryMCPConfig(t, MCPConfig{Transport: "streamable", URL: url})
	for _, tc := range []struct {
		name, namespace string
		load            func(*ToolRegistry) error
		want            []string
	}{
		{"skill-loaded", "skill-srv", func(r *ToolRegistry) error {
			_, err := r.LoadMCPServerWithNamespacePrefix(path, "skill")
			return err
		}, []string{"skill-srv__alpha", "skill-srv__beta", "skill-srv__gamma"}},
		{"restored with a subset", "srv", func(r *ToolRegistry) error {
			return r.LoadMCPServerWithFilter(path+"#srv", []string{"alpha", "beta"})
		}, []string{"srv__alpha", "srv__beta"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := NewToolRegistry(nil)
			defer registry.Close()
			if err := tc.load(registry); err != nil {
				t.Fatal(err)
			}
			running := registry.toolClients[tc.want[0]]
			closedBefore := closed.Load()
			result, err := registry.RestartMCPServer(tc.namespace)
			if err != nil {
				t.Fatal(err)
			}
			got := slices.Sorted(slices.Values(result.Servers[0].ToolNames))
			if !slices.Equal(got, tc.want) || !slices.Equal(toolNames(registry.All()), tc.want) {
				t.Fatalf("restarted tools = %v, loaded = %v, want %v", got, toolNames(registry.All()), tc.want)
			}
			fresh := registry.toolClients[tc.want[0]]
			if fresh == nil || fresh == running || fresh.Closed() {
				t.Fatal("the restart did not swap in a new client")
			}
			if !running.Closed() || closed.Load() != closedBefore+1 {
				t.Fatal("the restart left the running server's session open")
			}
		})
	}
}

func TestRestartMCPServerKeepsTheRunningServerWhenTheNewOneFails(t *testing.T) {
	url, _ := registryMCPServer(t, "alpha", "beta")
	path := writeRegistryMCPConfig(t, MCPConfig{Transport: "streamable", URL: url})
	registry := NewToolRegistry(nil)
	defer registry.Close()
	if _, err := registry.LoadMCPServer(path); err != nil {
		t.Fatal(err)
	}
	running := registry.toolClients["srv__alpha"]
	kept := func(stage string) {
		t.Helper()
		if registry.toolClients["srv__alpha"] != running || running.Closed() || len(registry.All()) != 2 {
			t.Fatalf("%s: the running server was replaced or closed", stage)
		}
	}

	if _, err := registry.RestartMCPServer("other"); err == nil || !strings.Contains(err.Error(), `no MCP server "other"`) {
		t.Fatalf("restart of an unknown server = %v", err)
	}
	rewriteRegistryMCPConfig(t, path, MCPConfig{Transport: "streamable", URL: "http://127.0.0.1:1/mcp"})
	if _, err := registry.RestartMCPServer("srv"); err == nil || !strings.Contains(err.Error(), "keeping the running one") {
		t.Fatalf("restart of an unreachable server = %v", err)
	}
	kept("unreachable")
	elsewhere, _ := registryMCPServer(t, "delta")
	rewriteRegistryMCPConfig(t, path, MCPConfig{Transport: "streamable", URL: elsewhere})
	if _, err := registry.RestartMCPServer("srv"); err == nil || !strings.Contains(err.Error(), "none of its tools") {
		t.Fatalf("restart of a server without its tools = %v", err)
	}
	kept("no tools")

	// A server that now offers some of its tools restarts without the rest.
	partial, _ := registryMCPServer(t, "alpha")
	rewriteRegistryMCPConfig(t, path, MCPConfig{Transport: "streamable", URL: partial})
	if _, err := registry.RestartMCPServer("srv"); err != nil {
		t.Fatal(err)
	}
	if names := toolNames(registry.All()); !slices.Equal(names, []string{"srv__alpha"}) {
		t.Fatalf("tools after a partial restart = %v, want only srv__alpha", names)
	}
	if !running.Closed() {
		t.Fatal("the partial restart left the old session open")
	}
}

// fixtureStdioMCPServer writes a stdio MCP server that answers the handshake
// and lists one tool, probe.
func fixtureStdioMCPServer(t *testing.T) string {
	t.Helper()
	script := `#!/bin/sh
while IFS= read -r line; do
	id=$(printf '%s\n' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
	case "$line" in
	*'"method":"initialize"'*)
		version=$(printf '%s\n' "$line" | sed -n 's/.*"protocolVersion":"\([^"]*\)".*/\1/p')
		printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"%s","capabilities":{"tools":{}},"serverInfo":{"name":"fixture","version":"1"}}}\n' "$id" "$version"
		;;
	*'"method":"tools/list"'*)
		printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"probe","inputSchema":{"type":"object"}}]}}\n' "$id"
		;;
	*)
		if [ -n "$id" ]; then
			printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
		fi
		;;
	esac
done
`
	path := filepath.Join(t.TempDir(), "fixture-server.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// A stdio server keeps the policy it started with until it restarts, and the
// restart builds its sandbox from the current base.
func TestRestartMCPServerAppliesTheCurrentPolicyToAStdioServer(t *testing.T) {
	skipIfWindows(t)
	dir := realTempDir(t)
	data, err := json.Marshal(MCPServersConfig{MCPServers: map[string]MCPConfig{"fixture": {Command: fixtureStdioMCPServer(t)}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	registry := stubSandboxRegistry(t, sandbox.Config{})
	defer registry.Close()
	if _, err := registry.LoadMCPServer(path); err != nil {
		t.Fatal(err)
	}
	reads := func() []string {
		client := registry.toolClients["fixture__probe"]
		if client == nil || !client.sandboxed || client.sandboxCfg == nil {
			t.Fatalf("fixture client = %+v, want a sandboxed stdio server", client)
		}
		return client.sandboxCfg.ReadPaths
	}

	change, err := registry.AppendBaseReadPaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(change.StaleServers, []string{"fixture"}) || slices.Contains(reads(), dir) {
		t.Fatalf("before the restart: stale = %v, server reads %v", change.StaleServers, reads())
	}
	if _, err := registry.RestartMCPServer("fixture"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(reads(), dir) {
		t.Fatalf("restarted server reads %v, want the added %q", reads(), dir)
	}
}
