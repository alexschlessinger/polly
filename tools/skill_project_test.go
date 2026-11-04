package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func TestSkillActivationHonorsCurrentReadPolicy(t *testing.T) {
	root := t.TempDir()
	createSkillWithScript(t, root, "helper")
	catalog := discoverSkills(t, root)
	manifest := filepath.Join(root, "helper", "SKILL.md")
	for _, denied := range []string{root, manifest} {
		registry := NewToolRegistry(nil, WithNativeTools(), WithSandboxFactory(mockSandboxFactory(&mockSandbox{}), sandbox.Config{
			DenyPaths: []string{denied}, DenyHostTemp: true,
		}))
		t.Cleanup(func() { registry.Close() })
		tool := NewSkillActivateTool(catalog, registry)
		text, err := tool.Execute(context.Background(), map[string]any{"name": "helper"})
		if err == nil || !strings.Contains(err.Error(), "blocked from reads") || text != "" {
			t.Fatalf("denied activation returned cached instructions/assets: %q, %v", text, err)
		}
		if len(tool.ActivatedSkills()) != 0 {
			t.Fatal("denied skill was marked active")
		}
	}
}

func TestNativeOpenToolsSkipsUnavailableProjectSkills(t *testing.T) {
	for _, unavailable := range []string{"missing directory", "missing manifest", "denied directory", "denied manifest"} {
		t.Run(unavailable, func(t *testing.T) {
			parent, child, external := t.TempDir(), t.TempDir(), t.TempDir()
			parentSkills := filepath.Join(parent, ".agents", "skills")
			childSkills := filepath.Join(child, ".agents", "skills")
			createSkillWithScript(t, parentSkills, "unavailable")
			createSkillWithScript(t, parentSkills, "available")
			createSkillWithScript(t, childSkills, "available")
			createSkillWithScript(t, external, "external-helper")
			denied := []string{parent}
			missingRoot := filepath.Join(childSkills, "unavailable")
			if unavailable != "missing directory" {
				createSkillWithScript(t, childSkills, "unavailable")
			}
			switch unavailable {
			case "missing manifest":
				if err := os.Remove(filepath.Join(missingRoot, "SKILL.md")); err != nil {
					t.Fatal(err)
				}
			case "denied directory":
				denied = append(denied, missingRoot)
			case "denied manifest":
				denied = append(denied, filepath.Join(missingRoot, "SKILL.md"))
			}
			catalog, err := skills.Discover([]string{parentSkills, external})
			if err != nil {
				t.Fatal(err)
			}
			source := NewToolRegistry(nil, WithNativeTools(), WithSandboxFactory(mockSandboxFactory(&mockSandbox{}), sandbox.Config{
				ReadPaths: []string{parent, external}, DenyHostTemp: true,
			}))
			defer source.Close()
			if _, err := NewSkillRuntime(catalog, source); err != nil {
				t.Fatal(err)
			}
			binding, err := NativeOpenTools(source)(context.Background(), ToolScope{
				Root: child, Grant: ExecutionGrant{ReadOnly: true, SourceRoot: parent, DeniedReads: denied},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer binding.Close()
			activate, ok := binding.Registry.Get("activate_skill")
			if !ok {
				t.Fatalf("one unavailable project skill removed all skill tools: %v", binding.Omitted)
			}
			if _, ok := binding.Registry.Get("read_skill_file"); !ok {
				t.Fatal("unavailable project skill removed the skill reader")
			}
			for _, name := range []string{"available", "external-helper"} {
				if _, err := activate.Execute(context.Background(), map[string]any{"name": name}); err != nil {
					t.Fatalf("available skill %s was lost: %v", name, err)
				}
			}
			if text, err := activate.Execute(context.Background(), map[string]any{"name": "unavailable"}); err == nil || text != "" {
				t.Fatalf("unavailable skill fell back to source assets: %q, %v", text, err)
			}
			cfg, active, err := binding.Registry.SandboxReadPolicy()
			if err != nil || !active {
				t.Fatalf("child policy = %v, %v", active, err)
			}
			if err := sandbox.ReadAllowed(cfg, filepath.Join(parentSkills, "unavailable", "SKILL.md")); err == nil {
				t.Fatal("parent skill was exposed to recover missing child assets")
			}
		})
	}
}

func TestRebindSkillCatalogStillRejectsMalformedSkills(t *testing.T) {
	parent, child := t.TempDir(), t.TempDir()
	createSkillWithScript(t, parent, "helper")
	createSkillWithScript(t, child, "helper")
	if err := os.WriteFile(filepath.Join(child, "helper", "SKILL.md"), []byte("not a skill"), 0644); err != nil {
		t.Fatal(err)
	}
	catalog := discoverSkills(t, parent)
	registry := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
	defer registry.Close()
	if _, err := rebindSkillCatalog(catalog, registry, ExecutionContext{SourceRoot: parent, Root: child}); err == nil || !strings.Contains(err.Error(), "missing YAML frontmatter") {
		t.Fatalf("malformed rebound manifest error = %v", err)
	}
}

func TestNativeOpenToolsRebindsProjectSkills(t *testing.T) {
	parent, child, external := t.TempDir(), t.TempDir(), t.TempDir()
	for root, marker := range map[string]string{parent: "PARENT", child: "CHILD"} {
		skillRoot := filepath.Join(root, ".agents", "skills")
		createSkillWithScript(t, skillRoot, "helper")
		for rel, text := range map[string]string{
			"SKILL.md":            "---\nname: helper\ndescription: project helper\n---\n" + marker + " {baseDir}",
			"references/guide.md": marker + " reference",
			"scripts/helper.sh":   "#!/bin/sh\n# " + marker,
		} {
			if err := os.WriteFile(filepath.Join(skillRoot, "helper", rel), []byte(text), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	createSkillWithScript(t, external, "external-helper")
	catalog, err := skills.Discover([]string{filepath.Join(parent, ".agents", "skills"), external})
	if err != nil {
		t.Fatal(err)
	}
	source := NewToolRegistry(nil, WithNativeTools(), WithSandboxFactory(mockSandboxFactory(&mockSandbox{}), sandbox.Config{
		ReadPaths: []string{parent, external}, DenyHostTemp: true,
	}))
	defer source.Close()
	if _, err := NewSkillRuntime(catalog, source); err != nil {
		t.Fatal(err)
	}
	binding, err := NativeOpenTools(source)(context.Background(), ToolScope{
		Root:  child,
		Grant: ExecutionGrant{ReadOnly: true, SourceRoot: parent, DeniedReads: []string{parent}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer binding.Close()
	activate, ok := binding.Registry.Get("activate_skill")
	if !ok {
		t.Fatalf("project skill tools were not rebound: omitted %v", binding.Omitted)
	}
	text, err := activate.Execute(context.Background(), map[string]any{"name": "helper"})
	if err != nil {
		t.Fatal(err)
	}
	childRoot, err := filepath.EvalSymlinks(filepath.Join(child, ".agents", "skills", "helper"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "CHILD "+childRoot) || !strings.Contains(text, filepath.Join(childRoot, "scripts", "helper.sh")) || strings.Contains(text, "PARENT") {
		t.Fatalf("activation did not use child instructions, baseDir and script paths: %s", text)
	}
	read, ok := binding.Registry.Get("read_skill_file")
	if !ok {
		t.Fatal("missing rebound skill reader")
	}
	for _, rel := range []string{"references/guide.md", "scripts/helper.sh"} {
		text, err := read.Execute(context.Background(), map[string]any{"skill": "helper", "path": rel})
		if err != nil || !strings.Contains(text, "CHILD") || strings.Contains(text, "PARENT") {
			t.Fatalf("rebound %s = %q, %v", rel, text, err)
		}
	}
	if _, err := activate.Execute(context.Background(), map[string]any{"name": "external-helper"}); err != nil {
		t.Fatalf("external skill was lost: %v", err)
	}
	cfg, active, err := binding.Registry.SandboxReadPolicy()
	if err != nil || !active {
		t.Fatalf("child read policy = %v, %v", active, err)
	}
	if err := sandbox.ReadAllowed(cfg, filepath.Join(parent, ".agents", "skills", "helper", "SKILL.md")); err == nil {
		t.Fatal("child binding reopened parent skill assets")
	}
}
