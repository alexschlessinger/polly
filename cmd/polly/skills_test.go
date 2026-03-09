package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestLoadSkillCatalogSkipsDiscoveryWhenDisabled(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "formatter")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	content := `---
name: formatter
description: Formats generated text
---
Use formatting guidance.
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	result, err := loadSkillCatalog(&Config{
		Settings: Settings{SkillDirs: []string{root}},
		NoSkills: true,
	}, nil)
	if err != nil {
		t.Fatalf("loadSkillCatalog() error = %v", err)
	}
	if result.catalog != nil {
		t.Fatal("loadSkillCatalog() should skip discovery when skills are disabled")
	}
}

func TestHandleListSkillsReportsDisabled(t *testing.T) {
	root := t.TempDir()
	output := captureStdout(t, func() {
		if err := handleListSkills(&Config{
			Settings: Settings{SkillDirs: []string{root}},
			NoSkills: true,
		}); err != nil {
			t.Fatalf("handleListSkills() error = %v", err)
		}
	})

	if !strings.Contains(output, "Skills are disabled") {
		t.Fatalf("handleListSkills() output = %q, want disabled message", output)
	}
}

func TestPersistActiveSkillsStoresMetadata(t *testing.T) {
	root := t.TempDir()
	createSkillWithScript(t, root, "formatter")

	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	store := sessions.NewSyncMapSessionStore(nil)
	session, err := store.Get("test")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	registry := tools.NewToolRegistry(nil)
	skillRuntime, err := newSkillRuntime(catalog, registry)
	if err != nil {
		t.Fatalf("newSkillRuntime() error = %v", err)
	}

	if _, err := skillRuntime.Activate("formatter"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}

	if err := persistActiveSkills(session, skillRuntime, nil); err != nil {
		t.Fatalf("persistActiveSkills() error = %v", err)
	}

	metadata := session.GetMetadata()
	if len(metadata.ActiveSkills) != 1 || metadata.ActiveSkills[0] != "formatter" {
		t.Fatalf("ActiveSkills = %v, want [formatter]", metadata.ActiveSkills)
	}
}

func TestRestoreActiveSkillsReloadsSkillTools(t *testing.T) {
	root := t.TempDir()
	createSkillWithScript(t, root, "formatter")

	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	registry := tools.NewToolRegistry(nil)
	skillRuntime, err := newSkillRuntime(catalog, registry)
	if err != nil {
		t.Fatalf("newSkillRuntime() error = %v", err)
	}

	metadata := &sessions.Metadata{
		ActiveSkills: []string{"formatter"},
	}
	if err := restoreActiveSkills(metadata, skillRuntime); err != nil {
		t.Fatalf("restoreActiveSkills() error = %v", err)
	}

	// With bash passthrough, scripts are not registered as tools.
	// Verify the skill is marked activated instead.
	if got := skillRuntime.ActivatedSkills(); len(got) != 1 || got[0] != "formatter" {
		t.Fatalf("ActivatedSkills() = %v, want [formatter]", got)
	}
}

func TestLoadSkillCatalogWithSkillFlag(t *testing.T) {
	root := t.TempDir()
	createSkillWithScript(t, root, "my-skill")

	result, err := loadSkillCatalog(&Config{
		Skills: []string{filepath.Join(root, "my-skill")},
	}, nil)
	if err != nil {
		t.Fatalf("loadSkillCatalog() error = %v", err)
	}


	if result.catalog == nil {
		t.Fatal("expected catalog to be non-nil")
	}
	if _, ok := result.catalog.Get("my-skill"); !ok {
		t.Fatal("expected my-skill in catalog")
	}
	if len(result.autoActivate) != 1 || result.autoActivate[0] != "my-skill" {
		t.Fatalf("autoActivate = %v, want [my-skill]", result.autoActivate)
	}
}

func TestLoadSkillCatalogCombinesSkillDirAndSkill(t *testing.T) {
	dirRoot := t.TempDir()
	createSkillWithScript(t, dirRoot, "dir-skill")

	skillRoot := t.TempDir()
	createSkillWithScript(t, skillRoot, "flag-skill")

	result, err := loadSkillCatalog(&Config{
		Settings: Settings{SkillDirs: []string{dirRoot}},
		Skills:   []string{filepath.Join(skillRoot, "flag-skill")},
	}, nil)
	if err != nil {
		t.Fatalf("loadSkillCatalog() error = %v", err)
	}


	if result.catalog == nil {
		t.Fatal("expected catalog to be non-nil")
	}
	if _, ok := result.catalog.Get("dir-skill"); !ok {
		t.Fatal("expected dir-skill from --skilldir")
	}
	if _, ok := result.catalog.Get("flag-skill"); !ok {
		t.Fatal("expected flag-skill from --skill")
	}
	if len(result.autoActivate) != 1 || result.autoActivate[0] != "flag-skill" {
		t.Fatalf("only --skill skills should be auto-activated, got %v", result.autoActivate)
	}
}

func TestAutoActivateSkills(t *testing.T) {
	root := t.TempDir()
	createSkillWithScript(t, root, "auto-skill")

	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	registry := tools.NewToolRegistry(nil)
	runtime, err := newSkillRuntime(catalog, registry)
	if err != nil {
		t.Fatalf("newSkillRuntime() error = %v", err)
	}

	if err := autoActivateSkills([]string{"auto-skill"}, runtime); err != nil {
		t.Fatalf("autoActivateSkills() error = %v", err)
	}

	if got := runtime.ActivatedSkills(); len(got) != 1 || got[0] != "auto-skill" {
		t.Fatalf("ActivatedSkills() = %v, want [auto-skill]", got)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	originalStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe() error = %v", err)
	}

	os.Stdout = writer
	defer func() {
		os.Stdout = originalStdout
	}()

	fn()

	if err := writer.Close(); err != nil {
		t.Fatalf("Close(writer) error = %v", err)
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close(reader) error = %v", err)
	}

	return string(data)
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
