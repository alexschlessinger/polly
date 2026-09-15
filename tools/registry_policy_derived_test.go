package tools

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func TestSandboxPolicyCommitFailureKeepsDerivedToolInstances(t *testing.T) {
	skipIfWindows(t)
	parent := stubSandboxRegistry(t, sandbox.Config{})
	defer parent.Close()
	child := parent.Derive().Derive()
	defer child.Close()
	before := make(map[*ToolRegistry]Tool)
	for _, r := range []*ToolRegistry{parent, child} {
		if _, err := r.LoadToolAuto("bash"); err != nil {
			t.Fatal(err)
		}
		before[r], _ = r.Get("bash")
	}
	failure := errors.New("profile could not be saved")
	calls := 0
	_, err := parent.SetSandboxLayerAndCommit("profile", &SandboxLayer{Config: sandbox.Config{Env: map[string]string{"BUILD_STATE": "/prepared"}}}, func() error {
		calls++
		return failure
	})
	if !errors.Is(err, failure) || calls != 1 {
		t.Fatalf("commit: %v, %d calls", err, calls)
	}
	for r, tool := range before {
		if after, _ := r.Get("bash"); after != tool {
			t.Fatal("failed persistence replaced a tool instance")
		}
		cfg, _, err := r.SandboxReadPolicy()
		if err != nil || cfg.Env["BUILD_STATE"] != "" {
			t.Fatalf("failed persistence published policy: %v %v", cfg.Env, err)
		}
	}
}

func TestSandboxPolicyRebuildsDerivedAndStagedTools(t *testing.T) {
	skipIfWindows(t)
	home := realTempDir(t)
	t.Setenv("HOME", home)
	private := filepath.Join(home, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	parent := stubSandboxRegistry(t, sandbox.Config{PrivateHome: true})
	t.Cleanup(func() { _ = parent.Close() })
	if _, err := parent.SetSandboxLayer("profile", &SandboxLayer{Config: sandbox.Config{ReadPaths: []string{private}, PassEnv: []string{"NPM_TOKEN"}}}); err != nil {
		t.Fatal(err)
	}
	child := parent.Derive()
	nested := child.Derive()
	registries := []*ToolRegistry{parent, child, nested}
	for _, registry := range registries {
		t.Cleanup(func() { _ = registry.Close() })
		if _, err := registry.LoadToolAuto("bash"); err != nil {
			t.Fatal(err)
		}
	}
	// Registering a tool borrowed from elsewhere must track the dependent
	// even before it has queried or built a sandbox of its own.
	borrowed := parent.Derive()
	t.Cleanup(func() { _ = borrowed.Close() })
	bash, _ := parent.Get("bash")
	borrowed.Register(bash)
	registries = append(registries, borrowed)
	script := createTestScript(t, t.TempDir())
	if _, err := child.LoadShellTool(script); err != nil {
		t.Fatal(err)
	}
	record, _, err := nested.prepareShellToolWithNamespace(script, "staged")
	if err != nil {
		t.Fatal(err)
	}
	nested.stagePreparedTools([]stagedToolRecord{record})
	running, _ := child.Get("bash")

	change, err := parent.SetSandboxLayer("profile", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"bash", "staged__test-tool", "test-tool__test-tool"}; !slices.Equal(change.Rebuilt, want) {
		t.Fatalf("rebuilt = %v, want %v", change.Rebuilt, want)
	}
	nested.CommitPendingChanges()
	for _, registry := range registries {
		policy, _, err := registry.SandboxReadPolicy()
		if err != nil || sandbox.ReadAllowed(policy, private) == nil {
			t.Fatalf("read policy after removal = %+v, %v; want the private path denied", policy, err)
		}
		for _, tool := range registry.All() {
			cfg := SandboxDetails(tool).Config
			if viewer, ok := unwrapTool(tool).(*viewImageTool); ok {
				live, _, err := viewer.registry.SandboxReadPolicy()
				if err != nil {
					t.Fatal(err)
				}
				cfg = &live
			}
			if cfg == nil || sandbox.ReadAllowed(*cfg, private) == nil || slices.Contains(cfg.PassEnv, "NPM_TOKEN") {
				t.Fatalf("%s kept the removed layer: %+v", tool.GetName(), cfg)
			}
		}
	}
	if sandbox.ReadAllowed(*SandboxDetails(running).Config, private) != nil {
		t.Fatal("a running call's sandbox changed under it")
	}

	// Closing and reusing an intermediate registry must neither detach its
	// surviving descendants nor leave newly loaded tools out of later changes.
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := child.LoadToolAuto("bash"); err != nil {
		t.Fatal(err)
	}
	if _, err := parent.AppendBaseReadPaths(private); err != nil {
		t.Fatal(err)
	}
	for _, registry := range registries {
		for _, tool := range registry.All() {
			cfg := SandboxDetails(tool).Config
			if viewer, ok := unwrapTool(tool).(*viewImageTool); ok {
				live, _, err := viewer.registry.SandboxReadPolicy()
				if err != nil {
					t.Fatal(err)
				}
				cfg = &live
			}
			if cfg == nil || sandbox.ReadAllowed(*cfg, private) != nil {
				t.Fatalf("%s missed the added read after Close: %+v", tool.GetName(), cfg)
			}
		}
	}
}

func TestSandboxPolicyRollsBackWhenADerivedToolCannotRebuild(t *testing.T) {
	skipIfWindows(t)
	reject := false
	factory := func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		if reject && cfg.Env["DERIVED_TOOL"] == "yes" {
			return nil, errors.New("derived tool refused")
		}
		return &mockSandbox{}, nil
	}
	parent := NewToolRegistry(nil, WithNativeTools(), WithSandboxFactory(factory, sandbox.Config{}), WithSandboxLayer("profile", SandboxLayer{Config: sandbox.Config{PassEnv: []string{"NPM_TOKEN"}}}))
	t.Cleanup(func() { _ = parent.Close() })
	child := parent.Derive()
	t.Cleanup(func() { _ = child.Close() })
	if _, err := parent.LoadToolAuto("bash"); err != nil {
		t.Fatal(err)
	}
	script := createTestScript(t, t.TempDir())
	data, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"title": "test-tool",`, `"title": "test-tool", "sandbox": {"env": {"DERIVED_TOOL": "yes"}},`, 1))
	if err := os.WriteFile(script, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := child.LoadShellTool(script); err != nil {
		t.Fatal(err)
	}
	before, _ := parent.Get("bash")
	childBefore, _ := child.Get("test-tool__test-tool")
	reject = true
	if _, err := parent.SetSandboxLayer("profile", nil); err == nil || !strings.Contains(err.Error(), "derived tool refused") {
		t.Fatalf("remove = %v, want the derived tool's refusal", err)
	}
	if after, _ := parent.Get("bash"); after != before {
		t.Fatal("failed rebuild replaced the parent's bash")
	}
	if after, _ := child.Get("test-tool__test-tool"); after != childBefore {
		t.Fatal("failed rebuild replaced the child's shell tool")
	}
	if cfg, _, err := parent.SandboxReadPolicy(); err != nil || !slices.Contains(cfg.PassEnv, "NPM_TOKEN") {
		t.Fatalf("failed rebuild changed the policy: %+v, %v", cfg, err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := parent.SetSandboxLayer("profile", nil); err != nil {
		t.Fatalf("closed child still prevented removal: %v", err)
	}
}
