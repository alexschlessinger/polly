package tools

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/alexschlessinger/pollytool/internal/scratch"
	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// ExecutionContext binds filesystem authority to a registry, never to a
// process-wide chdir. The runtime keeps context identities opaque to scripts.
type ExecutionContext struct {
	BuiltinTools []string       `json:"-"`
	SourceRoot   string         `json:"-"`
	Root         string         `json:"root"`
	ReadOnly     bool           `json:"readOnly"`
	Scratch      string         `json:"scratch,omitempty"`
	Sandbox      sandbox.Config `json:"-"`
	// sandboxPresets carries the explicit floor into descendants and makes
	// sandbox construction mandatory even when the source is unsandboxed.
	sandboxPresets []string
}

// ExecutionGrant is the authority a context receives beyond reading its root.
type ExecutionGrant struct {
	// SandboxPresets are ordered restrictions: the operator's child-default
	// floor, then the agent's own preset. Each can only narrow earlier policy.
	// An empty slice preserves ordinary inheritance.
	SandboxPresets []string
	// ReadOnly denies writes to the root. With a Scratch the context may write
	// only there; without one it denies every write.
	ReadOnly     bool
	DeniedReads  []string
	DeniedWrites []string
	// Scratch is an existing directory outside the root, exported to the
	// context's processes as TMPDIR, TMP and TEMP. It is the only
	// writable path of a read-only context. A missing or nested scratch fails
	// closed.
	Scratch string
	// SourceRoot is the workspace the root was made from, such as the
	// checkout a member's worktree copies. An env value a sandbox layer
	// points inside it is rebased into the root, so the context uses its own
	// copy of a workspace path.
	SourceRoot string
}

// OutputDirEnv names the context's output directory to its processes: the
// place a worker leaves files for whoever reads its result. The runtime that
// owns the scratch captures what is there when the work completes.
const OutputDirEnv = "POLLYTOOL_OUTPUT_DIR"

// OutputDir is the output directory of a context with the given scratch.
func OutputDir(scratch string) string {
	return filepath.Join(scratch, "output")
}

// scratchEnv points a context's temp files at its scratch. Nothing here names
// a toolchain: a tool whose cache is denied under the private home is pointed
// at the scratch through that tool's own environment variable, by the member
// or by the policy the operator grants.
func scratchEnv(dir string) map[string]string {
	return map[string]string{
		"TMPDIR": dir, "TMP": dir, "TEMP": dir,
		OutputDirEnv: OutputDir(dir),
		// A nested Polly must claim scratch inside this context's grant,
		// never the host default (which stays masked from its commands).
		scratch.RootEnv: scratch.RootUnder(dir),
	}
}

// ContextTool binds a tool to a new execution context: the built-ins and
// shell tools implement it, and custom Go tools may. A tool that neither
// rebinds nor declares independence is omitted rather than retaining
// authority over another cwd.
type ContextTool interface {
	Tool
	BindExecutionContext(*ToolRegistry, ExecutionContext) (Tool, error)
}

// ContextIndependentTool is a declaration by trusted Go tool authors. Local
// process tools still get their own sandbox; declarations never widen it.
type ContextIndependentTool interface {
	Tool
	ContextIndependent() bool
}

func (r *ToolRegistry) UnsafeNoSandbox() bool { return r != nil && r.unsafeNoSandbox }

func (r *ToolRegistry) ExecutionRoot() string {
	if r == nil {
		return ""
	}
	return r.executionRoot
}

// workRoot is the directory the registry works in: the execution root of a
// bound registry, else the process working directory.
func (r *ToolRegistry) workRoot() (string, error) {
	if root := r.ExecutionRoot(); root != "" {
		return root, nil
	}
	return os.Getwd()
}

func (r *ToolRegistry) ResolvePath(path string) (string, error) {
	if r != nil && r.executionRoot != "" && !filepath.IsAbs(path) && !strings.HasPrefix(path, "~") {
		path = filepath.Join(r.executionRoot, path)
	}
	return resolveLocalPath(path)
}

// ExecutionPolicy narrows the parent's grants. Workspace-dependent write
// roots are replaced; inherited deny rules, read grants (explicit credential
// grants included), Unix-socket grants and network policy are retained. A
// read-only grant with a scratch writes only there; without one it keeps the
// all-writes-denied policy. An operator's denyWrite base still wins. The
// member parts of the sandbox layers (see SandboxLayer) count as the base
// does, and their write grants and env reach the context too: the write
// grants only when the context may write, and each env value inside the
// grant's SourceRoot rebased into the root.
func (r *ToolRegistry) ExecutionPolicy(root string, grant ExecutionGrant) (ExecutionContext, error) {
	presets := slices.Concat(r.executionSandboxPresets, grant.SandboxPresets)
	for _, spec := range presets {
		if err := ValidateAgentSandboxPreset(spec); err != nil {
			return ExecutionContext{}, err
		}
	}
	release, err := r.TryEnvironmentUse()
	if err != nil {
		return ExecutionContext{}, err
	}
	defer release()
	abs, err := filepath.Abs(root)
	if err != nil {
		return ExecutionContext{}, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return ExecutionContext{}, err
	}
	scratch := ""
	if grant.Scratch != "" {
		if scratch, err = filepath.Abs(grant.Scratch); err != nil {
			return ExecutionContext{}, err
		}
		if scratch, err = filepath.EvalSymlinks(scratch); err != nil {
			return ExecutionContext{}, fmt.Errorf("execution scratch %q: must be an existing directory: %w", grant.Scratch, err)
		}
		if info, err := os.Stat(scratch); err != nil || !info.IsDir() {
			return ExecutionContext{}, fmt.Errorf("execution scratch %q: must be an existing directory", grant.Scratch)
		}
		if sandbox.PathWithin(scratch, abs) || sandbox.PathWithin(abs, scratch) {
			return ExecutionContext{}, errors.New("execution scratch must be outside the execution root")
		}
		if err := os.MkdirAll(OutputDir(scratch), 0o700); err != nil {
			return ExecutionContext{}, fmt.Errorf("execution output directory: %w", err)
		}
	}
	policy, err := r.currentSandboxPolicy()
	if err != nil {
		return ExecutionContext{}, err
	}
	if len(presets) != 0 && r.executionPolicy != nil {
		// An unsandboxed bound registry still has a native filesystem policy,
		// but no process-sandbox base. Explicit child sandboxing must inherit
		// those filesystem restrictions, not the empty process base.
		policy.base = *r.executionPolicy
	}
	layers, err := policy.memberConfig()
	if err != nil {
		return ExecutionContext{}, err
	}
	if !policy.base.DenyWrite && scratch != "" {
		for _, layer := range policy.layers {
			if layer.environment == nil {
				continue
			}
			env, err := environmentForContext(layer.environment, abs, scratch, grant.ReadOnly, slices.Concat(policy.base.DenyPaths, policy.base.DenyWritePaths, grant.DeniedReads, grant.DeniedWrites))
			if err != nil {
				return ExecutionContext{}, err
			}
			layers = env.Merge(layers)
		}
	}
	base := policy.base.Merge(layers)
	if len(presets) != 0 {
		// Validate inherited frozen identities before selecting paths for the
		// child's new policy. Rebinding must not recapture a replaced grant,
		// or treat an invalid parent policy as merely a read-only source.
		if _, err := sandbox.CompileReadPolicy(base); err != nil {
			return ExecutionContext{}, fmt.Errorf("inherit agent sandbox policy: %w", err)
		}
	}
	// Every restriction of the merged policy carries over; the grants below
	// are the only ones a context inherits.
	cfg := sandbox.DefaultConfig().Merge(restrictiveSandboxConfig(base))
	cfg.AllowNetwork = base.AllowNetwork
	cfg.AllowEnv = append([]string(nil), base.AllowEnv...)
	cfg.PassEnv = append([]string(nil), base.PassEnv...)
	denied := append(append([]string(nil), base.DenyPaths...), grant.DeniedReads...)
	cfg.ReadPaths = undeniedPaths(base.ReadPaths, denied)
	cfg.AllowUnixSockets = undeniedPaths(base.AllowUnixSockets, denied)
	cfg.DenyPaths = append(cfg.DenyPaths, grant.DeniedReads...)
	cfg.DenyWritePaths = append(cfg.DenyWritePaths, grant.DeniedWrites...)
	switch {
	case grant.ReadOnly && scratch != "":
		// Read-only research writes in its scratch and, like every context,
		// in host temp: the root is an explicit read-only island. Withholding
		// host temp would break here-documents, because macOS's bash 3.2 puts
		// them in a system temp directory or, failing that, the working
		// directory, which is the read-only checkout.
		cfg.WritablePaths = []string{scratch}
		cfg.DenyWritePaths = append(cfg.DenyWritePaths, abs)
	case grant.ReadOnly:
		cfg.WritablePaths = []string{abs}
		cfg.DenyWrite = true
	default:
		cfg.WritablePaths = []string{abs}
		if scratch != "" {
			cfg.WritablePaths = append(cfg.WritablePaths, scratch)
		}
		cfg.WritablePaths = append(cfg.WritablePaths, undeniedPaths(layers.WritablePaths, slices.Concat(denied, grant.DeniedWrites))...)
	}
	cfg.Env = rebasedEnv(layers.Env, grant.SourceRoot, abs)
	if len(presets) != 0 {
		cfg.Env = rebasedEnv(base.Env, grant.SourceRoot, abs)
	}
	if scratch != "" && !cfg.DenyWrite {
		cfg.Env = mergeEnv(cfg.Env, scratchEnv(scratch))
	}
	if len(presets) != 0 {
		cfg, err = r.restrictAgentSandbox(cfg, base, abs, scratch, grant, presets)
		if err != nil {
			return ExecutionContext{}, err
		}
	}
	rootReadOnly := grant.ReadOnly || cfg.DenyWrite || len(presets) != 0 && sandbox.WriteAllowed(cfg, abs) != nil
	if rootReadOnly && !isHomeDirectory(abs) {
		// Keep the checkout visible inside private roots; not a write grant.
		// The home directory itself is never exposed: a live root there sees
		// only what the policy already grants.
		cfg, err = sandbox.ExposeReadOnlyPaths(cfg, abs)
		if err != nil {
			return ExecutionContext{}, err
		}
	}
	return ExecutionContext{Root: abs, SourceRoot: grant.SourceRoot, ReadOnly: rootReadOnly, Scratch: scratch, Sandbox: cfg, sandboxPresets: presets}, nil
}

// rebasedEnv copies env with every value inside source rebased into root, so
// a context uses its own copy of a workspace path. It returns nil for an
// empty env.
func rebasedEnv(env map[string]string, source, root string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	if source != "" {
		if real, err := filepath.EvalSymlinks(source); err == nil {
			source = real
		}
		source = filepath.Clean(source)
	}
	rebased := make(map[string]string, len(env))
	for name, value := range env {
		rebased[name] = rebindSourcePath(value, source, root)
	}
	return rebased
}

// mergeEnv writes over into env, which may be nil, and returns the result.
func mergeEnv(env, over map[string]string) map[string]string {
	if env == nil {
		return over
	}
	maps.Copy(env, over)
	return env
}

// isHomeDirectory reports whether the canonical path is the user's home
// directory, which the sandbox keeps private and never grants whole.
func isHomeDirectory(abs string) bool {
	home := sandbox.ResolvedHomeDir()
	return home != "" && home == filepath.Clean(abs)
}

// undeniedPaths keeps the parent's read or socket grants a member may use:
// all of them except those the operator's denials or the member's own cover,
// so a member never inherits a grant into a path it may not read. An
// explicit credential grant (the ssh presets' files and agent socket, a
// --readpath into ~/.aws) was the operator's choice and reaches the member as
// it reaches a subagent. Denial is judged on canonical routes so a symlinked
// home still matches.
func undeniedPaths(paths, denied []string) []string {
	var kept []string
	for _, path := range paths {
		if !sandbox.DeniedBy(denied, path) {
			kept = append(kept, path)
		}
	}
	return kept
}

// inheritableReadPaths keeps the parent's read grants that make toolchains
// and configuration visible inside the private home directory, and drops
// credential exemptions (the ssh presets) and anything the given denials
// cover. Shell-tool schema discovery uses it: discovery runs a script before
// it is trusted, so it gets no credential. Masking is judged on a grant-free
// policy so the grant cannot cover itself, and on canonical routes so a
// symlinked home still matches.
func inheritableReadPaths(base sandbox.Config, deniedReads []string) []string {
	masks := sandbox.Config{DenyPaths: append(append([]string(nil), base.DenyPaths...), deniedReads...)}
	var kept []string
	for _, path := range base.ReadPaths {
		if sandbox.ReadMasked(masks, path) == nil {
			kept = append(kept, path)
		}
	}
	return kept
}

// exposeExecutable keeps an explicitly configured script or server executable
// readable inside private roots, together with its directory so a script can
// source siblings and a launcher can find its neighbours. The directory is
// not exposed when it is the home directory or a filesystem root; the file
// alone is then. A path that does not resolve is left to the later read
// check. A path that a denied path masks under the policy's existing grants
// is refused: the operator's denial wins over the executable's exposure.
func exposeExecutable(cfg sandbox.Config, path string) (sandbox.Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return cfg, nil
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return cfg, nil
	}
	if err := sandbox.ReadMasked(cfg, real); err != nil {
		return sandbox.Config{}, fmt.Errorf("cannot expose executable %s: %w", real, err)
	}
	dir := filepath.Dir(real)
	if isHomeDirectory(dir) || dir == filepath.Dir(dir) {
		return sandbox.ExposeReadOnlyPaths(cfg, real)
	}
	return sandbox.ExposeReadOnlyPaths(cfg, dir)
}

// rebindNative builds the bound registry's own copy of a built-in from its
// native factory, so the copy carries the context's root and policy.
func rebindNative(bound *ToolRegistry, name string) (Tool, error) {
	factory, ok := bound.nativeFactory(name)
	if !ok {
		return nil, nil
	}
	return factory()
}

// bindSkillTool installs the rebound skill catalog and runtime in r the first
// time one of the two skill tools binds, then returns the tool by name. A
// failed installation leaves nothing behind, so the other tool retries.
func (r *ToolRegistry) bindSkillTool(name string, catalog *skills.Catalog, ec ExecutionContext) (Tool, error) {
	if r.executionSkills == nil {
		rebound, err := rebindSkillCatalog(catalog, r, ec)
		if err != nil {
			return nil, err
		}
		if _, err := NewSkillRuntime(rebound, r); err != nil {
			return nil, err
		}
		r.executionSkills = rebound
	}
	tool, _ := r.Get(name)
	return tool, nil
}

// executionSourceState fences the whole binding, including caller-supplied
// instruction loading, without holding source locks across any constructors.
// Version identities work for arbitrary Tool values (not only MCP pointers).
type executionSourceState struct {
	source   *ToolRegistry
	policy   uint64
	chain    []executionSourceRegistry
	tools    []Tool
	selected []string
}

type executionSourceRegistry struct {
	registry   *ToolRegistry
	generation uint64
	versions   map[string]uint64
}

func (r *ToolRegistry) snapshotExecutionSource(allow []string) *executionSourceState {
	state := &executionSourceState{source: r, policy: r.SandboxPolicyRevision()}
	for current := r; current != nil; {
		current.mu.RLock()
		snapshot := executionSourceRegistry{registry: current, generation: current.closeGeneration, versions: make(map[string]uint64, len(current.tools))}
		for name := range current.tools {
			snapshot.versions[name] = current.toolVersions[name]
		}
		state.chain = append(state.chain, snapshot)
		parent := current.parent
		current.mu.RUnlock()
		current = parent
	}
	state.tools = r.All()
	for _, tool := range state.tools {
		name := tool.GetName()
		if allow == nil || matchesAnyToolPattern(allow, name) || r.isBuiltin(name) {
			state.selected = append(state.selected, name)
		}
	}
	return state
}

// executionToolIdentity follows the same shadowing and policy rules as Get,
// retaining the owning registry and mutation identity instead of tool values.
func (r *ToolRegistry) executionToolIdentity(name string) (*ToolRegistry, uint64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.viewVisibleLocked(name) || !r.isToolAllowedLocked(name) {
		return nil, 0, false
	}
	if _, exists := r.tools[name]; exists {
		return r, r.toolVersions[name], true
	}
	if r.parent != nil {
		return r.parent.executionToolIdentity(name)
	}
	return nil, 0, false
}

func (s *executionSourceState) validate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.policy != s.source.SandboxPolicyRevision() {
		return errors.New("source sandbox policy changed during tool construction")
	}
	for _, snapshot := range s.chain {
		snapshot.registry.mu.RLock()
		changed := snapshot.generation != snapshot.registry.closeGeneration
		snapshot.registry.mu.RUnlock()
		if changed {
			return errors.New("source registry closed during tool construction")
		}
	}
	for _, name := range s.selected {
		owner, version, allowed := s.source.executionToolIdentity(name)
		if !allowed {
			return errors.New("source tool selection changed during tool construction")
		}
		matched := false
		for _, snapshot := range s.chain {
			if original, exists := snapshot.versions[name]; exists {
				matched = owner == snapshot.registry && version == original
				break
			}
		}
		if !matched {
			return errors.New("source tool changed during tool construction")
		}
	}
	return nil
}

// BindExecutionContext owns fresh native tools and local MCP servers. It
// never derives a live view of another member's registry. Required tool
// patterns fail launch when no compatible tool can satisfy them; the
// registry's built-ins count. The receiver must have native tool setup.
// NativeOpenTools is the same binding without the final selection check,
// which its callers run once their own tools are registered.
func (r *ToolRegistry) BindExecutionContext(ec ExecutionContext, allow []string) (*ToolRegistry, []string, error) {
	return r.BindExecutionContextWithContext(context.Background(), ec, allow)
}

// BindExecutionContextWithContext is BindExecutionContext with cancellable MCP
// setup. Synchronous host-supplied ContextTool constructors must still return
// before cancellation can be observed. The caller owns a successful binding.
func (r *ToolRegistry) BindExecutionContextWithContext(ctx context.Context, ec ExecutionContext, allow []string) (*ToolRegistry, []string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if !r.native {
		return nil, nil, ErrNativeToolsRequired
	}
	state := r.snapshotExecutionSource(allow)
	bound, omitted, err := r.bindExecutionContext(ctx, ec, allow, state)
	if err != nil {
		return nil, nil, err
	}
	if err := bound.ValidateToolSelection(allow, ec.BuiltinTools); err != nil {
		bound.discardConstruction()
		return nil, omitted, err
	}
	if err := state.validate(ctx); err != nil {
		bound.discardConstruction()
		return nil, omitted, err
	}
	return bound, omitted, nil
}

func (r *ToolRegistry) bindExecutionContext(ctx context.Context, ec ExecutionContext, allow []string, state *executionSourceState) (*ToolRegistry, []string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if !r.native {
		return nil, nil, ErrNativeToolsRequired
	}
	if ec.Root == "" || !filepath.IsAbs(ec.Root) {
		return nil, nil, fmt.Errorf("execution root must be absolute")
	}
	opts := []RegistryOption{WithNativeTools()}
	explicitSandbox := len(ec.sandboxPresets) != 0
	factory := r.sandboxFactory
	if explicitSandbox && factory == nil {
		factory = sandbox.New
	}
	if factory != nil {
		opts = append(opts, WithSandboxFactory(factory, ec.Sandbox))
	}
	if r.unsafeNoSandbox && !explicitSandbox {
		opts = append(opts, WithUnsafeNoSandbox())
	}
	if tracker := r.ChangeTracker(); tracker != nil {
		opts = append(opts, WithChangeTracker(tracker))
	}
	bound := NewToolRegistry(nil, opts...)
	bound.environmentGate = r.environmentGate
	bound.executionRoot = ec.Root
	bound.executionSourceRoot = ec.SourceRoot
	prepared, err := sandbox.PrepareConfig(ec.Sandbox)
	if err != nil {
		bound.Close()
		return nil, nil, err
	}
	bound.executionPolicy = &prepared
	bound.executionSandboxPresets = slices.Clone(ec.sandboxPresets)
	if explicitSandbox {
		// Validate the backend even when no process tools are selected. A
		// missing/unsupported backend is a launch failure, never an omission
		// that could leave a seemingly sandboxed child running unsandboxed.
		if _, err := bound.constructPreparedSandbox(prepared); err != nil {
			bound.Close()
			return nil, nil, fmt.Errorf("construct agent sandbox: %w", err)
		}
	}
	bound.processGroups = &processGroups{}
	omitted := []string{}
	// All already applies the parent chain's policies and allow-lists.
	parentTools := state.tools
	visible := map[string]bool{}
	for _, tool := range parentTools {
		visible[tool.GetName()] = true
	}
	type mcpBindingKey struct{ source, namespace string }
	loadedMCP := map[mcpBindingKey]bool{}
	for _, original := range parentTools {
		if err := ctx.Err(); err != nil {
			bound.discardConstruction()
			return nil, omitted, err
		}
		name := original.GetName()
		if bound.isBuiltin(name) {
			// The bound registry's own native setup already serves it.
			continue
		}
		builtin := r.isBuiltin(name)
		if allow != nil && !matchesAnyToolPattern(allow, name) && !builtin {
			continue
		}
		var tool Tool
		var err error
		switch t := unwrapTool(original).(type) {
		case *MCPTool:
			// A server binds once for all of its tools; a launch that fails
			// omits the rest of them without another attempt.
			namespace, _, _ := splitNamespace(name)
			key := mcpBindingKey{source: t.Source, namespace: namespace}
			if !loadedMCP[key] {
				loadedMCP[key] = true
				spec := rebindSourcePath(t.Source, ec.SourceRoot, ec.Root)
				config, serverName, loadErr := loadMCPServerConfig(ParseServerSpec(spec))
				err = loadErr
				if err == nil {
					jsonFile, _ := ParseServerSpec(spec)
					var records []stagedToolRecord
					records, _, err = bound.prepareSingleMCPServerWithNamespace(ctx, jsonFile, serverName, namespace, &config)
					if err == nil {
						bound.mu.Lock()
						cleanup := bound.retireClientsLocked(bound.installStagedLocked(records))
						bound.mu.Unlock()
						cleanup()
					}
				}
			}
			if err == nil {
				tool, _ = bound.Get(name)
			}
		case ContextTool:
			tool, err = t.BindExecutionContext(bound, ec)
		case ContextIndependentTool:
			if t.ContextIndependent() {
				tool = original
			}
		}
		if err != nil || tool == nil {
			omitted = append(omitted, name)
			continue
		}
		if tool.GetName() != name {
			tool = &NamespacedTool{Tool: tool, namespacedName: name}
		}
		bound.Register(tool)
		if builtin {
			bound.MarkBuiltin(name)
		}
	}
	if err := state.validate(ctx); err != nil {
		bound.discardConstruction()
		return nil, omitted, err
	}
	// A relaunched MCP server may advertise more tools than requested.
	// Built-ins stay; everything else must be both visible in the source
	// and selected.
	for _, t := range bound.All() {
		name := t.GetName()
		if bound.isBuiltin(name) {
			continue
		}
		if !visible[name] || allow != nil && !matchesAnyToolPattern(allow, name) {
			bound.Remove(name)
		}
	}
	// This filter also bounds later skill activation. Tools the owner
	// registers on the bound registry afterwards, such as a swarm's member
	// tools, are marked always-allowed and pass it regardless.
	bound.viewAllowed = func(name string) bool {
		// Agent mutations are installed privately after binding. Unlike
		// always-allowed recall tools they must still pass source policies
		// and the explicit selection, even before they are registered.
		private := name == "restore_artifact" && r.privateMutationAllowed(name)
		return (visible[name] || private) && (allow == nil || matchesAnyToolPattern(allow, name))
	}
	return bound, omitted, nil
}

func (r *ToolRegistry) ExecutionSkills() *skills.Catalog { return r.executionSkills }

func rebindSourcePath(path, source, root string) string {
	if source == "" || !filepath.IsAbs(path) {
		return path
	}
	if sandbox.PathWithin(path, source) {
		rel, _ := filepath.Rel(source, path)
		return filepath.Join(root, rel)
	}
	return path
}

func rebindSkillCatalog(catalog *skills.Catalog, registry *ToolRegistry, ec ExecutionContext) (*skills.Catalog, error) {
	if catalog == nil {
		return nil, nil
	}
	parentRoot := ec.SourceRoot
	if parentRoot == "" {
		parentRoot, _ = os.Getwd()
	}
	policy, err := compileReadPolicy(registry)
	if err != nil {
		return nil, err
	}
	allow := func(path string) bool { return readRoutesAllowed(policy, path) == nil }
	var paths []string
	for _, skill := range catalog.List() {
		path := rebindSourcePath(skill.RootDir, parentRoot, ec.Root)
		manifest := filepath.Join(path, "SKILL.md")
		// An ignored skill or one added after this snapshot may not exist in
		// the child. Omit just that skill, never fall back to source assets or
		// lose unrelated skills because a child policy withholds one of them.
		if !allow(path) || !allow(manifest) {
			continue
		}
		if _, err := os.Stat(manifest); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("skill unavailable in execution context: %s: %w", skill.Name, err)
		}
		paths = append(paths, path)
	}
	return skills.Discover(paths, skills.WithDiscoveryFilter(allow))
}
