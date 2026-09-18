package tools

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// realTempDir returns a fresh temp directory spelled the way preparation
// freezes it.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// sandboxReadPaths returns the read grants of a loaded tool's sandbox.
func sandboxReadPaths(t *testing.T, tool Tool) []string {
	t.Helper()
	info := SandboxDetails(tool)
	if !info.Active || info.Config == nil {
		t.Fatalf("%s is not sandboxed with a known config: %+v", tool.GetName(), info)
	}
	return info.Config.ReadPaths
}

func TestAppendBaseReadPathsRebuildsLoadedBashAndShellTools(t *testing.T) {
	skipIfWindows(t)
	dir := realTempDir(t)
	registry := stubSandboxRegistry(t, sandbox.Config{})
	t.Cleanup(func() { _ = registry.Close() })
	if _, err := registry.LoadToolAuto("bash"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.LoadShellTool(createTestScript(t, t.TempDir())); err != nil {
		t.Fatal(err)
	}
	running, _ := registry.Get("bash")

	change, err := registry.AppendBaseReadPaths(dir)
	if err != nil {
		t.Fatalf("AppendBaseReadPaths: %v", err)
	}
	if want := []string{"bash", "test-tool__test-tool"}; !slices.Equal(change.Rebuilt, want) {
		t.Fatalf("rebuilt = %v, want %v", change.Rebuilt, want)
	}
	for _, name := range change.Rebuilt {
		tool, ok := registry.Get(name)
		if !ok {
			t.Fatalf("%s is gone after the rebuild", name)
		}
		if !slices.Contains(sandboxReadPaths(t, tool), dir) {
			t.Fatalf("%s read paths = %v, want the added %q", name, sandboxReadPaths(t, tool), dir)
		}
	}
	if slices.Contains(sandboxReadPaths(t, running), dir) {
		t.Fatal("the bash instance a running call holds changed under it")
	}
	if len(change.StaleServers) != 0 {
		t.Fatalf("stale servers = %v, want none", change.StaleServers)
	}
}

func TestSandboxPolicyChangeIsAllOrNothing(t *testing.T) {
	skipIfWindows(t)
	refused := realTempDir(t)
	factory := func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		if slices.Contains(cfg.ReadPaths, refused) {
			return nil, errors.New("factory refuses the grant")
		}
		return stubSandbox{}, nil
	}
	registry := NewToolRegistry(nil, WithSandboxFactory(factory, sandbox.Config{}))
	t.Cleanup(func() { _ = registry.Close() })
	unchanged := func(stage string) {
		t.Helper()
		cfg, _, err := registry.SandboxReadPolicy()
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(cfg.ReadPaths, refused) {
			t.Fatalf("%s: the refused change widened the policy: %v", stage, cfg.ReadPaths)
		}
	}

	// With nothing loaded the factory still judges the changed policy.
	if _, err := registry.AppendBaseReadPaths(refused); err == nil || !strings.Contains(err.Error(), "factory refuses") {
		t.Fatalf("AppendBaseReadPaths with no tools = %v, want the factory's refusal", err)
	}
	unchanged("no tools")

	if _, err := registry.LoadToolAuto("bash"); err != nil {
		t.Fatal(err)
	}
	before, _ := registry.Get("bash")
	if _, err := registry.AppendBaseReadPaths(refused); err == nil || !strings.Contains(err.Error(), "factory refuses") {
		t.Fatalf("AppendBaseReadPaths with bash = %v, want the factory's refusal", err)
	}
	unchanged("bash loaded")
	if after, _ := registry.Get("bash"); after != before {
		t.Fatal("a refused change replaced bash")
	}
}

func TestSandboxPolicyChangeNamesRunningServers(t *testing.T) {
	registry := stubSandboxRegistry(t, sandbox.Config{})
	t.Cleanup(func() { _ = registry.Close() })
	registry.mu.Lock()
	registry.setToolLocked("github__search", &testTool{name: "github__search"}, &MCPClient{sandboxed: true})
	registry.setToolLocked("github__issue", &testTool{name: "github__issue"}, &MCPClient{sandboxed: true})
	registry.setToolLocked("remote__fetch", &testTool{name: "remote__fetch"}, &MCPClient{})
	registry.pendingToolClients["fs__read"] = &MCPClient{sandboxed: true}
	registry.mu.Unlock()

	change, err := registry.AppendBaseReadPaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"fs", "github"}; !slices.Equal(change.StaleServers, want) {
		t.Fatalf("stale servers = %v, want the sandboxed stdio servers %v", change.StaleServers, want)
	}
	if len(change.Rebuilt) != 0 {
		t.Fatalf("rebuilt = %v, want no process tools", change.Rebuilt)
	}
}

func TestSandboxPolicyChangeRefusedOnBoundRegistry(t *testing.T) {
	registry := stubSandboxRegistry(t, sandbox.Config{})
	t.Cleanup(func() { _ = registry.Close() })
	ec, err := registry.ExecutionPolicy(t.TempDir(), ExecutionGrant{})
	if err != nil {
		t.Fatal(err)
	}
	bound, _, err := registry.BindExecutionContext(ec, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bound.Close() })
	if _, err := bound.AppendBaseReadPaths(t.TempDir()); err == nil || !strings.Contains(err.Error(), "fixed sandbox policy") {
		t.Fatalf("bound AppendBaseReadPaths = %v, want the fixed-policy refusal", err)
	}
}
