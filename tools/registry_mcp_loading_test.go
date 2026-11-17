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

// sandboxDeclaration decodes a raw "sandbox" entry; "" is an absent one.
func sandboxDeclaration(t *testing.T, raw string) sandbox.Declaration {
	t.Helper()
	var declaration sandbox.Declaration
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &declaration); err != nil {
			t.Fatal(err)
		}
	}
	return declaration
}

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
	// Drop connections a leaked client still holds open, so a leak fails
	// the test instead of hanging Close.
	t.Cleanup(func() {
		httpServer.CloseClientConnections()
		httpServer.Close()
	})
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
				path := writeRegistryMCPConfig(t, MCPConfig{Command: "must-not-start", Sandbox: sandboxDeclaration(t, tc.raw)})
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

func TestLoadRegistryClosesConnectedServersOnFailure(t *testing.T) {
	url, closed := registryMCPServer(t, "alpha")
	path := writeRegistryMCPConfig(t, MCPConfig{Transport: "streamable", URL: url})
	// MCP servers load before native tools, and bash refuses to load
	// without a sandbox.
	registry, err := LoadRegistry([]ToolLoaderInfo{
		{Type: "mcp", Source: path, Name: "alpha"},
		{Type: "native", Source: "builtin", Name: "bash"},
	}, WithNativeTools())
	if err == nil {
		_ = registry.Close()
		t.Fatal("bash loaded without a sandbox")
	}
	if closed.Load() != 1 {
		t.Fatalf("failed load leaked the connected server: closed=%d", closed.Load())
	}
}

func TestMCPLoadRevalidatesBeforeInstallation(t *testing.T) {
	for _, mutation := range []string{"close", "remove", "replace", "policy", "independent"} {
		t.Run(mutation, func(t *testing.T) {
			url, entered, unblock, armed, closed := lifecycleMCPGate(t, "tools/list")
			registry := stubSandboxRegistry(t, sandbox.Config{})
			defer registry.Close()
			path := writeRegistryMCPConfig(t, MCPConfig{URL: url, Transport: "streamable"})
			if _, err := registry.LoadMCPServer(path); err != nil {
				t.Fatal(err)
			}
			old := registry.toolClients["srv__probe"]
			armed.Store(true)
			done := make(chan error, 1)
			go func() { _, err := registry.RestartMCPServerContext(context.Background(), "srv"); done <- err }()
			awaitLifecycle(t, entered)
			switch mutation {
			case "close":
				registry.Close()
			case "remove":
				registry.Remove("srv__probe")
			case "replace":
				registry.Register(&testTool{name: "srv__probe"})
			case "policy":
				if _, err := registry.AppendBaseReadPaths(t.TempDir()); err != nil {
					t.Fatal(err)
				}
			case "independent":
				otherPath := writeRegistryMCPConfig(t, MCPConfig{URL: url, Transport: "streamable"})
				if _, err := registry.LoadMCPServerWithNamespacePrefix(otherPath, "other"); err != nil {
					t.Fatal(err)
				}
			}
			unblock()
			if err := <-done; mutation == "independent" {
				if err != nil {
					t.Fatalf("independent load invalidated restart: %v", err)
				}
				if registry.toolClients["srv__probe"] == old || !old.Closed() {
					t.Fatal("successful restart did not transfer ownership")
				}
			} else {
				if err == nil {
					t.Fatal("stale candidate was installed")
				}
				if closed.Load() == 0 {
					t.Fatal("losing candidate was not closed")
				}
				if mutation == "close" || mutation == "remove" {
					if _, exists := registry.Get("srv__probe"); exists {
						t.Fatal("removed tool resurrected")
					}
				} else if registry.toolClients["srv__probe"] != old {
					t.Fatal("failed replacement changed client ownership")
				}
			}
		})
	}
}

func TestNativeMCPBindingPreservesNamespaceOwnershipAndOutput(t *testing.T) {
	url, _ := registryMCPServer(t, "alpha", "beta")
	path := writeRegistryMCPConfig(t, MCPConfig{Transport: "streamable", URL: url, ContextIndependent: true})
	source := NewToolRegistry(nil, WithNativeTools())
	defer source.Close()
	if _, err := source.LoadMCPServerWithNamespacePrefix(path, "skill"); err != nil {
		t.Fatal(err)
	}
	original := source.toolClients["skill-srv__alpha"]
	open := NativeOpenTools(source)
	scope := ToolScope{Root: t.TempDir(), AllowedTools: []string{"skill-srv__alpha"}}
	var previous *MCPClient
	for range 2 {
		ctx, cancel := context.WithCancel(context.Background())
		binding, err := open(ctx, scope)
		cancel() // Setup context is not the successful binding's lifetime.
		if err != nil {
			t.Fatal(err)
		}
		if err := binding.Registry.ValidateToolSelection(scope.AllowedTools, nil); err != nil {
			t.Fatal(err)
		}
		client := binding.Registry.toolClients["skill-srv__alpha"]
		if client == nil || client == original || client == previous {
			t.Fatal("binding reused a process/session")
		}
		if _, ok := binding.Registry.Get("skill-srv__beta"); ok {
			t.Fatal("unselected tool exposed")
		}
		tool, ok := binding.Registry.Get("skill-srv__alpha")
		if !ok {
			t.Fatal("namespace lost during binding")
		}
		output, err := tool.(OutputTool).ExecuteOutput(context.Background(), nil)
		if err != nil || len(output.Media) != 1 || output.Data == nil {
			t.Fatalf("rich output=%+v err=%v", output, err)
		}
		if err := binding.Close(); err != nil {
			t.Fatal(err)
		}
		if err := binding.Close(); err != nil {
			t.Fatal(err)
		}
		if !client.Closed() || original.Closed() {
			t.Fatal("binding closed wrong owner")
		}
		previous = client
	}
}
