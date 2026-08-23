package sandbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

// Sandbox wraps an exec.Cmd to apply platform-specific restrictions. Built-in
// backends own descriptors needed until process start and therefore reject raw
// Wrap calls with ErrManagedWrapRequired; execution call sites must use
// WrapCmdManaged so those descriptors are closed after Start/Run.
type Sandbox interface {
	// Wrap modifies cmd so it runs inside the sandbox.
	// The original command and args are preserved semantically;
	// the implementation may prepend a wrapper binary.
	Wrap(cmd *exec.Cmd) error
}

// ExplicitEnvSandbox can distinguish environment variables explicitly chosen
// for one tool from ambient variables inherited by the parent process. Built-in
// sandboxes implement this so credential-shaped explicit values can reach only
// the target process without being exposed to a wrapper such as bwrap.
type ExplicitEnvSandbox interface {
	WrapWithEnv(cmd *exec.Cmd, explicitEnv map[string]string) error
}

// managedCommandSandbox is implemented by built-in backends whose wrapping
// allocates parent-owned descriptors. Keeping this method private prevents
// external Sandbox implementations from accidentally opting into lifecycle
// semantics they cannot satisfy.
type managedCommandSandbox interface {
	wrapManaged(cmd *exec.Cmd, explicitEnv map[string]string) error
}

// ErrManagedWrapRequired is returned when a descriptor-owning built-in backend
// is invoked through the legacy cleanup-less Wrap or WrapCmd API. Use
// WrapCmdManaged (or WrapCmdWithEnvManaged) and invoke its cleanup after Start,
// Run, Output, or CombinedOutput returns.
var ErrManagedWrapRequired = errors.New("sandbox backend requires WrapCmdManaged or WrapCmdWithEnvManaged for deterministic descriptor cleanup")

// WrapCmd applies sandbox restrictions to cmd if sb is non-nil. Returns nil
// when sb is nil (no sandbox available). Built-in backends return
// ErrManagedWrapRequired before mutating cmd; use WrapCmdManaged for them.
func WrapCmd(sb Sandbox, cmd *exec.Cmd) error {
	if sb == nil {
		return nil
	}
	return sb.Wrap(cmd)
}

// WrapCmdManaged applies a sandbox and returns an idempotent cleanup for only
// the descriptors appended by that Wrap call. Invoke cleanup after cmd.Start
// returns (or after Run/Output/CombinedOutput). Caller-owned ExtraFiles are
// never closed, including files appended by the caller after wrapping.
func WrapCmdManaged(sb Sandbox, cmd *exec.Cmd) (func() error, error) {
	if managed, ok := sb.(managedCommandSandbox); ok {
		return wrapCmdManaged(cmd, func() error { return managed.wrapManaged(cmd, nil) })
	}
	return wrapCmdManaged(cmd, func() error { return WrapCmd(sb, cmd) })
}

// WrapCmdWithEnv applies sandbox restrictions and adds environment variables
// explicitly configured for the target. Explicit values override inherited
// values and are merged after ambient-secret filtering by built-in sandboxes.
// Built-in backends return ErrManagedWrapRequired before mutating cmd; use
// WrapCmdWithEnvManaged for them.
func WrapCmdWithEnv(sb Sandbox, cmd *exec.Cmd, explicitEnv map[string]string) error {
	if len(explicitEnv) == 0 {
		return WrapCmd(sb, cmd)
	}
	if err := validateExplicitEnv(explicitEnv); err != nil {
		return err
	}
	if sb == nil {
		cmd.Env = mergeExplicitEnv(cmd.Env, explicitEnv)
		return nil
	}
	if explicitSandbox, ok := sb.(ExplicitEnvSandbox); ok {
		return explicitSandbox.WrapWithEnv(cmd, explicitEnv)
	}
	return fmt.Errorf("sandbox %T cannot safely pass explicit target environment", sb)
}

// WrapCmdWithEnvManaged is WrapCmdWithEnv with descriptor lifecycle cleanup.
func WrapCmdWithEnvManaged(sb Sandbox, cmd *exec.Cmd, explicitEnv map[string]string) (func() error, error) {
	if len(explicitEnv) == 0 {
		return WrapCmdManaged(sb, cmd)
	}
	if err := validateExplicitEnv(explicitEnv); err != nil {
		return func() error { return nil }, err
	}
	if managed, ok := sb.(managedCommandSandbox); ok {
		return wrapCmdManaged(cmd, func() error { return managed.wrapManaged(cmd, explicitEnv) })
	}
	return wrapCmdManaged(cmd, func() error { return WrapCmdWithEnv(sb, cmd, explicitEnv) })
}

func wrapCmdManaged(cmd *exec.Cmd, wrap func() error) (func() error, error) {
	start := len(cmd.ExtraFiles)
	err := wrap()
	var owned []*os.File
	if len(cmd.ExtraFiles) >= start {
		owned = append(owned, cmd.ExtraFiles[start:]...)
	}
	cleanup := sandboxFileCleanup(cmd, owned)
	if err != nil {
		_ = cleanup()
		return func() error { return nil }, err
	}
	return cleanup, nil
}

func sandboxFileCleanup(cmd *exec.Cmd, owned []*os.File) func() error {
	var once sync.Once
	var result error
	return func() error {
		once.Do(func() {
			ownedSet := make(map[*os.File]bool, len(owned))
			var closeErrors []error
			for _, file := range owned {
				ownedSet[file] = true
				if err := file.Close(); err != nil && !errors.Is(err, os.ErrInvalid) {
					closeErrors = append(closeErrors, err)
				}
			}
			kept := cmd.ExtraFiles[:0]
			for _, file := range cmd.ExtraFiles {
				if !ownedSet[file] {
					kept = append(kept, file)
				}
			}
			cmd.ExtraFiles = kept
			result = errors.Join(closeErrors...)
		})
		return result
	}
}

func validateExplicitEnv(explicitEnv map[string]string) error {
	for name, value := range explicitEnv {
		if name == "" || strings.ContainsRune(name, '=') || strings.IndexByte(name, 0) >= 0 {
			return fmt.Errorf("invalid explicit target environment name %q", name)
		}
		if strings.IndexByte(value, 0) >= 0 {
			return fmt.Errorf("explicit target environment value for %q contains NUL", name)
		}
	}
	return nil
}

func mergeExplicitEnv(env []string, explicitEnv map[string]string) []string {
	if env == nil {
		env = os.Environ()
	}
	merged := make([]string, 0, len(env)+len(explicitEnv))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if _, overridden := explicitEnv[name]; !overridden {
			merged = append(merged, entry)
		}
	}
	keys := make([]string, 0, len(explicitEnv))
	for name := range explicitEnv {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		merged = append(merged, name+"="+explicitEnv[name])
	}
	return merged
}

// resolveDenyWritePaths validates and resolves every write-denied path at the
// point a command is wrapped. A missing or unresolvable guardrail must fail the
// command instead of being silently skipped.
func resolveDenyWritePaths(paths, writablePaths []string) ([]string, error) {
	resolved := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		expanded := filepath.Clean(expandTilde(path))
		for _, writable := range writablePaths {
			writable = filepath.Clean(expandTilde(writable))
			rel, relErr := filepath.Rel(writable, expanded)
			if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			current := writable
			for _, component := range strings.Split(rel, string(filepath.Separator)) {
				if component == "" || component == "." {
					continue
				}
				current = filepath.Join(current, component)
				info, lstatErr := os.Lstat(current)
				if lstatErr != nil {
					break
				}
				if info.Mode()&os.ModeSymlink != 0 {
					return nil, fmt.Errorf("sandbox denyWritePaths route %q is a symlink inside writable path %q", current, writable)
				}
			}
		}
		real, err := filepath.EvalSymlinks(expanded)
		if err != nil {
			return nil, fmt.Errorf("resolve sandbox denyWritePaths entry %q: %w", expanded, err)
		}
		if seen[real] {
			continue
		}
		seen[real] = true
		resolved = append(resolved, real)
	}
	return resolved, nil
}

// Probe runs a trivial command through the sandbox to confirm it can actually
// start. Construction (New) only checks that the backend binary exists; it does
// not catch environments where the backend is present but fails at runtime
// (e.g. bwrap unable to create a mountpoint). Callers use this to fail fast with
// an actionable error instead of letting every tool call silently return a
// refusal. Returns nil when sb is nil (nothing to probe).
func Probe(sb Sandbox) error {
	if sb == nil {
		return nil
	}
	cmd := exec.Command("true")
	closeSandboxFiles, err := WrapCmdManaged(sb, cmd)
	if err != nil {
		return err
	}
	defer func() { _ = closeSandboxFiles() }()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// Config controls sandbox permissions. It can be unmarshaled from JSON
// (true for defaults, or an object with optional fields) and merged with
// another Config via the Merge method.
type Config struct {
	// Directories where file writes are allowed (supports ~ expansion).
	// The OS temp dir is included automatically unless DenyWrite is set. Paths
	// are resolved once at construction; missing grants are dropped and cannot
	// become writable later in the sandbox's lifetime.
	WritablePaths []string `json:"writablePaths,omitempty"`

	// Allow outbound network access (denied by default).
	AllowNetwork bool `json:"allowNetwork,omitempty"`

	// Block DNS resolution. Only effective when AllowNetwork is true.
	DenyDNS bool `json:"denyDNS,omitempty"`

	// Paths exempted from the DeniedPaths deny list. Paths are resolved once at
	// construction; missing exemptions are dropped and cannot appear later.
	ReadPaths []string `json:"readPaths,omitempty"`

	// Extra paths blocked from reads, in addition to DeniedPaths (supports ~ expansion).
	DenyPaths []string `json:"denyPaths,omitempty"`

	// Paths that stay readable but are denied writes, even when they sit
	// inside a WritablePaths entry (supports ~ expansion). Used to carve
	// read-only islands out of a writable tree — e.g. protecting complete Git
	// metadata trees when the workspace is writable. Every entry must exist;
	// symlink routing through a configured writable tree is rejected.
	// Redundant under DenyWrite, which already denies all writes.
	DenyWritePaths []string `json:"denyWritePaths,omitempty"`

	// If non-empty, only these env vars are passed through to the sandbox.
	AllowEnv []string `json:"allowEnv,omitempty"`

	// Deny all file writes, including to temp directories.
	DenyWrite bool `json:"denyWrite,omitempty"`

	// authorityPaths records the filesystem objects selected by a prior
	// PrepareConfig call. The metadata is deliberately private but survives
	// Config.Merge so a registry can reuse one approved base policy without a
	// later sandbox construction re-resolving a replaced path to wider host
	// authority.
	authorityPaths []authorityPathIdentity

	// readPathAliases preserves the lexical route a caller explicitly selected
	// when ReadPaths traversed one or more symlinks. The public path is frozen to
	// its canonical target, while this private record lets backends keep the
	// approved alias usable without accepting a later retarget or replacement.
	readPathAliases []readPathAliasIdentity
}

// DefaultConfig returns the standard base sandbox config (temp-dir-only writes).
func DefaultConfig() Config {
	return Config{WritablePaths: []string{os.TempDir()}}
}

// normalizeConfigPaths resolves policy paths once, when the sandbox is
// constructed. Relative paths use the caller's current working directory as
// their stable base; without this, Seatbelt receives ineffective relative
// vnode rules while bubblewrap rejects relative mount destinations.
func normalizeConfigPaths(cfg Config) (Config, error) {
	// Config is a value, but its slices are not. Keep the private policy record
	// isolated just like the public path slices below so backend-local
	// normalization and validation cannot mutate a registry's reusable base.
	cfg.authorityPaths = cloneAuthorityPathIdentities(cfg.authorityPaths)
	cfg.readPathAliases = cloneReadPathAliasIdentities(cfg.readPathAliases)
	cfg.AllowEnv = append([]string(nil), cfg.AllowEnv...)

	var cwd string
	normalize := func(field string, paths []string) ([]string, error) {
		if paths == nil {
			return nil, nil
		}
		out := make([]string, len(paths))
		for i, path := range paths {
			if path == "" {
				return nil, fmt.Errorf("sandbox %s contains an empty path", field)
			}
			path = expandTilde(path)
			if !filepath.IsAbs(path) {
				if cwd == "" {
					var err error
					cwd, err = os.Getwd()
					if err != nil {
						return nil, fmt.Errorf("resolve sandbox working directory: %w", err)
					}
				}
				path = filepath.Join(cwd, path)
			}
			out[i] = filepath.Clean(path)
		}
		return out, nil
	}

	var err error
	if cfg.WritablePaths, err = normalize("writablePaths", cfg.WritablePaths); err != nil {
		return Config{}, err
	}
	if cfg.ReadPaths, err = normalize("readPaths", cfg.ReadPaths); err != nil {
		return Config{}, err
	}
	if cfg.DenyPaths, err = normalize("denyPaths", cfg.DenyPaths); err != nil {
		return Config{}, err
	}
	if cfg.DenyWritePaths, err = normalize("denyWritePaths", cfg.DenyWritePaths); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// PrepareConfig resolves and freezes path-based authority before a Config is
// retained for later sandbox construction. It is safe to call repeatedly: a
// prepared grant keeps its original filesystem identity, and a later route or
// inode replacement fails closed instead of being re-resolved. Missing grants
// are dropped.
func PrepareConfig(cfg Config) (Config, error) {
	var err error
	cfg, err = normalizeConfigPaths(cfg)
	if err != nil {
		return Config{}, err
	}
	return freezeAuthorityPathsForPlatform(cfg)
}

// freezeAuthorityPaths turns path-based authority grants into the canonical
// routes that exist at sandbox construction. Re-resolving a configured symlink
// on every Wrap would let one sandboxed command retarget it to a host credential
// or home directory and have the next command inherit the wider authority.
// Missing grants are deliberately dropped: a later creation must not silently
// acquire permissions that were absent when the policy was approved.
func freezeAuthorityPaths(cfg Config, nonCoveringWritableRoots ...string) (Config, error) {
	// Preserve the normalized public spellings before canonicalization. A
	// prepared symlink grant may later be presented again through the same
	// lexical alias; its old route identity must remain active even if that alias
	// was retargeted and now resolves outside the old canonical ReadPaths target.
	rawReadPaths := append([]string(nil), cfg.ReadPaths...)
	carriedAliases := cloneReadPathAliasIdentities(cfg.readPathAliases)
	aliases := cloneReadPathAliasIdentities(carriedAliases)
	for _, path := range cfg.ReadPaths {
		path = filepath.Clean(path)
		// Narrowing a prepared alias grant to one of its descendants must still
		// validate the inherited route before capturing the child. Otherwise a
		// retargeted parent alias could be accepted as an unrelated fresh child
		// grant while the broader private alias record was pruned below.
		var routeGuards []readPathAliasIdentity
		for _, carriedAlias := range carriedAliases {
			if pathUsesReadPathAliasRoute(path, carriedAlias) {
				routeGuards = append(routeGuards, carriedAlias)
			}
		}
		if err := validateReadPathAliasIdentities(routeGuards); err != nil {
			return Config{}, err
		}
		alias, err := captureReadPathAliasIdentity(path)
		if err != nil {
			return Config{}, err
		}
		if alias != nil {
			// Preserve the already-approved route identities rather than replacing
			// them with a post-validation snapshot. The final validation below then
			// also closes a retarget race between the guard check and child capture.
			alias.symlinks = mergeReadPathSymlinkIdentities(routeGuards, alias.symlinks)
			aliases = append(aliases, *alias)
		}
	}
	carried := make(map[string]authorityPathIdentity, len(cfg.authorityPaths))
	for _, identity := range cfg.authorityPaths {
		path := filepath.Clean(identity.path)
		if prior, exists := carried[path]; exists {
			if !os.SameFile(prior.info, identity.info) {
				return Config{}, fmt.Errorf("conflicting prepared sandbox authority identities for %q", path)
			}
			prior.read = prior.read || identity.read
			prior.write = prior.write || identity.write
			identity = prior
		}
		carried[path] = identity
	}
	if cfg.DenyWrite {
		// DenyWrite is monotonic under Merge, so these grants can never become
		// effective again in a descendant config.
		cfg.WritablePaths = nil
	}

	freeze := func(field string, paths []string) ([]string, error) {
		frozen := make([]string, 0, len(paths))
		seen := make(map[string]bool, len(paths))
		for _, path := range paths {
			path = filepath.Clean(path)
			real := path
			if _, prepared := carried[path]; !prepared {
				var err error
				real, err = filepath.EvalSymlinks(path)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
						continue
					}
					return nil, fmt.Errorf("resolve sandbox %s entry %q: %w", field, path, err)
				}
			}
			real = filepath.Clean(real)
			if !seen[real] {
				seen[real] = true
				frozen = append(frozen, real)
			}
		}
		return frozen, nil
	}

	var err error
	if cfg.WritablePaths, err = freeze("writablePaths", cfg.WritablePaths); err != nil {
		return Config{}, err
	}
	if cfg.ReadPaths, err = freeze("readPaths", cfg.ReadPaths); err != nil {
		return Config{}, err
	}
	minimize := func(paths []string, nonCovering map[string]bool) []string {
		kept := make([]string, 0, len(paths))
		for _, path := range paths {
			covered := false
			for _, parent := range paths {
				if parent != path && !nonCovering[parent] && pathLexicallyWithinPolicy(path, parent) {
					covered = true
					break
				}
			}
			if !covered {
				kept = append(kept, path)
			}
		}
		return kept
	}
	nonCovering := make(map[string]bool, len(nonCoveringWritableRoots))
	for _, path := range nonCoveringWritableRoots {
		path = filepath.Clean(path)
		if real, err := filepath.EvalSymlinks(path); err == nil {
			path = filepath.Clean(real)
		}
		nonCovering[path] = true
	}
	cfg.WritablePaths = minimize(cfg.WritablePaths, nonCovering)
	cfg.ReadPaths = minimize(cfg.ReadPaths, nil)

	if cfg.DenyWrite {
		// ReadPaths are canonical now, so a caller that restored a prepared alias
		// spelling still selects the original target identity. Retain every old read
		// identity covered by a current canonical exemption; obsolete write-only
		// identities no longer affect this globally read-only policy.
		for path, identity := range carried {
			covered := false
			if identity.read {
				for _, readPath := range cfg.ReadPaths {
					if pathWithinPolicy(path, readPath) || pathWithinPolicy(readPath, path) {
						covered = true
						break
					}
				}
			}
			if !covered {
				delete(carried, path)
			}
		}
	}
	validatedCarried := make(map[string]bool, len(carried))
	for _, identity := range cfg.authorityPaths {
		path := filepath.Clean(identity.path)
		if _, relevant := carried[path]; relevant && !validatedCarried[path] {
			if err := validateAuthorityPathIdentities([]authorityPathIdentity{identity}); err != nil {
				return Config{}, err
			}
			validatedCarried[path] = true
		}
	}

	activeAliases := aliases[:0]
	for _, alias := range aliases {
		selectedLexically := false
		for _, readPath := range rawReadPaths {
			readPath = filepath.Clean(readPath)
			if filepath.Clean(alias.path) == readPath {
				selectedLexically = true
				break
			}
		}
		if selectedLexically {
			activeAliases = append(activeAliases, alias)
			continue
		}
		for _, readPath := range cfg.ReadPaths {
			if pathWithinPolicy(alias.target, readPath) {
				activeAliases = append(activeAliases, alias)
				break
			}
		}
	}
	aliases, aliasErr := dedupeReadPathAliasIdentities(activeAliases)
	if aliasErr != nil {
		return Config{}, aliasErr
	}
	if err := validateReadPathAliasIdentities(aliases); err != nil {
		return Config{}, err
	}
	cfg.readPathAliases = aliases

	paths := effectiveAuthorityPaths(cfg)
	readAuthority := make(map[string]bool, len(cfg.ReadPaths))
	for _, path := range cfg.ReadPaths {
		readAuthority[filepath.Clean(path)] = true
	}
	writeAuthority := make(map[string]bool, len(cfg.WritablePaths))
	if !cfg.DenyWrite {
		for _, path := range cfg.WritablePaths {
			writeAuthority[filepath.Clean(path)] = true
		}
	}
	identities := make([]authorityPathIdentity, 0, len(validatedCarried)+len(paths))
	seen := make(map[string]bool, len(validatedCarried)+len(paths))
	// Keep every still-relevant carried identity even when a newly merged
	// broader grant makes its public path redundant. A caller may retain the
	// merged prepared Config; discarding the narrower identity would then let a
	// later construction accept its replacement under the broader parent.
	for _, identity := range cfg.authorityPaths {
		path := filepath.Clean(identity.path)
		if _, relevant := carried[path]; relevant && validatedCarried[path] && !seen[path] {
			identity = carried[path]
			identity.read = identity.read || readAuthority[path]
			identity.write = identity.write || writeAuthority[path]
			seen[path] = true
			identities = append(identities, identity)
		}
	}
	for _, path := range paths {
		path = filepath.Clean(path)
		if seen[path] {
			continue
		}
		seen[path] = true
		if identity, prepared := carried[path]; prepared {
			identity.read = identity.read || readAuthority[path]
			identity.write = identity.write || writeAuthority[path]
			identities = append(identities, identity)
			continue
		}
		captured, err := captureAuthorityPathIdentities([]string{path})
		if err != nil {
			return Config{}, err
		}
		for i := range captured {
			captured[i].read = readAuthority[path]
			captured[i].write = writeAuthority[path]
		}
		identities = append(identities, captured...)
	}
	cfg.authorityPaths = identities
	return cfg, nil
}

type authorityPathIdentity struct {
	path  string
	info  os.FileInfo
	read  bool
	write bool
}

type readPathSymlinkIdentity struct {
	path       string
	linkTarget string
	info       os.FileInfo
}

type readPathAliasIdentity struct {
	path     string
	target   string
	symlinks []readPathSymlinkIdentity
}

func cloneReadPathAliasIdentities(aliases []readPathAliasIdentity) []readPathAliasIdentity {
	if len(aliases) == 0 {
		return nil
	}
	out := make([]readPathAliasIdentity, len(aliases))
	for i, alias := range aliases {
		out[i] = alias
		out[i].symlinks = append([]readPathSymlinkIdentity(nil), alias.symlinks...)
	}
	return out
}

func mergeReadPathSymlinkIdentities(guards []readPathAliasIdentity, current []readPathSymlinkIdentity) []readPathSymlinkIdentity {
	merged := make([]readPathSymlinkIdentity, 0, len(current))
	seen := make(map[string]bool)
	appendIdentity := func(identity readPathSymlinkIdentity) {
		identity.path = filepath.Clean(identity.path)
		if seen[identity.path] {
			return
		}
		seen[identity.path] = true
		merged = append(merged, identity)
	}
	for _, guard := range guards {
		for _, identity := range guard.symlinks {
			appendIdentity(identity)
		}
	}
	for _, identity := range current {
		appendIdentity(identity)
	}
	return merged
}

// pathUsesReadPathAliasRoute reports whether path is at or below the same
// lexical filesystem route as alias.path. filepath.Rel alone is insufficient:
// its string comparison misses case- or normalization-equivalent spellings on
// filesystems such as default APFS. Comparing each Lstat entry keeps those
// spellings tied to the inherited guard without conflating a distinct symlink
// that merely resolves to the same target.
func pathUsesReadPathAliasRoute(path string, alias readPathAliasIdentity) bool {
	path = filepath.Clean(path)
	aliasPath := filepath.Clean(alias.path)
	if pathLexicallyWithinPolicy(path, aliasPath) {
		return true
	}

	pathRoot, pathComponents := absolutePathComponents(path)
	aliasRoot, aliasComponents := absolutePathComponents(aliasPath)
	if len(pathComponents) < len(aliasComponents) {
		return false
	}
	pathRootInfo, err := os.Lstat(pathRoot)
	if err != nil {
		return false
	}
	aliasRootInfo, err := os.Lstat(aliasRoot)
	if err != nil || !os.SameFile(pathRootInfo, aliasRootInfo) {
		return false
	}

	pathPrefix := pathRoot
	aliasPrefix := aliasRoot
	for i, aliasComponent := range aliasComponents {
		pathPrefix = filepath.Join(pathPrefix, pathComponents[i])
		aliasPrefix = filepath.Join(aliasPrefix, aliasComponent)
		pathInfo, err := os.Lstat(pathPrefix)
		if err != nil {
			return false
		}
		aliasInfo, err := os.Lstat(aliasPrefix)
		if err != nil || !os.SameFile(pathInfo, aliasInfo) {
			return false
		}
	}
	return true
}

func captureReadPathAliasIdentity(path string) (*readPathAliasIdentity, error) {
	path = filepath.Clean(path)
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return nil, nil
		}
		return nil, fmt.Errorf("resolve sandbox readPaths alias %q: %w", path, err)
	}
	real = filepath.Clean(real)
	if real == path {
		return nil, nil
	}

	var route []string
	for current := path; ; current = filepath.Dir(current) {
		route = append(route, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	alias := readPathAliasIdentity{path: path, target: real}
	for i := len(route) - 1; i >= 0; i-- {
		component := route[i]
		info, err := os.Lstat(component)
		if err != nil {
			return nil, fmt.Errorf("inspect sandbox readPaths alias route %q: %w", component, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		linkTarget, err := os.Readlink(component)
		if err != nil {
			return nil, fmt.Errorf("read sandbox readPaths alias route %q: %w", component, err)
		}
		alias.symlinks = append(alias.symlinks, readPathSymlinkIdentity{
			path:       component,
			linkTarget: linkTarget,
			info:       info,
		})
	}
	if len(alias.symlinks) == 0 {
		return nil, fmt.Errorf("sandbox readPaths alias %q resolved to %q without an identifiable symlink route", path, real)
	}
	return &alias, nil
}

func validateReadPathAliasIdentities(aliases []readPathAliasIdentity) error {
	for _, alias := range aliases {
		real, err := filepath.EvalSymlinks(alias.path)
		if err != nil {
			return fmt.Errorf("resolve frozen sandbox readPaths alias %q: %w", alias.path, err)
		}
		if filepath.Clean(real) != filepath.Clean(alias.target) {
			return fmt.Errorf("frozen sandbox readPaths alias %q was retargeted", alias.path)
		}
		for _, symlink := range alias.symlinks {
			info, err := os.Lstat(symlink.path)
			if err != nil {
				return fmt.Errorf("inspect frozen sandbox readPaths alias route %q: %w", symlink.path, err)
			}
			if info.Mode()&os.ModeSymlink == 0 || !os.SameFile(symlink.info, info) {
				return fmt.Errorf("frozen sandbox readPaths alias route %q was replaced", symlink.path)
			}
			linkTarget, err := os.Readlink(symlink.path)
			if err != nil {
				return fmt.Errorf("read frozen sandbox readPaths alias route %q: %w", symlink.path, err)
			}
			if linkTarget != symlink.linkTarget {
				return fmt.Errorf("frozen sandbox readPaths alias route %q was retargeted", symlink.path)
			}
		}
	}
	return nil
}

func dedupeReadPathAliasIdentities(aliases []readPathAliasIdentity) ([]readPathAliasIdentity, error) {
	kept := make([]readPathAliasIdentity, 0, len(aliases))
	seen := make(map[string]readPathAliasIdentity, len(aliases))
	for _, alias := range aliases {
		alias.path = filepath.Clean(alias.path)
		alias.target = filepath.Clean(alias.target)
		if prior, exists := seen[alias.path]; exists {
			if prior.target != alias.target {
				return nil, fmt.Errorf("conflicting prepared sandbox readPaths aliases for %q", alias.path)
			}
			continue
		}
		seen[alias.path] = alias
		kept = append(kept, alias)
	}
	return kept, nil
}

func readPathAliasPaths(cfg Config) []string {
	paths := make([]string, 0, len(cfg.readPathAliases))
	for _, alias := range cfg.readPathAliases {
		paths = append(paths, alias.path)
	}
	return paths
}

func readPathAliasSymlinkSet(cfg Config) map[string]bool {
	paths := make(map[string]bool)
	for _, alias := range cfg.readPathAliases {
		for _, symlink := range alias.symlinks {
			paths[filepath.Clean(symlink.path)] = true
		}
	}
	return paths
}

func cloneAuthorityPathIdentities(identities []authorityPathIdentity) []authorityPathIdentity {
	if len(identities) == 0 {
		return nil
	}
	return append([]authorityPathIdentity(nil), identities...)
}

func effectiveAuthorityPaths(cfg Config) []string {
	paths := append([]string(nil), cfg.ReadPaths...)
	if !cfg.DenyWrite {
		paths = append(paths, cfg.WritablePaths...)
	}
	return paths
}

func captureAuthorityPathIdentities(paths []string) ([]authorityPathIdentity, error) {
	identities := make([]authorityPathIdentity, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		path = filepath.Clean(path)
		if seen[path] {
			continue
		}
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, fmt.Errorf("resolve frozen sandbox authority path %q: %w", path, err)
		}
		if filepath.Clean(real) != path {
			return nil, fmt.Errorf("frozen sandbox authority path %q no longer has its canonical route", path)
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect frozen sandbox authority path %q: %w", path, err)
		}
		seen[path] = true
		identities = append(identities, authorityPathIdentity{path: path, info: info})
	}
	return identities, nil
}

func validateAuthorityPathIdentities(identities []authorityPathIdentity) error {
	for _, identity := range identities {
		real, err := filepath.EvalSymlinks(identity.path)
		if err != nil {
			return fmt.Errorf("resolve frozen sandbox authority path %q: %w", identity.path, err)
		}
		if filepath.Clean(real) != identity.path {
			return fmt.Errorf("frozen sandbox authority path %q was rerouted", identity.path)
		}
		info, err := os.Stat(identity.path)
		if err != nil {
			return fmt.Errorf("inspect frozen sandbox authority path %q: %w", identity.path, err)
		}
		if !os.SameFile(identity.info, info) {
			return fmt.Errorf("frozen sandbox authority path %q was replaced", identity.path)
		}
	}
	return nil
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

// validateConfig rejects only configs the sandbox fundamentally cannot honor.
// It deliberately does NOT reject a missing writablePaths entry: a path that
// doesn't exist yet (or was removed since the config was saved) must not brick
// tool loading or session restore. freezeAuthorityPaths drops that stale grant
// at construction, so creating it later cannot silently activate new authority
// for a subsequent command.
func validateConfig(cfg Config) error {
	if _, err := os.UserHomeDir(); err != nil {
		return fmt.Errorf("cannot resolve home directory for credential masking: %w", err)
	}
	return nil
}

// DeniedPathKind identifies how a denied path should be masked by platform sandboxes.
type DeniedPathKind string

const (
	DeniedPathDir  DeniedPathKind = "dir"
	DeniedPathFile DeniedPathKind = "file"
)

// DeniedPath describes a sensitive path blocked from sandboxed reads.
type DeniedPath struct {
	Path string
	Kind DeniedPathKind
}

// DeniedPaths are sensitive locations blocked from read access inside the sandbox.
// Paths starting with ~ are expanded to the user's home directory.
var DeniedPaths = []DeniedPath{
	{Path: "~/.ssh", Kind: DeniedPathDir},
	{Path: "~/.gnupg", Kind: DeniedPathDir},
	{Path: "~/.gpg", Kind: DeniedPathDir},
	{Path: "~/.aws", Kind: DeniedPathDir},
	{Path: "~/.azure", Kind: DeniedPathDir},
	{Path: "~/.config/gcloud", Kind: DeniedPathDir},
	{Path: "~/.kube", Kind: DeniedPathDir},
	{Path: "~/.docker/config.json", Kind: DeniedPathFile},
	{Path: "~/.npmrc", Kind: DeniedPathFile},
	{Path: "~/.pypirc", Kind: DeniedPathFile},
	{Path: "~/.gem/credentials", Kind: DeniedPathFile},
	{Path: "~/.cargo/credentials", Kind: DeniedPathFile},
	{Path: "~/.config/gh", Kind: DeniedPathDir},
	{Path: "~/.netrc", Kind: DeniedPathFile},
	{Path: "~/.git-credentials", Kind: DeniedPathFile},
	{Path: "~/.local/share/keyrings", Kind: DeniedPathDir},
	{Path: "~/Library/Keychains", Kind: DeniedPathDir},
}

// ParseConfig parses a JSON sandbox field.
// Returns (nil, nil) for absent, null, or false.
// Returns an error for values that are not bool, null, or object
// (e.g. "yes", 123, []) so callers fail closed instead of silently
// running unsandboxed.
func ParseConfig(raw json.RawMessage) (*Config, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	// null
	if string(raw) == "null" {
		return nil, nil
	}
	// bool
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		if b {
			return &Config{}, nil
		}
		return nil, nil
	}
	// object
	var c Config
	if json.Unmarshal(raw, &c) == nil {
		return &c, nil
	}
	return nil, fmt.Errorf("unsupported sandbox value: %s (must be true, false, or an object)", string(raw))
}

// Merge returns a new Config combining c (base) with overlay.
// Booleans are OR'd (either side can widen allowances or add restrictions,
// but neither can reduce them). Slices are concatenated into fresh arrays.
func (c Config) Merge(overlay Config) Config {
	c.AllowNetwork = c.AllowNetwork || overlay.AllowNetwork
	c.DenyDNS = c.DenyDNS || overlay.DenyDNS
	c.WritablePaths = concatStrings(c.WritablePaths, overlay.WritablePaths)
	c.ReadPaths = concatStrings(c.ReadPaths, overlay.ReadPaths)
	c.DenyPaths = concatStrings(c.DenyPaths, overlay.DenyPaths)
	c.DenyWritePaths = concatStrings(c.DenyWritePaths, overlay.DenyWritePaths)
	c.AllowEnv = concatStrings(c.AllowEnv, overlay.AllowEnv)
	c.DenyWrite = c.DenyWrite || overlay.DenyWrite
	c.authorityPaths = append(cloneAuthorityPathIdentities(c.authorityPaths), overlay.authorityPaths...)
	c.readPathAliases = append(cloneReadPathAliasIdentities(c.readPathAliases), cloneReadPathAliasIdentities(overlay.readPathAliases)...)
	return c
}

// concatStrings returns a/b joined in a freshly allocated slice. Merge must not
// append onto its receiver's backing array: the registry reuses one
// baseSandboxCfg for every tool, so appending an overlay's entries in place
// could write into spare capacity shared with another tool's config and let one
// tool's denyPaths silently overwrite another's.
func concatStrings(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make([]string, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	return out
}

// Default-stripped env vars: agent sockets give a sandboxed process use of the
// user's keys even with ~/.ssh and ~/.gnupg masked, and credential-shaped
// names (FOO_API_KEY, FOO_TOKEN, ...) are secrets regardless of which tool
// they belong to. AllowEnv passes any of these through explicitly.
var (
	sensitiveEnvNames = map[string]bool{
		"SSH_AUTH_SOCK":            true,
		"SSH_AGENT_PID":            true,
		"GPG_AGENT_INFO":           true,
		"DBUS_SESSION_BUS_ADDRESS": true,
		"DBUS_SYSTEM_BUS_ADDRESS":  true,
		"DOCKER_HOST":              true,
		"CONTAINER_HOST":           true,
		"XDG_RUNTIME_DIR":          true,
		"WAYLAND_DISPLAY":          true,
		"PULSE_SERVER":             true,
	}
	sensitiveEnvPrefixes = []string{"POLLYTOOL_", "AWS_"}
	sensitiveEnvSuffixes = []string{
		"_API_KEY", "_APIKEY", "_TOKEN", "_SECRET", "_SECRET_KEY",
		"_ACCESS_KEY", "_PASSWORD", "_PASSPHRASE", "_CREDENTIALS", "_PRIVATE_KEY",
	}
)

func isSensitiveEnv(name string) bool {
	upper := strings.ToUpper(name)
	if sensitiveEnvNames[upper] {
		return true
	}
	for _, p := range sensitiveEnvPrefixes {
		if strings.HasPrefix(upper, p) {
			return true
		}
	}
	for _, s := range sensitiveEnvSuffixes {
		if strings.HasSuffix(upper, s) {
			return true
		}
	}
	return false
}

// filterEnv returns the env vars a sandboxed process should receive, plus the
// names (never values) of the vars it removed, for debug logging.
// With allowEnv set, only those names pass (an explicit allowlist wins over
// the sensitivity heuristics). Otherwise everything passes except
// sensitive-looking vars — see isSensitiveEnv.
func filterEnv(env, allowEnv []string) (filtered, stripped []string) {
	// Never leave filtered nil: os/exec treats a nil Env as "inherit the full
	// parent environment", so a config that strips every var (e.g. an allowEnv
	// listing only names absent from the environment) would hand the sandboxed
	// process every secret this function exists to remove. A non-nil empty
	// slice means "no environment", which is the fail-closed behavior we want.
	filtered = make([]string, 0, len(env))
	if len(allowEnv) > 0 {
		allowed := make(map[string]bool, len(allowEnv))
		for _, k := range allowEnv {
			allowed[k] = true
		}
		for _, e := range env {
			if k, _, _ := strings.Cut(e, "="); allowed[k] {
				filtered = append(filtered, e)
			} else {
				stripped = append(stripped, k)
			}
		}
		return filtered, stripped
	}
	for _, e := range env {
		if k, _, _ := strings.Cut(e, "="); !isSensitiveEnv(k) {
			filtered = append(filtered, e)
		} else {
			stripped = append(stripped, k)
		}
	}
	return filtered, stripped
}

// commandSummary returns the first two argv entries — enough to identify the
// wrapped tool in debug logs without capturing argument payloads.
func commandSummary(args []string) string {
	if len(args) > 2 {
		args = args[:2]
	}
	return strings.Join(args, " ")
}

// allDeniedPaths combines the built-in deny list with cfg.DenyPaths, all
// tilde-expanded. User entries get their kind from a stat (missing paths
// default to file; platforms that need existence handle that themselves).
func allDeniedPaths(cfg Config) []DeniedPath {
	paths := ExpandHome(DeniedPaths)
	for _, p := range cfg.DenyPaths {
		expanded := expandTilde(p)
		kind := DeniedPathFile
		if fi, err := os.Stat(expanded); err == nil && fi.IsDir() {
			kind = DeniedPathDir
		}
		paths = append(paths, DeniedPath{Path: expanded, Kind: kind})
	}
	return paths
}

// expandTilde resolves a ~ prefix to the user's home directory for a single path.
func expandTilde(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// resolvedExecutablePath returns the executable selected by os/exec before a
// platform wrapper replaces cmd.Path. Executing cmd.Args[0] again would repeat
// PATH lookup inside the filtered environment and can select a different
// program or fail entirely when PATH is intentionally omitted.
func resolvedExecutablePath(cmd *exec.Cmd) (string, error) {
	if cmd.Path == "" {
		return "", fmt.Errorf("sandbox command has no executable path")
	}
	path := cmd.Path
	if !filepath.IsAbs(path) {
		base, err := resolvedCommandDir(cmd.Dir)
		if err != nil {
			return "", err
		}
		path = filepath.Join(base, path)
	}
	path = filepath.Clean(path)
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	return path, nil
}

func resolvedCommandDir(dir string) (string, error) {
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir), nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve sandbox command directory: %w", err)
	}
	if dir == "" {
		return filepath.Clean(cwd), nil
	}
	return filepath.Clean(filepath.Join(cwd, dir)), nil
}

// ExpandHome resolves ~ prefixes to the user's home directory.
func ExpandHome(paths []DeniedPath) []DeniedPath {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	expanded := make([]DeniedPath, 0, len(paths))
	for _, p := range paths {
		path := p.Path
		if strings.HasPrefix(path, "~/") {
			path = filepath.Join(home, path[2:])
		}
		expanded = append(expanded, DeniedPath{Path: path, Kind: p.Kind})
	}
	return expanded
}
