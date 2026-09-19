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

func TestBuiltinThemeSkillMaterializesAndDocumentsTheTool(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	catalog, err := LoadBuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	skill, ok := catalog.Get("theme-designer")
	if !ok {
		t.Fatalf("theme-designer not discovered: %v", catalog.List())
	}
	wantRoot := filepath.Join(home, ".pollytool", "builtin-skills", "theme-designer")
	if skill.RootDir != wantRoot {
		t.Fatalf("root = %s, want %s", skill.RootDir, wantRoot)
	}
	if skill.Name != "theme-designer" {
		t.Fatalf("frontmatter name = %q, want the directory name theme-designer", skill.Name)
	}
	if _, err := os.Stat(filepath.Join(wantRoot, "SKILL.md")); err != nil {
		t.Fatalf("materialized file: %v", err)
	}

	// validateFrontmatter enforces the name/directory match, but the rest of the
	// frontmatter contract is on us: a short description and no allowed-tools,
	// because the registry widens access on activation and set_theme is
	// always-allowed at registration time.
	if skill.Description == "" {
		t.Fatal("description is empty")
	}
	if len(skill.Description) >= 1024 {
		t.Fatalf("description is %d characters", len(skill.Description))
	}
	if skill.AllowedTools != "" {
		t.Fatalf("theme skill must not declare allowed-tools: %q", skill.AllowedTools)
	}

	// The instructions must carry the role vocabulary, the value forms, the
	// two-call persist protocol, and the error codes the model self-corrects
	// from.
	roles := []string{
		"ok", "err", "run", "accent", "active", "muted", "code",
		"syn-comment", "syn-keyword", "syn-string", "syn-number", "syn-func", "syn-add", "syn-del",
		"polly-green", "polly-light", "polly-wing", "polly-crown", "polly-beak", "polly-mouth", "polly-face", "polly-eye", "polly-foot",
	}
	for _, role := range roles {
		if !strings.Contains(skill.Instructions, "`"+role+"`") {
			t.Fatalf("instructions do not document role %q", role)
		}
	}
	for _, form := range []string{"#rrggbb", "#rgb", "palette:N", "inherit", "Omit the key"} {
		if !strings.Contains(skill.Instructions, form) {
			t.Fatalf("instructions do not document value form %q", form)
		}
	}
	for _, protocol := range []string{"confirmation_required", "persist: true", "confirm: true", "overwrite: true"} {
		if !strings.Contains(skill.Instructions, protocol) {
			t.Fatalf("instructions do not document the persist protocol step %q", protocol)
		}
	}
	for _, code := range []string{"UNKNOWN_ROLE", "INVALID_COLOR", "INVALID_THEME", "THEME_EXISTS", "THEME_WRITE_FAILED"} {
		if !strings.Contains(skill.Instructions, code) {
			t.Fatalf("instructions do not document error code %q", code)
		}
	}

	// AGENTS.md: builtin skills are embedded in the binary, materialized
	// outside the repository, and run against arbitrary projects, so this one
	// must not point at the polly repository. Runtime paths such as
	// ~/.pollytool/themes and tool names such as set_theme are legitimate and
	// are deliberately not matched.
	for _, needle := range []string{"github.com/alexschlessinger", "cmd/polly", "docs/features"} {
		if strings.Contains(skill.Instructions, needle) {
			t.Fatalf("theme instructions reference the repository: %q", needle)
		}
		if strings.Contains(skill.Description, needle) {
			t.Fatalf("theme description references the repository: %q", needle)
		}
	}
}

// TestBuiltinSkillsReferenceNoRepository checks the module path, which is never
// legitimate in a builtin skill. Repository-local spellings such as
// docs/features are checked for the theme skill in its own test, because a
// workflow skill may legitimately name a docs/ features directory inside the
// user's own project.
func TestBuiltinSandboxSetupSkillDocumentsTheInitTools(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	catalog, err := LoadBuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	skill, ok := catalog.Get("sandbox-setup")
	if !ok {
		t.Fatalf("sandbox-setup not discovered: %v", catalog.List())
	}
	if skill.Description == "" || len(skill.Description) >= 1024 {
		t.Fatalf("description length = %d", len(skill.Description))
	}
	// /init adds the tools the skill drives; the skill widens nothing.
	if skill.AllowedTools != "" {
		t.Fatalf("sandbox-setup must not declare allowed-tools: %q", skill.AllowedTools)
	}
	for _, want := range []string{"sandbox_trial", "sandbox_propose", "/init", "@cache", "passenv", "only the user allows"} {
		if !strings.Contains(skill.Instructions+skill.Description, want) {
			t.Fatalf("sandbox-setup lacks %q", want)
		}
	}
}

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

func TestBuiltinSimplifySkillMaterializesAndStaysProjectAgnostic(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	catalog, err := LoadBuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	skill, ok := catalog.Get("simplify")
	if !ok {
		t.Fatalf("simplify not discovered: %v", catalog.List())
	}
	wantRoot := filepath.Join(home, ".pollytool", "builtin-skills", "simplify")
	if skill.RootDir != wantRoot {
		t.Fatalf("root = %s, want %s", skill.RootDir, wantRoot)
	}
	if skill.Description == "" || len(skill.Description) >= 1024 {
		t.Fatalf("description length = %d", len(skill.Description))
	}
	if skill.AllowedTools != "" {
		t.Fatalf("simplify skill must not declare allowed-tools: %q", skill.AllowedTools)
	}

	// The fan-out names the coordination tools it drives, keeps reviewers
	// read-only, and covers every lens; the fallback path must exist for
	// contexts without spawn_agent.
	for _, want := range []string{"spawn_agent", "read_only: true", "wait_agent", "Lens: reuse", "Lens: simplification", "Lens: efficiency", "Lens: altitude", "is not available"} {
		if !strings.Contains(skill.Instructions, want) {
			t.Fatalf("instructions missing %q", want)
		}
	}

	for _, needle := range []string{"github.com/alexschlessinger", "cmd/polly", "docs/features"} {
		if strings.Contains(skill.Instructions, needle) || strings.Contains(skill.Description, needle) {
			t.Fatalf("simplify skill references the repository: %q", needle)
		}
	}
}
