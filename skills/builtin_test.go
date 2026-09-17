package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadBuiltinCatalogMaterializesAndDiscovers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	catalog, err := LoadBuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if catalog == nil {
		t.Fatal("no builtin catalog")
	}
	skill, ok := catalog.Get("feature-workflow")
	if !ok {
		t.Fatalf("feature-workflow not discovered: %v", catalog.List())
	}
	wantRoot := filepath.Join(home, ".pollytool", "builtin-skills", "feature-workflow")
	if skill.RootDir != wantRoot {
		t.Fatalf("root = %s, want %s", skill.RootDir, wantRoot)
	}
	// The {baseDir} token resolves to the materialized root, and the workflow
	// scripts ship alongside SKILL.md.
	if !strings.Contains(skill.Instructions, wantRoot+"/feature-research.js") {
		t.Fatalf("instructions did not resolve {baseDir}: %s", skill.Instructions)
	}
	for _, file := range []string{"SKILL.md", "feature-research.js", "feature-implement.js"} {
		if _, err := os.Stat(filepath.Join(wantRoot, file)); err != nil {
			t.Fatalf("materialized file: %v", err)
		}
	}
}

// TestBuiltinSkillsReferenceNoRepository checks the module path, which is never
// legitimate in a builtin skill. Repository-local spellings such as
// docs/features are not checked: a workflow skill may legitimately name a
// docs/ features directory inside the user's own project.
func TestBuiltinSkillsReferenceNoRepository(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	catalog, err := LoadBuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if catalog.IsEmpty() {
		t.Fatal("no builtin skills discovered")
	}
	for _, skill := range catalog.List() {
		if strings.Contains(skill.Instructions, "github.com/alexschlessinger") {
			t.Fatalf("builtin skill %s references the repository module path", skill.Name)
		}
	}
}

func TestMaterializeBuiltinSyncsChangesAndPrunesStale(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir, err := MaterializeBuiltin()
	if err != nil {
		t.Fatal(err)
	}
	skillFile := filepath.Join(dir, "feature-workflow", "SKILL.md")
	original, err := os.ReadFile(skillFile)
	if err != nil {
		t.Fatal(err)
	}
	// User edits are restored and stale files and directories are pruned.
	if err := os.WriteFile(skillFile, []byte("clobbered"), 0o644); err != nil {
		t.Fatal(err)
	}
	staleFile := filepath.Join(dir, "feature-workflow", "stale.js")
	if err := os.WriteFile(staleFile, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	staleDir := filepath.Join(dir, "removed-skill")
	if err := os.MkdirAll(filepath.Join(staleDir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeBuiltin(); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(skillFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != string(original) {
		t.Fatal("edited builtin skill was not restored")
	}
	if _, err := os.Stat(staleFile); !os.IsNotExist(err) {
		t.Fatalf("stale file survived: %v", err)
	}
	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Fatalf("stale directory survived: %v", err)
	}
}

func TestCatalogMergeShadowsDuplicates(t *testing.T) {
	user := &Catalog{byName: make(map[string]*Skill)}
	builtin := &Catalog{byName: make(map[string]*Skill)}
	for _, catalog := range []*Catalog{user, builtin} {
		skill := &Skill{Name: "feature-workflow"}
		catalog.ordered = append(catalog.ordered, skill)
		catalog.byName[skill.Name] = skill
	}
	user.ordered[0].RootDir = "user"
	builtin.ordered[0].RootDir = "builtin"
	other := &Skill{Name: "other", RootDir: "builtin"}
	builtin.ordered = append(builtin.ordered, other)
	builtin.byName[other.Name] = other

	user.Merge(builtin)
	if user.Count() != 2 {
		t.Fatalf("merged count = %d", user.Count())
	}
	skill, _ := user.Get("feature-workflow")
	if skill.RootDir != "user" {
		t.Fatalf("user skill was shadowed by builtin: %s", skill.RootDir)
	}
	if _, ok := user.Get("other"); !ok {
		t.Fatal("non-duplicate builtin skill was not merged")
	}
	// Sorted by name after the merge.
	if user.List()[0].Name != "feature-workflow" || user.List()[1].Name != "other" {
		t.Fatalf("merged catalog not sorted: %v", user.List())
	}
}
