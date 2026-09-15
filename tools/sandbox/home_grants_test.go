package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func tempHome(t *testing.T) string {
	t.Helper()
	skipIfWindows(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	unsetTestEnv(t, "GIT_CONFIG_GLOBAL")
	unsetTestEnv(t, "XDG_CONFIG_HOME")
	return home
}

func TestGitUserConfigPathsDefaultsToHomeSources(t *testing.T) {
	home := tempHome(t)
	gitconfig := filepath.Join(home, ".gitconfig")
	xdgGit := filepath.Join(home, ".config", "git")
	if err := os.WriteFile(gitconfig, []byte("[user]\n\tname = t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(xdgGit, 0o700); err != nil {
		t.Fatal(err)
	}
	got := GitUserConfigPaths()
	want := []string{gitconfig, xdgGit}
	if !slices.Equal(got, want) {
		t.Fatalf("GitUserConfigPaths() = %v, want %v", got, want)
	}
	if err := os.Remove(gitconfig); err != nil {
		t.Fatal(err)
	}
	if got := GitUserConfigPaths(); !slices.Equal(got, []string{xdgGit}) {
		t.Fatalf("GitUserConfigPaths() after removal = %v, want only %q", got, xdgGit)
	}
}

func TestGitUserConfigPathsHonorsGlobalOverride(t *testing.T) {
	home := tempHome(t)
	inside := filepath.Join(home, "cfg", "gitconfig")
	if err := os.MkdirAll(filepath.Dir(inside), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", inside)
	if got := GitUserConfigPaths(); !slices.Equal(got, []string{inside}) {
		t.Fatalf("GitUserConfigPaths() = %v, want only the override %q", got, inside)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	if got := GitUserConfigPaths(); len(got) != 0 {
		t.Fatalf("GitUserConfigPaths() = %v, want nothing for a source outside home", got)
	}
}

func TestHomeToolchainGrantsCollectsExistingEntriesUnderHome(t *testing.T) {
	home := tempHome(t)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	binDir := filepath.Join(home, "tools", "bin")
	libDir := filepath.Join(home, "tools", "lib")
	modCache := filepath.Join(home, "gomod")
	for _, dir := range []string{binDir, libDir, modCache} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GOMODCACHE", modCache)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+filepath.Join(home, "missing", "bin")+string(os.PathListSeparator)+home+string(os.PathListSeparator)+"/usr/bin")
	got := computeHomeToolchainGrants(home)
	slices.Sort(got)
	want := []string{modCache, binDir, libDir}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("computeHomeToolchainGrants() = %v, want %v", got, want)
	}
}

func TestHomeToolchainGrantsAreCachedPerHome(t *testing.T) {
	home := tempHome(t)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GOMODCACHE", filepath.Join(home, "absent"))
	t.Setenv("PATH", "/usr/bin")
	first := HomeToolchainGrants()
	second := HomeToolchainGrants()
	if !slices.Equal(first, second) {
		t.Fatalf("HomeToolchainGrants() = %v then %v, want a stable cached result", first, second)
	}
	for _, grant := range first {
		if !PathWithin(grant, home) {
			t.Fatalf("grant %q lies outside the home directory", grant)
		}
	}
}
