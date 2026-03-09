package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/skills"
)

func TestSkillActivateToolLoadsScripts(t *testing.T) {
	root := t.TempDir()
	createSkillWithScript(t, root, "shell-helper")

	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	registry := NewToolRegistry(nil)
	tool := NewSkillActivateTool(catalog, registry)

	result, err := tool.Execute(context.Background(), map[string]any{"name": "shell-helper"})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(result, "Skill activated: shell-helper") {
		t.Fatalf("Execute() result missing activation summary: %s", result)
	}
	expectedPath := filepath.Join(root, "shell-helper", "scripts", "helper.sh")
	if !strings.Contains(result, expectedPath) {
		t.Fatalf("Execute() result missing script path %q: %s", expectedPath, result)
	}
	if !strings.Contains(result, "Available scripts") {
		t.Fatalf("Execute() result missing 'Available scripts' header: %s", result)
	}

	// Scripts should NOT be registered as tools (bash passthrough instead)
	registry.CommitPendingChanges()
	for _, tool := range registry.All() {
		if strings.HasPrefix(tool.GetName(), "shell-helper__") {
			t.Fatalf("script should not be registered as a tool: %s", tool.GetName())
		}
	}

	result, err = tool.Execute(context.Background(), map[string]any{"name": "shell-helper"})
	if err != nil {
		t.Fatalf("Execute() second activation error = %v", err)
	}
	if !strings.Contains(result, "already active for this run") {
		t.Fatalf("second activation should report already active: %s", result)
	}
}

func TestSkillActivateToolDoesNotLeakPartialActivationOnError(t *testing.T) {
	root := t.TempDir()
	createSkillWithScript(t, root, "rollback-skill")

	outsideDir := t.TempDir()
	outsideScript := filepath.Join(outsideDir, "outside.sh")
	if err := os.WriteFile(outsideScript, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatalf("WriteFile(outside script) error = %v", err)
	}

	leakPath := filepath.Join(root, "rollback-skill", "scripts", "leaky.sh")
	if err := os.Symlink(outsideScript, leakPath); err != nil {
		t.Skipf("Symlink() unavailable: %v", err)
	}

	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	registry := NewToolRegistry(nil)
	tool := NewSkillActivateTool(catalog, registry)

	if _, err := tool.Execute(context.Background(), map[string]any{"name": "rollback-skill"}); err == nil || !strings.Contains(err.Error(), "escapes the skill root") {
		t.Fatalf("Execute() error = %v, want symlink escape error", err)
	}

	if err := os.Remove(leakPath); err != nil {
		t.Fatalf("Remove(leaky.sh) error = %v", err)
	}

	result, err := tool.Execute(context.Background(), map[string]any{"name": "rollback-skill"})
	if err != nil {
		t.Fatalf("Execute() retry error = %v", err)
	}
	if strings.Contains(result, "already active for this run") {
		t.Fatalf("failed activation should not mark skill active: %s", result)
	}
}

func TestSkillActivateToolEnforcesAllowedToolsAfterCommit(t *testing.T) {
	root := t.TempDir()
	createSkillWithAllowedTools(t, root, "policy-skill", "filesystem__*")

	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	registry := NewToolRegistry([]Tool{
		&testTool{name: "activate_skill"},
		&testTool{name: "read_skill_file"},
		&testTool{name: "filesystem__read_file"},
		&testTool{name: "other__run"},
	})
	registry.MarkAlwaysAllowed("activate_skill")
	registry.MarkAlwaysAllowed("read_skill_file")

	tool := NewSkillActivateTool(catalog, registry)
	if _, err := tool.Execute(context.Background(), map[string]any{"name": "policy-skill"}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if _, ok := registry.Get("other__run"); !ok {
		t.Fatal("tools should remain visible before commit")
	}

	registry.CommitPendingChanges()

	if _, ok := registry.Get("filesystem__read_file"); !ok {
		t.Fatal("filesystem tool should remain allowed")
	}
	if _, ok := registry.Get("other__run"); ok {
		t.Fatal("other__run should be blocked by allowed-tools policy")
	}
	if _, ok := registry.Get("activate_skill"); !ok {
		t.Fatal("activate_skill should remain always allowed")
	}
}

func TestSkillAllowedToolsPolicyBlocksBash(t *testing.T) {
	root := t.TempDir()
	createSkillWithAllowedTools(t, root, "restricted-skill", "read_skill_file")

	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	registry := NewToolRegistry([]Tool{
		&testTool{name: "activate_skill"},
		&testTool{name: "read_skill_file"},
	})
	registry.Register(NewBashTool(""))
	registry.MarkAlwaysAllowed("activate_skill")
	registry.MarkAlwaysAllowed("read_skill_file")

	if _, ok := registry.Get("bash"); !ok {
		t.Fatal("bash should be available before policy activation")
	}

	tool := NewSkillActivateTool(catalog, registry)
	if _, err := tool.Execute(context.Background(), map[string]any{"name": "restricted-skill"}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	registry.CommitPendingChanges()

	if _, ok := registry.Get("bash"); ok {
		t.Fatal("bash should be blocked when not in allowed-tools")
	}
	if _, ok := registry.Get("read_skill_file"); !ok {
		t.Fatal("read_skill_file should remain allowed")
	}
}

func TestStageMCPServerWithNamespacePrefix(t *testing.T) {
	checkUvxAvailable(t)

	configPath := createMCPTestConfig(t, "time", "uvx", []string{"mcp-server-time"})
	registry := NewToolRegistry(nil)

	result, err := registry.stageMCPServerWithNamespacePrefix(configPath, "clock-skill")
	if err != nil {
		t.Fatalf("stageMCPServerWithNamespacePrefix() error = %v", err)
	}
	if len(result.Servers) != 1 {
		t.Fatalf("len(result.Servers) = %d, want 1", len(result.Servers))
	}
	if result.Servers[0].Name != "clock-skill-time" {
		t.Fatalf("server namespace = %q, want %q", result.Servers[0].Name, "clock-skill-time")
	}
	if len(registry.All()) != 0 {
		t.Fatal("staged MCP tools should not be visible before commit")
	}

	registry.CommitPendingChanges()

	foundPrefixedTool := false
	for _, tool := range registry.All() {
		if strings.HasPrefix(tool.GetName(), "clock-skill-time__") {
			foundPrefixedTool = true
			break
		}
	}
	if !foundPrefixedTool {
		t.Fatal("expected committed MCP tools to use the skill-prefixed namespace")
	}
}

func TestSkillReadFileToolReadsRelativeFile(t *testing.T) {
	root := t.TempDir()
	createSkillWithScript(t, root, "doc-reader")

	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	tool := NewSkillReadFileTool(catalog)
	result, err := tool.Execute(context.Background(), map[string]any{
		"skill": "doc-reader",
		"path":  "references/guide.md",
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(result, "reference text") {
		t.Fatalf("Execute() result = %q, want file contents", result)
	}
}

func createSkillWithScript(t *testing.T, root, name string) {
	t.Helper()

	skillDir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(skillDir, "references"), 0755); err != nil {
		t.Fatalf("MkdirAll(references) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(skillDir, "scripts"), 0755); err != nil {
		t.Fatalf("MkdirAll(scripts) error = %v", err)
	}

	skillFile := `---
name: ` + name + `
description: test skill
---
Use the helper script when needed.
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillFile), 0644); err != nil {
		t.Fatalf("WriteFile(SKILL.md) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "references", "guide.md"), []byte("reference text"), 0644); err != nil {
		t.Fatalf("WriteFile(reference) error = %v", err)
	}

	script := `#!/bin/sh
if [ "$1" = "--schema" ]; then
  printf '%s\n' '{"title":"say","description":"Say a canned message","type":"object","properties":{"text":{"type":"string"}},"required":["text"]}'
  exit 0
fi
if [ "$1" = "--execute" ]; then
  printf '%s\n' 'done'
  exit 0
fi
printf '%s\n' 'unexpected'
exit 1
`
	scriptPath := filepath.Join(skillDir, "scripts", "helper.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("WriteFile(script) error = %v", err)
	}
}

func createSkillWithBareScript(t *testing.T, root, name string) {
	t.Helper()

	skillDir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(skillDir, "references"), 0755); err != nil {
		t.Fatalf("MkdirAll(references) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(skillDir, "scripts"), 0755); err != nil {
		t.Fatalf("MkdirAll(scripts) error = %v", err)
	}

	skillFile := "---\nname: " + name + "\ndescription: test skill\n---\nRun scripts/helper.sh to say hello.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillFile), 0644); err != nil {
		t.Fatalf("WriteFile(SKILL.md) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "references", "guide.md"), []byte("reference text"), 0644); err != nil {
		t.Fatalf("WriteFile(reference) error = %v", err)
	}

	script := "#!/bin/sh\necho \"hello $1\"\n"
	scriptPath := filepath.Join(skillDir, "scripts", "helper.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("WriteFile(script) error = %v", err)
	}
}

func TestSkillActivateToolListsScriptsInResponse(t *testing.T) {
	root := t.TempDir()
	createSkillWithBareScript(t, root, "bare-skill")

	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	registry := NewToolRegistry(nil)
	tool := NewSkillActivateTool(catalog, registry)

	result, err := tool.Execute(context.Background(), map[string]any{"name": "bare-skill"})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	expectedPath := filepath.Join(root, "bare-skill", "scripts", "helper.sh")
	if !strings.Contains(result, expectedPath) {
		t.Fatalf("activation response missing script path %q:\n%s", expectedPath, result)
	}
	if !strings.Contains(result, "Available scripts") {
		t.Fatalf("activation response missing 'Available scripts' header:\n%s", result)
	}

	// Should NOT register any tools
	registry.CommitPendingChanges()
	for _, tool := range registry.All() {
		if strings.HasPrefix(tool.GetName(), "bare-skill__") {
			t.Fatalf("bare script should not be registered as a tool: %s", tool.GetName())
		}
	}
}

func TestSkillActivateToolListsArbitraryFiles(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "doc-skill")
	for _, dir := range []string{
		skillDir,
		filepath.Join(skillDir, "python", "api"),
		filepath.Join(skillDir, "shared"),
		filepath.Join(skillDir, "scripts"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", dir, err)
		}
	}

	skillFile := "---\nname: doc-skill\ndescription: test skill with arbitrary files\n---\nRead python/api/README.md for details.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillFile), 0644); err != nil {
		t.Fatalf("WriteFile(SKILL.md) error = %v", err)
	}
	// Root-level doc
	if err := os.WriteFile(filepath.Join(skillDir, "forms.md"), []byte("forms guide"), 0644); err != nil {
		t.Fatalf("WriteFile(forms.md) error = %v", err)
	}
	// Non-standard subdirectory
	if err := os.WriteFile(filepath.Join(skillDir, "python", "api", "README.md"), []byte("python api docs"), 0644); err != nil {
		t.Fatalf("WriteFile(python readme) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "shared", "concepts.md"), []byte("shared concepts"), 0644); err != nil {
		t.Fatalf("WriteFile(shared concepts) error = %v", err)
	}
	// Script (should be listed separately, not in skill files)
	if err := os.WriteFile(filepath.Join(skillDir, "scripts", "run.sh"), []byte("#!/bin/sh\necho hi\n"), 0755); err != nil {
		t.Fatalf("WriteFile(script) error = %v", err)
	}
	// LICENSE.txt and SKILL.md should be excluded
	if err := os.WriteFile(filepath.Join(skillDir, "LICENSE.txt"), []byte("MIT"), 0644); err != nil {
		t.Fatalf("WriteFile(LICENSE.txt) error = %v", err)
	}

	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	registry := NewToolRegistry(nil)
	tool := NewSkillActivateTool(catalog, registry)

	result, err := tool.Execute(context.Background(), map[string]any{"name": "doc-skill"})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// Arbitrary files should be listed
	for _, want := range []string{"forms.md", "python/api/README.md", "shared/concepts.md"} {
		if !strings.Contains(result, want) {
			t.Errorf("activation result missing %q:\n%s", want, result)
		}
	}
	if !strings.Contains(result, "Skill files") {
		t.Errorf("activation result missing 'Skill files' header:\n%s", result)
	}

	// Scripts listed separately
	if !strings.Contains(result, "Available scripts") {
		t.Errorf("activation result missing 'Available scripts' header:\n%s", result)
	}

	// Meta files excluded
	for _, excluded := range []string{"SKILL.md", "LICENSE.txt"} {
		// Check it's not in the skill files section (it may appear in the instructions)
		idx := strings.Index(result, "Skill files")
		if idx >= 0 && strings.Contains(result[idx:], excluded) {
			t.Errorf("activation result should not list %q in skill files:\n%s", excluded, result)
		}
	}
}

func TestSkillActivateStandardSkills(t *testing.T) {
	const skillsRepo = "/tmp/anthropic-skills/skills"
	if _, err := os.Stat(skillsRepo); err != nil {
		t.Skipf("standard skills repo not available at %s: %v", skillsRepo, err)
	}

	// Discover all standard skills at once.
	catalog, err := skills.Discover([]string{skillsRepo})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	allSkills := catalog.List()
	if len(allSkills) == 0 {
		t.Fatal("expected at least one standard skill to be discovered")
	}
	t.Logf("discovered %d standard skills", len(allSkills))

	// Activate every skill and verify invariants.
	for _, skill := range allSkills {
		t.Run(skill.Name, func(t *testing.T) {
			registry := NewToolRegistry(nil)
			activateTool := NewSkillActivateTool(catalog, registry)
			readTool := NewSkillReadFileTool(catalog)

			result, err := activateTool.Execute(context.Background(), map[string]any{"name": skill.Name})
			if err != nil {
				t.Fatalf("activate error: %v", err)
			}

			// Must contain activation header and instructions.
			if !strings.Contains(result, "Skill activated: "+skill.Name) {
				t.Errorf("missing activation header")
			}
			if !strings.Contains(result, "Instructions:") {
				t.Errorf("missing Instructions section")
			}

			// SKILL.md and LICENSE.txt must never appear in the file listing section.
			if idx := strings.Index(result, "Skill files"); idx >= 0 {
				fileSection := result[idx:]
				for _, excluded := range []string{"SKILL.md", "LICENSE.txt"} {
					if strings.Contains(fileSection, excluded) {
						t.Errorf("%q leaked into skill files section", excluded)
					}
				}
			}

			// Scripts in scripts/ must not appear in the skill files section.
			scriptFiles, _ := skill.ListFiles("scripts")
			if idx := strings.Index(result, "Skill files"); idx >= 0 && len(scriptFiles) > 0 {
				fileSection := result[idx:]
				for _, sf := range scriptFiles {
					if strings.Contains(fileSection, sf) {
						t.Errorf("script %q leaked into skill files section", sf)
					}
				}
			}

			// Extract listed items from a section (lines starting with "- " after the header,
			// stopping at the next non-list line or end of output).
			extractSection := func(header string) []string {
				idx := strings.Index(result, header)
				if idx < 0 {
					return nil
				}
				var items []string
				inSection := false
				for _, line := range strings.Split(result[idx:], "\n") {
					if !inSection {
						inSection = true // skip header line
						continue
					}
					if !strings.HasPrefix(line, "- ") {
						break
					}
					items = append(items, strings.TrimPrefix(line, "- "))
				}
				return items
			}

			// Every file listed under "Skill files" must be readable via read_skill_file.
			for _, relPath := range extractSection("Skill files") {
				readResult, err := readTool.Execute(context.Background(), map[string]any{
					"skill": skill.Name,
					"path":  relPath,
				})
				if err != nil {
					t.Errorf("read_skill_file(%q) error: %v", relPath, err)
				}
				if readResult == "" {
					t.Errorf("read_skill_file(%q) returned empty", relPath)
				}
			}

			// Every file listed under "Available scripts" must be readable via read_skill_file.
			for _, absPath := range extractSection("Available scripts") {
				relPath, err := filepath.Rel(skill.RootDir, absPath)
				if err != nil {
					t.Errorf("filepath.Rel(%q, %q) error: %v", skill.RootDir, absPath, err)
					continue
				}
				readResult, err := readTool.Execute(context.Background(), map[string]any{
					"skill": skill.Name,
					"path":  relPath,
				})
				if err != nil {
					t.Errorf("read_skill_file(%q) error: %v", relPath, err)
				}
				if readResult == "" {
					t.Errorf("read_skill_file(%q) returned empty", relPath)
				}
			}

			// Re-activation should report already active.
			result2, err := activateTool.Execute(context.Background(), map[string]any{"name": skill.Name})
			if err != nil {
				t.Fatalf("re-activate error: %v", err)
			}
			if !strings.Contains(result2, "already active for this run") {
				t.Errorf("re-activation should say already active")
			}
		})
	}
}

func TestBashAvailabilityWithStandardSkills(t *testing.T) {
	const skillsRepo = "/tmp/anthropic-skills/skills"
	if _, err := os.Stat(skillsRepo); err != nil {
		t.Skipf("standard skills repo not available at %s: %v", skillsRepo, err)
	}

	// Mix standard skills (no allowed-tools) with a restrictive skill.
	restrictedRoot := t.TempDir()
	createSkillWithAllowedTools(t, restrictedRoot, "locked-skill", "read_skill_file")

	t.Run("no-policy-skill-keeps-bash", func(t *testing.T) {
		catalog, err := skills.Discover([]string{skillsRepo})
		if err != nil {
			t.Fatalf("Discover() error = %v", err)
		}

		registry := NewToolRegistry(nil)
		registry.Register(NewBashTool(""))

		activateTool := NewSkillActivateTool(catalog, registry)
		if _, err := activateTool.Execute(context.Background(), map[string]any{"name": "pdf"}); err != nil {
			t.Fatalf("activate pdf error = %v", err)
		}
		registry.CommitPendingChanges()

		if _, ok := registry.Get("bash"); !ok {
			t.Fatal("bash should remain available after activating pdf (no allowed-tools policy)")
		}
	})

	t.Run("restrictive-policy-blocks-bash", func(t *testing.T) {
		catalog, err := skills.Discover([]string{restrictedRoot})
		if err != nil {
			t.Fatalf("Discover() error = %v", err)
		}

		registry := NewToolRegistry(nil)
		registry.Register(NewBashTool(""))
		registry.MarkAlwaysAllowed("activate_skill")
		registry.MarkAlwaysAllowed("read_skill_file")

		activateTool := NewSkillActivateTool(catalog, registry)
		if _, err := activateTool.Execute(context.Background(), map[string]any{"name": "locked-skill"}); err != nil {
			t.Fatalf("activate locked-skill error = %v", err)
		}
		registry.CommitPendingChanges()

		if _, ok := registry.Get("bash"); ok {
			t.Fatal("bash should be blocked after activating skill with allowed-tools: read_skill_file")
		}
	})

	t.Run("permissive-then-restrictive", func(t *testing.T) {
		catalog, err := skills.Discover([]string{skillsRepo, restrictedRoot})
		if err != nil {
			t.Fatalf("Discover() error = %v", err)
		}

		registry := NewToolRegistry(nil)
		registry.Register(NewBashTool(""))
		registry.MarkAlwaysAllowed("activate_skill")
		registry.MarkAlwaysAllowed("read_skill_file")

		activateTool := NewSkillActivateTool(catalog, registry)

		// Activate pdf first (no policy)
		if _, err := activateTool.Execute(context.Background(), map[string]any{"name": "pdf"}); err != nil {
			t.Fatalf("activate pdf error = %v", err)
		}
		registry.CommitPendingChanges()

		if _, ok := registry.Get("bash"); !ok {
			t.Fatal("bash should be available after pdf activation (no policy)")
		}

		// Now activate a restrictive skill — policy activates, bash not in patterns
		if _, err := activateTool.Execute(context.Background(), map[string]any{"name": "locked-skill"}); err != nil {
			t.Fatalf("activate locked-skill error = %v", err)
		}
		registry.CommitPendingChanges()

		if _, ok := registry.Get("bash"); ok {
			t.Fatal("bash should be blocked after restrictive skill activates policy")
		}
	})
}

func createSkillWithAllowedTools(t *testing.T, root, name, allowed string) {
	t.Helper()

	skillDir := filepath.Join(root, name)
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	skillFile := `---
name: ` + name + `
description: policy test skill
allowed-tools: ` + allowed + `
---
Use approved tools only.
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillFile), 0644); err != nil {
		t.Fatalf("WriteFile(SKILL.md) error = %v", err)
	}
}
