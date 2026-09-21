package sandbox

import (
	"fmt"
	"log/slog"
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
	return unmaskedGrants(existingHomeGrants(home, candidates))
}

// ExistingHomeGrants keeps the candidates that resolve strictly inside the
// home directory and outside the credential deny list: the automatic grants a
// caller adds for directories it uses (skills, caches) need no entry anywhere
// else. Each grant keeps the spelling it was given, so a symlinked directory
// stays reachable by its own name inside the private home.
func ExistingHomeGrants(candidates []string) []string {
	home := resolvedHomeDir()
	if home == "" {
		return nil
	}
	return unmaskedGrants(existingHomeGrants(home, candidates))
}

var (
	homeToolchainMu     sync.Mutex
	homeToolchainByHome = map[string][]string{}
)

// HomeToolchainGrants lists the read-only grants that keep common toolchains
// working while the home directory is private: the user's Git configuration
// with every file it includes and the excludes and attributes files it names,
// every PATH entry under the home directory, widened to the install prefix
// above a bin, sbin or shims entry so the prefix's lib, libexec, include and
// version directories come along (a shared root such as ~/.local is never
// widened to; see pathEntryPrefixes). Every entry exists
// and lies inside the home directory; a missing tool contributes nothing, and
// a candidate inside the credential deny list is dropped rather than granted.
// Policy is computed without running anything but the trusted Git. The
// result is computed once per home directory for the life of the process.
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
	candidates = append(candidates, gitUserConfigGrants()...)
	candidates = append(candidates, pathEntryPrefixes(home)...)
	return minimizePaths(unmaskedGrants(existingHomeGrants(home, candidates)), nil)
}

// gitUserConfigGrants resolves, through the trusted Git only, the path-typed
// settings and the include targets reachable from the user's global and
// system configuration. An untrusted Git yields nothing.
func gitUserConfigGrants() []string {
	git, err := trustedGitExecutable(nil)
	if err != nil {
		slog.Debug("home_toolchain_git_untrusted", "error", err)
		return nil
	}
	cache := newGitAuditQueryCache()
	paths := gitPathSettings(git, "core.excludesFile", "core.attributesFile")
	base, err := os.Getwd()
	if err != nil {
		base = string(filepath.Separator)
	}
	selectors, err := gitConfigSelectorPaths(git, base, cache)
	if err != nil {
		slog.Debug("home_toolchain_git_sources", "error", err)
		return paths
	}
	sources := make([]string, 0, len(selectors))
	for _, selector := range selectors {
		sources = append(sources, selector.path)
	}
	return append(paths, gitConfigIncludeTargets(git, cache, sources)...)
}

// gitPathSettings resolves path-typed Git settings the way the user's own git
// would, from outside any repository. Unset keys yield nothing.
func gitPathSettings(git string, keys ...string) []string {
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

// gitConfigIncludeTargets follows every include and includeIf path from the
// given config files, reading each file directly with includes disabled and
// resolving relative targets against the including file. includeIf conditions
// are ignored: a target that is inactive here may be active in a member's
// checkout. Missing files contribute nothing; the walk stops after 256 files.
func gitConfigIncludeTargets(git string, cache *gitAuditQueryCache, sources []string) []string {
	queue := append([]string(nil), sources...)
	seen := make(map[string]bool)
	var targets []string
	for len(queue) > 0 && len(seen) < 256 {
		configPath := filepath.Clean(queue[0])
		queue = queue[1:]
		if seen[configPath] {
			continue
		}
		seen[configPath] = true
		if info, err := os.Stat(configPath); err != nil || !info.Mode().IsRegular() {
			continue
		}
		output, found, err := runGitFileConfigQuery(git, cache, configPath,
			"--path", "--null", "--get-regexp", `^include(if\..*)?\.path$`)
		if err != nil || !found {
			continue
		}
		records, err := parseNULRecords(output, "Git config include records")
		if err != nil {
			continue
		}
		for _, record := range records {
			_, value, ok := strings.Cut(record, "\n")
			if !ok || value == "" || strings.ContainsAny(value, "\r\n") {
				continue
			}
			target, err := resolveGitConfigPath(value, filepath.Dir(configPath))
			if err != nil {
				continue
			}
			targets = append(targets, target)
			queue = append(queue, target)
		}
	}
	return targets
}

// pathEntryPrefixes lists every absolute PATH entry and, for a bin, sbin or
// shims entry, the install prefix above it: toolchains keep their libraries,
// headers and versioned installs beside the executables. The entry itself is
// listed too, so one directly under the home directory (which is never a
// grant) still gets its own grant. A prefix that is a shared root rather than
// one tool's install is never listed: the home directory, or a directory
// holding an XDG base directory, as ~/.local holds ~/.local/share and
// ~/.local/state, where programs of every kind keep data, history and
// tokens. Such an entry instead contributes the install prefixes its
// symlinked executables resolve to.
func pathEntryPrefixes(home string) []string {
	shared := xdgBaseDirs(home)
	var paths []string
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if entry == "" || !filepath.IsAbs(entry) {
			continue
		}
		entry = filepath.Clean(entry)
		paths = append(paths, entry)
		switch filepath.Base(entry) {
		case "bin", "sbin", "shims":
		default:
			continue
		}
		if prefix := filepath.Dir(entry); !isSharedRoot(prefix, home, shared) {
			paths = append(paths, prefix)
		} else if PathWithin(canonicalOrClean(entry), home) {
			paths = append(paths, linkedInstallPrefixes(entry, home, shared)...)
		}
	}
	return paths
}

// maxLinkedExecutables bounds the scan of one PATH entry for symlinks.
const maxLinkedExecutables = 4096

// linkedInstallPrefixes lists, for each symlink in a PATH entry that resolves
// outside the entry, the install prefix of its target: the directory above
// the bin or sbin directory holding it, else the directory holding it. A
// target whose prefix would be a shared root is listed alone. Targets outside
// the home directory need no grant and are dropped by the caller.
func linkedInstallPrefixes(entry, home string, shared []string) []string {
	dir, err := os.Open(entry)
	if err != nil {
		return nil
	}
	names, _ := dir.Readdirnames(maxLinkedExecutables)
	_ = dir.Close()
	realEntry := canonicalOrClean(entry)
	var prefixes []string
	for _, name := range names {
		link := filepath.Join(entry, name)
		if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		target, err := filepath.EvalSymlinks(link)
		if err != nil {
			continue
		}
		holder := filepath.Dir(target)
		if PathWithin(holder, realEntry) {
			continue
		}
		prefix := holder
		switch filepath.Base(holder) {
		case "bin", "sbin":
			prefix = filepath.Dir(holder)
		}
		if isSharedRoot(prefix, home, shared) {
			prefix = target
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}

// xdgBaseDirs lists the XDG base directories whose ancestors are shared
// roots: the defaults ~/.local/share, ~/.local/state, ~/.config and ~/.cache,
// plus $XDG_DATA_HOME, $XDG_STATE_HOME, $XDG_CONFIG_HOME and $XDG_CACHE_HOME
// when set to absolute paths, since programs use either.
func xdgBaseDirs(home string) []string {
	dirs := []string{
		filepath.Join(home, ".local", "share"),
		filepath.Join(home, ".local", "state"),
		filepath.Join(home, ".config"),
		filepath.Join(home, ".cache"),
	}
	for _, name := range []string{"XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
		if value := os.Getenv(name); filepath.IsAbs(value) {
			dirs = append(dirs, value)
		}
	}
	for i, dir := range dirs {
		dirs[i] = canonicalOrClean(dir)
	}
	return dirs
}

// SharedHomeDir is a directory in the home directory that programs of every
// kind keep their own directories or files in.
type SharedHomeDir struct {
	Path string
	// ProgramDirs marks one where each program keeps a directory of its own,
	// as the XDG base directories and macOS's caches and application support
	// have programs do.
	ProgramDirs bool
}

// SharedHomeDirs lists the shared directories of home, canonical where they
// exist: the XDG base directories with their $XDG_* overrides, ~/.local that
// holds two of them, and macOS's Library folders.
func SharedHomeDirs(home string) []SharedHomeDir {
	var dirs []SharedHomeDir
	for _, dir := range xdgBaseDirs(home) {
		dirs = append(dirs, SharedHomeDir{Path: dir, ProgramDirs: true})
	}
	for _, rel := range []string{"Library/Caches", "Library/Application Support"} {
		dirs = append(dirs, SharedHomeDir{Path: canonicalOrClean(filepath.Join(home, filepath.FromSlash(rel))), ProgramDirs: true})
	}
	for _, rel := range []string{".local", ".local/lib", "Library", "Library/Preferences", "Library/Logs", "Library/Containers", "Library/Group Containers", "Library/Developer"} {
		dirs = append(dirs, SharedHomeDir{Path: canonicalOrClean(filepath.Join(home, filepath.FromSlash(rel)))})
	}
	return dirs
}

// isSharedRoot reports whether a candidate install prefix is the home
// directory or holds one of the XDG base directories.
func isSharedRoot(prefix, home string, shared []string) bool {
	prefix = canonicalOrClean(prefix)
	if prefix == home {
		return true
	}
	for _, dir := range shared {
		if PathWithin(dir, prefix) {
			return true
		}
	}
	return false
}

// canonicalOrClean resolves symlinks in path, or cleans it when it cannot be
// resolved.
func canonicalOrClean(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(real)
	}
	return filepath.Clean(path)
}

// unmaskedGrants drops every candidate the built-in credential deny list
// masks, so a PATH or configuration entry planted inside ~/.ssh never
// becomes a grant that ties with its mask.
func unmaskedGrants(candidates []string) []string {
	kept := candidates[:0]
	for _, candidate := range candidates {
		if ReadMasked(Config{}, candidate) == nil {
			kept = append(kept, candidate)
		}
	}
	return kept
}

// ResolvedHomeDir is the canonical home directory the sandbox keeps private,
// or empty when it cannot serve as a private root.
func ResolvedHomeDir() string { return resolvedHomeDir() }

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

// privateHomeRoot is the canonical home directory the backends keep private.
// A home directory that cannot be resolved, is the filesystem root, or is
// not a directory cannot be kept private and fails sandbox construction on
// every platform, so a misconfigured HOME never silently disables the
// private home.
func privateHomeRoot() (string, error) {
	home := resolvedHomeDir()
	if home == "" {
		return "", fmt.Errorf("sandbox requires a resolvable home directory below the filesystem root to keep private")
	}
	info, err := os.Stat(home)
	if err != nil {
		return "", fmt.Errorf("inspect home directory %q: %w", home, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("home directory %q is not a directory", home)
	}
	return home, nil
}

// existingHomeGrants keeps the candidates that exist strictly inside home,
// spelled as given and without duplicates. The spelling matters: a PATH entry
// ~/bin that links to ~/tools/bin is granted as ~/bin, which the backends
// resolve to cover the target as well, whereas a grant of the target alone
// would leave the link itself hidden inside the private home.
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
		if real == home || !PathWithin(real, home) || seen[candidate] {
			continue
		}
		seen[candidate] = true
		grants = append(grants, candidate)
	}
	return grants
}
