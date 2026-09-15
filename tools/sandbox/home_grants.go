package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// GitUserConfigPaths lists the user's global Git configuration sources that
// exist inside the home directory: $GIT_CONFIG_GLOBAL when set, otherwise
// ~/.gitconfig and the XDG git directory ($XDG_CONFIG_HOME/git, by default
// ~/.config/git). Sources outside the home directory are omitted because only
// the home directory needs a grant to stay visible.
func GitUserConfigPaths() []string {
	home := resolvedHomeDir()
	if home == "" {
		return nil
	}
	var candidates []string
	if global, set := os.LookupEnv("GIT_CONFIG_GLOBAL"); set {
		if base, err := os.Getwd(); err == nil && global != "" {
			if path, err := resolveGitConfigPath(global, base); err == nil {
				candidates = append(candidates, path)
			}
		}
	} else {
		candidates = append(candidates, filepath.Join(home, ".gitconfig"))
		xdg := os.Getenv("XDG_CONFIG_HOME")
		if xdg == "" || !filepath.IsAbs(xdg) {
			xdg = filepath.Join(home, ".config")
		}
		candidates = append(candidates, filepath.Join(xdg, "git"))
	}
	return existingHomeGrants(home, candidates)
}

var (
	homeToolchainMu     sync.Mutex
	homeToolchainByHome = map[string][]string{}
)

// HomeToolchainGrants lists the read-only grants that keep common toolchains
// working while the home directory is private: the user's Git configuration
// with its excludes and attributes files, the Go root and module cache, and
// every PATH entry under the home directory together with the lib and libexec
// siblings of a bin entry. Every entry exists and lies inside the home
// directory; a missing tool contributes nothing. The result is computed once
// per home directory for the life of the process.
func HomeToolchainGrants() []string {
	home := resolvedHomeDir()
	if home == "" {
		return nil
	}
	homeToolchainMu.Lock()
	defer homeToolchainMu.Unlock()
	grants, ok := homeToolchainByHome[home]
	if !ok {
		grants = computeHomeToolchainGrants(home)
		homeToolchainByHome[home] = grants
	}
	return append([]string(nil), grants...)
}

func computeHomeToolchainGrants(home string) []string {
	candidates := GitUserConfigPaths()
	candidates = append(candidates, gitPathSettings("core.excludesFile", "core.attributesFile")...)
	candidates = append(candidates, goToolchainPaths(home)...)
	candidates = append(candidates, pathEntriesWithLibraries()...)
	return minimizePaths(existingHomeGrants(home, candidates), nil)
}

// gitPathSettings resolves path-typed Git settings the way the user's own git
// would, from outside any repository. Unset keys and a missing git yield nothing.
func gitPathSettings(keys ...string) []string {
	git, err := exec.LookPath("git")
	if err != nil {
		return nil
	}
	var paths []string
	for _, key := range keys {
		cmd := exec.Command(git, "config", "--get", "--type=path", key)
		cmd.Dir = string(filepath.Separator)
		cmd.Env = gitAuditEnvironment()
		output, found, err := gitQueryResult(cmd.Output())
		if err != nil || !found {
			continue
		}
		value := strings.TrimRight(string(output), "\r\n")
		if value != "" && filepath.IsAbs(value) {
			paths = append(paths, value)
		}
	}
	return paths
}

// goToolchainPaths names the Go module cache from the environment and, when a
// go executable is on PATH, the GOROOT and GOMODCACHE it reports. A toolchain
// download is never triggered.
func goToolchainPaths(home string) []string {
	var paths []string
	switch {
	case os.Getenv("GOMODCACHE") != "":
		paths = append(paths, os.Getenv("GOMODCACHE"))
	case os.Getenv("GOPATH") != "":
		if first := filepath.SplitList(os.Getenv("GOPATH"))[0]; first != "" {
			paths = append(paths, filepath.Join(first, "pkg", "mod"))
		}
	default:
		paths = append(paths, filepath.Join(home, "go", "pkg", "mod"))
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		return paths
	}
	cmd := exec.Command(goBin, "env", "GOROOT", "GOMODCACHE")
	cmd.Dir = string(filepath.Separator)
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	output, err := cmd.Output()
	if err != nil {
		return paths
	}
	for _, line := range strings.Split(string(output), "\n") {
		if line = strings.TrimSpace(line); line != "" && filepath.IsAbs(line) {
			paths = append(paths, line)
		}
	}
	return paths
}

// pathEntriesWithLibraries lists every absolute PATH entry and, for a bin
// entry, the lib and libexec siblings that launchers commonly symlink into.
func pathEntriesWithLibraries() []string {
	var paths []string
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if entry == "" || !filepath.IsAbs(entry) {
			continue
		}
		entry = filepath.Clean(entry)
		paths = append(paths, entry)
		if filepath.Base(entry) == "bin" {
			parent := filepath.Dir(entry)
			paths = append(paths, filepath.Join(parent, "lib"), filepath.Join(parent, "libexec"))
		}
	}
	return paths
}

// resolvedHomeDir is the canonical home directory, or empty when it cannot
// serve as a private root.
func resolvedHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return ""
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(home))
	if err != nil {
		return ""
	}
	real = filepath.Clean(real)
	if real == string(filepath.Separator) {
		return ""
	}
	return real
}

// existingHomeGrants keeps the candidates that exist strictly inside home, in
// canonical form and without duplicates.
func existingHomeGrants(home string, candidates []string) []string {
	seen := make(map[string]bool, len(candidates))
	var grants []string
	for _, candidate := range candidates {
		candidate = filepath.Clean(expandTilde(candidate))
		if !filepath.IsAbs(candidate) {
			continue
		}
		real, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		real = filepath.Clean(real)
		if real == home || !PathWithin(real, home) || seen[real] {
			continue
		}
		seen[real] = true
		grants = append(grants, real)
	}
	return grants
}
