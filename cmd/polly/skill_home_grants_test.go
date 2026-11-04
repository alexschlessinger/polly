package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func TestHomeReadGrantsRespectDeniedSkillRoots(t *testing.T) {
	skipIfWindows(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Chdir(home)
	root := filepath.Join(home, "repo", ".agents", "skills", "demo")
	links := filepath.Join(home, "links")
	credentialRoot := filepath.Join(home, ".ssh", "skill")
	for _, dir := range []string{root, links, credentialRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(links, "skills")
	if err := os.Symlink(filepath.Dir(root), alias); err != nil {
		t.Fatal(err)
	}
	aliasRoot := filepath.Join(alias, "demo")

	for _, source := range []string{"operator", "private"} {
		t.Run(source, func(t *testing.T) {
			for _, tc := range []struct {
				name      string
				candidate string
				denied    string
				wantGrant bool
			}{
				{"unmasked", root, "", true},
				{"ancestor", root, filepath.Join(home, "repo"), false},
				{"equal", root, root, false},
				{"relative-ancestor", root, "repo", false},
				{"tilde-ancestor", root, "~/repo", false},
				{"descendant", root, filepath.Join(root, "private"), true},
				{"sibling", root, root + "-other", true},
				{"unmasked-symlink", aliasRoot, "", true},
				{"symlink-target-ancestor", aliasRoot, filepath.Join(home, "repo"), false},
				{"symlink-target-equal", aliasRoot, root, false},
				{"symlink-lexical-ancestor", aliasRoot, links, false},
				{"symlink-lexical-equal", aliasRoot, aliasRoot, false},
				{"symlink-denial-ancestor", root, alias, false},
				{"symlink-denial-equal", root, aliasRoot, false},
				{"unrelated-symlink-parent", root, links, true},
				{"credential-mask", credentialRoot, "", false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var config *Config
					var privatePaths []string
					if tc.denied != "" {
						if source == "operator" {
							config = &Config{DenyPaths: []string{tc.denied}}
						} else {
							privatePaths = []string{tc.denied}
						}
					}
					got := homeReadGrants(config, []string{tc.candidate}, privatePaths...)
					var want []string
					if tc.wantGrant {
						want = []string{tc.candidate}
					}
					if !slices.Equal(got, want) {
						t.Fatalf("homeReadGrants() = %v, want %v", got, want)
					}
					if tc.name == "descendant" {
						cfg := sandbox.Config{ReadPaths: got, DenyPaths: []string{tc.denied}}
						if err := sandbox.ReadMasked(cfg, filepath.Join(tc.denied, "secret")); err == nil {
							t.Fatal("automatic parent grant reopened a descendant mask")
						}
					}
				})
			}
		})
	}
}

func TestHomeReadGrantsPreserveExplicitReadPaths(t *testing.T) {
	skipIfWindows(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	root := filepath.Join(home, "repo", ".agents", "skills", "demo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	explicit := "~/repo/.agents/skills/demo"
	for _, source := range []string{"operator", "private"} {
		t.Run(source, func(t *testing.T) {
			for _, denied := range []string{filepath.Join(home, "repo"), root} {
				config := &Config{ReadPaths: []string{explicit}}
				var privatePaths []string
				if source == "operator" {
					config.DenyPaths = []string{denied}
				} else {
					privatePaths = []string{denied}
				}
				got := homeReadGrants(config, []string{root}, privatePaths...)
				if !slices.Equal(got, []string{explicit}) {
					t.Fatalf("homeReadGrants() = %v, want explicit spelling %q only", got, explicit)
				}
				cfg, err := sandbox.PrepareConfig(sandbox.Config{ReadPaths: got, DenyPaths: []string{denied}})
				if err != nil {
					t.Fatal(err)
				}
				if err := sandbox.ReadAllowed(cfg, filepath.Join(root, "SKILL.md")); err != nil {
					t.Fatalf("explicit --readpath no longer overrides denial: %v", err)
				}
			}
		})
	}
}

func TestSandboxRegistryOptionsRespectDeniedSkillRoots(t *testing.T) {
	skipIfWindows(t)
	captured := captureSandboxConfig(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Chdir(t.TempDir())
	root := filepath.Join(home, "repo", ".agents", "skills", "demo")
	allowed := filepath.Join(home, "allowed", "skill")
	for _, dir := range []string{root, allowed} {
		createSkillWithScript(t, filepath.Dir(dir), filepath.Base(dir))
	}
	catalog, err := skills.Discover([]string{root, allowed})
	if err != nil {
		t.Fatal(err)
	}
	skill, _ := catalog.Get("demo")
	result := &skillCatalogResult{catalog: catalog}
	alias := filepath.Join(home, "skill-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}

	for _, source := range []string{"operator", "private"} {
		t.Run(source, func(t *testing.T) {
			for _, candidate := range []string{root, alias} {
				for _, denied := range []string{filepath.Join(home, "repo"), root} {
					config := &Config{SandboxPreset: "base+private-home", Quiet: true}
					var privatePaths []string
					if source == "operator" {
						config.DenyPaths = []string{denied}
					} else {
						privatePaths = []string{denied}
					}
					skill.RootDir = candidate
					_, probe, _, err := sandboxRegistryOptionsWithWarnings(config, nil, result, nil, privatePaths...)
					if err != nil {
						t.Fatal(err)
					}
					if err := probe.wait(context.Background()); err != nil {
						t.Fatal(err)
					}
					for _, path := range []string{root, alias} {
						if err := sandbox.ReadAllowed(*captured, filepath.Join(path, "SKILL.md")); err == nil {
							t.Fatalf("candidate %s reopened %s under denial %s", candidate, path, denied)
						}
					}
					if err := sandbox.ReadAllowed(*captured, filepath.Join(allowed, "SKILL.md")); err != nil {
						t.Fatalf("unmasked skill root not readable: %v", err)
					}
				}
			}
		})
	}
}
