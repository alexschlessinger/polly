package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func projectSkillGrantFixture(t *testing.T) (home, repo, root string, config *Config) {
	t.Helper()
	home, err := filepath.EvalSymlinks(skillDiscoveryHome(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	repo = filepath.Join(home, "repo")
	root = filepath.Join(repo, ".agents", "skills", "demo")
	writeRepositoryTestFile(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main")
	createSkillWithScript(t, filepath.Dir(root), "demo")
	cwd := filepath.Join(repo, "service")
	if err := os.Mkdir(cwd, 0755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	return home, repo, root, &Config{SandboxPreset: "base+private-home", Quiet: true}
}

func retargetProjectSkill(t *testing.T, root, target string) {
	t.Helper()
	if err := os.Rename(root, root+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, root); err != nil {
		t.Fatal(err)
	}
}

func TestProjectSkillGrantsRejectRetarget(t *testing.T) {
	skipIfWindows(t)
	for _, phase := range []string{"before freeze", "after freeze"} {
		for _, target := range []string{"private home", "runtime storage", "project ancestor"} {
			t.Run(phase+"/"+target, func(t *testing.T) {
				captureSandboxConfig(t)
				home, repo, root, config := projectSkillGrantFixture(t)
				destination := filepath.Join(home, "unrelated-private")
				if target == "runtime storage" {
					destination = filepath.Join(home, ".pollytool", "changes")
					config.SandboxPreset = "base"
				} else if target == "project ancestor" {
					destination = repo
				}
				writeRepositoryTestFile(t, filepath.Join(destination, "secret.txt"), "private data")
				if phase == "before freeze" {
					catalog, policy, err := loadDefaultSkillCatalog(config, nil)
					if err != nil {
						t.Fatal(err)
					}
					retargetProjectSkill(t, root, destination)
					if _, err := prepareProjectSkillGrants(config, catalog, policy, nil); err == nil {
						t.Fatal("project grant accepted a redirected discovery root")
					}
					return
				}
				result, err := loadSkillCatalog(config, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				retargetProjectSkill(t, root, destination)
				if _, _, _, err := sandboxRegistryOptionsWithWarnings(config, nil, result, nil); err == nil {
					t.Fatal("sandbox setup re-resolved a frozen project root")
				}
			})
		}
	}
}

func TestProjectSkillGrantsValidateFrozenTargetNotRestoredAlias(t *testing.T) {
	skipIfWindows(t)
	_, repo, root, config := projectSkillGrantFixture(t)
	catalog, policy, err := loadDefaultSkillCatalog(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	retargetProjectSkill(t, root, repo)
	allow := policy.allow
	restored := false
	policy.allow = func(path string) bool {
		// Restore the approved alias after PrepareConfig has frozen the broader
		// target. Checking only that alias would now authorize the wrong grant.
		if path == root && !restored {
			restored = true
			if err := os.Remove(root); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(root+"-old", root); err != nil {
				t.Fatal(err)
			}
		}
		return allow(path)
	}
	if _, err := prepareProjectSkillGrants(config, catalog, policy, nil); err == nil || !strings.Contains(err.Error(), "root changed") {
		t.Fatalf("frozen ancestor grant error = %v", err)
	}
	if !restored {
		t.Fatal("fixture did not restore the alias after freezing its target")
	}
}

func TestProjectSkillGrantsExposeOnlyWinningRoots(t *testing.T) {
	captured := captureSandboxConfig(t)
	_, repo, ancestorRoot, config := projectSkillGrantFixture(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	nearby := filepath.Join(cwd, ".agents", "skills")
	createSkillWithScript(t, nearby, "demo")
	result, err := loadSkillCatalog(config, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.projectGrants) != 1 {
		t.Fatalf("prepared %d project grants, want only the winning skill", len(result.projectGrants))
	}
	for skill, grant := range result.projectGrants {
		if skill.RootDir != filepath.Join(nearby, "demo") || len(grant.ReadPaths) != 1 || grant.ReadPaths[0] != skill.RootDir {
			t.Fatalf("project grant = %s, %v", skill.RootDir, grant.ReadPaths)
		}
	}
	_, probe, _, err := sandboxRegistryOptionsWithWarnings(config, nil, result, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.ReadAllowed(*captured, filepath.Join(nearby, "demo", "SKILL.md")); err != nil {
		t.Fatalf("winning project skill is unreadable: %v", err)
	}
	for _, hidden := range []string{ancestorRoot, filepath.Join(repo, "other-private.txt")} {
		if err := sandbox.ReadAllowed(*captured, hidden); err == nil {
			t.Fatalf("project discovery exposed unused path %s", hidden)
		}
	}
	config.Skills = []string{ancestorRoot}
	explicit, err := loadSkillCatalog(config, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(explicit.projectGrants) != 0 {
		t.Fatal("explicit override retained shadowed automatic skill grants")
	}
}

func TestProjectSkillGrantsRetainEmptyProvenance(t *testing.T) {
	skipIfWindows(t)
	captured := captureSandboxConfig(t)
	home := skillDiscoveryHome(t)
	repo := t.TempDir() // outside home, so the skill needs no automatic read grant
	writeRepositoryTestFile(t, filepath.Join(repo, ".git"), "gitdir: elsewhere")
	root := filepath.Join(repo, ".agents", "skills", "demo")
	createSkillWithScript(t, filepath.Dir(root), "demo")
	t.Chdir(repo)
	config := &Config{SandboxPreset: "base+private-home", Quiet: true}
	result, err := loadSkillCatalog(config, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	skill, ok := result.catalog.Get("demo")
	if !ok {
		t.Fatal("fixture skill was not discovered")
	}
	grant, recorded := result.projectGrants[skill]
	if !recorded || len(grant.ReadPaths) != 0 {
		t.Fatalf("outside-home project provenance = %v, %v", recorded, grant.ReadPaths)
	}
	destination := filepath.Join(home, "unrelated-private")
	writeRepositoryTestFile(t, filepath.Join(destination, "secret.txt"), "private data")
	retargetProjectSkill(t, root, destination)
	_, probe, _, err := sandboxRegistryOptionsWithWarnings(config, nil, result, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.ReadAllowed(*captured, filepath.Join(destination, "secret.txt")); err == nil {
		t.Fatal("empty provenance fell back to lexical granting of a new home target")
	}
}
