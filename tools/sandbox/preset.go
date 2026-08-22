package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
)

// PresetNames lists the valid components of a sandbox preset spec, for help
// text and error messages.
var PresetNames = []string{"base", "readonly", "workspace", "net"}

// ParsePreset builds a Config from a preset spec: one or more preset names
// joined with "+" (e.g. "workspace+net"). Components merge onto the base
// config, so every spec keeps temp-dir writes unless readonly denies them.
//
//	base      — the default sandbox: temp-dir writes only, no network
//	readonly  — deny all writes, including temp (analysis only)
//	workspace — the working directory is writable, with every discovered Git
//	            metadata directory carved back out as read-only so a sandboxed
//	            tool can't replace repository routing or plant host-side hooks
//	net       — allow outbound network
//
// An empty spec is the base config. Unknown names error so a typo fails
// closed instead of silently running with a different policy.
func ParsePreset(spec string) (Config, error) {
	cfg := DefaultConfig()
	if strings.TrimSpace(spec) == "" {
		return cfg, nil
	}
	for _, part := range strings.Split(spec, "+") {
		switch strings.TrimSpace(part) {
		case "base":
			// the starting point; nothing to add
		case "readonly":
			cfg.DenyWrite = true
		case "net":
			cfg.AllowNetwork = true
		case "workspace":
			cwd, err := os.Getwd()
			if err != nil {
				return Config{}, fmt.Errorf("sandbox preset %q: resolve working directory: %w", part, err)
			}
			gitPolicy, err := gitWorkspaceGuardrailPolicy(cwd)
			if err != nil {
				return Config{}, fmt.Errorf("sandbox preset %q: protect Git metadata: %w", part, err)
			}
			cfg.WritablePaths = append(cfg.WritablePaths, cwd)
			cfg.DenyWritePaths = append(cfg.DenyWritePaths, gitPolicy.protected...)
			cfg.gitPolicies = append(cfg.gitPolicies, gitPolicy)
		default:
			return Config{}, fmt.Errorf("unknown sandbox preset %q (valid: %s, joined with +)",
				strings.TrimSpace(part), strings.Join(PresetNames, ", "))
		}
	}
	return cfg, nil
}

// gitGuardrailPaths discovers Git repositories rooted anywhere under dir and
// returns the routing entry plus every metadata directory it can reach. The
// complete metadata directories are read-only, rather than only today's known
// hook/config leaves: otherwise an attacker can move .git aside, create a new
// one, use config.worktree, or exploit a missing protected leaf.
//
// A .git file (linked worktree or submodule) is followed via its "gitdir:"
// pointer. A linked worktree's commondir is followed as well. The routing file,
// per-worktree gitdir, commondir pointer, and common gitdir are all covered.
// Nested repositories are included because their routing entries also sit
// inside the writable workspace.
//
// Symlinked .git routing entries fail closed. Linux bind mounts follow the
// symlink and cannot pin the link itself, so allowing one would let a tool
// replace the routing entry while leaving the resolved target protected.
func gitGuardrailPaths(dir string) ([]string, error) {
	policy, err := gitWorkspaceGuardrailPolicy(dir)
	if err != nil {
		return nil, err
	}
	return append([]string(nil), policy.protected...), nil
}

func gitWorkspaceGuardrailPolicy(dir string) (gitWorkspacePolicy, error) {
	var paths []string
	var repositories []gitRepositoryContext
	seen := make(map[string]bool)
	add := func(path string) error {
		real, err := filepath.EvalSymlinks(filepath.Clean(path))
		if err != nil {
			return fmt.Errorf("resolve protected Git path %q: %w", path, err)
		}
		if !seen[real] {
			seen[real] = true
			paths = append(paths, real)
		}
		return nil
	}

	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("scan %q: %w", path, walkErr)
		}
		isGitEntry := strings.EqualFold(entry.Name(), ".git")
		if entry.IsDir() && !isGitEntry {
			bare, err := looksLikeBareGitRepository(path)
			if err != nil {
				return err
			}
			if bare {
				if path == filepath.Clean(dir) {
					return fmt.Errorf("working directory %q is a bare Git repository and cannot be made writable safely", path)
				}
				if err := rejectSymlinkedGitMetadata(path); err != nil {
					return err
				}
				if err := add(path); err != nil {
					return err
				}
				repositories = append(repositories, gitRepositoryContext{
					workTree: path,
					gitDir:   path,
					bare:     true,
				})
				return filepath.SkipDir
			}
		}
		if !isGitEntry {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("Git routing entry %q is a symlink and cannot be pinned safely", path)
		}

		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect Git routing entry %q: %w", path, err)
		}
		if err := add(path); err != nil {
			return err
		}

		gitDir := path
		switch {
		case info.IsDir():
			// Do not scan object databases and nested module metadata. Checked-out
			// submodules have their own .git routing file in the worktree and will
			// be found by the outer walk.
		case info.Mode().IsRegular():
			if hasMultipleLinks(info) {
				return fmt.Errorf("Git routing entry %q has multiple hard links and cannot be pinned safely", path)
			}
			target, err := readGitPointer(path, "gitdir:")
			if err != nil {
				return fmt.Errorf("read Git routing entry %q: %w", path, err)
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(path), target)
			}
			gitDir = filepath.Clean(target)
		default:
			return fmt.Errorf("Git routing entry %q is neither a directory nor a regular file", path)
		}

		gitDir, err = resolveGitDir(gitDir)
		if err != nil {
			return err
		}
		if err := rejectSymlinkedGitMetadata(gitDir); err != nil {
			return err
		}
		if err := add(gitDir); err != nil {
			return err
		}
		repositories = append(repositories, gitRepositoryContext{
			workTree: filepath.Dir(path),
			gitDir:   gitDir,
		})

		commonPointer := filepath.Join(gitDir, "commondir")
		commonInfo, err := os.Lstat(commonPointer)
		switch {
		case err == nil:
			if commonInfo.Mode()&os.ModeSymlink != 0 || !commonInfo.Mode().IsRegular() {
				return fmt.Errorf("Git commondir pointer %q is not a regular file", commonPointer)
			}
			if hasMultipleLinks(commonInfo) {
				return fmt.Errorf("Git commondir pointer %q has multiple hard links and cannot be pinned safely", commonPointer)
			}
			common, err := readGitPointer(commonPointer, "")
			if err != nil {
				return fmt.Errorf("read Git commondir pointer %q: %w", commonPointer, err)
			}
			if !filepath.IsAbs(common) {
				common = filepath.Join(gitDir, common)
			}
			common, err = resolveGitDir(filepath.Clean(common))
			if err != nil {
				return err
			}
			if err := rejectSymlinkedGitMetadata(common); err != nil {
				return err
			}
			// The pointer is already beneath the protected per-worktree gitdir,
			// but keeping it explicit documents and tests the routing invariant.
			if err := add(commonPointer); err != nil {
				return err
			}
			if err := add(common); err != nil {
				return err
			}
		case os.IsNotExist(err):
			// Ordinary repositories and submodule gitdirs have no commondir.
		case err != nil:
			return fmt.Errorf("inspect Git commondir pointer %q: %w", commonPointer, err)
		}

		if info.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return gitWorkspacePolicy{}, err
	}
	protected := minimalGitGuardrailPaths(paths)
	if err := rejectEffectiveHooksPaths(dir, repositories, protected); err != nil {
		return gitWorkspacePolicy{}, err
	}
	return gitWorkspacePolicy{
		workspace:    filepath.Clean(dir),
		repositories: repositories,
		protected:    protected,
	}, nil
}

type gitRepositoryContext struct {
	workTree string
	gitDir   string
	bare     bool
}

// gitWorkspacePolicy is the immutable result of the workspace preset's Git
// discovery pass. Config keeps it private and carries it through Merge so New
// can re-run the comparatively cheap config/hook audit after all writable path
// overlays are known, without walking every repository once per tool.
type gitWorkspacePolicy struct {
	workspace    string
	repositories []gitRepositoryContext
	protected    []string
}

// rejectEffectiveHooksPaths closes the remaining hook-planting route from
// trusted global/system Git config. Protecting repository-local config and
// .git/hooks is not enough when an existing core.hooksPath redirects Git to a
// writable worktree directory such as .githooks. Git itself is used only as a
// config resolver here so conditional includes and precedence match the host's
// next Git invocation; no repository hook or worktree command is executed.
func rejectEffectiveHooksPaths(workspace string, repositories []gitRepositoryContext, protected []string) error {
	if len(repositories) == 0 {
		return nil
	}
	writableRoots, err := gitAuditWritableRoots(workspace)
	if err != nil {
		return err
	}
	return rejectEffectiveHooksPathsWithWritableRoots(repositories, protected, writableRoots)
}

func rejectEffectiveHooksPathsWithWritableRoots(repositories []gitRepositoryContext, protected, writableRoots []string) error {
	if len(repositories) == 0 {
		return nil
	}
	gitPath, err := trustedGitExecutable(writableRoots)
	if err != nil {
		return err
	}

	for _, repository := range repositories {
		if err := rejectWritableGitConfigSources(gitPath, repository, writableRoots, protected); err != nil {
			return err
		}

		hooksPath, configured, err := effectiveHooksPath(gitPath, repository)
		if err != nil {
			return err
		}
		if !configured {
			continue
		}
		if err := validateConfiguredHooksPath(hooksPath, "effective core.hooksPath", writableRoots, protected); err != nil {
			return fmt.Errorf("repository %q: %w", repository.workTree, err)
		}
	}
	return nil
}

// trustedGitExecutable resolves PATH without executing it, then accepts only
// the fixed OS Git or a standard Homebrew Git installation on Darwin. The
// latter must use Homebrew's direct bin/git -> Cellar/git/<version>/bin/git
// route, and neither route nor target may be writable by the eventual sandbox.
// Executing the resolved selected Git matters: its compiled system-config path
// and %(prefix) expansion must match the user's next host Git invocation.
func trustedGitExecutable(writableRoots []string) (string, error) {
	const systemGit = "/usr/bin/git"
	selected, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("resolve PATH-selected Git executable: %w", err)
	}
	return validateTrustedGitExecutable(selected, systemGit, writableRoots, homebrewGitPrefixes())
}

func validateTrustedGitExecutable(selected, systemGit string, writableRoots, homebrewPrefixes []string) (string, error) {
	if !filepath.IsAbs(selected) {
		return "", fmt.Errorf("PATH-selected Git executable %q is not absolute", selected)
	}
	selected = filepath.Clean(selected)
	systemGit = filepath.Clean(systemGit)
	systemInfo, err := os.Stat(systemGit)
	if err != nil {
		return "", fmt.Errorf("inspect trusted Git executable %q: %w", systemGit, err)
	}
	if !systemInfo.Mode().IsRegular() || systemInfo.Mode()&0o111 == 0 {
		return "", fmt.Errorf("trusted Git executable %q is not an executable regular file", systemGit)
	}
	selectedInfo, err := os.Stat(selected)
	if err != nil {
		return "", fmt.Errorf("inspect PATH-selected Git executable %q: %w", selected, err)
	}
	if os.SameFile(selectedInfo, systemInfo) {
		if symlink, err := firstSymlinkComponent(selected); err != nil {
			return "", fmt.Errorf("inspect PATH-selected Git route %q: %w", selected, err)
		} else if symlink != "" {
			return "", fmt.Errorf("PATH-selected Git route %q traverses symlink %q and cannot be trusted persistently", selected, symlink)
		}
		for _, writable := range writableRoots {
			if pathWithinPolicy(selected, writable) {
				return "", fmt.Errorf("PATH-selected Git route %q is inside writable sandbox path %q", selected, writable)
			}
		}
		real, err := filepath.EvalSymlinks(systemGit)
		if err != nil {
			return "", fmt.Errorf("resolve trusted Git executable %q: %w", systemGit, err)
		}
		for _, writable := range writableRoots {
			if pathWithinPolicy(systemGit, writable) || pathWithinPolicy(real, writable) {
				return "", fmt.Errorf("trusted Git executable %q resolves inside writable sandbox path %q", systemGit, writable)
			}
		}
		return real, nil
	}

	homebrewTarget, supported, err := homebrewGitTarget(selected, homebrewPrefixes)
	if err != nil {
		return "", err
	}
	if !supported {
		return "", fmt.Errorf("PATH-selected Git executable %q does not match trusted OS Git %q or a supported Homebrew installation", selected, systemGit)
	}
	targetInfo, err := os.Lstat(homebrewTarget)
	if err != nil {
		return "", fmt.Errorf("inspect Homebrew Git target %q: %w", homebrewTarget, err)
	}
	if !targetInfo.Mode().IsRegular() || targetInfo.Mode()&0o111 == 0 {
		return "", fmt.Errorf("Homebrew Git target %q is not an executable regular file", homebrewTarget)
	}
	if targetInfo.Mode().Perm()&0o222 != 0 {
		return "", fmt.Errorf("Homebrew Git target %q is writable and cannot be trusted", homebrewTarget)
	}
	if targetInfo.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return "", fmt.Errorf("Homebrew Git target %q has privilege bits and cannot be trusted", homebrewTarget)
	}
	if hasMultipleLinks(targetInfo) {
		return "", fmt.Errorf("Homebrew Git target %q has multiple hard links and cannot be trusted", homebrewTarget)
	}
	if !os.SameFile(selectedInfo, targetInfo) {
		return "", fmt.Errorf("Homebrew Git route %q changed while it was inspected", selected)
	}
	for _, writable := range writableRoots {
		if pathWithinPolicy(selected, writable) {
			return "", fmt.Errorf("PATH-selected Git route %q is inside writable sandbox path %q", selected, writable)
		}
		if pathWithinPolicy(homebrewTarget, writable) {
			return "", fmt.Errorf("Homebrew Git target %q is inside writable sandbox path %q", homebrewTarget, writable)
		}
	}
	return homebrewTarget, nil
}

func homebrewGitPrefixes() []string {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return []string{"/opt/homebrew", "/usr/local"}
}

func homebrewGitTarget(selected string, prefixes []string) (string, bool, error) {
	for _, prefix := range prefixes {
		prefix = filepath.Clean(prefix)
		if selected != filepath.Join(prefix, "bin", "git") {
			continue
		}
		symlink, err := firstSymlinkComponent(selected)
		if err != nil {
			return "", false, fmt.Errorf("inspect Homebrew Git route %q: %w", selected, err)
		}
		if symlink != selected {
			if symlink == "" {
				return "", false, fmt.Errorf("Homebrew Git route %q is not a direct Cellar symlink", selected)
			}
			return "", false, fmt.Errorf("Homebrew Git route %q traverses unexpected symlink %q", selected, symlink)
		}
		target, err := os.Readlink(selected)
		if err != nil {
			return "", false, fmt.Errorf("read Homebrew Git route %q: %w", selected, err)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(selected), target)
		}
		target = filepath.Clean(target)
		cellarGit := filepath.Join(prefix, "Cellar", "git")
		rel, err := filepath.Rel(cellarGit, target)
		components := strings.Split(rel, string(filepath.Separator))
		if err != nil || len(components) != 3 || components[0] == "" || components[0] == "." ||
			components[0] == ".." || components[1] != "bin" || components[2] != "git" {
			return "", false, fmt.Errorf("Homebrew Git route %q targets unexpected path %q", selected, target)
		}
		if symlink, err := firstSymlinkComponent(target); err != nil {
			return "", false, fmt.Errorf("inspect Homebrew Git target route %q: %w", target, err)
		} else if symlink != "" {
			return "", false, fmt.Errorf("Homebrew Git target route %q traverses unexpected symlink %q", target, symlink)
		}
		return target, true, nil
	}
	return "", false, nil
}

func firstSymlinkComponent(path string) (string, error) {
	path = filepath.Clean(path)
	volume := filepath.VolumeName(path)
	current := volume + string(filepath.Separator)
	rel := strings.TrimPrefix(path, current)
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return current, nil
		}
	}
	return "", nil
}

func effectiveHooksPath(gitPath string, repository gitRepositoryContext) (string, bool, error) {
	base := gitRepositoryBase(repository)
	output, found, err := runGitConfigQuery(gitPath, repository, "config", "--path", "--get", "core.hooksPath")
	if err != nil {
		return "", false, fmt.Errorf("resolve effective core.hooksPath for repository %q: %w", repository.workTree, err)
	}
	if !found {
		return "", false, nil
	}
	value := strings.TrimSuffix(string(output), "\n")
	value = strings.TrimSuffix(value, "\r")
	if strings.IndexByte(value, 0) >= 0 {
		return "", false, fmt.Errorf("effective core.hooksPath for repository %q contains NUL", repository.workTree)
	}
	if strings.ContainsAny(value, "\r\n") {
		return "", false, fmt.Errorf("effective core.hooksPath for repository %q contains more than one line", repository.workTree)
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(base, value)
	}
	return filepath.Clean(value), true, nil
}

func gitRepositoryBase(repository gitRepositoryContext) string {
	if repository.bare {
		return repository.gitDir
	}
	return repository.workTree
}

func runGitConfigQuery(gitPath string, repository gitRepositoryContext, args ...string) ([]byte, bool, error) {
	gitArgs := []string{"--git-dir=" + repository.gitDir}
	if !repository.bare {
		gitArgs = append(gitArgs, "--work-tree="+repository.workTree)
	}
	gitArgs = append(gitArgs, args...)
	cmd := exec.Command(gitPath, gitArgs...)
	cmd.Dir = gitRepositoryBase(repository)
	cmd.Env = gitAuditEnvironment()
	output, err := cmd.Output()
	if err == nil {
		return output, true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if exitErr.ExitCode() == 1 {
			return nil, false, nil
		}
		stderr := strings.TrimSpace(string(exitErr.Stderr))
		if stderr != "" {
			return nil, false, fmt.Errorf("%w: %s", err, stderr)
		}
	}
	return nil, false, err
}

// gitAuditEnvironment keeps the config selectors an ordinary host Git command
// would honor while dropping repository routing, pager, editor, and executable
// lookup variables. Bare GIT_CONFIG is intentionally omitted: it only changes
// `git config`, so preserving it would make this audit inspect a different
// source than the user's later commit/checkout command.
func gitAuditEnvironment() []string {
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		allowed := name == "HOME" || name == "XDG_CONFIG_HOME" ||
			name == "GIT_CONFIG_GLOBAL" || name == "GIT_CONFIG_SYSTEM" ||
			name == "GIT_CONFIG_NOSYSTEM" || name == "GIT_CONFIG_COUNT" ||
			name == "GIT_CONFIG_PARAMETERS" || strings.HasPrefix(name, "GIT_CONFIG_KEY_") ||
			strings.HasPrefix(name, "GIT_CONFIG_VALUE_")
		if allowed {
			env = append(env, entry)
		}
	}
	return append(env, "LC_ALL=C")
}

func gitAuditWritableRoots(workspace string) ([]string, error) {
	workspace = filepath.Clean(workspace)
	realWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace for Git hook protection %q: %w", workspace, err)
	}
	roots := []string{realWorkspace}
	// Linux replaces host temp directories with private tmpfs mounts. Seatbelt
	// instead grants writes to the host's temp trees, so a config or hook there
	// remains a persistent host-side planting route on Darwin.
	if runtime.GOOS == "darwin" {
		roots = append(roots, "/private/tmp")
		if temp := os.TempDir(); temp != "" {
			roots = append(roots, temp)
		}
	}
	for i, root := range roots {
		if !filepath.IsAbs(root) {
			return nil, fmt.Errorf("Git audit writable path %q is not absolute", root)
		}
		roots[i] = filepath.Clean(root)
	}
	return roots, nil
}

// applyFinalGitPolicy re-audits the immutable repository contexts discovered
// by ParsePreset after every CLI and per-tool Config overlay has been merged
// and normalized. A later WritablePaths grant must not make a global config,
// include, or configured hook target mutable behind the preset's back.
func applyFinalGitPolicy(cfg Config) (Config, error) {
	return applyFinalGitPolicyWithHostWritable(cfg, gitHostWritablePath)
}

func applyFinalGitPolicyWithHostWritable(cfg Config, hostWritable func(string) bool) (Config, error) {
	if cfg.DenyWrite || len(cfg.gitPolicies) == 0 {
		return cfg, nil
	}
	for _, policy := range cfg.gitPolicies {
		writableRoots, err := gitAuditWritableRootsForConfig(policy.workspace, cfg.WritablePaths, hostWritable)
		if err != nil {
			return Config{}, fmt.Errorf("revalidate workspace Git policy for %q: %w", policy.workspace, err)
		}
		protected := concatStrings(policy.protected, cfg.DenyWritePaths)
		if err := rejectEffectiveHooksPathsWithWritableRoots(policy.repositories, protected, writableRoots); err != nil {
			return Config{}, fmt.Errorf("revalidate workspace Git policy for %q after sandbox config merge: %w", policy.workspace, err)
		}
	}
	return cfg, nil
}

func gitAuditWritableRootsForConfig(workspace string, configured []string, hostWritable func(string) bool) ([]string, error) {
	roots, err := gitAuditWritableRoots(workspace)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(roots)+len(configured))
	for _, root := range roots {
		seen[filepath.Clean(root)] = true
	}
	for _, root := range configured {
		root = filepath.Clean(root)
		if !filepath.IsAbs(root) {
			return nil, fmt.Errorf("Git audit writable path %q is not absolute", root)
		}
		if !hostWritable(root) || seen[root] {
			continue
		}
		seen[root] = true
		roots = append(roots, root)
	}
	return roots, nil
}

func rejectWritableGitConfigSources(gitPath string, repository gitRepositoryContext, writableRoots, protected []string) error {
	base := gitRepositoryBase(repository)
	selectors, err := gitConfigSelectorPaths(gitPath, base)
	if err != nil {
		return fmt.Errorf("resolve Git config selectors for repository %q: %w", repository.workTree, err)
	}
	configFiles := make([]string, 0, len(selectors))
	for _, selector := range selectors {
		if err := rejectWritableGitPolicyPath(selector.path, selector.kind, writableRoots, protected); err != nil {
			return fmt.Errorf("repository %q: %w", repository.workTree, err)
		}
		configFiles = append(configFiles, selector.path)
	}

	output, _, err := runGitConfigQuery(gitPath, repository,
		"config", "--includes", "--show-origin", "--name-only", "--null", "--list")
	if err != nil {
		return fmt.Errorf("resolve active Git config origins for repository %q: %w", repository.workTree, err)
	}
	origins, err := parseNULPairs(output, "active Git config origins")
	if err != nil {
		return fmt.Errorf("repository %q: %w", repository.workTree, err)
	}
	for _, pair := range origins {
		originPath, ok := strings.CutPrefix(pair.first, "file:")
		if !ok {
			continue
		}
		originPath, err = resolveGitConfigPath(originPath, base)
		if err != nil {
			return fmt.Errorf("resolve active Git config origin %q: %w", pair.first, err)
		}
		if err := rejectWritableGitPolicyPath(originPath, "active Git config source", writableRoots, protected); err != nil {
			return fmt.Errorf("repository %q: %w", repository.workTree, err)
		}
		configFiles = append(configFiles, originPath)
	}

	output, found, err := runGitConfigQuery(gitPath, repository,
		"config", "--includes", "--show-origin", "--null", "--path", "--get-regexp", `^include(if\..*)?\.path$`)
	if err != nil {
		return fmt.Errorf("resolve Git config includes for repository %q: %w", repository.workTree, err)
	}
	if found {
		includes, err := parseNULPairs(output, "Git config includes")
		if err != nil {
			return fmt.Errorf("repository %q: %w", repository.workTree, err)
		}
		for _, pair := range includes {
			_, includePath, ok := strings.Cut(pair.second, "\n")
			if !ok || includePath == "" || strings.ContainsAny(includePath, "\r\n") {
				return fmt.Errorf("repository %q: malformed Git config include record", repository.workTree)
			}
			includeBase := base
			if originPath, fileOrigin := strings.CutPrefix(pair.first, "file:"); fileOrigin {
				originPath, err = resolveGitConfigPath(originPath, base)
				if err != nil {
					return fmt.Errorf("resolve Git config include origin %q: %w", pair.first, err)
				}
				includeBase = filepath.Dir(originPath)
			}
			includePath, err = resolveGitConfigPath(includePath, includeBase)
			if err != nil {
				return fmt.Errorf("resolve Git config include path %q: %w", includePath, err)
			}
			if err := rejectWritableGitPolicyPath(includePath, "Git config include", writableRoots, protected); err != nil {
				return fmt.Errorf("repository %q: %w", repository.workTree, err)
			}
			configFiles = append(configFiles, includePath)
		}
	}
	if err := inspectGitConfigFiles(gitPath, repository, configFiles, writableRoots, protected); err != nil {
		return fmt.Errorf("repository %q: %w", repository.workTree, err)
	}
	return nil
}

type namedGitConfigPath struct {
	kind string
	path string
}

func gitConfigSelectorPaths(gitPath, base string) ([]namedGitConfigPath, error) {
	var paths []namedGitConfigPath
	global, globalSet := os.LookupEnv("GIT_CONFIG_GLOBAL")
	if globalSet {
		if global == "" {
			return nil, fmt.Errorf("GIT_CONFIG_GLOBAL is empty")
		}
		path, err := resolveGitConfigPath(global, base)
		if err != nil {
			return nil, err
		}
		paths = append(paths, namedGitConfigPath{kind: "global Git config source", path: path})
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home Git config: %w", err)
		}
		if !filepath.IsAbs(home) {
			return nil, fmt.Errorf("home directory %q is not absolute", home)
		}
		paths = append(paths, namedGitConfigPath{kind: "global Git config source", path: filepath.Join(home, ".gitconfig")})
		xdg := os.Getenv("XDG_CONFIG_HOME")
		if xdg == "" {
			xdg = filepath.Join(home, ".config")
		} else if !filepath.IsAbs(xdg) {
			return nil, fmt.Errorf("XDG_CONFIG_HOME %q is not absolute", xdg)
		}
		paths = append(paths, namedGitConfigPath{kind: "global Git config source", path: filepath.Join(xdg, "git", "config")})
	}

	system, systemSet := os.LookupEnv("GIT_CONFIG_SYSTEM")
	if systemSet {
		// Explicit GIT_CONFIG_SYSTEM overrides GIT_CONFIG_NOSYSTEM in Git.
		if system == "" {
			return nil, fmt.Errorf("GIT_CONFIG_SYSTEM is empty")
		}
		path, err := resolveGitConfigPath(system, base)
		if err != nil {
			return nil, err
		}
		paths = append(paths, namedGitConfigPath{kind: "system Git config source", path: path})
	} else {
		noSystem, noSystemSet := os.LookupEnv("GIT_CONFIG_NOSYSTEM")
		if !noSystemSet || !gitConfigBooleanTrue(noSystem) {
			path, err := compiledSystemGitConfigPath(gitPath)
			if err != nil {
				return nil, err
			}
			if path != "" {
				paths = append(paths, namedGitConfigPath{kind: "system Git config source", path: path})
			}
		}
	}
	return paths, nil
}

func compiledSystemGitConfigPath(gitPath string) (string, error) {
	cmd := exec.Command(gitPath, "var", "GIT_CONFIG_SYSTEM")
	cmd.Dir = string(filepath.Separator)
	cmd.Env = gitFileAuditEnvironment()
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", nil
		}
		return "", fmt.Errorf("resolve compiled system Git config path: %w", err)
	}
	value := strings.TrimSuffix(string(output), "\n")
	value = strings.TrimSuffix(value, "\r")
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("compiled system Git config path is empty or malformed")
	}
	if !filepath.IsAbs(value) {
		return "", fmt.Errorf("compiled system Git config path %q is not absolute", value)
	}
	return filepath.Clean(value), nil
}

func gitConfigBooleanTrue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		// Invalid and false values both audit the system selector. Git will
		// reject an invalid value later; auditing conservatively here prevents
		// a missing system file from slipping through first.
		return false
	}
}

func resolveGitConfigPath(path, base string) (string, error) {
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return "", fmt.Errorf("invalid empty or NUL-containing path")
	}
	path = expandTilde(path)
	if strings.HasPrefix(path, "~") {
		return "", fmt.Errorf("cannot resolve home-relative path %q", path)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	return filepath.Clean(path), nil
}

type gitConfigPair struct {
	first  string
	second string
}

func parseNULPairs(output []byte, description string) ([]gitConfigPair, error) {
	if len(output) == 0 {
		return nil, nil
	}
	if output[len(output)-1] != 0 {
		return nil, fmt.Errorf("%s output is not NUL terminated", description)
	}
	records := strings.Split(string(output[:len(output)-1]), "\x00")
	if len(records)%2 != 0 {
		return nil, fmt.Errorf("%s output has an incomplete record", description)
	}
	pairs := make([]gitConfigPair, 0, len(records)/2)
	for i := 0; i < len(records); i += 2 {
		if records[i] == "" || records[i+1] == "" {
			return nil, fmt.Errorf("%s output has an empty record", description)
		}
		pairs = append(pairs, gitConfigPair{first: records[i], second: records[i+1]})
	}
	return pairs, nil
}

// inspectGitConfigFiles reads only the hook/include keys from each selected
// file with includes disabled, then follows every include and includeIf path
// itself. This intentionally ignores includeIf conditions: a currently
// inactive file may become active after the user changes branch or worktree,
// so its hook path must be safe before the sandbox can start.
func inspectGitConfigFiles(gitPath string, repository gitRepositoryContext, initial []string, writableRoots, protected []string) error {
	queue := append([]string(nil), initial...)
	seen := make(map[string]bool)
	devNullInfo, _ := os.Stat("/dev/null")
	inspected := 0
	for len(queue) > 0 {
		configPath := filepath.Clean(queue[0])
		queue = queue[1:]
		if seen[configPath] {
			continue
		}
		seen[configPath] = true
		inspected++
		if inspected > 256 {
			return fmt.Errorf("Git config include graph exceeds 256 distinct paths")
		}
		if err := rejectWritableGitPolicyPath(configPath, "Git config source", writableRoots, protected); err != nil {
			return err
		}
		info, err := os.Stat(configPath)
		if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect Git config source %q: %w", configPath, err)
		}
		if devNullInfo != nil && os.SameFile(info, devNullInfo) {
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("Git config source %q is not a regular file", configPath)
		}
		if hasMultipleLinks(info) {
			return fmt.Errorf("Git config source %q has multiple hard links and cannot be pinned safely", configPath)
		}
		output, found, err := runGitFileConfigQuery(gitPath, repository, configPath,
			"--path", "--null", "--get-all", "core.hooksPath")
		if err != nil {
			return fmt.Errorf("inspect core.hooksPath in Git config %q: %w", configPath, err)
		}
		if found {
			values, err := parseNULRecords(output, "core.hooksPath values", true)
			if err != nil {
				return fmt.Errorf("Git config %q: %w", configPath, err)
			}
			for _, value := range values {
				if strings.ContainsAny(value, "\r\n") {
					return fmt.Errorf("Git config %q has a multiline core.hooksPath", configPath)
				}
				hooksPath := value
				if !filepath.IsAbs(hooksPath) {
					hooksPath = filepath.Join(gitRepositoryBase(repository), hooksPath)
				}
				if err := validateConfiguredHooksPath(filepath.Clean(hooksPath), "possible core.hooksPath", writableRoots, protected); err != nil {
					return fmt.Errorf("Git config %q: %w", configPath, err)
				}
			}
		}

		output, found, err = runGitFileConfigQuery(gitPath, repository, configPath,
			"--path", "--null", "--get-regexp", `^include(if\..*)?\.path$`)
		if err != nil {
			return fmt.Errorf("inspect includes in Git config %q: %w", configPath, err)
		}
		if !found {
			continue
		}
		records, err := parseNULRecords(output, "Git config include values", false)
		if err != nil {
			return fmt.Errorf("Git config %q: %w", configPath, err)
		}
		for _, record := range records {
			_, includePath, ok := strings.Cut(record, "\n")
			if !ok || includePath == "" || strings.ContainsAny(includePath, "\r\n") {
				return fmt.Errorf("Git config %q has a malformed include record", configPath)
			}
			includePath, err = resolveGitConfigPath(includePath, filepath.Dir(configPath))
			if err != nil {
				return fmt.Errorf("resolve include in Git config %q: %w", configPath, err)
			}
			if err := rejectWritableGitPolicyPath(includePath, "Git config include", writableRoots, protected); err != nil {
				return err
			}
			queue = append(queue, includePath)
		}
	}
	return nil
}

func runGitFileConfigQuery(gitPath string, repository gitRepositoryContext, configPath string, args ...string) ([]byte, bool, error) {
	gitArgs := []string{"config", "--file", configPath, "--no-includes"}
	gitArgs = append(gitArgs, args...)
	cmd := exec.Command(gitPath, gitArgs...)
	// An explicit --file query does not need repository discovery. Run from
	// the filesystem root so a deliberately minimal/fake .git entry in a unit
	// fixture (or a damaged worktree in real use) cannot affect direct parsing.
	cmd.Dir = filepath.VolumeName(configPath) + string(filepath.Separator)
	cmd.Env = gitFileAuditEnvironment()
	output, err := cmd.Output()
	if err == nil {
		return output, true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if exitErr.ExitCode() == 1 {
			return nil, false, nil
		}
		stderr := strings.TrimSpace(string(exitErr.Stderr))
		if stderr != "" {
			return nil, false, fmt.Errorf("%w: %s", err, stderr)
		}
	}
	return nil, false, err
}

func gitFileAuditEnvironment() []string {
	env := []string{"LC_ALL=C"}
	if home, ok := os.LookupEnv("HOME"); ok {
		env = append(env, "HOME="+home)
	}
	return env
}

func parseNULRecords(output []byte, description string, allowEmpty bool) ([]string, error) {
	if len(output) == 0 {
		return nil, nil
	}
	if output[len(output)-1] != 0 {
		return nil, fmt.Errorf("%s output is not NUL terminated", description)
	}
	records := strings.Split(string(output[:len(output)-1]), "\x00")
	if !allowEmpty {
		for _, record := range records {
			if record == "" {
				return nil, fmt.Errorf("%s output has an empty record", description)
			}
		}
	}
	return records, nil
}

func rejectWritableGitPolicyPath(path, kind string, writableRoots, protected []string) error {
	resolved, err := resolveExistingPathPrefix(path)
	if err != nil {
		return fmt.Errorf("resolve %s %q: %w", kind, path, err)
	}
	for _, writable := range writableRoots {
		if !pathWithinPolicy(path, writable) && !pathWithinPolicy(resolved, writable) {
			continue
		}
		// Only the direct protected route is safe. A writable symlink elsewhere
		// in the workspace that happens to resolve into .git must remain a
		// rejection because the tool can retarget that alias after discovery.
		directPath := normalizeTrustedGitPolicyAlias(path)
		if pathLexicallyProtectedByGitGuardrail(directPath, protected) && pathProtectedByGitGuardrail(resolved, protected) {
			return nil
		}
		return fmt.Errorf("%s %q resolves inside writable sandbox path %q and cannot be pinned safely", kind, path, writable)
	}
	return nil
}

func validateConfiguredHooksPath(path, kind string, writableRoots, protected []string) error {
	if err := rejectWritableGitPolicyPath(path, kind, writableRoots, protected); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s %q: %w", kind, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s %q is a symlink and cannot be pinned safely", kind, path)
	}
	// Git documents /dev/null as a way to disable hooks. It is an immutable
	// external device on supported hosts, not a hook directory, and the
	// writable-path check above already rejects any unsafe alias spelling.
	if devNullInfo, statErr := os.Stat("/dev/null"); statErr == nil && os.SameFile(info, devNullInfo) {
		return nil
	}
	if !info.IsDir() {
		return fmt.Errorf("%s %q is not a directory", kind, path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("inspect %s directory %q: %w", kind, path, err)
	}
	for _, entry := range entries {
		hookPath := filepath.Join(path, entry.Name())
		hookInfo, err := os.Lstat(hookPath)
		if err != nil {
			return fmt.Errorf("inspect configured Git hook %q: %w", hookPath, err)
		}
		if hookInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("configured Git hook %q is a symlink and cannot be pinned safely", hookPath)
		}
		if hookInfo.Mode().IsRegular() && hasMultipleLinks(hookInfo) {
			return fmt.Errorf("configured Git hook %q has multiple hard links and cannot be pinned safely", hookPath)
		}
	}
	return nil
}

// normalizeTrustedGitPolicyAlias accounts only for the immutable top-level
// aliases installed by macOS. Resolving arbitrary workspace symlinks here
// would incorrectly turn a retargetable alias into a protected direct route.
func normalizeTrustedGitPolicyAlias(path string) string {
	for alias, target := range map[string]string{
		"/etc": "/private/etc",
		"/tmp": "/private/tmp",
		"/var": "/private/var",
	} {
		if !pathLexicallyWithinPolicy(path, alias) {
			continue
		}
		info, err := os.Lstat(alias)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		real, err := filepath.EvalSymlinks(alias)
		if err != nil || filepath.Clean(real) != target {
			continue
		}
		rel, err := filepath.Rel(alias, path)
		if err == nil {
			return filepath.Join(target, rel)
		}
	}
	return path
}

// resolveExistingPathPrefix resolves symlinks component by component while
// preserving missing trailing entries. Reading link targets directly matters
// for a dangling external link into the workspace: EvalSymlinks alone loses
// that target precisely until a sandboxed tool creates it.
func resolveExistingPathPrefix(path string) (string, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q is not absolute", path)
	}
	root, pending := absolutePathComponents(path)
	resolved := root
	symlinks := 0
	for len(pending) > 0 {
		component := pending[0]
		pending = pending[1:]
		candidate := filepath.Join(resolved, component)
		info, err := os.Lstat(candidate)
		if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
			for _, trailing := range pending {
				candidate = filepath.Join(candidate, trailing)
			}
			return filepath.Clean(candidate), nil
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			resolved = candidate
			continue
		}
		symlinks++
		if symlinks > 255 {
			return "", fmt.Errorf("too many symlinks while resolving %q", path)
		}
		target, err := os.Readlink(candidate)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(candidate), target)
		}
		for _, trailing := range pending {
			target = filepath.Join(target, trailing)
		}
		root, pending = absolutePathComponents(filepath.Clean(target))
		resolved = root
	}
	return filepath.Clean(resolved), nil
}

func absolutePathComponents(path string) (string, []string) {
	volume := filepath.VolumeName(path)
	root := volume + string(filepath.Separator)
	rel := strings.TrimPrefix(path, root)
	components := strings.Split(rel, string(filepath.Separator))
	filtered := components[:0]
	for _, component := range components {
		if component != "" && component != "." {
			filtered = append(filtered, component)
		}
	}
	return root, filtered
}

func pathWithinPolicy(path, parent string) bool {
	if pathLexicallyWithinPolicy(path, parent) {
		return true
	}
	parentInfo, err := os.Stat(filepath.Clean(parent))
	if err != nil {
		return false
	}
	if existingAncestorHasIdentity(path, parentInfo) {
		return true
	}
	// Resolve an existing symlink prefix as well. This catches a missing path
	// reached through an external symlink into the workspace, while SameFile
	// makes case-varied spellings work only on filesystems that actually alias
	// those names (for example default APFS, but not a case-sensitive volume).
	resolved, err := resolveExistingPathPrefix(path)
	return err == nil && resolved != filepath.Clean(path) && existingAncestorHasIdentity(resolved, parentInfo)
}

func pathLexicallyWithinPolicy(path, parent string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func existingAncestorHasIdentity(path string, parentInfo fs.FileInfo) bool {
	current := filepath.Clean(path)
	for {
		info, err := os.Stat(current)
		if err == nil {
			if os.SameFile(info, parentInfo) {
				return true
			}
		} else if !os.IsNotExist(err) && !errors.Is(err, syscall.ENOTDIR) {
			return false
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
		current = parent
	}
}

func pathProtectedByGitGuardrail(path string, protected []string) bool {
	for _, parent := range protected {
		if pathWithinPolicy(path, parent) {
			return true
		}
	}
	return false
}

func pathLexicallyProtectedByGitGuardrail(path string, protected []string) bool {
	for _, parent := range protected {
		if pathLexicallyWithinPolicy(path, parent) {
			return true
		}
	}
	return false
}

// minimalGitGuardrailPaths drops a protected path already covered by a
// protected ancestor. Besides keeping profiles small, this prevents a backend
// from accidentally re-binding a read-only parent writable while trying to
// pin a redundant protected child (for example .git plus .git/modules/sub).
func minimalGitGuardrailPaths(paths []string) []string {
	minimal := make([]string, 0, len(paths))
	for i, path := range paths {
		covered := false
		for j, other := range paths {
			if i == j || path == other {
				continue
			}
			rel, err := filepath.Rel(other, path)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				covered = true
				break
			}
		}
		if !covered {
			minimal = append(minimal, path)
		}
	}
	return minimal
}

// readGitPointer reads a single-line git pointer file (a .git file's
// "gitdir: <path>" or a worktree's commondir) and returns the path after
// stripping prefix. Pointer files are deliberately strict: extra non-empty
// lines are rejected so an ambiguous routing entry cannot weaken the policy.
func readGitPointer(path, prefix string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	line, rest, _ := strings.Cut(string(data), "\n")
	if strings.TrimSpace(rest) != "" {
		return "", fmt.Errorf("contains more than one non-empty line")
	}
	if prefix != "" {
		var ok bool
		if line, ok = strings.CutPrefix(line, prefix); !ok {
			return "", fmt.Errorf("does not start with %q", prefix)
		}
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", fmt.Errorf("contains an empty path")
	}
	return line, nil
}

func resolveGitDir(path string) (string, error) {
	if symlink, err := unsafeGitRoutingSymlink(path); err != nil {
		return "", err
	} else if symlink != "" {
		return "", fmt.Errorf("Git routing path %q traverses symlink %q and cannot be pinned safely", path, symlink)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve Git metadata directory %q: %w", path, err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("inspect Git metadata directory %q: %w", real, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("Git metadata path %q is not a directory", real)
	}
	return filepath.Clean(real), nil
}

// unsafeGitRoutingSymlink reports a symlink traversed by a gitdir/commondir
// pointer. Resolving only the final target is not enough: a merged tool policy
// could make an external link's parent writable and repoint the same routing
// file to unprotected metadata. The only exceptions are macOS's immutable
// top-level aliases, which are canonical names for the same system trees.
func unsafeGitRoutingSymlink(path string) (string, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("Git routing path %q is not absolute", path)
	}
	volume := filepath.VolumeName(path)
	root := volume + string(filepath.Separator)
	rel := strings.TrimPrefix(path, root)
	current := root
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("inspect Git routing component %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		real, err := filepath.EvalSymlinks(current)
		if err != nil {
			return "", fmt.Errorf("resolve Git routing symlink %q: %w", current, err)
		}
		trusted := map[string]string{
			"/etc": "/private/etc",
			"/tmp": "/private/tmp",
			"/var": "/private/var",
		}
		if trusted[current] != filepath.Clean(real) {
			return current, nil
		}
	}
	return "", nil
}

// rejectSymlinkedGitMetadata rejects the Git-controlled paths that can select
// executable hooks or configuration. A read-only bind of the gitdir protects
// a symlink inode but follows it for data access, so a writable target would
// remain an escape. Rejecting this uncommon layout is the only portable way to
// fail closed without guessing which external target trees may later become
// writable through a merged tool policy.
func rejectSymlinkedGitMetadata(gitDir string) error {
	for _, name := range []string{"config", "config.worktree", "hooks"} {
		path := filepath.Join(gitDir, name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect protected Git metadata %q: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("protected Git metadata %q is a symlink and cannot be pinned safely", path)
		}
		if info.Mode().IsRegular() {
			if hasMultipleLinks(info) {
				return fmt.Errorf("protected Git metadata %q has multiple hard links and cannot be pinned safely", path)
			}
			if name == "config" || name == "config.worktree" {
				if err := rejectGitConfigIndirection(path); err != nil {
					return err
				}
			}
		}
		if name != "hooks" || !info.IsDir() {
			continue
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return fmt.Errorf("inspect protected Git hooks %q: %w", path, err)
		}
		for _, hook := range entries {
			hookPath := filepath.Join(path, hook.Name())
			hookInfo, err := os.Lstat(hookPath)
			if err != nil {
				return fmt.Errorf("inspect protected Git hook %q: %w", hookPath, err)
			}
			if hookInfo.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("protected Git hook %q is a symlink and cannot be pinned safely", hookPath)
			}
			if hookInfo.Mode().IsRegular() && hasMultipleLinks(hookInfo) {
				return fmt.Errorf("protected Git hook %q has multiple hard links and cannot be pinned safely", hookPath)
			}
		}
	}
	return nil
}

// rejectGitConfigIndirection fails closed on repository-local configuration
// that redirects hook discovery or includes another config file. Protecting
// the config inode would not protect a writable hooks/include target selected
// by its existing contents. Resolving Git's full conditional include language
// without invoking repository-aware Git is error-prone, so these uncommon
// layouts must use a tighter non-workspace preset instead.
func rejectGitConfigIndirection(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read protected Git config %q: %w", path, err)
	}
	data = []byte(strings.TrimPrefix(string(data), "\xef\xbb\xbf"))
	section := ""
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				return fmt.Errorf("protected Git config %q has a malformed section header", path)
			}
			name := strings.TrimSpace(line[1:end])
			if cut := strings.IndexAny(name, " \t\""); cut >= 0 {
				name = name[:cut]
			}
			section = strings.ToLower(name)
			continue
		}
		key := line
		if before, _, ok := strings.Cut(key, "="); ok {
			key = before
		} else if fields := strings.Fields(key); len(fields) > 0 {
			key = fields[0]
		}
		key = strings.ToLower(strings.TrimSpace(key))
		switch {
		case section == "core" && key == "hookspath":
			return fmt.Errorf("protected Git config %q sets core.hooksPath, whose target cannot be pinned safely", path)
		case (section == "include" || section == "includeif") && key == "path":
			return fmt.Errorf("protected Git config %q includes another config whose target cannot be pinned safely", path)
		}
	}
	return nil
}

// hasMultipleLinks catches protected config/hook files that also have a
// writable hard-link alias. FileInfo.Sys is platform-specific, so reflection
// keeps this discovery code buildable for unsupported platforms while using
// the Nlink field exposed by both Darwin and Linux stat structures.
func hasMultipleLinks(info fs.FileInfo) bool {
	value := reflect.ValueOf(info.Sys())
	if !value.IsValid() {
		return false
	}
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return false
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return false
	}
	links := value.FieldByName("Nlink")
	switch links.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return links.Uint() > 1
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return links.Int() > 1
	default:
		return false
	}
}

// looksLikeBareGitRepository recognizes the minimum standard bare-repository
// layout without invoking git (which would load repository-controlled config
// before containment exists). Treating every complete candidate layout as bare
// is deliberately conservative: Git accepts several boolean spellings and
// config includes, so hand-parsing only core.bare=true would miss valid repos.
func looksLikeBareGitRepository(dir string) (bool, error) {
	for _, name := range []string{"objects", "refs"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if os.IsNotExist(err) || (err == nil && !info.IsDir()) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("inspect possible bare Git repository %q: %w", dir, err)
		}
	}
	for _, name := range []string{"HEAD", "config"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if os.IsNotExist(err) || (err == nil && !info.Mode().IsRegular()) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("inspect possible bare Git repository %q: %w", dir, err)
		}
	}
	return true, nil
}
