package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools"
)

func skillDiscoveryHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func assertDiscoveredSkill(t *testing.T, catalog *skills.Catalog, name, root string) {
	t.Helper()
	if catalog == nil {
		t.Fatal("missing skill catalog")
	}
	skill, ok := catalog.Get(name)
	if !ok {
		t.Fatalf("missing skill %q", name)
	}
	got, err := filepath.EvalSymlinks(skill.RootDir)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("skill %q root = %s, want %s", name, got, want)
	}
}

func TestProjectSkillDiscoveryScopeAndPrecedence(t *testing.T) {
	home := skillDiscoveryHome(t)
	user := filepath.Join(home, ".pollytool", "skills")
	outer := t.TempDir()
	root := filepath.Join(outer, "repo")
	middle := filepath.Join(root, "services")
	cwd := filepath.Join(middle, "api")
	writeRepositoryTestFile(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main")
	project := func(dir string) string { return filepath.Join(dir, ".agents", "skills") }
	for _, dir := range []string{user, project(root), project(middle), project(cwd)} {
		createSkillWithScript(t, dir, "shared")
	}
	for name, dir := range map[string]string{
		"user-only": user, "root-only": project(root), "middle-only": project(middle), "local-only": project(cwd),
		"theme-designer": user, "sandbox-setup": project(root),
	} {
		createSkillWithScript(t, dir, name)
	}
	createSkillWithScript(t, user, "root-over-user")
	createSkillWithScript(t, project(root), "root-over-user")
	createSkillWithScript(t, project(root), "middle-over-root")
	createSkillWithScript(t, project(middle), "middle-over-root")
	for name, dir := range map[string]string{
		"outside": outer, "sibling": filepath.Join(root, "sibling"), "descendant": filepath.Join(cwd, "deeper"),
	} {
		createSkillWithScript(t, project(dir), name)
	}
	t.Chdir(cwd)

	config := &Config{}
	result, err := loadSkillCatalog(config, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{
		"shared": project(cwd), "root-over-user": project(root), "middle-over-root": project(middle),
		"user-only": user, "root-only": project(root), "middle-only": project(middle), "local-only": project(cwd),
		"theme-designer": user, "sandbox-setup": project(root),
	} {
		assertDiscoveredSkill(t, result.catalog, name, dir)
	}
	for _, name := range []string{"outside", "sibling", "descendant"} {
		if _, ok := result.catalog.Get(name); ok {
			t.Fatalf("discovered out-of-scope skill %q", name)
		}
	}
	if _, ok := result.catalog.Get("simplify"); !ok {
		t.Fatal("missing builtin fallback")
	}
	if len(result.autoActivate) != 0 || len(result.sources) != 0 || len(config.Launch.SkillDirs) != 0 {
		t.Fatalf("automatic discovery activated or persisted sources: %+v, %+v", result, config)
	}
	registry := tools.NewToolRegistry(nil, tools.WithNativeTools(), tools.WithUnsafeNoSandbox())
	t.Cleanup(func() { registry.Close() })
	runtime, err := newSkillRuntime(result.catalog, registry)
	if err != nil {
		t.Fatal(err)
	}
	if got := runtime.ActivatedSkills(); len(got) != 0 {
		t.Fatalf("discovery activated skills: %v", got)
	}
}

func TestProjectSkillDiscoveryNearestRootAndNonRepository(t *testing.T) {
	for _, gitMarker := range []bool{false, true} {
		t.Run(map[bool]string{false: "outside Git", true: "nested worktree"}[gitMarker], func(t *testing.T) {
			skillDiscoveryHome(t)
			outer := t.TempDir()
			if !gitMarker {
				skipInsideRepository(t, outer)
			}
			cwd := filepath.Join(outer, "work")
			createSkillWithScript(t, filepath.Join(outer, ".agents", "skills"), "parent-skill")
			createSkillWithScript(t, filepath.Join(cwd, ".agents", "skills"), "local-skill")
			if gitMarker {
				writeRepositoryTestFile(t, filepath.Join(outer, ".git", "HEAD"), "outer repository")
				writeRepositoryTestFile(t, filepath.Join(cwd, ".git"), "gitdir: elsewhere")
			}
			t.Chdir(cwd)
			result, err := loadSkillCatalog(&Config{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			assertDiscoveredSkill(t, result.catalog, "local-skill", filepath.Join(cwd, ".agents", "skills"))
			if _, ok := result.catalog.Get("parent-skill"); ok {
				t.Fatal("discovered skills above the project boundary")
			}
		})
	}
}

func TestProjectSkillDiscoveryExplicitSources(t *testing.T) {
	home := skillDiscoveryHome(t)
	user := filepath.Join(home, ".pollytool", "skills")
	cwd := t.TempDir()
	project := filepath.Join(cwd, ".agents", "skills")
	explicit := t.TempDir()
	source := t.TempDir()
	for _, dir := range []string{user, project, explicit, source} {
		createSkillWithScript(t, dir, "shared")
	}
	createSkillWithScript(t, user, "user-only")
	createSkillWithScript(t, project, "project-only")
	createSkillWithScript(t, explicit, "explicit-only")
	writeRepositoryTestFile(t, filepath.Join(cwd, ".git"), "gitdir: elsewhere")
	t.Chdir(cwd)
	flagSource := filepath.Join(source, "shared")

	for _, tc := range []struct {
		name      string
		dirs      []string
		sources   []string
		persisted []string
		wantRoot  string
		wantAuto  []string
	}{
		{name: "skilldir replaces defaults", dirs: []string{explicit}, wantRoot: explicit},
		{name: "skill adds to defaults", sources: []string{flagSource}, wantRoot: source, wantAuto: []string{"shared"}},
		{name: "persisted skill adds to defaults", persisted: []string{flagSource}, wantRoot: source, wantAuto: []string{"shared"}},
		{name: "skill overrides skilldir", dirs: []string{explicit}, sources: []string{flagSource}, wantRoot: source, wantAuto: []string{"shared"}},
		{name: "flag replaces persisted source", sources: []string{flagSource}, persisted: []string{"/missing/old-skill"}, wantRoot: source, wantAuto: []string{"shared"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := loadSkillCatalog(&Config{Skills: tc.sources}, tc.dirs, tc.persisted)
			if err != nil {
				t.Fatal(err)
			}
			assertDiscoveredSkill(t, result.catalog, "shared", tc.wantRoot)
			for _, name := range []string{"user-only", "project-only"} {
				if _, ok := result.catalog.Get(name); ok != (len(tc.dirs) == 0) {
					t.Fatalf("default skill %q present = %v, explicit directories = %v", name, ok, tc.dirs)
				}
			}
			if !slices.Equal(result.autoActivate, tc.wantAuto) {
				t.Fatalf("autoActivate = %v, want %v", result.autoActivate, tc.wantAuto)
			}
		})
	}
	other := t.TempDir()
	createSkillWithScript(t, other, "shared")
	if _, err := loadSkillCatalog(&Config{}, []string{explicit, other}, nil); err == nil || !strings.Contains(err.Error(), "duplicate skill") {
		t.Fatalf("duplicate explicit names error = %v", err)
	}
	if _, err := loadSkillCatalog(&Config{}, []string{filepath.Join(cwd, "missing")}, nil); err == nil {
		t.Fatal("missing explicit directory should remain an error")
	}
}

func TestProjectSkillDiscoveryMissingDisabledAndList(t *testing.T) {
	skillDiscoveryHome(t)
	cwd := t.TempDir()
	writeRepositoryTestFile(t, filepath.Join(cwd, ".git"), "gitdir: elsewhere")
	t.Chdir(cwd)
	if _, err := loadSkillCatalog(&Config{}, nil, nil); err != nil {
		t.Fatalf("absent project/user directories: %v", err)
	}
	project := filepath.Join(cwd, ".agents", "skills")
	createSkillWithScript(t, project, "new-skill")
	output := captureStdout(t, func() {
		if err := handleListSkills(&Config{}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(output, "new-skill - test skill [") {
		t.Fatalf("--listskills did not rediscover project skill: %s", output)
	}
	writeRepositoryTestFile(t, filepath.Join(project, "broken", "SKILL.md"), "not a skill")
	if _, err := loadSkillCatalog(&Config{}, nil, nil); err == nil || !strings.Contains(err.Error(), "SKILL.md") {
		t.Fatalf("malformed project skill error = %v", err)
	}
	result, err := loadSkillCatalog(&Config{NoSkills: true, Skills: []string{"/missing/skill"}}, nil, []string{"/missing/persisted"})
	if err != nil || result.catalog != nil || len(result.autoActivate) != 0 || len(result.sources) != 0 {
		t.Fatalf("--noskills must skip discovery and sources: %+v, %v", result, err)
	}
}

func TestProjectSkillDiscoveryHonorsMasks(t *testing.T) {
	for _, target := range []string{"project", "container", "skill", "manifest", "private storage", "symlink", "runtime"} {
		t.Run(target, func(t *testing.T) {
			home := skillDiscoveryHome(t)
			cwd := t.TempDir()
			writeRepositoryTestFile(t, filepath.Join(cwd, ".git"), "gitdir: elsewhere")
			project := filepath.Join(cwd, ".agents", "skills")
			createSkillWithScript(t, project, "hidden")
			createSkillWithScript(t, project, "visible")
			manifest := filepath.Join(project, "hidden", "SKILL.md")
			config := &Config{}
			var privatePaths []string
			switch target {
			case "project":
				config.DenyPaths = []string{cwd}
			case "container":
				config.DenyPaths = []string{project}
			case "skill":
				config.DenyPaths = []string{filepath.Dir(manifest)}
			case "manifest":
				config.DenyPaths = []string{manifest}
			case "private storage":
				privatePaths = []string{manifest}
			case "symlink", "runtime":
				skipIfWindows(t)
				// A manifest symlink stays inside the skill in the first case,
				// so the denial, not containment, must hide it.
				denied := filepath.Join(filepath.Dir(manifest), "secret.md")
				if target == "runtime" {
					denied = filepath.Join(home, ".pollytool", "private.md")
				} else {
					config.DenyPaths = []string{denied}
				}
				writeRepositoryTestFile(t, denied, "invalid private content must not be parsed")
				if err := os.Remove(manifest); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(denied, manifest); err != nil {
					t.Fatal(err)
				}
			}
			if target != "symlink" && target != "runtime" {
				writeRepositoryTestFile(t, manifest, "invalid private content must not be parsed")
			}
			t.Chdir(cwd)
			result, err := loadSkillCatalog(config, nil, nil, privatePaths...)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := result.catalog.Get("hidden"); ok {
				t.Fatal("discovered masked project skill")
			}
			if _, ok := result.catalog.Get("visible"); ok != (target != "project" && target != "container") {
				t.Fatalf("visible sibling present = %v for mask %s", ok, target)
			}
		})
	}
}

func TestProjectSkillDiscoveryHonorsExplicitGrants(t *testing.T) {
	skillDiscoveryHome(t)
	cwd := t.TempDir()
	writeRepositoryTestFile(t, filepath.Join(cwd, ".git"), "gitdir: elsewhere")
	project := filepath.Join(cwd, ".agents", "skills")
	createSkillWithScript(t, project, "granted")
	t.Chdir(cwd)
	for _, tc := range []struct {
		name   string
		config Config
		want   bool
	}{
		{"denial alone", Config{DenyPaths: []string{cwd}}, false},
		{"read grant", Config{DenyPaths: []string{cwd}, ReadPaths: []string{project}}, true},
		{"write grant", Config{DenyPaths: []string{cwd}, WritePaths: []string{project}}, true},
		{"readonly read grant", Config{SandboxPreset: "base+readonly", DenyPaths: []string{cwd}, ReadPaths: []string{project}}, true},
		{"readonly ignores write grant", Config{SandboxPreset: "base+readonly", DenyPaths: []string{cwd}, WritePaths: []string{project}}, false},
		{"disabled sandbox", Config{NoSandbox: true, DenyPaths: []string{cwd}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := loadSkillCatalog(&tc.config, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := result.catalog.Get("granted"); ok != tc.want {
				t.Fatalf("explicitly granted skill present = %v, want %v", ok, tc.want)
			}
		})
	}
}

func TestProjectSkillDiscoveryRejectsExternalSymlink(t *testing.T) {
	skipIfWindows(t)
	skillDiscoveryHome(t)
	cwd := t.TempDir()
	writeRepositoryTestFile(t, filepath.Join(cwd, ".git"), "gitdir: elsewhere")
	external := t.TempDir()
	createSkillWithScript(t, external, "external-skill")
	if err := os.Mkdir(filepath.Join(cwd, ".agents"), 0755); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(cwd, ".agents", "skills")
	if err := os.Symlink(external, project); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	result, err := loadSkillCatalog(&Config{NoSandbox: true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.catalog.Get("external-skill"); ok {
		t.Fatal("automatically followed project skill symlink outside the project")
	}
	result, err = loadSkillCatalog(&Config{NoSandbox: true}, []string{project}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertDiscoveredSkill(t, result.catalog, "external-skill", external)
}
