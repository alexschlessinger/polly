package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func writeRegistryMCPConfig(t *testing.T, config MCPConfig) string {
	t.Helper()
	data, err := json.Marshal(MCPServersConfig{MCPServers: map[string]MCPConfig{"srv": config}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func registryMCPServer(t *testing.T, names ...string) (string, *atomic.Int32) {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "registry-test", Version: "1"}, nil)
	for _, name := range names {
		server.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content:           []mcp.Content{&mcp.TextContent{Text: "reply"}, &mcp.ImageContent{Data: []byte("image"), MIMEType: "image/png"}},
				StructuredContent: map[string]any{"ok": true},
			}, nil
		})
	}
	closed := new(atomic.Int32)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			closed.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	return httpServer.URL, closed
}

func TestMCPLoadingPreservesSourcesNamesAndRichOutput(t *testing.T) {
	url, _ := registryMCPServer(t, "alpha", "beta")
	path := writeRegistryMCPConfig(t, MCPConfig{Transport: "streamable", URL: url})
	for _, tc := range []struct {
		name, suffix, prefix string
		filtered             bool
		allowed              []string
	}{
		{name: "normal", suffix: "#srv", prefix: "skill-srv"},
		{name: "restore bare names and implicit server", prefix: "srv", filtered: true, allowed: []string{"alpha", "beta"}},
		{name: "restore namespaced names", suffix: "#srv", prefix: "srv", filtered: true, allowed: []string{"srv__alpha", "srv__beta"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := NewToolRegistry(nil)
			defer registry.Close()
			var err error
			if tc.filtered {
				err = registry.LoadMCPServerWithFilter(path+tc.suffix, tc.allowed)
			} else {
				_, err = registry.LoadMCPServerWithNamespacePrefix(path, "skill")
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := registry.serverTools[path+tc.suffix]; !reflect.DeepEqual(got, []string{tc.prefix + "__alpha", tc.prefix + "__beta"}) {
				t.Fatalf("server tools = %v", registry.serverTools)
			}
			for _, tool := range registry.All() {
				if tool.GetSource() != path+tc.suffix {
					t.Fatalf("source = %q", tool.GetSource())
				}
				output, err := tool.(OutputTool).ExecuteOutput(context.Background(), nil)
				if err != nil || !strings.Contains(output.Text, "reply") || !reflect.DeepEqual(output.Data, map[string]any{"ok": true}) || len(output.Media) != 1 || string(output.Media[0].Data) != "image" {
					t.Fatalf("output = %#v, error = %v", output, err)
				}
			}
		})
	}
}

func TestMCPFilteredReloadOwnsOnlyRetainedClients(t *testing.T) {
	url, closed := registryMCPServer(t, "alpha", "beta")
	path := writeRegistryMCPConfig(t, MCPConfig{Transport: "streamable", URL: url})
	registry := NewToolRegistry(nil)
	defer registry.Close()
	if err := registry.LoadMCPServerWithFilter(path, []string{"alpha", "beta"}); err != nil {
		t.Fatal(err)
	}
	first := registry.toolClients["srv__alpha"]
	if err := registry.LoadMCPServerWithFilter(path, []string{"srv__beta"}); err != nil {
		t.Fatal(err)
	}
	second := registry.toolClients["srv__beta"]
	if !first.Closed() || second == nil || second.Closed() || len(registry.All()) != 1 || closed.Load() != 1 {
		t.Fatal("reload did not replace the old client and selected tools")
	}
	if err := registry.LoadMCPServerWithFilter(path, nil); err != nil {
		t.Fatal(err)
	}
	if !second.Closed() || len(registry.All()) != 0 || len(registry.serverTools) != 0 || len(registry.toolClients) != 0 || closed.Load() != 3 {
		t.Fatalf("empty selection retained tools or a connection: tools=%v, servers=%v, clients=%v, closed=%d", registry.All(), registry.serverTools, registry.toolClients, closed.Load())
	}
}

func TestMCPLoadingClosesServerWithNoTools(t *testing.T) {
	url, closed := registryMCPServer(t)
	path := writeRegistryMCPConfig(t, MCPConfig{Transport: "streamable", URL: url})
	registry := NewToolRegistry(nil)
	defer registry.Close()
	if _, err := registry.LoadMCPServer(path); err != nil {
		t.Fatal(err)
	}
	if len(registry.All()) != 0 || closed.Load() != 1 {
		t.Fatalf("empty server retained a connection: tools=%v, closed=%d", registry.All(), closed.Load())
	}
}

func TestMCPLoadingKeepsContextRebindingAtNormalEntryPoint(t *testing.T) {
	url, _ := registryMCPServer(t, "alpha")
	path := writeRegistryMCPConfig(t, MCPConfig{Transport: "streamable", URL: url})
	registry := NewToolRegistry(nil)
	defer registry.Close()
	registry.executionRoot = t.TempDir()
	if _, err := registry.LoadMCPServer(path); err == nil || !strings.Contains(err.Error(), "must declare contextIndependent") {
		t.Fatalf("normal context-bound load error = %v", err)
	}
	// Restoration uses its saved configuration directly; sharing preparation
	// must not introduce the normal loader's context-rebinding requirements.
	if err := registry.LoadMCPServerWithFilter(path, []string{"alpha"}); err != nil {
		t.Fatalf("restoration error = %v", err)
	}
}

func TestMCPLoadingEnforcesSandboxPolicyForBothEntryPoints(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
		factory         bool
	}{
		{name: "no sandbox", want: "requires sandboxing"},
		{name: "opt out", raw: "false", factory: true, want: "requested sandbox:false"},
		{name: "invalid policy", raw: `"invalid"`, factory: true, want: "invalid sandbox config"},
		{name: "factory failure", factory: true, want: "sandbox for MCP server srv"},
	} {
		for _, filtered := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/normal", true: "/filtered"}[filtered], func(t *testing.T) {
				path := writeRegistryMCPConfig(t, MCPConfig{Command: "must-not-start", Sandbox: json.RawMessage(tc.raw)})
				var opts []RegistryOption
				if tc.factory {
					opts = append(opts, WithSandboxFactory(failingSandboxFactory(), sandbox.Config{}))
				}
				registry := NewToolRegistry(nil, opts...)
				defer registry.Close()
				var err error
				if filtered {
					err = registry.LoadMCPServerWithFilter(path, []string{"alpha"})
				} else {
					_, err = registry.LoadMCPServer(path)
				}
				if err == nil || !strings.Contains(err.Error(), tc.want) || len(registry.All()) != 0 {
					t.Fatalf("load error = %v, want %q; tools = %v", err, tc.want, registry.All())
				}
			})
		}
	}
}
