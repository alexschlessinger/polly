//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const linuxBwrapPath = "/usr/bin/bwrap"

type linuxSandbox struct {
	cfg            Config
	bwrapPath      string
	tempRoots      []string
	runRoots       []string
	authorityPaths []authorityPathIdentity
}

type deniedReservationEntry struct {
	name       string
	linkTarget string
	symlink    bool
	info       os.FileInfo
}

type deniedReservation struct {
	root      string
	rootInfo  os.FileInfo
	omitted   map[string]bool
	entries   []deniedReservationEntry
	ancestors []authorityPathIdentity
}

// New creates a Sandbox for Linux using bubblewrap (bwrap).
func New(cfg Config) (Sandbox, error) {
	if err := validateLinuxBwrapExecutable(linuxBwrapPath); err != nil {
		return nil, err
	}
	tempRoots, runRoots := privateLinuxRoots()
	var err error
	cfg, err = prepareLinuxConfig(cfg, tempRoots, runRoots)
	if err != nil {
		return nil, err
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if err := validateLinuxSpecialMountRestrictions(cfg); err != nil {
		return nil, err
	}
	if !cfg.DenyWrite {
		if _, err := resolveDenyWritePaths(cfg.DenyWritePaths, cfg.WritablePaths); err != nil {
			return nil, err
		}
	}
	if _, _, err := nativeAuditArch(); err != nil {
		return nil, err
	}
	privateRoots := append(append([]string{}, tempRoots...), runRoots...)
	authorityPaths, err := captureAuthorityPathIdentities(linuxAuthoritySourcePaths(cfg, privateRoots))
	if err != nil {
		return nil, err
	}
	return &linuxSandbox{
		cfg:            cfg,
		bwrapPath:      linuxBwrapPath,
		tempRoots:      append([]string(nil), tempRoots...),
		runRoots:       append([]string(nil), runRoots...),
		authorityPaths: authorityPaths,
	}, nil
}

func prepareLinuxConfig(cfg Config, tempRoots, runRoots []string) (Config, error) {
	var err error
	cfg, err = normalizeConfigPaths(cfg)
	if err != nil {
		return Config{}, err
	}
	privateRoots := append(append([]string(nil), tempRoots...), runRoots...)
	cfg, err = freezeAuthorityPaths(cfg, privateRoots...)
	if err != nil {
		return Config{}, err
	}
	return applyFinalGitPolicyWithHostWritable(cfg, func(path string) bool {
		return !pathEqualsAny(path, privateRoots)
	})
}

func freezeAuthorityPathsForPlatform(cfg Config) (Config, error) {
	tempRoots, runRoots := privateLinuxRoots()
	nonCoveringWritableRoots := append(append([]string{}, tempRoots...), runRoots...)
	// PrepareConfig may be retained and passed to New after TMPDIR changes.
	// Avoid discarding descendant grants based on a private-root snapshot that
	// is not yet bound to a sandbox instance; prepareLinuxConfig performs the
	// final minimization against the roots captured by New.
	nonCoveringWritableRoots = append(nonCoveringWritableRoots, cfg.WritablePaths...)
	return freezeAuthorityPaths(cfg, nonCoveringWritableRoots...)
}

func validateLinuxBwrapExecutable(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("Linux sandbox backend unavailable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("Linux sandbox backend %q is not a regular executable", path)
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return fmt.Errorf("Linux sandbox backend %q must not be setuid or setgid", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("Linux sandbox backend %q is not root-owned and immutable to non-root users", path)
	}
	return nil
}

// validateLinuxSpecialMountRestrictions rejects deny rules that bubblewrap
// cannot preserve. The backend mounts fresh /dev and /proc filesystems after
// constructing the ordinary path policy; either mount would cover a deny mask
// on one of its descendants. Rejecting intersecting rules is safer than
// accepting a configuration that silently runs with less protection.
func validateLinuxSpecialMountRestrictions(cfg Config) error {
	validate := func(field, path string) error {
		path = filepath.Clean(expandTilde(path))
		intersectionError := func(root string) error {
			return fmt.Errorf("sandbox %s entry %q intersects Linux special mount %q and cannot be enforced", field, path, root)
		}
		checkRoute := func(candidate string) error {
			for _, root := range []string{"/dev", "/proc"} {
				if isPathWithin(candidate, root) || isPathWithin(root, candidate) {
					return intersectionError(root)
				}
			}
			return nil
		}
		checkTraversal := func(candidate string) error {
			for _, root := range []string{"/dev", "/proc"} {
				if isPathWithin(candidate, root) {
					return intersectionError(root)
				}
			}
			return nil
		}
		// Check the lexical route as well as its current target. Procfs magic
		// links such as /proc/self/cwd can resolve outside /proc in the parent
		// while pointing somewhere else in the sandboxed process.
		if err := checkRoute(path); err != nil {
			return err
		}
		resolved, err := resolveExistingPathPrefixObserved(path, checkTraversal)
		if err != nil {
			return fmt.Errorf("resolve sandbox %s entry %q for Linux special mounts: %w", field, path, err)
		}
		if err := checkRoute(resolved); err != nil {
			return err
		}
		return nil
	}

	for _, denied := range allDeniedPaths(cfg) {
		if err := validate("denyPaths", denied.Path); err != nil {
			return err
		}
	}
	for _, path := range cfg.DenyWritePaths {
		if err := validate("denyWritePaths", path); err != nil {
			return err
		}
	}
	return nil
}

// existingDeniedPaths resolves each deny-path to its real location and drops
// the ones that don't exist. Two failure modes motivate this:
//
//   - Missing paths: masking one forces bwrap to mkdir a mountpoint under the
//     read-only root bind, which fails ("Can't mkdir <path>: Read-only file
//     system") and aborts the whole sandbox — so one absent credential dir
//     (e.g. ~/.gnupg) would break every command.
//   - Symlinks (e.g. WSL's ~/.aws -> /mnt/c/Users/.../.aws): bwrap can't mount a
//     tmpfs on the link itself ("Can't mount tmpfs ...: No such file or
//     directory"), and masking the link wouldn't cover the real target anyway.
//
// Only a confirmed not-exist drops a mask. ENOTDIR counts as not-exist: a deny
// entry like ~/.docker/config.json resolves with ENOTDIR (not ENOENT) when
// ~/.docker is a regular file, and the credential file cannot exist under a
// non-directory — keeping it would make bwrap try to mkdir a mountpoint under a
// file and abort every command. Any OTHER resolution failure (permissions, I/O)
// keeps the original path — if bwrap then can't mask it, the command errors
// instead of running with the path readable.
// Resolved paths are de-duplicated so two links to the same target don't
// double-mask.
func existingDeniedPaths(paths []DeniedPath) []DeniedPath {
	kept := make([]DeniedPath, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		real, err := filepath.EvalSymlinks(p.Path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
				continue
			}
			real = p.Path
		}
		if seen[real] {
			continue
		}
		seen[real] = true
		kept = append(kept, DeniedPath{Path: real, Kind: p.Kind})
	}
	return kept
}

// deniedMountPaths also retains missing entries. Reservations give bwrap a
// private, writable parent in which it can safely create a mountpoint, so the
// target path itself can then be made read-only. This prevents a readPaths
// ancestor from turning a credential that was absent at construction into a
// writable in-sandbox path.
func deniedMountPaths(paths []DeniedPath) []DeniedPath {
	kept := make([]DeniedPath, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, denied := range paths {
		path := filepath.Clean(denied.Path)
		real, err := filepath.EvalSymlinks(path)
		switch {
		case err == nil:
			path = filepath.Clean(real)
		case errors.Is(err, syscall.ENOTDIR):
			continue
		case errors.Is(err, fs.ErrNotExist):
			// Keep the lexical route. planDeniedReservations has already replaced
			// its nearest existing parent with a private snapshot.
		default:
			// Keep the lexical route so an inspection failure aborts at bwrap
			// rather than silently dropping the deny rule.
		}
		if !seen[path] {
			seen[path] = true
			kept = append(kept, DeniedPath{Path: path, Kind: denied.Kind})
		}
	}
	return kept
}

func privateLinuxRoots() (tempRoots []string, runRoots []string) {
	add := func(paths *[]string, path string) {
		path = filepath.Clean(path)
		if real, err := filepath.EvalSymlinks(path); err == nil {
			path = real
		}
		for _, existing := range *paths {
			if path == existing || isPathWithin(path, existing) {
				return
			}
		}
		*paths = append(*paths, path)
	}
	add(&tempRoots, "/tmp")
	add(&tempRoots, os.TempDir())
	add(&runRoots, "/run")
	if real, err := filepath.EvalSymlinks("/var/run"); err == nil {
		add(&runRoots, real)
	}
	return tempRoots, runRoots
}

func deniedReadSet(cfg Config) map[string]bool {
	readSet := make(map[string]bool, len(cfg.ReadPaths))
	for _, path := range cfg.ReadPaths {
		expanded := filepath.Clean(expandTilde(path))
		readSet[expanded] = true
	}
	return readSet
}

func isReadExempt(path string, readSet map[string]bool) bool {
	for parent := range readSet {
		if isPathWithin(path, parent) {
			return true
		}
	}
	return false
}

func deniedReservationRoute(path string) (root, omitted string, err error) {
	// If any existing routing component is a symlink, freeze its parent and
	// omit the symlink itself. Masking only the resolved leaf would let the host
	// retarget that symlink after the long-lived sandbox starts.
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, lstatErr := os.Lstat(current)
		if lstatErr != nil {
			if errors.Is(lstatErr, fs.ErrNotExist) || errors.Is(lstatErr, syscall.ENOTDIR) {
				break
			}
			return "", "", fmt.Errorf("inspect denied path route %q: %w", current, lstatErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			parent := filepath.Dir(current)
			if parent == "/" {
				return "", "", fmt.Errorf("cannot safely reserve symlinked denied path %q below filesystem root", path)
			}
			real, resolveErr := filepath.EvalSymlinks(parent)
			if resolveErr != nil {
				return "", "", fmt.Errorf("resolve denied path route parent %q: %w", parent, resolveErr)
			}
			return real, filepath.Base(current), nil
		}
	}

	candidate := filepath.Dir(path)
	for {
		if candidate == "/" || candidate == "." {
			return "", "", fmt.Errorf("cannot safely reserve denied path %q below filesystem root", path)
		}
		real, resolveErr := filepath.EvalSymlinks(candidate)
		if resolveErr == nil {
			info, statErr := os.Stat(real)
			if statErr != nil {
				return "", "", fmt.Errorf("inspect denied path parent %q: %w", candidate, statErr)
			}
			if info.IsDir() {
				rel, relErr := filepath.Rel(candidate, path)
				if relErr != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					return "", "", fmt.Errorf("derive reservation for denied path %q", path)
				}
				return real, strings.Split(rel, string(filepath.Separator))[0], nil
			}
		}
		candidate = filepath.Dir(candidate)
	}
}

// planDeniedReservations freezes the directory entries that route to denied
// paths. A leaf-only mount is insufficient for a long-lived process: the host
// can create a formerly missing entry or replace an existing entry after bwrap
// starts, detaching the leaf mount and exposing the replacement. Each plan
// stages the nearest existing parent, replaces it with a private tmpfs, and
// restores its pre-existing siblings while omitting the denied entry.
func planDeniedReservations(paths []DeniedPath, _ map[string]bool, cfg Config, privateRoots []string) ([]deniedReservation, error) {
	plansByRoot := make(map[string]*deniedReservation)
	readAliasSymlinks := readPathAliasSymlinkSet(cfg)
	for _, denied := range paths {
		path := filepath.Clean(denied.Path)
		private := false
		for _, root := range privateRoots {
			if isPathWithin(path, root) {
				private = true
				break
			}
		}
		if private && !pathExplicitlyExposedWithRoots(path, cfg, privateRoots) {
			continue
		}

		root, omitted, err := deniedReservationRoute(path)
		if err != nil {
			return nil, err
		}
		restoreAlias := readAliasSymlinks[filepath.Join(root, omitted)]

		plan := plansByRoot[root]
		if plan == nil {
			plan = &deniedReservation{root: root, omitted: make(map[string]bool)}
			plansByRoot[root] = plan
		}
		if !restoreAlias {
			plan.omitted[omitted] = true
		}
	}

	plans := make([]deniedReservation, 0, len(plansByRoot))
	for _, plan := range plansByRoot {
		rootInfo, err := os.Stat(plan.root)
		if err != nil {
			return nil, fmt.Errorf("snapshot denied path parent %q: %w", plan.root, err)
		}
		if !rootInfo.IsDir() {
			return nil, fmt.Errorf("snapshot denied path parent %q: not a directory", plan.root)
		}
		plan.rootInfo = rootInfo
		entries, err := os.ReadDir(plan.root)
		if err != nil {
			return nil, fmt.Errorf("snapshot denied path parent %q: %w", plan.root, err)
		}
		for _, entry := range entries {
			if plan.omitted[entry.Name()] {
				continue
			}
			item := deniedReservationEntry{name: entry.Name()}
			info, err := os.Lstat(filepath.Join(plan.root, entry.Name()))
			if err != nil {
				return nil, fmt.Errorf("snapshot denied path sibling %q: %w", filepath.Join(plan.root, entry.Name()), err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				item.symlink = true
				item.linkTarget, err = os.Readlink(filepath.Join(plan.root, entry.Name()))
				if err != nil {
					return nil, fmt.Errorf("snapshot denied path symlink %q: %w", filepath.Join(plan.root, entry.Name()), err)
				}
			} else {
				item.info = info
			}
			plan.entries = append(plan.entries, item)
		}
		plans = append(plans, *plan)
	}
	seenAncestors := make(map[string]bool)
	for i := range plans {
		for _, writable := range cfg.WritablePaths {
			writable = filepath.Clean(expandTilde(writable))
			if resolved, err := filepath.EvalSymlinks(writable); err == nil {
				writable = filepath.Clean(resolved)
			}
			if pathEqualsAny(writable, privateRoots) || !isPathWithin(plans[i].root, writable) {
				continue
			}
			var ancestors []string
			for ancestor := filepath.Dir(plans[i].root); ancestor != writable && isPathWithin(ancestor, writable); ancestor = filepath.Dir(ancestor) {
				ancestors = append(ancestors, ancestor)
			}
			for j := len(ancestors) - 1; j >= 0; j-- {
				ancestor := ancestors[j]
				if seenAncestors[ancestor] {
					continue
				}
				info, err := os.Stat(ancestor)
				if err != nil {
					return nil, fmt.Errorf("pin denied reservation ancestor %q: %w", ancestor, err)
				}
				plans[i].ancestors = append(plans[i].ancestors, authorityPathIdentity{path: ancestor, info: info})
				seenAncestors[ancestor] = true
			}
		}
	}
	sort.Slice(plans, func(i, j int) bool {
		depthI := strings.Count(plans[i].root, string(filepath.Separator))
		depthJ := strings.Count(plans[j].root, string(filepath.Separator))
		if depthI != depthJ {
			return depthI < depthJ
		}
		return plans[i].root < plans[j].root
	})
	return plans, nil
}

func (s *linuxSandbox) Wrap(cmd *exec.Cmd) error {
	return ErrManagedWrapRequired
}

func (s *linuxSandbox) WrapWithEnv(cmd *exec.Cmd, explicitEnv map[string]string) error {
	if err := validateExplicitEnv(explicitEnv); err != nil {
		return err
	}
	return ErrManagedWrapRequired
}

func (s *linuxSandbox) wrapManaged(cmd *exec.Cmd, explicitEnv map[string]string) error {
	if err := validateExplicitEnv(explicitEnv); err != nil {
		return err
	}
	if err := validateReadPathAliasIdentities(s.cfg.readPathAliases); err != nil {
		return err
	}
	if err := validateLinuxSpecialMountRestrictions(s.cfg); err != nil {
		return err
	}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	filtered, stripped := filterEnv(env, s.cfg.AllowEnv)
	filtered = mergeExplicitEnv(filtered, explicitEnv)
	for i, entry := range filtered {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "TMPDIR", "TMP", "TEMP":
			filtered[i] = name + "=/tmp"
		}
	}

	allDenied := allDeniedPaths(s.cfg)
	denied := existingDeniedPaths(allDenied)
	deniedMounts := deniedMountPaths(allDenied)
	tempRoots := s.tempRoots
	runRoots := s.runRoots
	privateRoots := append(append([]string{}, tempRoots...), runRoots...)
	readSet := deniedReadSet(s.cfg)
	reservations, err := planDeniedReservations(allDenied, readSet, s.cfg, privateRoots)
	if err != nil {
		return err
	}
	cfg := s.cfg
	if !cfg.DenyWrite {
		resolvedDenyWrite, err := resolveDenyWritePaths(cfg.DenyWritePaths, cfg.WritablePaths)
		if err != nil {
			return err
		}
		cfg.DenyWritePaths = resolvedDenyWrite
	}
	denyWritePlan, err := planDenyWriteMounts(cfg, true)
	if err != nil {
		return err
	}
	readExemptionBinds, err := planLinuxReadExemptionBinds(cfg, denied, true)
	if err != nil {
		return err
	}

	origArgs := cmd.Args
	origPath, err := resolvedExecutablePath(cmd)
	if err != nil {
		return err
	}
	slog.Debug("sandbox_wrap",
		"command", commandSummary(origArgs),
		"network", s.cfg.AllowNetwork,
		"deny_write", s.cfg.DenyWrite,
		"writable_paths", s.cfg.WritablePaths,
		"env_stripped", stripped,
		"denied_paths", len(denied),
		"denied_reservations", len(reservations))
	cmd.Path = s.bwrapPath
	workingDir, err := resolvedCommandDir(cmd.Dir)
	if err != nil {
		return err
	}
	targetWorkingDir := workingDir
	if isWithinAny(workingDir, tempRoots) && !pathExplicitlyExposedWithRoots(workingDir, cfg, privateRoots) {
		targetWorkingDir = "/"
	}
	extraFilesStart := len(cmd.ExtraFiles)
	closeAddedFiles := func() {
		for _, file := range cmd.ExtraFiles[extraFilesStart:] {
			_ = file.Close()
		}
		cmd.ExtraFiles = cmd.ExtraFiles[:extraFilesStart]
	}
	bootstrapFD, err := attachLinuxBootstrapExecutable(cmd)
	if err != nil {
		closeAddedFiles()
		return fmt.Errorf("prepare target environment bootstrap: %w", err)
	}
	targetEnvFD, err := attachLinuxTargetEnvironment(cmd, filtered)
	if err != nil {
		closeAddedFiles()
		return fmt.Errorf("prepare target environment: %w", err)
	}
	reservationValidationOvermounts := make([]string, 0, len(deniedMounts)+len(readExemptionBinds))
	for _, denied := range deniedMounts {
		reservationValidationOvermounts = append(reservationValidationOvermounts, denied.Path)
	}
	for _, bind := range readExemptionBinds {
		reservationValidationOvermounts = append(reservationValidationOvermounts, bind.destination)
	}
	reservationValidationFD, err := attachLinuxReservationValidation(cmd, reservations, reservationValidationOvermounts)
	if err != nil {
		closeAddedFiles()
		return fmt.Errorf("prepare denied reservation validation: %w", err)
	}
	pinnedIdentities := cloneAuthorityPathIdentities(s.authorityPaths)
	pinnedIdentities = append(pinnedIdentities, reservationSourceIdentities(reservations)...)
	pinnedIdentities = append(pinnedIdentities, denyWritePlan.ancestors...)
	pinnedIdentities = append(pinnedIdentities, denyWritePlan.protected...)
	pinnedIdentities = append(pinnedIdentities, readExemptionSourceIdentities(readExemptionBinds)...)
	authoritySources, authorityFDs, err := attachLinuxAuthorityPaths(cmd, pinnedIdentities)
	if err != nil {
		closeAddedFiles()
		return err
	}
	if err := addLinuxReservationSources(reservations, authoritySources); err != nil {
		closeAddedFiles()
		return err
	}
	seccompFD, err := attachUnixSocketFilter(cmd, cfg.AllowNetwork)
	if err != nil {
		closeAddedFiles()
		return fmt.Errorf("prepare seccomp filter: %w", err)
	}
	args, err := buildBwrapArgsCheckedWithSourcesAndRoots(cfg, deniedMounts, reservations, &denyWritePlan, readExemptionBinds, authoritySources, tempRoots, runRoots, origPath)
	if err != nil {
		closeAddedFiles()
		return err
	}
	args = append(args, "--chdir", targetWorkingDir)
	args = append(args, "--seccomp", strconv.Itoa(seccompFD))
	targetArgs := origArgs
	if len(targetArgs) == 0 {
		targetArgs = []string{origPath}
	}
	bootstrapPath := "/proc/self/fd/" + strconv.Itoa(bootstrapFD)
	cmd.Args = make([]string, 0, len(args)+8+len(targetArgs))
	cmd.Args = append(cmd.Args, args...)
	cmd.Args = append(cmd.Args, "--")
	cmd.Args = append(cmd.Args, bootstrapPath, linuxEnvBootstrapArg,
		strconv.Itoa(bootstrapFD), strconv.Itoa(targetEnvFD), formatLinuxOptionalFD(reservationValidationFD),
		formatLinuxFDList(authorityFDs), origPath)
	cmd.Args = append(cmd.Args, targetArgs...)

	// bwrap itself receives no target environment values in its environment or
	// argv. The original target argv is intentionally forwarded unchanged. It
	// transports an opaque, unlinked payload FD and executes the current process
	// through its pinned /proc/self/exe descriptor only after namespaces, mounts,
	// and seccomp are active. The internal bootstrap then closes the sandbox FDs
	// and execs the already-resolved target with the exact filtered environment.
	cmd.Env = []string{}
	// Do not let bwrap inherit a host cwd that a later private mount covers;
	// the target cwd is selected explicitly with --chdir above.
	cmd.Dir = "/"
	return nil
}

func attachLinuxBootstrapExecutable(cmd *exec.Cmd) (int, error) {
	f, err := os.Open("/proc/self/exe")
	if err != nil {
		return 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		_ = f.Close()
		return 0, fmt.Errorf("/proc/self/exe is not a regular executable")
	}
	fd := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, f)
	return fd, nil
}

func attachLinuxAuthorityPaths(cmd *exec.Cmd, identities []authorityPathIdentity) (map[string]string, []int, error) {
	sources := make(map[string]string, len(identities))
	fds := make([]int, 0, len(identities))
	seen := make(map[string]authorityPathIdentity, len(identities))
	for _, identity := range identities {
		identity.path = filepath.Clean(identity.path)
		if prior, exists := seen[identity.path]; exists {
			if !os.SameFile(prior.info, identity.info) {
				return nil, nil, fmt.Errorf("conflicting sandbox mount source identities for %q", identity.path)
			}
			continue
		}
		seen[identity.path] = identity
		real, err := filepath.EvalSymlinks(identity.path)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve frozen sandbox authority path %q: %w", identity.path, err)
		}
		if filepath.Clean(real) != identity.path {
			return nil, nil, fmt.Errorf("frozen sandbox authority path %q was rerouted", identity.path)
		}
		rawFD, err := unix.Open(identity.path, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("open frozen sandbox authority path %q: %w", identity.path, err)
		}
		file := os.NewFile(uintptr(rawFD), identity.path)
		if file == nil {
			_ = unix.Close(rawFD)
			return nil, nil, fmt.Errorf("open frozen sandbox authority path %q: invalid descriptor", identity.path)
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return nil, nil, fmt.Errorf("inspect frozen sandbox authority path %q: %w", identity.path, err)
		}
		if !os.SameFile(identity.info, info) {
			_ = file.Close()
			return nil, nil, fmt.Errorf("frozen sandbox authority path %q was replaced", identity.path)
		}
		fd := 3 + len(cmd.ExtraFiles)
		cmd.ExtraFiles = append(cmd.ExtraFiles, file)
		fds = append(fds, fd)
		sources[identity.path] = "/proc/self/fd/" + strconv.Itoa(fd)
	}
	return sources, fds, nil
}

func reservationSourceIdentities(reservations []deniedReservation) []authorityPathIdentity {
	identities := make([]authorityPathIdentity, 0, len(reservations))
	for _, reservation := range reservations {
		identities = append(identities, reservation.ancestors...)
		if reservation.rootInfo != nil {
			identities = append(identities, authorityPathIdentity{
				path: reservation.root,
				info: reservation.rootInfo,
			})
		}
	}
	return identities
}

// addLinuxReservationSources routes every restored sibling through the one
// pinned descriptor for its reservation root. The path lookup can still race a
// host-side replacement, so the post-containment bootstrap verifies the final
// bind mount identities before the target is allowed to start.
func addLinuxReservationSources(reservations []deniedReservation, sources map[string]string) error {
	for _, reservation := range reservations {
		rootSource, err := linuxPinnedSource(reservation.root, sources)
		if err != nil {
			return err
		}
		for _, entry := range reservation.entries {
			if entry.symlink || entry.info == nil {
				continue
			}
			destination := filepath.Join(reservation.root, entry.name)
			if sources[destination] == "" {
				sources[destination] = filepath.Join(rootSource, entry.name)
			}
		}
	}
	return nil
}

type linuxReadExemptionBind struct {
	source      string
	destination string
	info        os.FileInfo
}

func planLinuxReadExemptionBinds(cfg Config, deniedPaths []DeniedPath, strict bool) ([]linuxReadExemptionBind, error) {
	binds := make([]linuxReadExemptionBind, 0)
	seen := make(map[string]bool)
	for _, denied := range deniedPaths {
		deniedPath := filepath.Clean(denied.Path)
		for _, readPath := range cfg.ReadPaths {
			readPath = filepath.Clean(expandTilde(readPath))
			candidate := ""
			switch {
			case isPathWithin(deniedPath, readPath):
				candidate = deniedPath
			case isPathWithin(readPath, deniedPath):
				candidate = readPath
			}
			if candidate == "" || seen[candidate] {
				continue
			}
			info, err := os.Stat(candidate)
			if err != nil {
				if strict {
					return nil, fmt.Errorf("pin readPaths exemption %q: %w", candidate, err)
				}
				continue
			}
			seen[candidate] = true
			binds = append(binds, linuxReadExemptionBind{
				source:      candidate,
				destination: candidate,
				info:        info,
			})
		}
	}
	return binds, nil
}

func readExemptionSourceIdentities(binds []linuxReadExemptionBind) []authorityPathIdentity {
	identities := make([]authorityPathIdentity, 0, len(binds))
	for _, bind := range binds {
		identities = append(identities, authorityPathIdentity{path: bind.source, info: bind.info})
	}
	return identities
}

func buildBwrapArgs(cfg Config, deniedPaths []DeniedPath, commandPaths ...string) []string {
	args, _ := buildBwrapArgsInternal(cfg, deniedPaths, nil, false, nil, commandPaths...)
	return args
}

func buildBwrapArgsChecked(cfg Config, deniedPaths []DeniedPath, reservations []deniedReservation, commandPaths ...string) ([]string, error) {
	return buildBwrapArgsInternal(cfg, deniedPaths, reservations, true, nil, commandPaths...)
}

func buildBwrapArgsCheckedWithSources(cfg Config, deniedPaths []DeniedPath, reservations []deniedReservation, denyWritePlan *denyWriteMountPlan, authoritySources map[string]string, commandPaths ...string) ([]string, error) {
	return buildBwrapArgsInternalWithPlan(cfg, deniedPaths, reservations, true, denyWritePlan, authoritySources, commandPaths...)
}

func buildBwrapArgsCheckedWithSourcesAndRoots(cfg Config, deniedPaths []DeniedPath, reservations []deniedReservation, denyWritePlan *denyWriteMountPlan, readExemptionBinds []linuxReadExemptionBind, authoritySources map[string]string, tempRoots, runRoots []string, commandPaths ...string) ([]string, error) {
	return buildBwrapArgsInternalWithPlanAndRoots(cfg, deniedPaths, reservations, true, denyWritePlan, readExemptionBinds, authoritySources, tempRoots, runRoots, commandPaths...)
}

func buildBwrapArgsInternal(cfg Config, deniedPaths []DeniedPath, reservations []deniedReservation, strict bool, authoritySources map[string]string, commandPaths ...string) ([]string, error) {
	return buildBwrapArgsInternalWithPlan(cfg, deniedPaths, reservations, strict, nil, authoritySources, commandPaths...)
}

func buildBwrapArgsInternalWithPlan(cfg Config, deniedPaths []DeniedPath, reservations []deniedReservation, strict bool, denyWritePlan *denyWriteMountPlan, authoritySources map[string]string, commandPaths ...string) ([]string, error) {
	tempRoots, runRoots := privateLinuxRoots()
	return buildBwrapArgsInternalWithPlanAndRoots(cfg, deniedPaths, reservations, strict, denyWritePlan, nil, authoritySources, tempRoots, runRoots, commandPaths...)
}

func buildBwrapArgsInternalWithPlanAndRoots(cfg Config, deniedPaths []DeniedPath, reservations []deniedReservation, strict bool, denyWritePlan *denyWriteMountPlan, readExemptionBinds []linuxReadExemptionBind, authoritySources map[string]string, tempRoots, runRoots []string, commandPaths ...string) ([]string, error) {
	args := []string{"bwrap", "--ro-bind", "/", "/"}
	privateRoots := append(append([]string{}, tempRoots...), runRoots...)
	privateRun := runRoots[0]
	var routingAfterPrivateRoots []string
	if !cfg.DenyWrite {
		if denyWritePlan == nil {
			planned, err := planDenyWriteMounts(cfg, strict)
			if err != nil {
				return nil, err
			}
			denyWritePlan = &planned
		}
		routingArgs, err := appendLinuxRoutingMounts(nil, cfg, privateRoots, reservations, *denyWritePlan, authoritySources)
		if err != nil {
			return nil, err
		}
		for i := 0; i < len(routingArgs); i += 3 {
			mountArgs := routingArgs[i : i+3]
			if linuxRoutingMountCoversPrivateRoot(mountArgs[2], privateRoots) {
				args = append(args, mountArgs...)
			} else {
				routingAfterPrivateRoots = append(routingAfterPrivateRoots, mountArgs...)
			}
		}
	}
	if denyWritePlan == nil {
		denyWritePlan = &denyWriteMountPlan{}
	}
	for _, root := range tempRoots {
		args = append(args, "--tmpfs", root)
	}
	for _, root := range runRoots {
		args = append(args, "--tmpfs", root)
	}

	resolvInPrivateRun := false
	if cfg.AllowNetwork {
		// systemd-resolved commonly makes /etc/resolv.conf a symlink into /run.
		if real, err := filepath.EvalSymlinks("/etc/resolv.conf"); err == nil && isPathWithin(real, privateRun) {
			source := "/etc/resolv.conf"
			if cfg.DenyDNS {
				source = "/dev/null"
			}
			args = append(args, "--ro-bind", source, real)
			resolvInPrivateRun = true
		}
	}

	// Re-expose only explicitly selected executable files hidden by private temp
	// or runtime mounts. Binding the containing directory would leak unrelated
	// host-temp contents. This includes a distinct TMPDIR-backed os.TempDir.
	seenCommandBinds := make(map[string]bool)
	for _, commandPath := range commandPaths {
		for _, privateRoot := range privateRoots {
			if !isPathWithin(commandPath, privateRoot) {
				continue
			}
			if !seenCommandBinds[commandPath] {
				args = append(args, "--ro-bind", commandPath, commandPath)
				seenCommandBinds[commandPath] = true
			}
		}
	}
	for _, readPath := range cfg.ReadPaths {
		readPath = filepath.Clean(expandTilde(readPath))
		if isWithinAny(readPath, privateRoots) {
			if _, err := os.Stat(readPath); err == nil {
				args = append(args, "--ro-bind", linuxAuthoritySource(readPath, authoritySources), readPath)
			}
		}
	}

	args = append(args, routingAfterPrivateRoots...)

	readSet := deniedReadSet(cfg)
	if readExemptionBinds == nil {
		planned, err := planLinuxReadExemptionBinds(cfg, deniedPaths, strict)
		if err != nil {
			return nil, err
		}
		readExemptionBinds = planned
	}

	protectedSchedule := planDenyWriteProtectedSchedule(*denyWritePlan, reservations)
	var reservationReadOnlyRoots []string
	for _, reservation := range reservations {
		args = append(args, "--tmpfs", reservation.root)
		rootWritable := !cfg.DenyWrite && writableByAncestor(reservation.root, cfg.WritablePaths) && !coveredByDenyWritePath(reservation.root, *denyWritePlan)
		if !rootWritable {
			reservationReadOnlyRoots = append(reservationReadOnlyRoots, reservation.root)
		}
		for _, entry := range reservation.entries {
			destination := filepath.Join(reservation.root, entry.name)
			if entry.symlink {
				args = append(args, "--symlink", entry.linkTarget, destination)
				continue
			}
			bindFlag := "--ro-bind"
			if !cfg.DenyWrite && writableByAncestor(destination, cfg.WritablePaths) && !coveredByDenyWritePath(destination, *denyWritePlan) {
				bindFlag = "--bind"
			}
			source, err := linuxPinnedSource(filepath.Join(reservation.root, entry.name), authoritySources)
			if err != nil {
				return nil, err
			}
			args = append(args, bindFlag, source, destination)
		}
		if !cfg.DenyWrite {
			for _, writable := range cfg.WritablePaths {
				writable = filepath.Clean(expandTilde(writable))
				if writable == reservation.root || !isPathWithin(writable, reservation.root) || coveredByDenyWritePath(writable, *denyWritePlan) {
					continue
				}
				rel, _ := filepath.Rel(reservation.root, writable)
				first := strings.Split(rel, string(filepath.Separator))[0]
				if reservation.omitted[first] {
					continue
				}
				if _, err := os.Stat(writable); err == nil {
					args = append(args, "--bind", linuxAuthoritySource(writable, authoritySources), writable)
				}
			}
		}
		for _, identity := range protectedSchedule.after[reservation.root] {
			source, err := linuxPinnedSource(identity.path, authoritySources)
			if err != nil {
				return nil, err
			}
			args = append(args, "--ro-bind", source, identity.path)
		}
	}

	if !cfg.DenyWrite {
		var err error
		args, err = appendDenyWriteProtectedMounts(args, *denyWritePlan, reservations, authoritySources)
		if err != nil {
			return nil, err
		}
	}

	readRestores := make(map[string]bool, len(readExemptionBinds))
	for _, bind := range readExemptionBinds {
		readRestores[filepath.Clean(bind.destination)] = true
	}
	var deniedReadOnlyPaths []string
	for _, denied := range deniedPaths {
		if isReadExempt(denied.Path, readSet) && readRestores[filepath.Clean(denied.Path)] {
			continue
		}
		switch denied.Kind {
		case DeniedPathFile:
			args = append(args, "--ro-bind", "/dev/null", denied.Path)
		default:
			args = append(args, "--tmpfs", denied.Path)
			deniedReadOnlyPaths = append(deniedReadOnlyPaths, denied.Path)
		}
	}
	for _, bind := range readExemptionBinds {
		source, err := linuxPinnedSource(bind.source, authoritySources)
		if err != nil {
			return nil, err
		}
		args = append(args, "--ro-bind", source, bind.destination)
	}
	for _, path := range deniedReadOnlyPaths {
		args = append(args, "--remount-ro", path)
	}
	for _, path := range reservationReadOnlyRoots {
		args = append(args, "--remount-ro", path)
	}
	for _, path := range runRoots {
		args = append(args, "--remount-ro", path)
	}
	if cfg.DenyWrite {
		for _, path := range tempRoots {
			args = append(args, "--remount-ro", path)
		}
	}

	args = append(args, "--dev", "/dev", "--proc", "/proc")
	if cfg.DenyWrite {
		// Fresh special filesystems are mounted after the read-only root. Keep
		// their directory trees and procfs controls read-only too; character
		// devices such as /dev/null remain usable through a read-only devtmpfs.
		args = append(args, "--remount-ro", "/dev", "--remount-ro", "/proc")
	}
	args = append(args, "--unshare-pid", "--unshare-ipc")
	if !cfg.AllowNetwork {
		args = append(args, "--unshare-net")
	} else if cfg.DenyDNS && !resolvInPrivateRun {
		args = append(args, "--ro-bind", "/dev/null", "/etc/resolv.conf")
	}
	args = append(args, "--cap-drop", "ALL", "--die-with-parent", "--new-session")
	return args, nil
}

func linuxRoutingMountCoversPrivateRoot(destination string, privateRoots []string) bool {
	for _, root := range privateRoots {
		if isPathWithin(root, destination) {
			return true
		}
	}
	return false
}

// appendAuthorityPathMounts pins every writable grant and every mutable route
// leading to a nested writable/read grant. Bind mountpoints cannot be renamed
// or replaced by one sandboxed command, so a later Wrap keeps using the
// canonical authority frozen at construction.
func linuxAuthoritySourcePaths(cfg Config, privateRoots []string) []string {
	seen := make(map[string]bool)
	var paths []string
	add := func(path string) {
		path = filepath.Clean(expandTilde(path))
		if !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	if !cfg.DenyWrite {
		for _, writable := range cfg.WritablePaths {
			writable = filepath.Clean(expandTilde(writable))
			if !pathEqualsAny(writable, privateRoots) {
				add(writable)
			}
		}
	}
	for _, readPath := range cfg.ReadPaths {
		add(readPath)
	}
	if !cfg.DenyWrite {
		for _, authority := range append(append([]string{}, cfg.WritablePaths...), cfg.ReadPaths...) {
			authority = filepath.Clean(expandTilde(authority))
			for _, writable := range cfg.WritablePaths {
				writable = filepath.Clean(expandTilde(writable))
				if pathEqualsAny(writable, privateRoots) || !pathWithinPolicy(authority, writable) {
					continue
				}
				add(authority)
				for ancestor := filepath.Dir(authority); pathWithinPolicy(ancestor, writable); ancestor = filepath.Dir(ancestor) {
					add(ancestor)
					if ancestor == writable {
						break
					}
				}
			}
		}
	}
	sort.Slice(paths, func(i, j int) bool {
		depthI := strings.Count(paths[i], string(filepath.Separator))
		depthJ := strings.Count(paths[j], string(filepath.Separator))
		if depthI != depthJ {
			return depthI < depthJ
		}
		return paths[i] < paths[j]
	})
	return paths
}

func appendLinuxRoutingMounts(args []string, cfg Config, privateRoots []string, reservations []deniedReservation, denyWritePlan denyWriteMountPlan, authoritySources map[string]string) ([]string, error) {
	readOnly := make(map[string]bool)
	var paths []string
	add := func(path string, ro bool) {
		path = filepath.Clean(path)
		if _, seen := readOnly[path]; !seen {
			paths = append(paths, path)
		}
		readOnly[path] = readOnly[path] || ro
	}
	protectedSchedule := planDenyWriteProtectedSchedule(denyWritePlan, reservations)
	earlyProtected := protectedSchedule.initial
	for _, path := range linuxAuthoritySourcePaths(cfg, privateRoots) {
		if hostWritableByAncestor(path, cfg.WritablePaths, privateRoots) && !pathBelowAny(path, earlyProtected) {
			add(path, false)
		}
	}
	for _, identity := range denyWritePlan.ancestors {
		if !pathBelowAny(identity.path, earlyProtected) {
			add(identity.path, false)
		}
	}
	for _, reservation := range reservations {
		for _, identity := range reservation.ancestors {
			if !pathBelowAny(identity.path, earlyProtected) {
				add(identity.path, false)
			}
		}
	}
	for _, identity := range earlyProtected {
		add(identity.path, true)
	}
	sort.Slice(paths, func(i, j int) bool {
		depthI := strings.Count(paths[i], string(filepath.Separator))
		depthJ := strings.Count(paths[j], string(filepath.Separator))
		if depthI != depthJ {
			return depthI < depthJ
		}
		return paths[i] < paths[j]
	})
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			if authoritySources != nil {
				return nil, fmt.Errorf("inspect frozen sandbox routing path %q: %w", path, err)
			}
			slog.Warn("sandbox_skip_frozen_authority_path", "path", path, "reason", err)
			continue
		}
		source, err := linuxPinnedSource(path, authoritySources)
		if err != nil {
			return nil, err
		}
		flag := "--bind"
		if readOnly[path] {
			flag = "--ro-bind"
		}
		args = append(args, flag, source, path)
	}
	return args, nil
}

type denyWriteProtectedSchedule struct {
	initial []authorityPathIdentity
	after   map[string][]authorityPathIdentity
	late    []authorityPathIdentity
}

func planDenyWriteProtectedSchedule(plan denyWriteMountPlan, reservations []deniedReservation) denyWriteProtectedSchedule {
	schedule := denyWriteProtectedSchedule{after: make(map[string][]authorityPathIdentity)}
	for _, identity := range plan.protected {
		deepestCovering := -1
		containsReservation := false
		for i, reservation := range reservations {
			if isPathWithin(identity.path, reservation.root) {
				if deepestCovering < 0 || pathDepth(reservation.root) > pathDepth(reservations[deepestCovering].root) {
					deepestCovering = i
				}
			}
			if isPathWithin(reservation.root, identity.path) {
				containsReservation = true
			}
		}
		if deepestCovering >= 0 {
			covering := reservations[deepestCovering]
			if identity.path == covering.root {
				// The protected path is installed initially to pin its route. The
				// reservation then replaces it with a private snapshot whose restored
				// entries and root are made read-only.
				schedule.initial = append(schedule.initial, identity)
				continue
			}
			if hiddenByDeniedReservation(identity.path, reservations[:deepestCovering+1]) {
				continue
			}
			// An outer reservation covers this protected descendant. Reinstall it
			// immediately after the deepest covering snapshot, before any nested
			// reservation can mask credentials below it.
			schedule.after[covering.root] = append(schedule.after[covering.root], identity)
			continue
		}
		if containsReservation {
			schedule.initial = append(schedule.initial, identity)
		} else {
			schedule.late = append(schedule.late, identity)
		}
	}
	return schedule
}

func pathDepth(path string) int {
	return strings.Count(filepath.Clean(path), string(filepath.Separator))
}

func pathBelowAny(path string, roots []authorityPathIdentity) bool {
	for _, root := range roots {
		if path != root.path && isPathWithin(path, root.path) {
			return true
		}
	}
	return false
}

func coveredByDenyWritePath(path string, plan denyWriteMountPlan) bool {
	for _, protected := range plan.protected {
		if isPathWithin(path, protected.path) {
			return true
		}
	}
	return false
}

func hostWritableByAncestor(path string, writablePaths, privateRoots []string) bool {
	for _, writable := range writablePaths {
		writable = filepath.Clean(expandTilde(writable))
		if !pathEqualsAny(writable, privateRoots) && pathWithinPolicy(path, writable) {
			return true
		}
	}
	return false
}

func linuxAuthoritySource(path string, sources map[string]string) string {
	path = filepath.Clean(expandTilde(path))
	if source := sources[path]; source != "" {
		return source
	}
	return path
}

type denyWriteMountPlan struct {
	ancestors []authorityPathIdentity
	protected []authorityPathIdentity
}

func planDenyWriteMounts(cfg Config, strict bool) (denyWriteMountPlan, error) {
	if cfg.DenyWrite || len(cfg.DenyWritePaths) == 0 {
		return denyWriteMountPlan{}, nil
	}
	seenAncestors := make(map[string]bool)
	seenProtected := make(map[string]bool)
	var protectedPaths []string
	for _, protected := range cfg.DenyWritePaths {
		real, err := filepath.EvalSymlinks(filepath.Clean(expandTilde(protected)))
		if err != nil {
			if strict {
				return denyWriteMountPlan{}, fmt.Errorf("resolve sandbox denyWritePaths entry %q: %w", protected, err)
			}
			continue
		}
		if !seenProtected[real] {
			seenProtected[real] = true
			protectedPaths = append(protectedPaths, real)
		}
	}
	sort.Slice(protectedPaths, func(i, j int) bool {
		return strings.Count(protectedPaths[i], string(filepath.Separator)) < strings.Count(protectedPaths[j], string(filepath.Separator))
	})
	kept := protectedPaths[:0]
	for _, protected := range protectedPaths {
		covered := false
		for _, parent := range kept {
			if isPathWithin(protected, parent) {
				covered = true
				break
			}
		}
		if !covered {
			kept = append(kept, protected)
		}
	}

	var plan denyWriteMountPlan
	for _, real := range kept {
		info, err := os.Stat(real)
		if err != nil {
			if strict {
				return denyWriteMountPlan{}, fmt.Errorf("inspect sandbox denyWritePaths entry %q: %w", real, err)
			}
			continue
		}
		plan.protected = append(plan.protected, authorityPathIdentity{path: real, info: info})
		for _, writable := range cfg.WritablePaths {
			writable = filepath.Clean(expandTilde(writable))
			if resolved, err := filepath.EvalSymlinks(writable); err == nil {
				writable = resolved
			}
			if !isPathWithin(real, writable) {
				continue
			}
			var ancestors []string
			for ancestor := filepath.Dir(real); ancestor != writable && isPathWithin(ancestor, writable); ancestor = filepath.Dir(ancestor) {
				ancestors = append(ancestors, ancestor)
			}
			for i := len(ancestors) - 1; i >= 0; i-- {
				ancestor := ancestors[i]
				if seenAncestors[ancestor] {
					continue
				}
				if _, err := os.Stat(ancestor); err != nil {
					if strict {
						return denyWriteMountPlan{}, fmt.Errorf("pin denyWritePaths ancestor %q: %w", ancestor, err)
					}
					continue
				}
				info, err := os.Stat(ancestor)
				if err != nil {
					if strict {
						return denyWriteMountPlan{}, fmt.Errorf("pin denyWritePaths ancestor %q: %w", ancestor, err)
					}
					continue
				}
				plan.ancestors = append(plan.ancestors, authorityPathIdentity{path: ancestor, info: info})
				seenAncestors[ancestor] = true
			}
		}
	}
	return plan, nil
}

func appendDenyWriteProtectedMounts(args []string, plan denyWriteMountPlan, reservations []deniedReservation, sources map[string]string) ([]string, error) {
	// Writable routing ancestors are mounted globally before any reservation or
	// deny mask. Only protected leaves that neither contain nor sit behind a
	// reservation may be installed at this later phase; another parent bind here
	// could reopen an earlier credential mask.
	for _, identity := range planDenyWriteProtectedSchedule(plan, reservations).late {
		source, err := linuxPinnedSource(identity.path, sources)
		if err != nil {
			return nil, err
		}
		args = append(args, "--ro-bind", source, identity.path)
	}
	return args, nil
}

func hiddenByDeniedReservation(path string, reservations []deniedReservation) bool {
	for _, reservation := range reservations {
		for omitted := range reservation.omitted {
			if isPathWithin(path, filepath.Join(reservation.root, omitted)) {
				return true
			}
		}
	}
	return false
}

func linuxPinnedSource(path string, sources map[string]string) (string, error) {
	path = filepath.Clean(expandTilde(path))
	if sources == nil {
		return path, nil
	}
	if source := sources[path]; source != "" {
		return source, nil
	}
	return "", fmt.Errorf("sandbox mount source %q was not pinned", path)
}

func writableByAncestor(path string, writablePaths []string) bool {
	for _, writable := range writablePaths {
		writable = filepath.Clean(expandTilde(writable))
		if isPathWithin(path, writable) {
			return true
		}
	}
	return false
}

func isWithinAny(path string, roots []string) bool {
	for _, root := range roots {
		if isPathWithin(path, root) {
			return true
		}
	}
	return false
}

func pathEqualsAny(path string, roots []string) bool {
	path = filepath.Clean(path)
	for _, root := range roots {
		if path == filepath.Clean(root) {
			return true
		}
	}
	return false
}

func pathExplicitlyExposed(path string, cfg Config) bool {
	tempRoots, runRoots := privateLinuxRoots()
	privateRoots := append(append([]string{}, tempRoots...), runRoots...)
	return pathExplicitlyExposedWithRoots(path, cfg, privateRoots)
}

func pathExplicitlyExposedWithRoots(path string, cfg Config, privateRoots []string) bool {
	if !cfg.DenyWrite {
		for _, writable := range cfg.WritablePaths {
			writable = filepath.Clean(expandTilde(writable))
			if pathEqualsAny(writable, privateRoots) {
				continue
			}
			if isPathWithin(path, writable) {
				return true
			}
		}
	}
	for _, readPath := range cfg.ReadPaths {
		readPath = filepath.Clean(expandTilde(readPath))
		if isPathWithin(path, readPath) {
			return true
		}
	}
	for _, alias := range cfg.readPathAliases {
		if isPathWithin(path, alias.path) || isPathWithin(alias.path, path) {
			return true
		}
		for _, symlink := range alias.symlinks {
			if isPathWithin(path, symlink.path) || isPathWithin(symlink.path, path) {
				return true
			}
		}
	}
	return false
}

func isPathWithin(path, parent string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
