package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
)

// PresetNames lists the valid components of a sandbox preset spec, for help
// text and error messages.
var PresetNames = []string{"base", "readonly", "workspace", "git", "net", "ssh", "sshkeys"}

// gitProtectMode selects how the workspace preset protects discovered Git
// metadata: the whole metadata tree read-only (the historical default), or
// only the leaves that can select host-executed hooks or configuration, so
// ordinary Git write operations (commit, rebase, fetch) work in the sandbox.
type gitProtectMode int

const (
	gitProtectWholeTree gitProtectMode = iota
	gitProtectLeaves
)

// ParsePreset builds a Config from a preset spec: one or more preset names
// joined with "+" (e.g. "workspace+net+git"). Components merge onto the base
// config, so every spec keeps temp-dir writes unless readonly denies them.
//
//	base      — the default sandbox: temp-dir writes only, no network
//	readonly  — deny all writes, including temp (analysis only)
//	workspace — the working directory is writable, with every discovered Git
//	            metadata directory carved back out as read-only so a sandboxed
//	            tool can't replace repository routing or plant host-side hooks;
//	            broad home-directory, filesystem-root, mounted-volume-root, and
//	            platform-private-root workspaces are rejected
//	git       — with workspace: pin only the dangerous Git metadata leaves
//	            (config, config.worktree, hooks, routing and worktree
//	            pointers) instead of whole metadata trees, so commit, rebase,
//	            and fetch work inside the sandbox; requires workspace
//	net       — allow outbound network
//	ssh       — agent-based SSH: pass SSH_AUTH_SOCK through, allow connecting
//	            to exactly that socket, and exempt ~/.ssh/config and
//	            ~/.ssh/known_hosts from the credential deny list; private
//	            keys stay masked and ~/.ssh stays unwritable
//	sshkeys   — exempt all of ~/.ssh from the credential deny list, private
//	            keys included, for agentless setups; writes stay denied
//
// An empty spec is the base config. Unknown names error so a typo fails
// closed instead of silently running with a different policy.
//
// Components are collected before any policy is materialized: Config merging
// is monotonic and cannot remove an earlier entry, so the workspace Git
// policy must be built exactly once with the final protection mode. This also
// makes component order irrelevant ("git+workspace" == "workspace+git").
func ParsePreset(spec string) (Config, error) {
	cfg := DefaultConfig()
	if strings.TrimSpace(spec) == "" {
		return cfg, nil
	}
	var workspaceSelected, gitSelected bool
	for _, part := range strings.Split(spec, "+") {
		switch strings.TrimSpace(part) {
		case "base":
			// the starting point; nothing to add
		case "readonly":
			cfg.DenyWrite = true
		case "net":
			cfg.AllowNetwork = true
		case "workspace":
			workspaceSelected = true
		case "git":
			gitSelected = true
		case "ssh":
			cfg.ReadPaths = append(cfg.ReadPaths, "~/.ssh/config", "~/.ssh/known_hosts")
			// Passing the name through is harmless when the variable is unset;
			// the socket grant itself requires a live socket at construction.
			cfg.PassEnv = append(cfg.PassEnv, "SSH_AUTH_SOCK")
			if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" && filepath.IsAbs(sock) {
				// A relative value is skipped: normalization would cwd-join it
				// into an unintended grant.
				cfg.AllowUnixSockets = append(cfg.AllowUnixSockets, sock)
			}
		case "sshkeys":
			cfg.ReadPaths = append(cfg.ReadPaths, "~/.ssh")
		default:
			return Config{}, fmt.Errorf("unknown sandbox preset %q (valid: %s, joined with +)",
				strings.TrimSpace(part), strings.Join(PresetNames, ", "))
		}
	}
	if gitSelected && !workspaceSelected {
		return Config{}, fmt.Errorf("sandbox preset %q requires %q (e.g. workspace+git): it selects how workspace Git metadata is protected", "git", "workspace")
	}
	if workspaceSelected {
		name := "workspace"
		mode := gitProtectWholeTree
		// Under a global write denial the mode is irrelevant, and whole-tree
		// protection avoids the leaf materializer creating files in .git for
		// a sandbox that cannot write anything anyway.
		if gitSelected && !cfg.DenyWrite {
			name = "workspace+git"
			mode = gitProtectLeaves
		}
		cwd, err := os.Getwd()
		if err != nil {
			return Config{}, fmt.Errorf("sandbox preset %q: resolve working directory: %w", name, err)
		}
		workspace, err := canonicalWorkspace(cwd)
		if err != nil {
			return Config{}, fmt.Errorf("sandbox preset %q: canonicalize working directory %q: %w", name, cwd, err)
		}
		if err := rejectBroadWorkspace(workspace); err != nil {
			return Config{}, fmt.Errorf("sandbox preset %q: %w", name, err)
		}
		gitPolicy, err := gitWorkspaceGuardrailPolicyForMode(workspace, mode)
		if err != nil {
			return Config{}, fmt.Errorf("sandbox preset %q: protect Git metadata: %w", name, err)
		}
		cfg.WritablePaths = append(cfg.WritablePaths, workspace)
		cfg.DenyWritePaths = append(cfg.DenyWritePaths, gitPolicy.protected...)
		if len(gitPolicy.repositories) != 0 {
			cfg.gitPolicies = append(cfg.gitPolicies, gitPolicy)
		}
	}
	return cfg, nil
}

// rejectBroadWorkspace keeps the recursive Git metadata discovery pass bounded.
// A home directory commonly contains OS-protected descendants that cannot be
// scanned, and accepting a partial scan would leave undiscovered repositories
// writable. Filesystem, mounted-volume, and platform-private roots have the
// same safety problem at a larger scale.
func rejectBroadWorkspace(dir string) error {
	rawDir := dir
	var err error
	dir, err = canonicalWorkspace(dir)
	if err != nil {
		return fmt.Errorf("resolve candidate workspace %q: %w", rawDir, err)
	}
	if filepath.IsAbs(dir) && filepath.Dir(dir) == dir {
		return broadWorkspaceError(dir, "filesystem root")
	}

	if home, homeErr := os.UserHomeDir(); homeErr == nil && home != "" {
		home = filepath.Clean(home)
		if dir == home {
			return broadWorkspaceError(dir, "home directory")
		}
		dirInfo, dirErr := os.Stat(dir)
		homeInfo, homeErr := os.Stat(home)
		if dirErr == nil && homeErr == nil && os.SameFile(dirInfo, homeInfo) {
			return broadWorkspaceError(dir, "home directory")
		}
	}
	return rejectPlatformBroadWorkspace(dir)
}

func canonicalWorkspace(dir string) (string, error) {
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("workspace path %q is not absolute", dir)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace path %q is not a directory", real)
	}
	return filepath.Clean(real), nil
}

func broadWorkspaceError(dir, kind string) error {
	return fmt.Errorf("refusing to make the %s %q a writable workspace: Git metadata protection requires a bounded project directory; cd into a project directory or use --sandbox base", kind, dir)
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

// gitLeafGuardrailPaths is the leaf-mode counterpart of gitGuardrailPaths,
// used by tests to exercise the workspace+git protection set directly.
func gitLeafGuardrailPaths(dir string) ([]string, error) {
	policy, err := gitWorkspaceGuardrailPolicyForMode(dir, gitProtectLeaves)
	if err != nil {
		return nil, err
	}
	return append([]string(nil), policy.protected...), nil
}

func gitWorkspaceGuardrailPolicy(dir string) (gitWorkspacePolicy, error) {
	return gitWorkspaceGuardrailPolicyForMode(dir, gitProtectWholeTree)
}

func gitWorkspaceGuardrailPolicyForMode(dir string, mode gitProtectMode) (gitWorkspacePolicy, error) {
	rawDir := dir
	var err error
	dir, err = canonicalWorkspace(dir)
	if err != nil {
		return gitWorkspacePolicy{}, fmt.Errorf("resolve workspace root %q: %w", rawDir, err)
	}
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

	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
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
		// A .git routing file (worktree/submodule pointer) is itself a
		// dangerous leaf and is pinned in both modes. The .git directory is
		// pinned whole only in whole-tree mode; leaf mode pins its dangerous
		// leaves after the walk instead.
		if mode == gitProtectWholeTree || !info.IsDir() {
			if err := add(path); err != nil {
				return err
			}
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
		if mode == gitProtectWholeTree {
			if err := add(gitDir); err != nil {
				return err
			}
		}
		repository := gitRepositoryContext{
			workTree: filepath.Dir(path),
			gitDir:   gitDir,
		}

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
			if mode == gitProtectWholeTree {
				if err := add(common); err != nil {
					return err
				}
			}
			repository.commonDir = common
		case os.IsNotExist(err):
			// Ordinary repositories and submodule gitdirs have no commondir.
		case err != nil:
			return fmt.Errorf("inspect Git commondir pointer %q: %w", commonPointer, err)
		}
		repositories = append(repositories, repository)

		if info.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return gitWorkspacePolicy{}, err
	}
	if mode == gitProtectLeaves && len(repositories) != 0 {
		if err := materializeGitLeafProtections(dir, repositories, add); err != nil {
			return gitWorkspacePolicy{}, err
		}
	}
	protected := minimalGitGuardrailPaths(paths)
	policy := gitWorkspacePolicy{
		workspace:    filepath.Clean(dir),
		repositories: repositories,
		protected:    protected,
		mode:         mode,
		audited:      &gitAuditMemo{},
	}
	if len(repositories) != 0 {
		writableRoots, err := gitAuditWritableRoots(dir)
		if err != nil {
			return gitWorkspacePolicy{}, err
		}
		if err := policy.auditHooksAndConfig(protected, writableRoots); err != nil {
			return gitWorkspacePolicy{}, err
		}
	}
	return policy, nil
}

type gitRepositoryContext struct {
	workTree string
	gitDir   string
	// commonDir is the resolved common gitdir for a linked-worktree context
	// (empty otherwise). Leaf mode uses it to identify worktree contexts and
	// to whole-pin an external common tree the walk never discovered.
	commonDir string
	bare      bool
}

// GitMetadataReadOnly reports whether a workspace Git policy in this config
// pins whole metadata trees (the workspace preset without the git component),
// meaning ordinary Git write operations such as commit fail inside the
// sandbox. Callers use it to say so up front instead of surfacing EPERM.
func (c Config) GitMetadataReadOnly() bool {
	for _, policy := range c.gitPolicies {
		if len(policy.repositories) != 0 && policy.mode == gitProtectWholeTree {
			return true
		}
	}
	return false
}

// materializeGitLeafProtections adds leaf-mode protections for every
// discovered repository context: the dangerous metadata leaves for in-workspace
// gitdirs (created when missing so every pinned name exists), whole-tree pins
// for external gitdirs and common trees (parity with whole-tree mode; nothing
// is ever created outside the workspace), and whole-tree pins for the metadata
// subtrees the workspace walk never enters (dormant submodule gitdirs, stale
// or external worktree entries).
func materializeGitLeafProtections(workspace string, repositories []gitRepositoryContext, add func(string) error) error {
	writableRoots, err := gitAuditWritableRoots(workspace)
	if err != nil {
		return err
	}
	gitPath, err := trustedGitExecutable(writableRoots)
	if err != nil {
		return err
	}
	discovered := make(map[string]bool, len(repositories))
	for _, repository := range repositories {
		discovered[repository.gitDir] = true
	}
	workspace = filepath.Clean(workspace)
	for _, repository := range repositories {
		if repository.bare {
			// Bare repositories keep their whole-tree pin from the walk:
			// nothing commits from inside a bare metadata tree.
			continue
		}
		if !pathLexicallyWithinPolicy(repository.gitDir, workspace) {
			// A gitdir outside the workspace (separate-git-dir layouts,
			// worktrees of an external repository) is not writable in the
			// sandbox anyway; keep whole-tree parity so a later writable-path
			// overlay cannot open it, and create nothing outside the
			// workspace.
			if err := add(repository.gitDir); err != nil {
				return err
			}
			if repository.commonDir != "" && !discovered[repository.commonDir] {
				if err := add(repository.commonDir); err != nil {
					return err
				}
			}
			continue
		}
		leaves, fallback, err := gitLeafProtections(repository, gitPath)
		if err != nil {
			return err
		}
		if fallback {
			if err := add(repository.gitDir); err != nil {
				return err
			}
			continue
		}
		for _, leaf := range leaves {
			if err := add(leaf); err != nil {
				return err
			}
		}
		if err := pinUndiscoveredGitMetadata(repository.gitDir, discovered, add); err != nil {
			return err
		}
		if repository.commonDir != "" && !discovered[repository.commonDir] {
			if err := add(repository.commonDir); err != nil {
				return err
			}
		}
	}
	return nil
}

// errGitLeafUnsafe marks an existing leaf whose shape cannot be pinned safely.
// Unlike a creation failure, it fails policy construction closed rather than
// falling back to a whole-tree pin.
var errGitLeafUnsafe = errors.New("unsafe Git metadata leaf")

// gitLeafProtections returns the dangerous metadata leaves to pin for one
// discovered in-workspace repository context, creating missing inert leaves so
// every pinned name exists (DenyWritePaths entries must exist on disk).
// fallback reports that a leaf could not be materialized and the caller must
// whole-pin ctx.gitDir instead — never weaker than whole-tree mode.
func gitLeafProtections(ctx gitRepositoryContext, gitPath string) (leaves []string, fallback bool, err error) {
	if ctx.commonDir != "" {
		// A linked worktree's config and hooks live in the common gitdir; git
		// never consults worktrees/<id>/config or hooks. The reverse gitdir
		// pointer is dangerous on its own: a host-side `git worktree repair`
		// writes through it, so a retargetable pointer is a write primitive.
		pointer := filepath.Join(ctx.gitDir, "gitdir")
		info, lerr := os.Lstat(pointer)
		switch {
		case os.IsNotExist(lerr):
			// A registered worktree without its reverse pointer is broken;
			// leaf mode cannot support committing from it.
			return nil, true, nil
		case lerr != nil:
			return nil, false, fmt.Errorf("inspect Git worktree pointer %q: %w", pointer, lerr)
		case info.Mode()&os.ModeSymlink != 0:
			return nil, false, fmt.Errorf("Git worktree pointer %q is a symlink and cannot be pinned safely", pointer)
		case !info.Mode().IsRegular():
			return nil, false, fmt.Errorf("Git worktree pointer %q is not a regular file", pointer)
		case hasMultipleLinks(info):
			return nil, false, fmt.Errorf("Git worktree pointer %q has multiple hard links and cannot be pinned safely", pointer)
		}
		leaves = append(leaves, pointer)
	} else {
		configPath := filepath.Join(ctx.gitDir, "config")
		if err := ensureGitLeafFile(configPath); err != nil {
			return gitLeafEnsureFailure(ctx, configPath, err)
		}
		leaves = append(leaves, configPath)
		hooksPath := filepath.Join(ctx.gitDir, "hooks")
		if err := ensureGitLeafDir(hooksPath); err != nil {
			return gitLeafEnsureFailure(ctx, hooksPath, err)
		}
		leaves = append(leaves, hooksPath)
	}

	configWorktree := filepath.Join(ctx.gitDir, "config.worktree")
	_, lerr := os.Lstat(configWorktree)
	switch {
	case lerr == nil:
		// Shape validation (symlink, hard links, hook/include indirection)
		// already ran in rejectSymlinkedGitMetadata during discovery.
		leaves = append(leaves, configWorktree)
	case os.IsNotExist(lerr):
		if gitWorktreeConfigEnabled(gitPath, ctx) {
			if err := ensureGitLeafFile(configWorktree); err != nil {
				return gitLeafEnsureFailure(ctx, configWorktree, err)
			}
			leaves = append(leaves, configWorktree)
		}
	default:
		return nil, false, fmt.Errorf("inspect Git worktree config %q: %w", configWorktree, lerr)
	}
	return leaves, false, nil
}

// gitLeafEnsureFailure classifies a leaf materialization error: unsafe shapes
// fail closed, while plain creation failures (a read-only .git, permissions)
// degrade to a whole-tree pin for that repository so the sandbox still starts
// with protection never weaker than whole-tree mode.
func gitLeafEnsureFailure(ctx gitRepositoryContext, path string, err error) ([]string, bool, error) {
	if errors.Is(err, errGitLeafUnsafe) {
		return nil, false, err
	}
	slog.Warn("sandbox_git_leaf_fallback", "gitdir", ctx.gitDir, "path", path, "error", err.Error())
	return nil, true, nil
}

// ensureGitLeafFile creates path as an empty regular file when missing (the
// shape `git init` writes) so the pinned name exists. An existing file is
// revalidated for shapes discovery could have missed in a race.
func ensureGitLeafFile(path string) error {
	handle, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err == nil {
		return handle.Close()
	}
	if !errors.Is(err, fs.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%w: %q is a symlink", errGitLeafUnsafe, path)
	case !info.Mode().IsRegular():
		return fmt.Errorf("%w: %q is not a regular file", errGitLeafUnsafe, path)
	case hasMultipleLinks(info):
		return fmt.Errorf("%w: %q has multiple hard links", errGitLeafUnsafe, path)
	}
	return nil
}

// ensureGitLeafDir creates path as an empty directory when missing so the
// pinned name exists.
func ensureGitLeafDir(path string) error {
	err := os.Mkdir(path, 0o755)
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return err
	}
	info, lerr := os.Lstat(path)
	if lerr != nil {
		return lerr
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%w: %q is a symlink", errGitLeafUnsafe, path)
	case !info.IsDir():
		return fmt.Errorf("%w: %q is not a directory", errGitLeafUnsafe, path)
	}
	return nil
}

// gitWorktreeConfigEnabled reports whether extensions.worktreeConfig is
// effectively enabled for the repository. Git ignores config.worktree
// entirely when the extension is off, so a missing file only needs a
// reservation when it is on. Errors err toward enabled: creating the inert
// empty file is the safe direction.
func gitWorktreeConfigEnabled(gitPath string, ctx gitRepositoryContext) bool {
	output, found, err := runGitConfigQuery(gitPath, ctx, "config", "--type=bool", "--get", "extensions.worktreeconfig")
	if err != nil {
		return true
	}
	if !found {
		return false
	}
	return strings.TrimSpace(string(output)) == "true"
}

// pinUndiscoveredGitMetadata whole-pins metadata subtrees reachable only from
// inside a leaf-pinned gitdir. The workspace walk never descends into .git,
// so dormant (registered but not checked out) submodule gitdirs under
// modules/ and worktrees/<id> entries whose worktree lives outside the
// workspace would otherwise become writable in leaf mode; the whole-tree pin
// covered them incidentally.
func pinUndiscoveredGitMetadata(gitDir string, discovered map[string]bool, add func(string) error) error {
	modules := filepath.Join(gitDir, "modules")
	info, err := os.Lstat(modules)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Git modules directory %q is a symlink and cannot be pinned safely", modules)
		}
		if !info.IsDir() {
			break
		}
		err := filepath.WalkDir(modules, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return fmt.Errorf("scan Git modules %q: %w", path, walkErr)
			}
			if path == modules {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("Git modules entry %q is a symlink and cannot be pinned safely", path)
			}
			if !entry.IsDir() {
				return nil
			}
			if discovered[path] {
				// A checked-out submodule gitdir: its own repository context
				// pins its leaves and scans its subtrees.
				return filepath.SkipDir
			}
			bare, err := looksLikeBareGitRepository(path)
			if err != nil {
				return err
			}
			if bare {
				if err := rejectSymlinkedGitMetadata(path); err != nil {
					return err
				}
				if err := add(path); err != nil {
					return err
				}
				return filepath.SkipDir
			}
			// An intermediate component of a slash-named submodule.
			return nil
		})
		if err != nil {
			return err
		}
	case !os.IsNotExist(err):
		return fmt.Errorf("inspect Git modules directory %q: %w", modules, err)
	}

	worktrees := filepath.Join(gitDir, "worktrees")
	entries, err := os.ReadDir(worktrees)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect Git worktrees directory %q: %w", worktrees, err)
	}
	for _, entry := range entries {
		path := filepath.Join(worktrees, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("Git worktrees entry %q is a symlink and cannot be pinned safely", path)
		}
		if !entry.IsDir() || discovered[path] {
			continue
		}
		if err := rejectSymlinkedGitMetadata(path); err != nil {
			return err
		}
		if err := add(path); err != nil {
			return err
		}
	}
	return nil
}

// gitWorkspacePolicy is the immutable result of the workspace preset's Git
// discovery pass. Config keeps it private and carries it through Merge so New
// can re-run the config/hook audit after all writable path overlays are known,
// without walking every repository once per tool.
type gitWorkspacePolicy struct {
	workspace    string
	repositories []gitRepositoryContext
	protected    []string
	mode         gitProtectMode
	// audited is shared by every clone of this policy so the subprocess-heavy
	// config/hook audit runs once per distinct writable-root/protected input,
	// not once per PrepareConfig/New call. Test-constructed policies leave it
	// nil and re-audit on every call.
	audited *gitAuditMemo
}

// gitAuditMemo records audit inputs that already passed for a policy's
// repositories. The audit's verdict is a function of the repository set, the
// writable roots, the protected paths, and on-disk Git config; repeating it
// with an identical fingerprint (e.g. PrepareConfig followed by New during one
// startup) re-reads the same config to reach the same verdict, so a recorded
// pass is skipped. A merge that widens the writable roots produces a new
// fingerprint and still triggers a full re-audit.
type gitAuditMemo struct {
	mu     sync.Mutex
	passed map[string]bool
}

func (m *gitAuditMemo) alreadyPassed(fingerprint string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.passed[fingerprint]
}

func (m *gitAuditMemo) recordPass(fingerprint string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.passed == nil {
		m.passed = make(map[string]bool)
	}
	m.passed[fingerprint] = true
}

// auditFingerprint canonicalizes the audit inputs: order and duplicates do not
// affect the verdict (both slices are matched any-of), so sorted deduplicated
// sets keep e.g. ParsePreset's protected list and the merged
// protected+DenyWritePaths list from reading as different audits.
func auditFingerprint(protected, writableRoots []string) string {
	canonical := func(values []string) []string {
		out := slices.Clone(values)
		slices.Sort(out)
		return slices.Compact(out)
	}
	return strings.Join(canonical(writableRoots), "\x00") + "\x01" + strings.Join(canonical(protected), "\x00")
}

// auditHooksAndConfig runs the config/hook audit for this policy's
// repositories unless an identical audit already passed.
func (p *gitWorkspacePolicy) auditHooksAndConfig(protected, writableRoots []string) error {
	fingerprint := auditFingerprint(protected, writableRoots)
	if p.audited.alreadyPassed(fingerprint) {
		return nil
	}
	if err := rejectEffectiveHooksPathsWithWritableRoots(p.repositories, protected, writableRoots); err != nil {
		return err
	}
	p.audited.recordPass(fingerprint)
	return nil
}

// rejectEffectiveHooksPathsWithWritableRoots closes the remaining
// hook-planting route from trusted global/system Git config. Protecting
// repository-local config and .git/hooks is not enough when an existing
// core.hooksPath redirects Git to a writable worktree directory such as
// .githooks. Git itself is used only as a config resolver here so conditional
// includes and precedence match the host's next Git invocation; no repository
// hook or worktree command is executed.
func rejectEffectiveHooksPathsWithWritableRoots(repositories []gitRepositoryContext, protected, writableRoots []string) error {
	if len(repositories) == 0 {
		return nil
	}
	gitPath, err := trustedGitExecutable(writableRoots)
	if err != nil {
		return err
	}

	// Repositories are audited concurrently: the audit is dominated by git
	// subprocess round-trips, and repositories in one workspace share most of
	// their config sources through the query cache. Errors keep repository
	// order so a multi-repository failure reports deterministically.
	cache := newGitAuditQueryCache()
	auditErrs := make([]error, len(repositories))
	var wg sync.WaitGroup
	for i, repository := range repositories {
		wg.Add(1)
		go func(i int, repository gitRepositoryContext) {
			defer wg.Done()
			auditErrs[i] = auditRepositoryHooksAndConfig(gitPath, repository, writableRoots, protected, cache)
		}(i, repository)
	}
	wg.Wait()
	for _, err := range auditErrs {
		if err != nil {
			return err
		}
	}
	return nil
}

func auditRepositoryHooksAndConfig(gitPath string, repository gitRepositoryContext, writableRoots, protected []string, cache *gitAuditQueryCache) error {
	if err := rejectWritableGitConfigSources(gitPath, repository, writableRoots, protected, cache); err != nil {
		return err
	}

	hooksPath, configured, err := effectiveHooksPath(gitPath, repository)
	if err != nil {
		return err
	}
	if !configured {
		return nil
	}
	if err := validateConfiguredHooksPath(hooksPath, "effective core.hooksPath", writableRoots, protected); err != nil {
		return fmt.Errorf("repository %q: %w", repository.workTree, err)
	}
	return nil
}

// gitAuditQueryCache deduplicates git subprocess queries whose result does not
// depend on the repository being audited, keyed by the full command identity.
// Config files shared by several repositories in one workspace (the global and
// system configs, at minimum) are then read once per audit instead of once per
// repository.
type gitAuditQueryCache struct {
	mu      sync.Mutex
	entries map[string]*gitAuditQueryEntry
}

type gitAuditQueryEntry struct {
	once   sync.Once
	output []byte
	found  bool
	err    error
}

func newGitAuditQueryCache() *gitAuditQueryCache {
	return &gitAuditQueryCache{entries: make(map[string]*gitAuditQueryEntry)}
}

func (c *gitAuditQueryCache) do(key string, run func() ([]byte, bool, error)) ([]byte, bool, error) {
	if c == nil {
		return run()
	}
	c.mu.Lock()
	entry := c.entries[key]
	if entry == nil {
		entry = &gitAuditQueryEntry{}
		c.entries[key] = entry
	}
	c.mu.Unlock()
	entry.once.Do(func() {
		entry.output, entry.found, entry.err = run()
	})
	return entry.output, entry.found, entry.err
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
			// Canonicalize like the workspace above. Path checks match by
			// filesystem identity either way, but the audit memo fingerprints
			// roots by string, and PrepareConfig contributes this same grant
			// in canonical form after freezing authority paths.
			if real, err := filepath.EvalSymlinks(temp); err == nil {
				temp = real
			}
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
	for i := range cfg.gitPolicies {
		policy := &cfg.gitPolicies[i]
		writableRoots, err := gitAuditWritableRootsForConfig(policy.workspace, cfg.WritablePaths, hostWritable)
		if err != nil {
			return Config{}, fmt.Errorf("revalidate workspace Git policy for %q: %w", policy.workspace, err)
		}
		protected := concatStrings(policy.protected, cfg.DenyWritePaths)
		if err := policy.auditHooksAndConfig(protected, writableRoots); err != nil {
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

func rejectWritableGitConfigSources(gitPath string, repository gitRepositoryContext, writableRoots, protected []string, cache *gitAuditQueryCache) error {
	base := gitRepositoryBase(repository)
	selectors, err := gitConfigSelectorPaths(gitPath, base, cache)
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
	if err := inspectGitConfigFiles(gitPath, repository, configFiles, writableRoots, protected, cache); err != nil {
		return fmt.Errorf("repository %q: %w", repository.workTree, err)
	}
	return nil
}

type namedGitConfigPath struct {
	kind string
	path string
}

func gitConfigSelectorPaths(gitPath, base string, cache *gitAuditQueryCache) ([]namedGitConfigPath, error) {
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
			path, err := compiledSystemGitConfigPath(gitPath, cache)
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

func compiledSystemGitConfigPath(gitPath string, cache *gitAuditQueryCache) (string, error) {
	output, _, err := cache.do("var\x00"+gitPath, func() ([]byte, bool, error) {
		cmd := exec.Command(gitPath, "var", "GIT_CONFIG_SYSTEM")
		cmd.Dir = string(filepath.Separator)
		cmd.Env = gitFileAuditEnvironment()
		out, runErr := cmd.Output()
		return out, runErr == nil, runErr
	})
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
func inspectGitConfigFiles(gitPath string, repository gitRepositoryContext, initial []string, writableRoots, protected []string, cache *gitAuditQueryCache) error {
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
		// One query fetches both audited key families; records carry the
		// canonical (lowercased) key name so they dispatch below.
		output, found, err := runGitFileConfigQuery(gitPath, cache, configPath,
			"--path", "--null", "--get-regexp", `^(core\.hookspath|include(if\..*)?\.path)$`)
		if err != nil {
			return fmt.Errorf("inspect hook and include keys in Git config %q: %w", configPath, err)
		}
		if !found {
			continue
		}
		records, err := parseNULRecords(output, "Git config hook and include records", false)
		if err != nil {
			return fmt.Errorf("Git config %q: %w", configPath, err)
		}
		for _, record := range records {
			name, value, ok := strings.Cut(record, "\n")
			if !ok {
				return fmt.Errorf("Git config %q has a malformed audit record", configPath)
			}
			if name == "core.hookspath" {
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
				continue
			}
			includePath := value
			if includePath == "" || strings.ContainsAny(includePath, "\r\n") {
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

// runGitFileConfigQuery reads one config file directly; the result depends
// only on the file and the query, so it is shared through the audit cache when
// several repositories select the same file.
func runGitFileConfigQuery(gitPath string, cache *gitAuditQueryCache, configPath string, args ...string) ([]byte, bool, error) {
	key := strings.Join(append([]string{"file", gitPath, configPath}, args...), "\x00")
	return cache.do(key, func() ([]byte, bool, error) {
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
	})
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
	return resolveExistingPathPrefixObserved(path, nil)
}

// resolveExistingPathPrefixObserved is resolveExistingPathPrefix with a hook
// for every candidate reached during traversal. Symlink targets are processed
// component by component so that dot-dot entries apply after earlier symlinks,
// matching kernel path resolution rather than lexical filepath.Clean behavior.
func resolveExistingPathPrefixObserved(path string, observe func(string) error) (string, error) {
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
		if component == ".." {
			resolved = filepath.Dir(resolved)
			continue
		}
		candidate := filepath.Join(resolved, component)
		if observe != nil {
			if err := observe(candidate); err != nil {
				return "", err
			}
		}
		info, err := os.Lstat(candidate)
		if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
			resolved = candidate
			for _, trailing := range pending {
				if trailing == ".." {
					resolved = filepath.Dir(resolved)
					continue
				}
				resolved = filepath.Join(resolved, trailing)
				if observe != nil {
					if err := observe(resolved); err != nil {
						return "", err
					}
				}
			}
			return filepath.Clean(resolved), nil
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
		targetRoot, targetComponents := absolutePathComponents(target)
		if filepath.IsAbs(target) {
			resolved = targetRoot
		} else {
			resolved = filepath.Dir(candidate)
		}
		pending = append(targetComponents, pending...)
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
