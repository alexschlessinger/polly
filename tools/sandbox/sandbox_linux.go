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
	"slices"
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
	homeRoots      []string
	authorityPaths []authorityPathIdentity
}

// New creates a Sandbox for Linux using bubblewrap (bwrap).
func New(cfg Config) (Sandbox, error) {
	if err := validateLinuxBwrapExecutable(linuxBwrapPath); err != nil {
		return nil, err
	}
	tempRoots, runRoots := privateLinuxRoots()
	homeRoots, err := linuxPrivateHomeRoots()
	if err != nil {
		return nil, err
	}
	cfg, err = prepareLinuxConfig(cfg, tempRoots, runRoots, homeRoots)
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
	privateRoots := concatStrings(concatStrings(tempRoots, runRoots), homeRoots)
	authorityPaths, err := captureAuthorityPathIdentities(linuxAuthoritySourcePaths(cfg, privateRoots))
	if err != nil {
		return nil, err
	}
	return &linuxSandbox{
		cfg:            cfg,
		bwrapPath:      linuxBwrapPath,
		tempRoots:      tempRoots,
		runRoots:       runRoots,
		homeRoots:      homeRoots,
		authorityPaths: authorityPaths,
	}, nil
}

func prepareLinuxConfig(cfg Config, tempRoots, runRoots, homeRoots []string) (Config, error) {
	var err error
	cfg, err = normalizeConfigPaths(cfg)
	if err != nil {
		return Config{}, err
	}
	privateRoots := concatStrings(concatStrings(tempRoots, runRoots), homeRoots)
	cfg, err = freezeAuthorityPaths(cfg, privateRoots...)
	if err != nil {
		return Config{}, err
	}
	if err := rejectHomeGrant(cfg, homeRoots); err != nil {
		return Config{}, err
	}
	return applyFinalGitPolicyWithHostWritable(cfg, func(path string) bool {
		return !pathEqualsAny(path, privateRoots)
	})
}

func freezeAuthorityPathsForPlatform(cfg Config) (Config, error) {
	// PrepareConfig may be retained and passed to New after TMPDIR changes.
	// Avoid discarding descendant grants based on a private-root snapshot that
	// is not yet bound to a sandbox instance; prepareLinuxConfig performs the
	// final minimization against the roots captured by New.
	cfg, err := freezeAuthorityPaths(cfg, concatStrings(allPrivateLinuxRoots(), cfg.WritablePaths)...)
	if err != nil {
		return Config{}, err
	}
	if home := resolvedHomeDir(); home != "" {
		if err := rejectHomeGrant(cfg, []string{home}); err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
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
				if PathWithin(candidate, root) || PathWithin(root, candidate) {
					return intersectionError(root)
				}
			}
			return nil
		}
		checkTraversal := func(candidate string) error {
			for _, root := range []string{"/dev", "/proc"} {
				if PathWithin(candidate, root) {
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

func privateLinuxRoots() (tempRoots []string, runRoots []string) {
	add := func(paths *[]string, path string) {
		path = filepath.Clean(path)
		if real, err := filepath.EvalSymlinks(path); err == nil {
			path = real
		}
		for _, existing := range *paths {
			if path == existing || PathWithin(path, existing) {
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

// linuxPrivateHomeRoots names the home directory as a private root. Nothing
// under it is visible to a command unless a grant re-binds it, so the
// credential list, sibling workspaces and runtime state need no masking of
// their own. A home directory that cannot be resolved, is the filesystem
// root, or is not a directory cannot be kept private and fails construction.
func linuxPrivateHomeRoots() ([]string, error) {
	home, err := privateHomeRoot()
	if err != nil {
		return nil, err
	}
	roots := []string{home}
	// Custom XDG storage may lie outside both home and temp.
	for _, path := range traversablePrivateRoots() {
		if info, err := os.Stat(path); err == nil && info.IsDir() && !isWithinAny(path, roots) {
			roots = append(roots, path)
		}
	}
	return roots, nil
}

// allPrivateLinuxRoots returns the temp, run and home roots as one list for
// callers that do not distinguish them.
func allPrivateLinuxRoots() []string {
	tempRoots, runRoots := privateLinuxRoots()
	roots := concatStrings(tempRoots, runRoots)
	if home := resolvedHomeDir(); home != "" {
		roots = append(roots, home)
	}
	roots = append(roots, traversablePrivateRoots()...)
	return roots
}

// linuxGrant is a path a command may see inside private roots: bound
// read-write for a writable grant, read-only otherwise.
type linuxGrant struct {
	path     string
	writable bool
}

// planLinuxGrants lists the writable and read grants, shallow first. A grant
// equal to a private root or to the filesystem root is dropped (the root wins
// that tie), and a writable grant wins over a read grant at the same path.
func planLinuxGrants(cfg Config, roots []string) []linuxGrant {
	var grants []linuxGrant
	seen := make(map[string]bool)
	add := func(path string, writable bool) {
		path = filepath.Clean(expandTilde(path))
		if seen[path] || path == string(filepath.Separator) || pathEqualsAny(path, roots) {
			return
		}
		seen[path] = true
		grants = append(grants, linuxGrant{path: path, writable: writable})
	}
	if !cfg.DenyWrite {
		for _, path := range cfg.WritablePaths {
			add(path, true)
		}
	}
	for _, path := range readAuthorityPaths(cfg) {
		add(path, false)
	}
	slices.SortFunc(grants, func(a, b linuxGrant) int { return comparePathDepth(a.path, b.path) })
	return grants
}

// linuxMask is a denied path that still needs a mount: a directory becomes a
// read-only tmpfs, a file is covered by /dev/null.
type linuxMask struct {
	path string
	dir  bool
}

// planLinuxMasks resolves the denied paths that still need a mount. An entry
// is dropped when it is missing, equal to a grant (the grant wins that tie
// and is reported in islands, so a read grant stays read-only there), or
// already hidden by a private root or an outer mask with no grant in between.
// Any other inspection failure fails closed.
func planLinuxMasks(cfg Config, grants []linuxGrant, roots []string) (masks []linuxMask, islands []string, err error) {
	var candidates []linuxMask
	seen := make(map[string]bool)
	for _, denied := range allDeniedPaths(cfg) {
		path := filepath.Clean(denied.Path)
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
				continue
			}
			return nil, nil, fmt.Errorf("resolve sandbox denied path %q: %w", path, err)
		}
		real = filepath.Clean(real)
		if seen[real] || real == string(filepath.Separator) {
			continue
		}
		info, err := os.Stat(real)
		if err != nil {
			return nil, nil, fmt.Errorf("inspect sandbox denied path %q: %w", real, err)
		}
		seen[real] = true
		candidates = append(candidates, linuxMask{path: real, dir: info.IsDir()})
	}
	slices.SortFunc(candidates, func(a, b linuxMask) int { return comparePathDepth(a.path, b.path) })
	grantPaths := make([]string, 0, len(grants))
	for _, grant := range grants {
		grantPaths = append(grantPaths, grant.path)
	}
	hidden := append([]string(nil), roots...)
	for _, candidate := range candidates {
		if pathEqualsAny(candidate.path, roots) {
			// A private root already hides everything; a second mount there
			// would conflict with the root's own tmpfs.
			continue
		}
		if pathEqualsAny(candidate.path, grantPaths) {
			islands = append(islands, candidate.path)
			continue
		}
		if deepestStrictlyContaining(candidate.path, hidden) > deepestStrictlyContaining(candidate.path, grantPaths) {
			continue
		}
		masks = append(masks, candidate)
		if candidate.dir {
			hidden = append(hidden, candidate.path)
		}
	}
	return masks, islands, nil
}

// deepestStrictlyContaining returns the depth of the deepest route that
// lexically contains path without equalling it, or -1 when none does.
func deepestStrictlyContaining(path string, routes []string) int {
	deepest := -1
	for _, route := range routes {
		if route != path && PathWithin(path, route) {
			if depth := pathDepth(route); depth > deepest {
				deepest = depth
			}
		}
	}
	return deepest
}

type linuxRuleKind uint8

const (
	linuxRuleNone linuxRuleKind = iota
	linuxRuleHidden
	linuxRuleWritable
	linuxRuleReadOnly
)

// linuxRuleSet tracks the path-scoped rules already planned so each later
// mount can ask what its nearest enclosing rule is: the deepest rule wins.
type linuxRuleSet struct {
	hidden   []string
	writable []string
	readOnly []string
}

func (r *linuxRuleSet) nearest(path string) linuxRuleKind {
	kind := linuxRuleNone
	deepest := -1
	for _, entry := range []struct {
		kind   linuxRuleKind
		routes []string
	}{{linuxRuleHidden, r.hidden}, {linuxRuleWritable, r.writable}, {linuxRuleReadOnly, r.readOnly}} {
		if depth := deepestStrictlyContaining(path, entry.routes); depth > deepest {
			deepest, kind = depth, entry.kind
		}
	}
	return kind
}

type linuxMountKind uint8

const (
	linuxMountTmpfs linuxMountKind = iota
	linuxMountBind
	linuxMountROBind
	linuxMountSymlink
	linuxMountDevNull
)

// linuxMountOp is one bubblewrap mount. A pinned source is routed through
// the frozen descriptor recorded for it; an unpinned source is used as is.
type linuxMountOp struct {
	kind   linuxMountKind
	dest   string
	source string
	pinned bool
}

// linuxMountPlan is the depth-ordered mount sequence for one command plus the
// read-only remounts that follow it. hidden and visible let the working
// directory decision reuse the same rules.
type linuxMountPlan struct {
	ops                []linuxMountOp
	remountRO          []string
	hidden             []string
	visible            []string
	resolvInPrivateRun bool
}

// planLinuxMounts assembles every mount for one command: a tmpfs over each
// private root and kept directory mask, binds for the grants that would
// otherwise be hidden, writable binds for every writable grant, the pinned
// ancestors and read-only leaves of the deny-write plan, recreated symlinks
// for granted spellings the tmpfs erased, the command executable and granted
// sockets when hidden, and /dev/null over file masks. Ordering by depth makes
// every mount land on top of the one that contains it.
func planLinuxMounts(cfg Config, roots linuxPrivateRootSet, grants []linuxGrant, masks []linuxMask, islands []string, denyWritePlan denyWriteMountPlan, socketBinds []linuxUnixSocketBind, commandPaths []string) (linuxMountPlan, error) {
	var plan linuxMountPlan
	ops := make(map[string]linuxMountOp)
	add := func(op linuxMountOp) error {
		op.dest = filepath.Clean(op.dest)
		existing, exists := ops[op.dest]
		switch {
		case !exists:
			ops[op.dest] = op
		case existing.kind == linuxMountBind && op.kind == linuxMountROBind:
			// A deny-write leaf equal to a writable grant stays read-only.
			ops[op.dest] = op
		case existing.kind == linuxMountROBind && op.kind == linuxMountBind:
		case op.kind == linuxMountSymlink, op.kind == linuxMountROBind && !op.pinned:
			// A symlink or command re-exposure never replaces a planned mount.
		case existing.kind == linuxMountROBind && op.kind == linuxMountROBind && existing.source == op.source:
			// A read grant tying a denied path and a deny-write leaf at the
			// same path ask for the same read-only bind.
		default:
			return fmt.Errorf("conflicting sandbox mounts at %q", op.dest)
		}
		return nil
	}
	rules := linuxRuleSet{hidden: append([]string(nil), roots.all()...)}
	for _, root := range roots.all() {
		if err := add(linuxMountOp{kind: linuxMountTmpfs, dest: root}); err != nil {
			return linuxMountPlan{}, err
		}
	}
	// A writable grant that ties with a denied path is the read-only island
	// the deny asked for: writes never win that tie.
	grants = append([]linuxGrant(nil), grants...)
	for i := range grants {
		if grants[i].writable && pathEqualsAny(grants[i].path, islands) {
			grants[i].writable = false
		}
	}
	for _, grant := range grants {
		if grant.writable {
			rules.writable = append(rules.writable, grant.path)
			if err := add(linuxMountOp{kind: linuxMountBind, dest: grant.path, source: grant.path, pinned: true}); err != nil {
				return linuxMountPlan{}, err
			}
		} else {
			rules.readOnly = append(rules.readOnly, grant.path)
		}
	}
	for _, mask := range masks {
		if mask.dir {
			rules.hidden = append(rules.hidden, mask.path)
			plan.remountRO = append(plan.remountRO, mask.path)
			if err := add(linuxMountOp{kind: linuxMountTmpfs, dest: mask.path}); err != nil {
				return linuxMountPlan{}, err
			}
		} else if err := add(linuxMountOp{kind: linuxMountDevNull, dest: mask.path}); err != nil {
			return linuxMountPlan{}, err
		}
	}
	// A read grant is bound where a private root or mask would otherwise hide
	// it. A read grant that ties with a denied path is bound everywhere: inside
	// a writable tree it is the read-only island the deny asked for.
	for _, grant := range grants {
		if grant.writable {
			continue
		}
		if rules.nearest(grant.path) != linuxRuleHidden && !pathEqualsAny(grant.path, islands) {
			continue
		}
		if err := add(linuxMountOp{kind: linuxMountROBind, dest: grant.path, source: grant.path, pinned: true}); err != nil {
			return linuxMountPlan{}, err
		}
	}
	if !cfg.DenyWrite {
		ancestors := append([]authorityPathIdentity(nil), denyWritePlan.ancestors...)
		slices.SortFunc(ancestors, func(a, b authorityPathIdentity) int { return comparePathDepth(a.path, b.path) })
		for _, ancestor := range ancestors {
			if rules.nearest(ancestor.path) != linuxRuleWritable {
				continue
			}
			rules.writable = append(rules.writable, ancestor.path)
			if err := add(linuxMountOp{kind: linuxMountBind, dest: ancestor.path, source: ancestor.path, pinned: true}); err != nil {
				return linuxMountPlan{}, err
			}
		}
		for _, leaf := range denyWritePlan.protected {
			if rules.nearest(leaf.path) != linuxRuleWritable && !slices.Contains(rules.writable, leaf.path) {
				continue
			}
			rules.readOnly = append(rules.readOnly, leaf.path)
			if err := add(linuxMountOp{kind: linuxMountROBind, dest: leaf.path, source: leaf.path, pinned: true}); err != nil {
				return linuxMountPlan{}, err
			}
		}
	}
	var symlinks []string
	for _, link := range cfg.grantSymlinks {
		if rules.nearest(link.path) != linuxRuleHidden || isStrictlyWithinAny(link.path, symlinks) {
			continue
		}
		symlinks = append(symlinks, link.path)
		if err := add(linuxMountOp{kind: linuxMountSymlink, dest: link.path, source: link.target}); err != nil {
			return linuxMountPlan{}, err
		}
	}
	for _, commandPath := range commandPaths {
		commandPath = filepath.Clean(commandPath)
		if rules.nearest(commandPath) != linuxRuleHidden {
			continue
		}
		if err := add(linuxMountOp{kind: linuxMountROBind, dest: commandPath, source: commandPath}); err != nil {
			return linuxMountPlan{}, err
		}
	}
	for _, bind := range socketBinds {
		if rules.nearest(bind.path) != linuxRuleHidden {
			continue
		}
		if err := add(linuxMountOp{kind: linuxMountROBind, dest: bind.path, source: bind.path, pinned: true}); err != nil {
			return linuxMountPlan{}, err
		}
	}
	if cfg.AllowNetwork {
		// systemd-resolved commonly makes /etc/resolv.conf a symlink into /run.
		// The private /run hides the target, so it is bound back, unless DNS
		// or the file itself is denied: an explicit mask under /run needs no
		// mount of its own and must not be undone by the re-exposure.
		if real, err := filepath.EvalSymlinks("/etc/resolv.conf"); err == nil && isWithinAny(real, roots.run) {
			source := "/etc/resolv.conf"
			if cfg.DenyDNS || ReadMasked(cfg, real) != nil {
				source = "/dev/null"
			}
			if err := add(linuxMountOp{kind: linuxMountROBind, dest: real, source: source}); err != nil {
				return linuxMountPlan{}, err
			}
			plan.resolvInPrivateRun = true
		}
	}
	for _, op := range ops {
		plan.ops = append(plan.ops, op)
	}
	slices.SortFunc(plan.ops, func(a, b linuxMountOp) int { return comparePathDepth(a.dest, b.dest) })
	slices.SortFunc(plan.remountRO, func(a, b string) int { return -comparePathDepth(a, b) })
	plan.remountRO = append(plan.remountRO, roots.run...)
	if cfg.DenyWrite {
		plan.remountRO = append(plan.remountRO, roots.temp...)
		plan.remountRO = append(plan.remountRO, roots.home...)
	}
	plan.hidden = rules.hidden
	plan.visible = concatStrings(rules.writable, rules.readOnly)
	return plan, nil
}

// isStrictlyWithinAny reports whether path lies beneath one of roots without
// equalling it.
func isStrictlyWithinAny(path string, roots []string) bool {
	return deepestStrictlyContaining(path, roots) >= 0
}

// linuxPrivateRootSet groups the private roots by the treatment they get:
// temp roots stay writable unless DenyWrite, run roots are always read-only,
// and the home root follows the temp roots.
type linuxPrivateRootSet struct {
	temp, run, home []string
}

// all lists the temp, run and home roots once each: TMPDIR may be the home
// directory, and one tmpfs per root is all the plan may emit.
func (r linuxPrivateRootSet) all() []string {
	roots := concatStrings(concatStrings(r.temp, r.run), r.home)
	seen := make(map[string]bool, len(roots))
	kept := roots[:0]
	for _, root := range roots {
		if !seen[root] {
			seen[root] = true
			kept = append(kept, root)
		}
	}
	return kept
}

// linuxWorkingDirectory selects the target's cwd: the host directory when the
// deepest rule containing it keeps it visible, else the root. Equality counts,
// so a cwd that is itself a private root resets and one that is itself a
// grant survives.
func linuxWorkingDirectory(workingDir string, plan linuxMountPlan) string {
	if deepestContainingLexical(workingDir, plan.hidden) > deepestContainingLexical(workingDir, plan.visible) {
		return string(filepath.Separator)
	}
	return workingDir
}

func deepestContainingLexical(path string, routes []string) int {
	deepest := -1
	for _, route := range routes {
		if PathWithin(path, route) {
			if depth := pathDepth(route); depth > deepest {
				deepest = depth
			}
		}
	}
	return deepest
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

func (s *linuxSandbox) privateRoots() linuxPrivateRootSet {
	return linuxPrivateRootSet{temp: s.tempRoots, run: s.runRoots, home: s.homeRoots}
}

func (s *linuxSandbox) wrapManaged(cmd *exec.Cmd, explicitEnv map[string]string) error {
	if err := validateExplicitEnv(explicitEnv); err != nil {
		return err
	}
	if err := validateLinuxSpecialMountRestrictions(s.cfg); err != nil {
		return err
	}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	filtered, stripped := filterEnv(env, s.cfg.AllowEnv, s.cfg.PassEnv)
	filtered = mergeExplicitEnv(filtered, explicitEnv)
	for i, entry := range filtered {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "TMPDIR", "TMP", "TEMP":
			filtered[i] = name + "=/tmp"
		}
	}
	if len(s.cfg.Env) > 0 {
		// Policy env is the final layer: a scratch TMPDIR bound through
		// WritablePaths must survive the private-temp rewrite above.
		filtered = mergeExplicitEnv(filtered, s.cfg.Env)
	}

	cfg := s.cfg
	if !cfg.DenyWrite {
		resolvedDenyWrite, err := resolveDenyWritePaths(cfg.DenyWritePaths, cfg.WritablePaths)
		if err != nil {
			return err
		}
		cfg.DenyWritePaths = resolvedDenyWrite
	}
	roots := s.privateRoots()
	grants := planLinuxGrants(cfg, roots.all())
	masks, islands, err := planLinuxMasks(cfg, grants, roots.all())
	if err != nil {
		return err
	}
	socketGrants := effectiveUnixSocketGrants(cfg)
	socketBinds := planLinuxUnixSocketBinds(socketGrants)
	denyWritePlan, err := planDenyWriteMounts(cfg, true)
	if err != nil {
		return err
	}

	origArgs := cmd.Args
	origPath, err := resolvedExecutablePath(cmd)
	if err != nil {
		return err
	}
	plan, err := planLinuxMounts(cfg, roots, grants, masks, islands, denyWritePlan, socketBinds, []string{origPath})
	if err != nil {
		return err
	}
	slog.Debug("sandbox_wrap",
		"command", commandSummary(origArgs),
		"network", cfg.AllowNetwork,
		"deny_write", cfg.DenyWrite,
		"writable_paths", cfg.WritablePaths,
		"env_stripped", stripped,
		"private_roots", len(roots.all()),
		"grants", len(grants),
		"masks", len(masks),
		"unix_sockets", len(socketGrants))
	cmd.Path = s.bwrapPath
	workingDir, err := resolvedCommandDir(cmd.Dir)
	if err != nil {
		return err
	}
	targetWorkingDir := linuxWorkingDirectory(workingDir, plan)
	// Descriptors appended below are owned by wrapCmdManaged, which closes and
	// trims them if any later step fails.
	bootstrapFD, err := attachLinuxBootstrapExecutable(cmd)
	if err != nil {
		return fmt.Errorf("prepare target environment bootstrap: %w", err)
	}
	targetEnvFD, err := attachLinuxTargetEnvironment(cmd, filtered)
	if err != nil {
		return fmt.Errorf("prepare target environment: %w", err)
	}
	pinnedIdentities := cloneAuthorityPathIdentities(s.authorityPaths)
	pinnedIdentities = append(pinnedIdentities, denyWritePlan.ancestors...)
	pinnedIdentities = append(pinnedIdentities, denyWritePlan.protected...)
	pinnedIdentities = append(pinnedIdentities, unixSocketBindIdentities(socketBinds)...)
	authoritySources, authorityFDs, err := attachLinuxAuthorityPaths(cmd, pinnedIdentities)
	if err != nil {
		return err
	}
	seccompFD, err := attachUnixSocketFilter(cmd, cfg.AllowNetwork, len(socketGrants) > 0)
	if err != nil {
		return fmt.Errorf("prepare seccomp filter: %w", err)
	}
	args, err := bwrapArgs(cfg, plan, authoritySources)
	if err != nil {
		return err
	}
	args = append(args, "--chdir", targetWorkingDir)
	args = append(args, "--seccomp", strconv.Itoa(seccompFD))
	targetArgs := origArgs
	if len(targetArgs) == 0 {
		targetArgs = []string{origPath}
	}
	bootstrapPath := "/proc/self/fd/" + strconv.Itoa(bootstrapFD)
	cmd.Args = make([]string, 0, len(args)+7+len(targetArgs))
	cmd.Args = append(cmd.Args, args...)
	cmd.Args = append(cmd.Args, "--")
	cmd.Args = append(cmd.Args, bootstrapPath, linuxEnvBootstrapArg,
		strconv.Itoa(bootstrapFD), strconv.Itoa(targetEnvFD), formatLinuxFDList(authorityFDs), origPath)
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
	return appendExtraFile(cmd, f), nil
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
		fd := appendExtraFile(cmd, file)
		fds = append(fds, fd)
		sources[identity.path] = "/proc/self/fd/" + strconv.Itoa(fd)
	}
	return sources, fds, nil
}

// linuxUnixSocketBind re-exposes one granted Unix socket at its own path when
// a private root or mask hides it. Sockets elsewhere need no bind: the
// read-only root already makes them visible (connect() is exempt from the
// read-only mount check for sockets), and only seccomp gated them.
type linuxUnixSocketBind struct {
	path string
	info os.FileInfo
}

func planLinuxUnixSocketBinds(grants []unixSocketGrant) []linuxUnixSocketBind {
	binds := make([]linuxUnixSocketBind, 0, len(grants))
	for _, grant := range grants {
		binds = append(binds, linuxUnixSocketBind{path: grant.path, info: grant.info})
	}
	return binds
}

func unixSocketBindIdentities(binds []linuxUnixSocketBind) []authorityPathIdentity {
	identities := make([]authorityPathIdentity, 0, len(binds))
	for _, bind := range binds {
		identities = append(identities, authorityPathIdentity{path: bind.path, info: bind.info})
	}
	return identities
}

// bwrapArgs renders a mount plan as the bubblewrap argument vector: the host
// read-only at the root, the plan's mounts in depth order with every pinned
// source routed through its frozen descriptor, the read-only remounts, fresh
// /dev and /proc, and the namespace and capability settings. A nil sources
// map (as the tests pass) uses the host paths directly.
func bwrapArgs(cfg Config, plan linuxMountPlan, sources map[string]string) ([]string, error) {
	args := []string{"bwrap", "--ro-bind", "/", "/"}
	for _, op := range plan.ops {
		switch op.kind {
		case linuxMountTmpfs:
			args = append(args, "--tmpfs", op.dest)
		case linuxMountBind, linuxMountROBind:
			source := op.source
			if op.pinned {
				var err error
				if source, err = linuxPinnedSource(op.source, sources); err != nil {
					return nil, err
				}
			}
			flag := "--bind"
			if op.kind == linuxMountROBind {
				flag = "--ro-bind"
			}
			args = append(args, flag, source, op.dest)
		case linuxMountSymlink:
			args = append(args, "--symlink", op.source, op.dest)
		case linuxMountDevNull:
			args = append(args, "--ro-bind", "/dev/null", op.dest)
		}
	}
	for _, path := range plan.remountRO {
		args = append(args, "--remount-ro", path)
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
	} else if cfg.DenyDNS && !plan.resolvInPrivateRun {
		args = append(args, "--ro-bind", "/dev/null", "/etc/resolv.conf")
	}
	args = append(args, "--cap-drop", "ALL", "--die-with-parent", "--new-session")
	return args, nil
}

// linuxAuthoritySourcePaths pins every writable grant, every read grant, and
// every mutable route leading to a nested grant inside a writable tree. Bind
// mountpoints cannot be renamed or replaced by one sandboxed command, so a
// later Wrap keeps using the canonical authority frozen at construction.
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
	for _, readPath := range readAuthorityPaths(cfg) {
		add(readPath)
	}
	if !cfg.DenyWrite {
		for _, authority := range concatStrings(cfg.WritablePaths, readAuthorityPaths(cfg)) {
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
	slices.SortFunc(paths, comparePathDepth)
	return paths
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
			if PathWithin(protected, parent) {
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
			if !PathWithin(real, writable) {
				continue
			}
			var ancestors []string
			for ancestor := filepath.Dir(real); ancestor != writable && PathWithin(ancestor, writable); ancestor = filepath.Dir(ancestor) {
				ancestors = append(ancestors, ancestor)
			}
			for i := len(ancestors) - 1; i >= 0; i-- {
				ancestor := ancestors[i]
				if seenAncestors[ancestor] {
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
