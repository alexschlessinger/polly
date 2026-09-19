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
	// A layer never reaches a server, so its change leaves none stale.
	change, err = registry.SetSandboxLayer("profile", &sandbox.Config{ReadPaths: []string{t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if len(change.StaleServers) != 0 {
		t.Fatalf("stale servers after a layer change = %v, want none", change.StaleServers)
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

// recordingFactory hands out stub sandboxes and keeps every config it built
// one for, the latest last.
func recordingFactory() (func(sandbox.Config) (sandbox.Sandbox, error), *[]sandbox.Config) {
	var built []sandbox.Config
	return func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		built = append(built, cfg)
		return stubSandbox{}, nil
	}, &built
}

func TestSandboxLayerReachesProcessToolsOnly(t *testing.T) {
	skipIfWindows(t)
	dir := realTempDir(t)
	factory, built := recordingFactory()
	registry := NewToolRegistry(nil, WithSandboxFactory(factory, sandbox.Config{}),
		WithSandboxLayer("profile", sandbox.Config{ReadPaths: []string{dir}, PassEnv: []string{"NPM_TOKEN"}}))
	t.Cleanup(func() { _ = registry.Close() })
	layered := func(cfg sandbox.Config) bool {
		return slices.Contains(cfg.ReadPaths, dir) && slices.Contains(cfg.PassEnv, "NPM_TOKEN")
	}
	untouched := func(cfg sandbox.Config) bool {
		return !slices.Contains(cfg.ReadPaths, dir) && !slices.Contains(cfg.PassEnv, "NPM_TOKEN")
	}
	latest := func() sandbox.Config { return (*built)[len(*built)-1] }

	// Reached: bash, shell tools, NewSandbox, and the process policy, of the
	// registry and of one derived from it.
	script := createTestScript(t, t.TempDir())
	if _, err := registry.LoadToolAuto("bash"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.LoadShellTool(script); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bash", "test-tool__test-tool"} {
		tool, _ := registry.Get(name)
		if cfg := SandboxDetails(tool).Config; cfg == nil || !layered(*cfg) {
			t.Fatalf("%s sandbox = %+v, want the layer", name, cfg)
		}
	}
	for name, reg := range map[string]*ToolRegistry{"registry": registry, "derived": registry.Derive()} {
		if _, err := reg.NewSandbox(nil); err != nil {
			t.Fatal(err)
		}
		if !layered(latest()) {
			t.Fatalf("%s NewSandbox built %+v, want the layer", name, latest())
		}
		if cfg, active, err := reg.ProcessSandboxPolicy(); err != nil || !active || !layered(cfg) {
			t.Fatalf("%s ProcessSandboxPolicy = %+v, %v, %v; want the layer", name, cfg, active, err)
		}
	}

	// Not reached: the in-process read policy, stdio MCP servers, members,
	// and schema discovery.
	if cfg, _, err := registry.SandboxReadPolicy(); err != nil || !untouched(cfg) {
		t.Fatalf("SandboxReadPolicy = %+v, %v; want the base alone", cfg, err)
	}
	if _, cfg, err := registry.newServerSandbox("server", nil); err != nil || !untouched(cfg) {
		t.Fatalf("server sandbox = %+v, %v; want the base alone", cfg, err)
	}
	ec, err := registry.ExecutionPolicy(t.TempDir(), ExecutionGrant{})
	if err != nil || !untouched(ec.Sandbox) {
		t.Fatalf("member policy = %+v, %v; want the base alone", ec.Sandbox, err)
	}
	if _, err := registry.newSchemaSandbox(script); err != nil || !untouched(latest()) {
		t.Fatalf("schema sandbox = %+v, %v; want the base alone", latest(), err)
	}
}

func TestSetSandboxLayerReplacesAndRemoves(t *testing.T) {
	skipIfWindows(t)
	first, second := realTempDir(t), realTempDir(t)
	registry := stubSandboxRegistry(t, sandbox.Config{})
	t.Cleanup(func() { _ = registry.Close() })
	if _, err := registry.LoadToolAuto("bash"); err != nil {
		t.Fatal(err)
	}
	earlier := registry.Derive()
	for _, step := range []struct {
		name      string
		layer     *sandbox.Config
		want, not []string
	}{
		{"set", &sandbox.Config{ReadPaths: []string{first}}, []string{first}, []string{second}},
		{"replace", &sandbox.Config{ReadPaths: []string{second}}, []string{second}, []string{first}},
		{"remove", nil, nil, []string{first, second}},
	} {
		change, err := registry.SetSandboxLayer("profile", step.layer)
		if err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if !slices.Equal(change.Rebuilt, []string{"bash"}) {
			t.Fatalf("%s: rebuilt = %v, want bash", step.name, change.Rebuilt)
		}
		bash, _ := registry.Get("bash")
		derived, _, err := earlier.ProcessSandboxPolicy()
		if err != nil {
			t.Fatal(err)
		}
		for label, reads := range map[string][]string{"bash": sandboxReadPaths(t, bash), "derived policy": derived.ReadPaths} {
			for _, path := range step.want {
				if !slices.Contains(reads, path) {
					t.Fatalf("%s: %s reads %v, want %q", step.name, label, reads, path)
				}
			}
			for _, path := range step.not {
				if slices.Contains(reads, path) {
					t.Fatalf("%s: %s reads %v, still %q", step.name, label, reads, path)
				}
			}
		}
	}
	if _, err := earlier.SetSandboxLayer("profile", nil); err == nil || !strings.Contains(err.Error(), "shares its parent's sandbox policy") {
		t.Fatalf("derived SetSandboxLayer = %v, want the shared-policy refusal", err)
	}
	if _, err := registry.SetSandboxLayer("", &sandbox.Config{}); err == nil {
		t.Fatal("SetSandboxLayer accepted an unnamed layer")
	}
}

func TestSandboxLayersMergeInNameOrderBeforeTheToolOverlay(t *testing.T) {
	factory, built := recordingFactory()
	registry := NewToolRegistry(nil, WithSandboxFactory(factory, sandbox.Config{}),
		WithSandboxLayer("b", sandbox.Config{Env: map[string]string{"CACHE": "b", "ONLY_B": "b"}}),
		WithSandboxLayer("a", sandbox.Config{Env: map[string]string{"CACHE": "a"}}))
	t.Cleanup(func() { _ = registry.Close() })
	cfg, _, err := registry.ProcessSandboxPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env["CACHE"] != "b" || cfg.Env["ONLY_B"] != "b" {
		t.Fatalf("process env = %v, want layer b merged after layer a", cfg.Env)
	}
	if _, err := registry.NewSandbox(&sandbox.Config{Env: map[string]string{"CACHE": "tool"}}); err != nil {
		t.Fatal(err)
	}
	if got := (*built)[len(*built)-1].Env["CACHE"]; got != "tool" {
		t.Fatalf("tool overlay CACHE = %q, want the tool's own value over the layers", got)
	}
}

func TestSandboxLayerThatCannotBePreparedFailsClosed(t *testing.T) {
	skipIfWindows(t)
	home := sandbox.Config{ReadPaths: []string{"~"}}
	factory, _ := recordingFactory()
	registry := NewToolRegistry(nil, WithSandboxFactory(factory, sandbox.Config{}), WithSandboxLayer("bad", home))
	t.Cleanup(func() { _ = registry.Close() })
	if _, err := registry.LoadToolAuto("bash"); err == nil || !strings.Contains(err.Error(), `sandbox layer "bad"`) {
		t.Fatalf("bash under an unprepared layer = %v, want the layer's preparation error", err)
	}

	valid := stubSandboxRegistry(t, sandbox.Config{})
	t.Cleanup(func() { _ = valid.Close() })
	if _, err := valid.SetSandboxLayer("bad", &home); err == nil || !strings.Contains(err.Error(), `sandbox layer "bad"`) {
		t.Fatalf("SetSandboxLayer with a home grant = %v, want the preparation error", err)
	}
	if cfg, _, err := valid.ProcessSandboxPolicy(); err != nil || len(cfg.ReadPaths) != 0 {
		t.Fatalf("policy after the refused layer = %+v, %v; want it unchanged", cfg, err)
	}
	// Replacing the unprepared layer repairs the registry.
	if _, err := registry.SetSandboxLayer("bad", nil); err != nil {
		t.Fatalf("removing the unprepared layer: %v", err)
	}
	if _, err := registry.LoadToolAuto("bash"); err != nil {
		t.Fatalf("bash after removing the unprepared layer: %v", err)
	}
}
