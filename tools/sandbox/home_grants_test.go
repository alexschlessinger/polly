package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	shims := filepath.Join(home, ".pyenv", "shims")
	homeBin := filepath.Join(home, "bin")
	planted := filepath.Join(home, ".ssh", "bin")
	for _, dir := range []string{binDir, libDir, shims, homeBin, planted} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	entries := []string{binDir, filepath.Join(home, "missing", "bin"), home, shims, homeBin, planted, "/usr/bin"}
	t.Setenv("PATH", joinPathList(entries...))
	got := computeHomeToolchainGrants(home)
	slices.Sort(got)
	// bin, sbin and shims entries widen to their install prefix; an entry
	// directly under the home keeps only itself; a planted credential path
	// and the home itself grant nothing.
	want := []string{filepath.Join(home, "tools"), filepath.Join(home, ".pyenv"), homeBin}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("computeHomeToolchainGrants() = %v, want %v", got, want)
	}
}

// ~/.local holds the XDG data and state directories, where programs of every
// kind keep data, history and tokens, so ~/.local/bin is not widened to it.
// Its symlinked executables bring their own install prefixes instead, and a
// dedicated prefix such as ~/.cargo still widens, its credentials masked.
func TestHomeToolchainGrantsKeepSharedRootsPrivate(t *testing.T) {
	home := tempHome(t)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, name := range []string{"XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		unsetTestEnv(t, name)
	}
	local := filepath.Join(home, ".local")
	localBin := filepath.Join(local, "bin")
	python := filepath.Join(local, "share", "uv", "python", "cpython-3.13", "bin", "python3.13")
	zig := filepath.Join(local, "share", "zigup", "0.17", "zig")
	loose := filepath.Join(local, "share", "loose-tool")
	history := filepath.Join(local, "share", "atuin", "history.db")
	cargoBin := filepath.Join(home, ".cargo", "bin")
	credentials := filepath.Join(home, ".cargo", "credentials.toml")
	for _, file := range []string{python, zig, loose, history, filepath.Join(localBin, "uv"), filepath.Join(cargoBin, "cargo"), credentials} {
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, nil, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{
		"python3.13": python,
		"python3":    "python3.13",
		"zig":        zig,
		"loose-tool": loose,
		"dangling":   filepath.Join(local, "share", "missing"),
	} {
		if err := os.Symlink(target, filepath.Join(localBin, link)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	t.Setenv("PATH", joinPathList(localBin, cargoBin, "/usr/bin"))
	got := computeHomeToolchainGrants(home)
	slices.Sort(got)
	want := []string{
		localBin,
		filepath.Dir(filepath.Dir(python)),
		filepath.Dir(zig),
		loose,
		filepath.Join(home, ".cargo"),
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("computeHomeToolchainGrants() = %v, want %v", got, want)
	}
	cfg := Config{PrivateHome: true, ReadPaths: got}
	if ReadAllowed(cfg, history) == nil {
		t.Fatal("shell history under ~/.local/share is readable through the PATH grants")
	}
	if ReadAllowed(cfg, credentials) == nil {
		t.Fatal("~/.cargo/credentials.toml is readable through the widened ~/.cargo grant")
	}
}

// A PATH entry whose parent holds an XDG base directory named by the
// environment is not widened either.
func TestHomeToolchainGrantsHonorXDGOverrides(t *testing.T) {
	home := tempHome(t)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	bin := filepath.Join(home, "xdg", "bin")
	data := filepath.Join(home, "xdg", "data")
	for _, dir := range []string{bin, data} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PATH", joinPathList(bin, "/usr/bin"))
	if got := computeHomeToolchainGrants(home); !slices.Equal(got, []string{bin}) {
		t.Fatalf("computeHomeToolchainGrants() = %v, want only %v", got, bin)
	}
}

func joinPathList(entries ...string) string {
	return strings.Join(entries, string(os.PathListSeparator))
}

func TestHomeToolchainGrantsFollowGitConfigIncludes(t *testing.T) {
	home := tempHome(t)
	if _, err := trustedGitExecutable(nil); err != nil {
		t.Skipf("no trusted git: %v", err)
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("PATH", "/usr/bin")
	global := filepath.Join(home, ".gitconfig")
	local := filepath.Join(home, ".gitconfig.local")
	nested := filepath.Join(home, "cfg", "nested.gitconfig")
	conditional := filepath.Join(home, "cfg", "work.gitconfig")
	excludes := filepath.Join(home, "cfg", "ignore")
	if err := os.MkdirAll(filepath.Dir(nested), 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		global:      "[include]\n\tpath = ~/.gitconfig.local\n[includeIf \"gitdir:/nowhere/\"]\n\tpath = cfg/work.gitconfig\n",
		local:       "[include]\n\tpath = cfg/nested.gitconfig\n[core]\n\texcludesFile = ~/cfg/ignore\n",
		nested:      "[user]\n\tname = nested\n",
		conditional: "[user]\n\tname = work\n",
		excludes:    "*.o\n",
	}
	for path, contents := range files {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := computeHomeToolchainGrants(home)
	slices.Sort(got)
	want := []string{global, local, nested, conditional, excludes}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("computeHomeToolchainGrants() = %v, want the config, its includes and the excludes file %v", got, want)
	}
}

func TestHomeToolchainGrantsAreCachedPerHome(t *testing.T) {
	home := tempHome(t)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
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

func TestHomeToolchainGrantsKeepSymlinkedPathEntrySpelling(t *testing.T) {
	home := tempHome(t)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	target := filepath.Join(home, "tools", "bin")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "bin")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("PATH", joinPathList(link, "/usr/bin"))
	got := computeHomeToolchainGrants(home)
	// The link is the name PATH lookups use inside the sandbox; the backends
	// resolve the grant to cover its target.
	if want := []string{link}; !slices.Equal(got, want) {
		t.Fatalf("computeHomeToolchainGrants() = %v, want the link spelling %v", got, want)
	}
}

func TestExistingHomeGrantsDropsMaskedAndOutsideCandidates(t *testing.T) {
	home := tempHome(t)
	skills := filepath.Join(home, ".polly", "skills")
	planted := filepath.Join(home, ".ssh", "skills")
	for _, dir := range []string{skills, planted} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	got := ExistingHomeGrants([]string{skills, skills, planted, home, filepath.Join(home, "missing"), "/usr/share"})
	if want := []string{skills}; !slices.Equal(got, want) {
		t.Fatalf("ExistingHomeGrants() = %v, want %v", got, want)
	}
}

func TestSharedHomeDirs(t *testing.T) {
	home := t.TempDir()
	for _, name := range []string{"XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME"} {
		t.Setenv(name, "")
	}
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "xdg-cache"))
	programDirs := make(map[string]bool)
	for _, dir := range SharedHomeDirs(home) {
		programDirs[dir.Path] = dir.ProgramDirs
	}
	for rel, want := range map[string]bool{
		".cache": true, ".config": true, ".local/share": true, ".local/state": true, "xdg-cache": true,
		"Library/Caches": true, "Library/Application Support": true,
		".local": false, "Library": false, "Library/Preferences": false,
	} {
		path := filepath.Join(home, filepath.FromSlash(rel))
		if got, ok := programDirs[path]; !ok || got != want {
			t.Errorf("%s: listed %v, programDirs %v; want listed with programDirs %v", rel, ok, got, want)
		}
	}
	if _, ok := programDirs[home]; ok {
		t.Error("the home directory is listed as a shared directory")
	}
}
