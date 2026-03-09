package tools

import (
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/skills"
)

func TestNewSkillRuntimeRegistersBuiltins(t *testing.T) {
	root := t.TempDir()
	createSkillWithScript(t, root, "runtime-skill")

	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	registry := NewToolRegistry(nil)
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

func TestSkillRuntimeActivateCommitsTools(t *testing.T) {
	root := t.TempDir()
	createSkillWithScript(t, root, "runtime-skill")

	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	registry := NewToolRegistry(nil)
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

	registry := NewToolRegistry(nil)
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

	registry := NewToolRegistry(nil)
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
