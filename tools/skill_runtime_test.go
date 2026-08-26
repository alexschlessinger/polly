package tools

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func TestNewSkillRuntimeRegistersBuiltins(t *testing.T) {
	root := t.TempDir()
	createSkillWithScript(t, root, "runtime-skill")

	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	registry := NewToolRegistry(nil, WithUnsafeNoSandbox())
	runtime, err := NewSkillRuntime(catalog, registry)
	if err != nil {
		t.Fatalf("NewSkillRuntime() error = %v", err)
	}
	if !runtime.Enabled() {
		t.Fatal("runtime should be enabled")
	}
	if _, ok := registry.Get("activate_skill"); !ok {
		t.Fatal("expected activate_skill to be registered")
	}
	if _, ok := registry.Get("read_skill_file"); !ok {
		t.Fatal("expected read_skill_file to be registered")
	}
}

func TestNewSkillRuntimeSandboxFailureFailsClosed(t *testing.T) {
	root := t.TempDir()
	createSkillWithScript(t, root, "runtime-skill")

	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	registry := NewToolRegistry(nil, WithSandboxFactory(failingSandboxFactory(), sandbox.Config{}))
	if _, err := NewSkillRuntime(catalog, registry); err == nil {
		t.Fatal("expected NewSkillRuntime to fail when the skill bash sandbox can't be constructed")
	}
	for _, name := range []string{"activate_skill", "read_skill_file", "bash"} {
		if _, ok := registry.Get(name); ok {
			t.Fatalf("%s registered after NewSkillRuntime failed", name)
		}
	}
}

func TestNewSkillRuntimeFailurePreservesCollidingTools(t *testing.T) {
	root := t.TempDir()
	createSkillWithScript(t, root, "runtime-skill")
	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatal(err)
	}

	activate := &testTool{name: "activate_skill"}
	readFile := &testTool{name: "read_skill_file"}
	registry := NewToolRegistry([]Tool{activate, readFile},
		WithSandboxFactory(failingSandboxFactory(), sandbox.Config{}))
	if _, err := NewSkillRuntime(catalog, registry); err == nil {
		t.Fatal("expected NewSkillRuntime to fail")
	}
	for name, want := range map[string]Tool{
		"activate_skill":  activate,
		"read_skill_file": readFile,
	} {
		got, ok := registry.registeredTool(name)
		if !ok || got != want {
			t.Fatalf("registeredTool(%q) = %v, %v; want original %v", name, got, ok, want)
		}
	}
	if _, ok := registry.registeredTool("bash"); ok {
		t.Fatal("bash registered after NewSkillRuntime failed")
	}
}

func TestNewSkillRuntimeRequiresExplicitProcessPolicy(t *testing.T) {
	root := t.TempDir()
	createSkillWithScript(t, root, "runtime-skill")
	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatal(err)
	}

	registry := NewToolRegistry(nil)
	if _, err := NewSkillRuntime(catalog, registry); err == nil || !strings.Contains(err.Error(), "requires sandboxing") {
		t.Fatalf("NewSkillRuntime() error = %v, want secure-default refusal", err)
	}
	for _, name := range []string{"activate_skill", "read_skill_file", "bash"} {
		if _, ok := registry.Get(name); ok {
			t.Fatalf("%s registered after NewSkillRuntime failed", name)
		}
	}
}

func TestNewSkillRuntimeInheritsBaseSandboxPolicy(t *testing.T) {
	skipIfWindows(t)
	root := t.TempDir()
	createSkillWithScript(t, root, "runtime-skill")
	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatal(err)
	}

	base := sandbox.Config{
		DenyPaths: []string{"/private/project-secret"},
		DenyWrite: true,
	}
	var received sandbox.Config
	factory := func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		received = cfg
		return &mockSandbox{}, nil
	}
	registry := NewToolRegistry(nil, WithSandboxFactory(factory, base))
	if _, err := NewSkillRuntime(catalog, registry); err != nil {
		t.Fatal(err)
	}

	if received.AllowNetwork {
		t.Fatal("skill bash unexpectedly enabled network")
	}
	if !received.DenyWrite {
		t.Fatal("skill bash dropped base DenyWrite")
	}
	if len(received.DenyPaths) != 1 || received.DenyPaths[0] != base.DenyPaths[0] {
		t.Fatalf("skill bash DenyPaths = %v, want %v", received.DenyPaths, base.DenyPaths)
	}
	tool, ok := registry.Get("bash")
	if !ok {
		t.Fatal("skill bash was not registered")
	}
	info := SandboxDetails(tool)
	if !info.Active || info.Config == nil || info.Config.AllowNetwork || !info.Config.DenyWrite {
		t.Fatalf("skill bash sandbox details = %+v", info)
	}
}

func TestSkillRuntimeReportsOnlyCurrentKnownBashLimits(t *testing.T) {
	root := t.TempDir()
	createSkillWithScript(t, root, "runtime-skill")
	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatal(err)
	}

	writeDir := t.TempDir()
	registry := NewToolRegistry(nil, WithSandboxFactory(func(sandbox.Config) (sandbox.Sandbox, error) {
		return &mockSandbox{}, nil
	}, sandbox.Config{WritablePaths: []string{writeDir}}))
	runtime, err := NewSkillRuntime(catalog, registry)
	if err != nil {
		t.Fatal(err)
	}

	realWriteDir, err := filepath.EvalSymlinks(writeDir)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Activate("runtime-skill")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Sandbox: bash commands can only write to: "+realWriteDir) {
		t.Fatalf("sandboxed skill bash omitted effective writable path: %s", result)
	}

	registry.Register(newBashTool("").WithSandbox(&mockSandbox{}))
	result, err = runtime.Activate("runtime-skill")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result, "Sandbox: bash commands can only write to:") {
		t.Fatalf("active bash with unknown config reported invented write restrictions: %s", result)
	}

	registry.Register(NewUnsafeBashTool(""))
	result, err = runtime.Activate("runtime-skill")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result, "Sandbox: bash commands can only write to:") {
		t.Fatalf("replaced unsafe bash retained stale write restrictions: %s", result)
	}
}

func TestSkillRuntimeActivateCommitsTools(t *testing.T) {
	root := t.TempDir()
	createSkillWithScript(t, root, "runtime-skill")

	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	registry := NewToolRegistry(nil, WithUnsafeNoSandbox())
	runtime, err := NewSkillRuntime(catalog, registry)
	if err != nil {
		t.Fatalf("NewSkillRuntime() error = %v", err)
	}

	result, err := runtime.Activate("runtime-skill")
	if err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	if !strings.Contains(result, "Skill activated: runtime-skill") {
		t.Fatalf("Activate() result = %q, want activation summary", result)
	}
	if !strings.Contains(result, "Available scripts") {
		t.Fatalf("Activate() result missing 'Available scripts': %s", result)
	}
	if strings.Contains(result, "Sandbox: bash commands can only write to:") {
		t.Fatalf("unsandboxed skill bash reported nonexistent write restrictions: %s", result)
	}
	// Scripts are listed as paths, not registered as tools
	for _, tool := range registry.All() {
		if strings.HasPrefix(tool.GetName(), "runtime-skill__") {
			t.Fatalf("script should not be registered as a tool: %s", tool.GetName())
		}
	}
	if got := runtime.ActivatedSkills(); len(got) != 1 || got[0] != "runtime-skill" {
		t.Fatalf("ActivatedSkills() = %v, want [runtime-skill]", got)
	}
}

func TestSkillRuntimeRestoreCommitsTools(t *testing.T) {
	root := t.TempDir()
	createSkillWithScript(t, root, "runtime-skill")

	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	registry := NewToolRegistry(nil, WithUnsafeNoSandbox())
	runtime, err := NewSkillRuntime(catalog, registry)
	if err != nil {
		t.Fatalf("NewSkillRuntime() error = %v", err)
	}

	if err := runtime.Restore([]string{"runtime-skill"}); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	// Scripts are listed as paths for bash passthrough, not registered as tools
	for _, tool := range registry.All() {
		if strings.HasPrefix(tool.GetName(), "runtime-skill__") {
			t.Fatalf("script should not be registered as a tool: %s", tool.GetName())
		}
	}
	if got := runtime.ActivatedSkills(); len(got) != 1 || got[0] != "runtime-skill" {
		t.Fatalf("ActivatedSkills() = %v, want [runtime-skill]", got)
	}
}

func TestNewSkillRuntimeRegistersBash(t *testing.T) {
	root := t.TempDir()
	createSkillWithScript(t, root, "bash-skill")

	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	registry := NewToolRegistry(nil, WithUnsafeNoSandbox())
	_, err = NewSkillRuntime(catalog, registry)
	if err != nil {
		t.Fatalf("NewSkillRuntime() error = %v", err)
	}
	if _, ok := registry.Get("bash"); !ok {
		t.Fatal("expected bash tool to be auto-registered when skills are enabled")
	}
}

func TestNewSkillRuntimeNoBashWithoutSkills(t *testing.T) {
	registry := NewToolRegistry(nil)
	_, err := NewSkillRuntime(nil, registry)
	if err != nil {
		t.Fatalf("NewSkillRuntime() error = %v", err)
	}
	if _, ok := registry.Get("bash"); ok {
		t.Fatal("bash tool should not be registered when no skills are available")
	}
}
