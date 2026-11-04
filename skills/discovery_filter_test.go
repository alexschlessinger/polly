package skills

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDiscoveryFilterSkipsRejectedRoot(t *testing.T) {
	for _, loader := range []struct {
		name string
		load func([]string, ...DiscoverOption) (*Catalog, error)
	}{
		{"Discover", Discover},
		{"LoadCatalog", LoadCatalog},
	} {
		for _, kind := range []string{"container", "skill", "missing", "file"} {
			t.Run(loader.name+"/"+kind, func(t *testing.T) {
				root := t.TempDir()
				allowed := createTestSkill(t, root, "allowed", "Allowed skill")
				denied := filepath.Join(root, "denied")
				switch kind {
				case "container":
					writeSkillFile(t, denied, "malformed", "not a skill")
				case "skill":
					writeSkillFile(t, root, "denied", "not a skill")
				case "file":
					if err := os.WriteFile(denied, []byte("not a directory"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				var checked []string
				catalog, err := loader.load([]string{denied, allowed}, WithDiscoveryFilter(func(path string) bool {
					checked = append(checked, path)
					return path != denied
				}))
				if err != nil {
					t.Fatalf("load() error = %v", err)
				}
				assertDiscoveryNames(t, catalog, "allowed")
				if !slices.Contains(checked, denied) {
					t.Fatalf("filter did not check root %s: %v", denied, checked)
				}
				for _, path := range checked {
					if strings.HasPrefix(path, denied+string(os.PathSeparator)) {
						t.Fatalf("filter reached %s below rejected root %s", path, denied)
					}
				}
			})
		}
	}
}

func TestDiscoveryFilterSkipsRejectedChildOrManifest(t *testing.T) {
	for _, loader := range []struct {
		name string
		load func([]string, ...DiscoverOption) (*Catalog, error)
	}{
		{"Discover", Discover},
		{"LoadCatalog", LoadCatalog},
	} {
		for _, kind := range []string{"child", "manifest"} {
			t.Run(loader.name+"/"+kind, func(t *testing.T) {
				root := t.TempDir()
				createTestSkill(t, root, "allowed", "Allowed skill")
				createTestSkill(t, root, "other", "Another allowed skill")
				deniedDir := writeSkillFile(t, root, "denied", "malformed denied content must not be parsed")
				manifest := filepath.Join(deniedDir, skillFileName)
				denied := deniedDir
				if kind == "manifest" {
					denied = manifest
				}
				var checked []string
				catalog, err := loader.load([]string{root}, WithDiscoveryFilter(func(path string) bool {
					checked = append(checked, path)
					return path != denied
				}))
				if err != nil {
					t.Fatalf("load() error = %v", err)
				}
				assertDiscoveryNames(t, catalog, "allowed", "other")
				if !slices.Contains(checked, denied) {
					t.Fatalf("filter did not check %s: %v", denied, checked)
				}
				if kind == "child" && slices.Contains(checked, manifest) {
					t.Fatalf("filter reached manifest below rejected child: %v", checked)
				}
			})
		}
	}
}

func TestDiscoveryFilterChecksSymlinkTargets(t *testing.T) {
	for _, kind := range []string{"root", "manifest"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			allowed := createTestSkill(t, root, "allowed", "Allowed skill")
			outside := t.TempDir()
			deniedDir := writeSkillFile(t, outside, "denied", "malformed target must not be parsed")
			link := filepath.Join(root, "denied")
			target := deniedDir
			paths := []string{link, allowed}
			if kind == "manifest" {
				if err := os.Mkdir(link, 0755); err != nil {
					t.Fatal(err)
				}
				link = filepath.Join(link, skillFileName)
				target = filepath.Join(deniedDir, skillFileName)
				paths = []string{root}
			}
			if err := os.Symlink(target, link); err != nil {
				t.Skipf("Symlink() unavailable: %v", err)
			}
			canonicalTarget, err := filepath.EvalSymlinks(target)
			if err != nil {
				t.Fatal(err)
			}
			var checked []string
			catalog, err := Discover(paths, WithDiscoveryFilter(func(path string) bool {
				checked = append(checked, path)
				return path != canonicalTarget
			}))
			if err != nil {
				t.Fatalf("Discover() error = %v", err)
			}
			assertDiscoveryNames(t, catalog, "allowed")
			if !slices.Contains(checked, link) || !slices.Contains(checked, canonicalTarget) {
				t.Fatalf("filter must check both %s and %s: %v", link, canonicalTarget, checked)
			}
		})
	}
}

func TestDiscoveryFilterRejectsManifestBeforeResolving(t *testing.T) {
	root := t.TempDir()
	createTestSkill(t, root, "allowed", "Allowed skill")
	denied := filepath.Join(root, "denied")
	if err := os.Mkdir(denied, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(denied, skillFileName)
	if err := os.Symlink(skillFileName, manifest); err != nil {
		t.Skipf("Symlink() unavailable: %v", err)
	}
	catalog, err := Discover([]string{root}, WithDiscoveryFilter(func(path string) bool { return path != manifest }))
	if err != nil {
		t.Fatalf("Discover() resolved rejected looping symlink: %v", err)
	}
	assertDiscoveryNames(t, catalog, "allowed")
}

func TestDiscoveryFilterRejectsReplacedManifestSymlink(t *testing.T) {
	root := t.TempDir()
	skillDir := createTestSkill(t, root, "allowed", "Allowed skill")
	manifest := filepath.Join(skillDir, skillFileName)
	canonicalManifest, err := filepath.EvalSymlinks(manifest)
	if err != nil {
		t.Fatal(err)
	}
	outside := createTestSkill(t, t.TempDir(), "allowed", "Unapproved replacement")
	// Start with a symlink so its lexical and canonical spellings are distinct,
	// and the replacement can happen deterministically in the canonical check.
	link := filepath.Join(t.TempDir(), "allowed")
	if err := os.Symlink(skillDir, link); err != nil {
		t.Skipf("Symlink() unavailable: %v", err)
	}
	replaced := false
	_, err = Discover([]string{link}, WithDiscoveryFilter(func(path string) bool {
		if path == canonicalManifest && !replaced {
			replaced = true
			if err := os.Remove(manifest); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(outside, skillFileName), manifest); err != nil {
				t.Fatal(err)
			}
		}
		return true
	}))
	if !replaced {
		t.Fatal("filter did not check canonical manifest")
	}
	if err == nil || !strings.Contains(err.Error(), "read ") {
		t.Fatalf("Discover() error = %v, want refusal to open replaced symlink", err)
	}
}

func TestDiscoveryFilterPreservesAllowedErrors(t *testing.T) {
	for _, loader := range []struct {
		name string
		load func([]string, ...DiscoverOption) (*Catalog, error)
	}{
		{"Discover", Discover},
		{"LoadCatalog", LoadCatalog},
	} {
		t.Run(loader.name+"/parse", func(t *testing.T) {
			root := t.TempDir()
			writeSkillFile(t, root, "malformed", "not a skill")
			_, err := loader.load([]string{root}, WithDiscoveryFilter(func(string) bool { return true }))
			if err == nil || !strings.Contains(err.Error(), "missing YAML frontmatter") {
				t.Fatalf("load() error = %v, want parse error", err)
			}
		})
		t.Run(loader.name+"/duplicate", func(t *testing.T) {
			first, second := t.TempDir(), t.TempDir()
			createTestSkill(t, first, "duplicate", "First copy")
			createTestSkill(t, second, "duplicate", "Second copy")
			_, err := loader.load([]string{first, second}, WithDiscoveryFilter(func(string) bool { return true }))
			if err == nil || !strings.Contains(err.Error(), `duplicate skill "duplicate"`) {
				t.Fatalf("load() error = %v, want duplicate error", err)
			}
		})
	}
}

func TestLoadCatalogFilterSkipsDefaultRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	root := filepath.Join(home, ".pollytool", "skills")
	writeSkillFile(t, root, "malformed", "not a skill")
	checked := false
	catalog, err := LoadCatalog(nil, WithDiscoveryFilter(func(path string) bool {
		if path == root {
			checked = true
			return false
		}
		return true
	}))
	if err != nil || catalog != nil || !checked {
		t.Fatalf("LoadCatalog() = %v, %v; checked default root = %v", catalog, err, checked)
	}
}

func TestDiscoveryFilterAllowsContainedSymlinks(t *testing.T) {
	root := t.TempDir()
	skillRoot := createTestSkill(t, root, "helper", "Linked skill")
	manifest := filepath.Join(skillRoot, skillFileName)
	if err := os.Rename(manifest, filepath.Join(skillRoot, "instructions.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("instructions.md", manifest); err != nil {
		t.Skipf("Symlink() unavailable: %v", err)
	}
	alias := filepath.Join(t.TempDir(), "skills")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	catalog, err := LoadCatalog([]string{alias}, WithDiscoveryFilter(func(string) bool { return true }))
	if err != nil {
		t.Fatal(err)
	}
	skill, ok := catalog.Get("helper")
	if !ok || skill.RootDir != filepath.Join(alias, "helper") || skill.Description != "Linked skill" {
		t.Fatalf("contained symlink discovery = %+v", skill)
	}
	if _, err := skill.ReadFile(skillFileName); err != nil {
		t.Fatalf("contained manifest symlink is unreadable: %v", err)
	}
}

func TestDiscoveryFilterPinsRootBeforeReadingManifest(t *testing.T) {
	for _, phase := range []string{"before manifest resolution", "after manifest approval"} {
		t.Run(phase, func(t *testing.T) {
			broader := t.TempDir()
			root := createTestSkill(t, broader, "helper", "Approved skill")
			content := "---\nname: helper\ndescription: Broader directory\n---\nUnapproved instructions"
			if err := os.WriteFile(filepath.Join(broader, skillFileName), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(t.TempDir(), "helper")
			if err := os.Symlink(root, alias); err != nil {
				t.Skipf("Symlink() unavailable: %v", err)
			}
			trigger := filepath.Join(alias, skillFileName)
			if phase == "after manifest approval" {
				var err error
				trigger, err = filepath.EvalSymlinks(filepath.Join(root, skillFileName))
				if err != nil {
					t.Fatal(err)
				}
			}
			retargeted := false
			catalog, err := Discover([]string{alias}, WithDiscoveryFilter(func(path string) bool {
				if path == trigger && !retargeted {
					retargeted = true
					if err := os.Remove(alias); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(broader, alias); err != nil {
						t.Fatal(err)
					}
				}
				return true
			}))
			if !retargeted {
				t.Fatal("fixture did not retarget the approved root")
			}
			if phase == "before manifest resolution" {
				if err == nil || !strings.Contains(err.Error(), "escapes the approved skill root") {
					t.Fatalf("redirected manifest error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			skill, ok := catalog.Get("helper")
			if !ok || skill.Description != "Approved skill" {
				t.Fatalf("discovery did not read the approved manifest: %+v", skill)
			}
			if _, err := skill.ResolvePath("."); err == nil {
				t.Fatal("discovery repinned the root after reading its approved manifest")
			}
			pinned := *skill
			pinned.RootDir = broader
			if _, err := pinned.ResolvePath("."); err == nil {
				t.Fatal("discovered root provenance accepted a broader frozen grant")
			}
		})
	}
}

func TestDiscoveryFilterBoundsManifestSize(t *testing.T) {
	for _, loader := range []struct {
		name string
		load func([]string, ...DiscoverOption) (*Catalog, error)
	}{
		{"Discover", Discover},
		{"LoadCatalog", LoadCatalog},
	} {
		for _, oversized := range []bool{false, true} {
			t.Run(loader.name+map[bool]string{false: "/at limit", true: "/over limit"}[oversized], func(t *testing.T) {
				root := t.TempDir()
				size := maxSkillFileSize
				if oversized {
					size++
				}
				content := "---\nname: bounded\ndescription: Bounded skill\n---\n"
				content += strings.Repeat("x", size-len(content))
				writeSkillFile(t, root, "bounded", content)
				catalog, err := loader.load([]string{root}, WithDiscoveryFilter(func(string) bool { return true }))
				if oversized {
					if err == nil || !strings.Contains(err.Error(), "SKILL.md exceeds") {
						t.Fatalf("oversized manifest error = %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				assertDiscoveryNames(t, catalog, "bounded")
			})
		}
	}
}

func TestDiscoveryFilterNilRetainsDefaultBehavior(t *testing.T) {
	root := t.TempDir()
	createTestSkill(t, root, "allowed", "Allowed skill")
	catalog, err := LoadCatalog([]string{root, root}, WithDiscoveryFilter(nil))
	if err != nil {
		t.Fatalf("LoadCatalog() error = %v", err)
	}
	assertDiscoveryNames(t, catalog, "allowed")
}

func assertDiscoveryNames(t *testing.T, catalog *Catalog, want ...string) {
	t.Helper()
	var got []string
	for _, skill := range catalog.List() {
		got = append(got, skill.Name)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("discovered names = %v, want %v", got, want)
	}
}
