package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

type testTool struct {
	name   string
	source string
}

func TestRegistryReturnsToolsAndSchemasInLexicalOrder(t *testing.T) {
	first := NewToolRegistry([]Tool{
		&testTool{name: "zebra"},
		&testTool{name: "alpha"},
		&testTool{name: "middle"},
	})
	second := NewToolRegistry([]Tool{
		&testTool{name: "middle"},
		&testTool{name: "zebra"},
		&testTool{name: "alpha"},
	})

	names := func(registry *ToolRegistry) []string {
		all := registry.All()
		out := make([]string, len(all))
		for i, tool := range all {
			out[i] = tool.GetName()
		}
		return out
	}
	want := []string{"alpha", "middle", "zebra"}
	if got := names(first); !reflect.DeepEqual(got, want) {
		t.Fatalf("All() names = %#v, want %#v", got, want)
	}
	if got := names(second); !reflect.DeepEqual(got, want) {
		t.Fatalf("All() names after different registration order = %#v, want %#v", got, want)
	}

	firstJSON, err := json.Marshal(first.GetSchemas())
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second.GetSchemas())
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("schema bytes differ by registration order:\n%s\n%s", firstJSON, secondJSON)
	}
}

func (t *testTool) GetSchema() *schema.ToolSchema {
	return schema.Tool(t.name, "Test tool", nil)
}

func (t *testTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	return "test result", nil
}

func (t *testTool) GetName() string {
	return t.name
}

func (t *testTool) GetType() string {
	return "test"
}

func (t *testTool) GetSource() string {
	if t.source != "" {
		return t.source
	}
	return "test-source"
}

func TestRegistryRemove(t *testing.T) {
	registry := NewToolRegistry([]Tool{})
	tool := &testTool{name: "removable"}
	registry.Register(tool)

	_, exists := registry.Get("removable")
	if !exists {
		t.Error("Expected tool to exist before removal")
	}

	registry.Remove("removable")

	_, exists = registry.Get("removable")
	if exists {
		t.Error("Expected tool to not exist after removal")
	}
}

func TestSetToolLockedClosesOrphanedClient(t *testing.T) {
	registry := NewToolRegistry(nil)
	first := &MCPClient{}
	second := &MCPClient{}
	registry.mu.Lock()
	registry.setToolLocked("srv__a", &testTool{name: "srv__a"}, first)
	registry.setToolLocked("srv__b", &testTool{name: "srv__b"}, first)
	// Replacing one of two tools keeps the shared client alive.
	registry.setToolLocked("srv__a", &testTool{name: "srv__a"}, second)
	if first.Closed() {
		t.Fatal("client closed while another tool still used it")
	}
	// Replacing the last tool that used it closes it.
	registry.setToolLocked("srv__b", &testTool{name: "srv__b"}, nil)
	registry.mu.Unlock()
	if !first.Closed() {
		t.Fatal("orphaned client was not closed")
	}
	if second.Closed() {
		t.Fatal("live client was closed")
	}
	if _, ok := registry.toolClients["srv__b"]; ok {
		t.Fatal("non-MCP replacement left a client mapping behind")
	}
}

func TestSetToolLockedKeepsPendingClientAlive(t *testing.T) {
	registry := NewToolRegistry(nil)
	client := &MCPClient{}
	registry.mu.Lock()
	registry.setToolLocked("srv__a", &testTool{name: "srv__a"}, client)
	registry.pendingToolClients["srv__c"] = client
	registry.setToolLocked("srv__a", &testTool{name: "srv__a"}, nil)
	registry.mu.Unlock()
	if client.Closed() {
		t.Fatal("client referenced by a pending tool was closed")
	}
}

func TestDropServerToolsLockedClosesPreviousClient(t *testing.T) {
	registry := NewToolRegistry(nil)
	old := &MCPClient{}
	registry.mu.Lock()
	registry.setToolLocked("srv__a", &testTool{name: "srv__a"}, old)
	registry.setToolLocked("srv__b", &testTool{name: "srv__b"}, old)
	registry.serverTools["/cfg.json#srv"] = []string{"srv__a", "srv__b"}
	registry.dropServerToolsLocked("/cfg.json#srv")
	registry.mu.Unlock()
	if !old.Closed() {
		t.Fatal("previous server client was not closed")
	}
	if len(registry.tools) != 0 || len(registry.toolClients) != 0 || len(registry.serverTools) != 0 {
		t.Fatalf("stale registration left behind: tools=%d clients=%d servers=%d", len(registry.tools), len(registry.toolClients), len(registry.serverTools))
	}
}

func TestMCPClientCloseIsIdempotent(t *testing.T) {
	client := &MCPClient{}
	if client.Closed() {
		t.Fatal("new client reports closed")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if !client.Closed() {
		t.Fatal("client does not report closed")
	}
}

func TestAppendBaseReadPathsGrantsReadPolicy(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	registry := stubSandboxRegistry(t, sandbox.Config{})
	if _, err := registry.AppendBaseReadPaths(dir); err != nil {
		t.Fatalf("AppendBaseReadPaths: %v", err)
	}
	cfg, active, err := registry.SandboxReadPolicy()
	if err != nil {
		t.Fatalf("SandboxReadPolicy: %v", err)
	}
	if !active {
		t.Fatal("SandboxReadPolicy not active with a sandbox factory")
	}
	if !slices.Contains(cfg.ReadPaths, real) {
		t.Fatalf("SandboxReadPolicy read paths = %v, want %q", cfg.ReadPaths, real)
	}
	_, effective, err := registry.newSandboxFor("test", nil)
	if err != nil {
		t.Fatalf("newSandboxFor after append: %v", err)
	}
	if !slices.Contains(effective.ReadPaths, real) {
		t.Fatalf("per-tool sandbox read paths = %v, want %q", effective.ReadPaths, real)
	}
}

func TestAppendBaseReadPathsReachesDerivedRegistries(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	registry := stubSandboxRegistry(t, sandbox.Config{})
	earlier := registry.Derive()
	nested := earlier.Derive()
	if _, err := registry.AppendBaseReadPaths(dir); err != nil {
		t.Fatalf("AppendBaseReadPaths: %v", err)
	}
	for name, derived := range map[string]*ToolRegistry{"derived before": earlier, "derived from a derived": nested, "derived after": registry.Derive()} {
		cfg, active, err := derived.SandboxReadPolicy()
		if err != nil || !active {
			t.Fatalf("%s: SandboxReadPolicy active=%v err=%v", name, active, err)
		}
		if !slices.Contains(cfg.ReadPaths, real) {
			t.Fatalf("%s: read paths = %v, want %q", name, cfg.ReadPaths, real)
		}
	}
	if _, err := earlier.AppendBaseReadPaths(t.TempDir()); err == nil || !strings.Contains(err.Error(), "shares its parent's sandbox policy") {
		t.Fatalf("derived AppendBaseReadPaths = %v, want the shared-policy refusal", err)
	}
}

// The agent reads through a registry derived when the session opened, so an
// extra directory added mid-session must reach that registry's file checks.
func TestAppendBaseReadPathsReachesAnEarlierDerivedViewImage(t *testing.T) {
	denied := realTempDir(t)
	added := filepath.Join(denied, "added")
	if err := os.Mkdir(added, 0o700); err != nil {
		t.Fatal(err)
	}
	path := writeTestPNG(t, added, "shot.png")
	registry := stubSandboxRegistry(t, sandbox.Config{DenyPaths: []string{denied}})
	tool := NewViewImageTool(registry.Derive())
	args := map[string]any{"source": path}
	if _, err := tool.ExecuteOutput(context.Background(), args); err == nil || !strings.Contains(err.Error(), "sandbox policy") {
		t.Fatalf("view_image before the add = %v, want the sandbox denial", err)
	}
	if _, err := registry.AppendBaseReadPaths(added); err != nil {
		t.Fatalf("AppendBaseReadPaths: %v", err)
	}
	if _, err := tool.ExecuteOutput(context.Background(), args); err != nil {
		t.Fatalf("view_image after the add = %v, want the read allowed", err)
	}
}

func TestAppendBaseReadPathsWithoutSandboxFactoryIsNoOp(t *testing.T) {
	dir := t.TempDir()

	registry := NewToolRegistry(nil)
	if _, err := registry.AppendBaseReadPaths(dir); err != nil {
		t.Fatalf("AppendBaseReadPaths without factory: %v", err)
	}
	if registry.HasSandbox() {
		t.Fatal("registry without factory reports a sandbox")
	}
	cfg, active, err := registry.SandboxReadPolicy()
	if err != nil {
		t.Fatalf("SandboxReadPolicy: %v", err)
	}
	if active || len(cfg.ReadPaths) != 0 {
		t.Fatalf("no-op append changed the read policy: active=%v read paths=%v", active, cfg.ReadPaths)
	}

	unsafe := NewToolRegistry(nil, WithUnsafeNoSandbox())
	if _, err := unsafe.AppendBaseReadPaths(dir); err != nil {
		t.Fatalf("AppendBaseReadPaths with unsafe no-sandbox: %v", err)
	}
	cfg, active, err = unsafe.SandboxReadPolicy()
	if err != nil {
		t.Fatalf("SandboxReadPolicy: %v", err)
	}
	if active || len(cfg.ReadPaths) != 0 {
		t.Fatalf("no-op append changed the unsafe read policy: active=%v read paths=%v", active, cfg.ReadPaths)
	}
}

func TestAppendBaseReadPathsDropsMissingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	registry := stubSandboxRegistry(t, sandbox.Config{})
	if _, err := registry.AppendBaseReadPaths(missing); err != nil {
		t.Fatalf("AppendBaseReadPaths with missing path: %v", err)
	}
	cfg, active, err := registry.SandboxReadPolicy()
	if err != nil {
		t.Fatalf("SandboxReadPolicy: %v", err)
	}
	if !active {
		t.Fatal("SandboxReadPolicy not active with a sandbox factory")
	}
	if slices.Contains(cfg.ReadPaths, missing) {
		t.Fatalf("missing path granted: %v", cfg.ReadPaths)
	}
	if _, err := registry.NewSandbox(nil); err != nil {
		t.Fatalf("NewSandbox after appending missing path: %v", err)
	}
}
