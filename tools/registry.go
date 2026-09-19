package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// LoadResult contains information about tools that were loaded
type LoadResult struct {
	Type    string         // "native", "shell", "mcp"
	Servers []ServerResult // For MCP, one per server loaded; for shell/native, single entry
}

// ServerResult contains information about tools loaded from a single source
type ServerResult struct {
	Name      string   // Server/tool name (e.g., "git", "filesystem", "datetime")
	ToolNames []string // Fully namespaced tool names loaded
}

// NamespacedTool wraps a tool to provide a namespaced schema
type NamespacedTool struct {
	Tool
	namespacedName string
}

// ExecuteOutput preserves an optional rich result through the namespace
// wrapper. Without this forwarding method, wrapping an MCP tool would narrow
// it back to Tool and force image bytes through Execute's textual JSON path.
func (n *NamespacedTool) ExecuteOutput(ctx context.Context, args map[string]any) (ToolOutput, error) {
	if rich, ok := n.Tool.(OutputTool); ok {
		return rich.ExecuteOutput(ctx, args)
	}
	text, err := n.Tool.Execute(ctx, args)
	return ToolOutput{Text: text}, err
}

func (n *NamespacedTool) ExclusiveBatch() bool {
	t, ok := n.Tool.(ExclusiveTool)
	return ok && t.ExclusiveBatch()
}
func (n *NamespacedTool) Untimed() bool { t, ok := n.Tool.(UntimedTool); return ok && t.Untimed() }
func (n *NamespacedTool) RecallStub() string {
	stub, _ := RecallStub(n.Tool)
	return stub
}

func (n *NamespacedTool) Coordinates() bool {
	t, ok := n.Tool.(CoordinationTool)
	return ok && t.Coordinates()
}

// GetSchema returns a schema with the namespaced title
func (n *NamespacedTool) GetSchema() *schema.ToolSchema {
	c := n.Tool.GetSchema().Copy()
	if c == nil {
		return nil
	}
	c.SetTitle(n.namespacedName)
	return c
}

// GetName returns the namespaced name
func (n *NamespacedTool) GetName() string {
	return n.namespacedName
}

// ToolRegistry manages available tools
type ToolRegistry struct {
	environmentGate     *ExecutionGate
	executionGate       *ExecutionGate
	executionSkills     *skills.Catalog
	executionSourceRoot string
	executionRoot       string
	executionPolicy     *sandbox.Config
	changeTracker       ChangeTracker
	mu                  sync.RWMutex
	tools               map[string]Tool

	// Native tool factories
	nativeTools map[string]func() (Tool, error) // toolName -> factory

	// MCP tracking
	toolClients map[string]*MCPClient // toolName -> client
	serverTools map[string][]string   // serverSpec -> toolNames

	// Runtime activation state
	pendingTools       map[string]Tool
	pendingToolClients map[string]*MCPClient
	pendingServerTools map[string][]string

	alwaysAllowedTools map[string]bool
	policyActive       bool
	allowedPatterns    []string
	autoAllowedTools   map[string]bool

	pendingPolicyActive    bool
	pendingAllowedPatterns []string
	pendingAutoAllowed     map[string]bool

	// Sandbox factory and base config
	sandboxFactory        func(sandbox.Config) (sandbox.Sandbox, error)
	sandboxConfigMu       sync.Mutex
	baseSandboxCfg        sandbox.Config
	baseSandboxPrepared   bool
	baseSandboxPrepareErr error
	unsafeNoSandbox       bool
	// sandboxLayers are the named overlays of the policy (see
	// SetSandboxLayer), prepared and in name order. Guarded by
	// sandboxConfigMu.
	sandboxLayers []sandboxLayer
	// sandboxParent is the registry whose sandbox policy a derived registry
	// uses (see Derive). It is set once at Derive and never cleared, so a
	// policy change on the parent reaches every registry derived from it,
	// before and after Close.
	sandboxParent *ToolRegistry
	// sandboxDependents tracks derived registries using this owner's policy.
	// Guarded by sandboxConfigMu; Close removes a dependent, and another
	// policy lookup registers it again if it is reused.
	sandboxDependents map[*ToolRegistry]struct{}

	// parent makes this a derived registry (see Derive): a lookup that misses
	// the registry's own tools continues in the parent, whose tools and MCP
	// clients are shared rather than loaded again, and narrowed by
	// viewAllowed. Both are fixed at Derive and cleared by Close. A parent
	// never holds its tool lock while locking a derived registry: lookups
	// may call the parent while holding the derived registry's lock.
	parent      *ToolRegistry
	viewAllowed func(name string) bool
}

type registryOptions struct {
	sandboxFactory        func(sandbox.Config) (sandbox.Sandbox, error)
	baseSandboxCfg        sandbox.Config
	baseSandboxPrepared   bool
	baseSandboxPrepareErr error
	sandboxLayers         []sandboxLayer
	unsafeNoSandbox       bool
	changeTracker         ChangeTracker
}

// RegistryOption configures a ToolRegistry.
type RegistryOption func(*registryOptions)

// WithSandboxFactory sets the sandbox factory and snapshots the prepared base
// config immediately. Preparation errors surface when the registry first
// constructs a process sandbox because RegistryOption cannot return an error.
func WithSandboxFactory(factory func(sandbox.Config) (sandbox.Sandbox, error), baseCfg sandbox.Config) RegistryOption {
	preparedBase, prepareErr := sandbox.PrepareConfig(baseCfg.Merge(sandbox.Config{}))
	return func(o *registryOptions) {
		o.sandboxFactory = factory
		// Snapshot both the slices and filesystem identities when the option is
		// created. Registry construction and native-tool loading may be delayed;
		// neither caller mutation nor path replacement during that gap may change
		// which authority was approved.
		o.baseSandboxCfg = preparedBase.Merge(sandbox.Config{})
		o.baseSandboxPrepared = true
		o.baseSandboxPrepareErr = prepareErr
	}
}

// SandboxLayer is a named overlay of a registry's sandbox policy that can be
// set, replaced and removed while the registry runs (see SetSandboxLayer).
type SandboxLayer struct {
	// Config is merged over the base for the registry's own commands and
	// in-process file checks, and for the registries derived from it.
	Config sandbox.Config
	// Members is what a context bound by ExecutionPolicy takes from the
	// layer; the zero value gives it nothing. ExecutionPolicy judges it the
	// way it judges the base: a read or socket grant the context's denials
	// cover is dropped, write grants reach only a writable context, and an
	// env value inside the context's source root is rebased into its root.
	Members     sandbox.Config
	Environment *SandboxEnvironment
}

// WithSandboxLayer adds the named layer to the registry's sandbox policy from
// the start; SetSandboxLayer says what a layer reaches. Like
// WithSandboxFactory it prepares the layer when the option is created, and a
// preparation error surfaces when the registry first builds a process tool's
// sandbox. A later option with the same name replaces the layer.
func WithSandboxLayer(name string, layer SandboxLayer) RegistryOption {
	prepared, err := prepareSandboxLayer(name, layer)
	prepared.name, prepared.err = name, err
	return func(o *registryOptions) {
		o.sandboxLayers = withSandboxLayer(o.sandboxLayers, prepared)
	}
}

// sandboxLayer is one named overlay of a registry's sandbox policy, both of
// its parts prepared once, with the preparation error WithSandboxLayer could
// not return.
type sandboxLayer struct {
	name        string
	cfg         sandbox.Config
	members     sandbox.Config
	err         error
	environment *SandboxEnvironment
}

// prepareSandboxLayer freezes a copy of both parts of layer, so neither
// caller mutation nor a later path replacement changes the authority that
// was approved.
func prepareSandboxLayer(name string, layer SandboxLayer) (sandboxLayer, error) {
	if name == "" {
		return sandboxLayer{}, errors.New("sandbox layer needs a name")
	}
	cfg, err := sandbox.PrepareConfig(layer.Config.Merge(sandbox.Config{}))
	if err != nil {
		return sandboxLayer{}, fmt.Errorf("prepare sandbox layer %q: %w", name, err)
	}
	members, err := sandbox.PrepareConfig(layer.Members.Merge(sandbox.Config{}))
	if err != nil {
		return sandboxLayer{}, fmt.Errorf("prepare sandbox layer %q for members: %w", name, err)
	}
	if layer.Environment != nil {
		if err := layer.Environment.Storage.Validate(); err != nil {
			return sandboxLayer{}, err
		}
		for _, ref := range layer.Environment.Env {
			if _, _, err := layer.Environment.Storage.Lookup(ref); err != nil {
				return sandboxLayer{}, err
			}
		}
	}
	return sandboxLayer{name: name, cfg: cfg, members: members, environment: layer.Environment.clone()}, nil
}

// withSandboxLayer returns layers with layer in place of any of the same name,
// in name order, without mutating layers.
func withSandboxLayer(layers []sandboxLayer, layer sandboxLayer) []sandboxLayer {
	out := append(withoutSandboxLayer(layers, layer.name), layer)
	slices.SortFunc(out, func(a, b sandboxLayer) int { return strings.Compare(a.name, b.name) })
	return out
}

// withoutSandboxLayer returns layers without the named one, without mutating
// layers.
func withoutSandboxLayer(layers []sandboxLayer, name string) []sandboxLayer {
	out := make([]sandboxLayer, 0, len(layers)+1)
	for _, layer := range layers {
		if layer.name != name {
			out = append(out, layer)
		}
	}
	return out
}

// sandboxPolicy is one consistent view of a registry's prepared authority:
// the base every sandbox starts from and the layers merged over it.
type sandboxPolicy struct {
	base   sandbox.Config
	layers []sandboxLayer
}

// processConfig returns the policy a process tool starts from: the base with
// every layer merged in name order.
func (p sandboxPolicy) processConfig() (sandbox.Config, error) {
	cfg := p.base
	for _, layer := range p.layers {
		if layer.err != nil {
			return sandbox.Config{}, layer.err
		}
		cfg = cfg.Merge(layer.cfg)
	}
	return cfg, nil
}

// memberConfig returns what a bound context takes from the layers: their
// member parts merged in name order.
func (p sandboxPolicy) memberConfig() (sandbox.Config, error) {
	var cfg sandbox.Config
	for _, layer := range p.layers {
		if layer.err != nil {
			return sandbox.Config{}, layer.err
		}
		cfg = cfg.Merge(layer.members)
	}
	return cfg, nil
}

// WithUnsafeNoSandbox explicitly permits process-backed tools to run without
// an OS sandbox. Without this option, registries that lack a sandbox factory
// reject bash, shell tools, and stdio MCP servers instead of silently running
// them with ambient host access.
func WithUnsafeNoSandbox() RegistryOption {
	return func(o *registryOptions) {
		o.unsafeNoSandbox = true
	}
}

// HasNativeTool reports whether name is a built-in tool this registry can
// load, loaded or not. Session restoration uses it to drop a tool that a
// saved session names but Polly no longer ships.
func (r *ToolRegistry) HasNativeTool(name string) bool {
	_, ok := r.nativeFactory(name)
	return ok
}

// nativeFactory returns the factory registered for a built-in tool.
func (r *ToolRegistry) nativeFactory(name string) (func() (Tool, error), bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	factory, ok := r.nativeTools[name]
	return factory, ok
}

// HasSandbox reports whether sandboxing is available.
func (r *ToolRegistry) HasSandbox() bool {
	return r.sandboxFactory != nil
}

func (r *ToolRegistry) requireProcessSandbox(kind string) error {
	if r.sandboxFactory != nil || r.unsafeNoSandbox {
		return nil
	}
	return fmt.Errorf("%s requires sandboxing; configure WithSandboxFactory or explicitly use WithUnsafeNoSandbox", kind)
}

// NewSandbox creates a sandbox with the policy a process tool starts from —
// the base config and its layers — merged with optional per-tool overrides.
func (r *ToolRegistry) NewSandbox(overlay *sandbox.Config) (sandbox.Sandbox, error) {
	sb, _, err := r.newSandboxFor("", overlay)
	return sb, err
}

func (r *ToolRegistry) constructPreparedSandbox(cfg sandbox.Config) (sandbox.Sandbox, error) {
	if r.sandboxFactory == nil {
		return nil, fmt.Errorf("sandboxing not available")
	}
	sb, err := r.sandboxFactory(cfg)
	if err != nil {
		return nil, err
	}
	if sb == nil {
		return nil, fmt.Errorf("sandbox factory returned no sandbox")
	}
	return sb, nil
}

// preparedBaseSandboxConfig freezes the caller-approved base authority once
// for the lifetime of the registry. Backends are constructed lazily, so
// re-preparing the original path spellings for each tool would let an earlier
// sandbox retarget a symlink before a later tool is loaded. A derived
// registry reads its root's.
func (r *ToolRegistry) preparedBaseSandboxConfig() (sandbox.Config, error) {
	r = r.sandboxPolicyOwner()
	r.sandboxConfigMu.Lock()
	defer r.sandboxConfigMu.Unlock()
	return r.preparedBaseLocked()
}

// preparedBaseLocked is preparedBaseSandboxConfig for the registry holding
// the policy. Caller must hold r.sandboxConfigMu.
func (r *ToolRegistry) preparedBaseLocked() (sandbox.Config, error) {
	if !r.baseSandboxPrepared {
		r.baseSandboxCfg, r.baseSandboxPrepareErr = sandbox.PrepareConfig(r.baseSandboxCfg)
		r.baseSandboxPrepared = true
	}
	return r.baseSandboxCfg, r.baseSandboxPrepareErr
}

// sandboxPolicyOwner returns the registry holding this registry's sandbox
// policy: itself, or the root of the registries it was derived from.
func (r *ToolRegistry) sandboxPolicyOwner() *ToolRegistry {
	for r.sandboxParent != nil {
		r = r.sandboxParent
	}
	return r
}

// currentSandboxPolicy returns the prepared base and the layers of the
// registry holding this registry's policy.
func (r *ToolRegistry) currentSandboxPolicy() (sandboxPolicy, error) {
	owner := r.sandboxPolicyOwner()
	owner.sandboxConfigMu.Lock()
	defer owner.sandboxConfigMu.Unlock()
	owner.trackSandboxDependentLocked(r)
	base, err := owner.preparedBaseLocked()
	if err != nil {
		return sandboxPolicy{}, fmt.Errorf("prepare base sandbox config: %w", err)
	}
	// Changes replace the layer slice and never write into it.
	return sandboxPolicy{base: base, layers: owner.sandboxLayers}, nil
}

// trackSandboxDependentLocked includes dependent's own tools in changes to
// r's policy. Caller must hold r.sandboxConfigMu.
func (r *ToolRegistry) trackSandboxDependentLocked(dependent *ToolRegistry) {
	if dependent == r {
		return
	}
	if r.sandboxDependents == nil {
		r.sandboxDependents = make(map[*ToolRegistry]struct{})
	}
	r.sandboxDependents[dependent] = struct{}{}
}

// SandboxReadPolicy returns the policy in-process reads and writes are
// checked against, via sandbox.ReadAllowed and sandbox.WriteAllowed, when
// process sandboxing is active: the prepared base with every sandbox layer
// merged in name order, which is also the policy a bash or shell tool starts
// from before its own overlay, so a file tool reaches what a command
// reaches. A registry bound to an execution context returns the context's
// policy. active is false when no sandbox factory is configured, in which
// case in-process access is unrestricted just like wrapped commands.
func (r *ToolRegistry) SandboxReadPolicy() (cfg sandbox.Config, active bool, err error) {
	if r.executionPolicy != nil {
		return *r.executionPolicy, true, nil
	}
	if r.sandboxFactory == nil {
		return sandbox.Config{}, false, nil
	}
	policy, err := r.currentSandboxPolicy()
	if err != nil {
		return sandbox.Config{}, true, err
	}
	cfg, err = policy.processConfig()
	return cfg, true, err
}

// BaseSandboxPolicy is SandboxReadPolicy without the sandbox layers: the
// prepared base, which Polly's own processes, such as the worktree package's
// runtime Git, start from. A registry bound to an execution context returns
// the context's policy.
func (r *ToolRegistry) BaseSandboxPolicy() (cfg sandbox.Config, active bool, err error) {
	if r.executionPolicy != nil {
		return *r.executionPolicy, true, nil
	}
	if r.sandboxFactory == nil {
		return sandbox.Config{}, false, nil
	}
	cfg, err = r.preparedBaseSandboxConfig()
	return cfg, true, err
}

// SetSandboxLayer replaces the named layer of the registry's sandbox policy,
// or removes it when layer is nil, then rebuilds the loaded and staged bash
// and shell tools, including derived registries' own tools, under the result
// the way AppendBaseReadPaths does. A layer's Config
// is merged over the base, in name order and before a tool's own overlay,
// into the sandboxes of bash, shell tools and NewSandbox and into
// SandboxReadPolicy, which the in-process file tools check, so it can widen
// or narrow what the registry's commands and file tools reach and be taken
// back mid-session. Registries derived via Derive share it. Its Members part
// reaches the contexts ExecutionPolicy binds from then on; a context bound
// earlier keeps the policy it was bound with. A layer never reaches stdio
// MCP servers, which a narrowing change could not rebuild, shell-tool schema
// discovery, or BaseSandboxPolicy. Without a sandbox factory, or under
// WithUnsafeNoSandbox, the call does nothing.
func (r *ToolRegistry) SetSandboxLayer(name string, layer *SandboxLayer) (SandboxChange, error) {
	if name == "" {
		return SandboxChange{}, errors.New("sandbox layer needs a name")
	}
	return r.changeSandboxPolicy(func(policy sandboxPolicy) (sandboxPolicy, error) {
		if layer == nil {
			policy.layers = withoutSandboxLayer(policy.layers, name)
			return policy, nil
		}
		prepared, err := prepareSandboxLayer(name, *layer)
		if err != nil {
			return sandboxPolicy{}, err
		}
		policy.layers = withSandboxLayer(policy.layers, prepared)
		return policy, nil
	})
}

// SandboxChange reports how a change to a registry's sandbox policy reached
// the tools it has loaded.
type SandboxChange struct {
	// Rebuilt names, once each, the loaded or staged bash and shell tools
	// rebuilt here or in a derived registry under the changed policy.
	Rebuilt []string
	// StaleServers names, by tool namespace, the loaded stdio MCP servers a
	// change to the base did not reach: a running server keeps the policy it
	// started with until it is loaded again. Layers never reach servers, so
	// a layer change leaves it empty.
	StaleServers []string
}

// AppendBaseReadPaths adds canonical extra read directories to the frozen
// base sandbox config as read-only grants. The registry freezes the
// caller-approved base authority once (see preparedBaseSandboxConfig) and
// never re-prepares the original spellings, so a mid-session add must
// prepare the new paths on their own and merge: the merged entries are
// frozen exactly once here, and Config.Merge preserves every identity
// already frozen. After the call, SandboxReadPolicy, the loaded bash and
// shell tools (rebuilt, see SandboxChange), every later sandbox, and every
// registry derived via Derive, before or after, see the new paths; a stdio
// MCP server already running keeps its snapshot until it is loaded again.
// Missing paths are dropped by preparation and grant nothing, without error.
// Without a sandbox factory — or under WithUnsafeNoSandbox — there is no
// policy to widen, so the call is a documented no-op; the caller still
// records the list on the session. A derived registry shares its parent's
// policy and refuses the call. The merged grant list is not re-minimized:
// overlapping grants are harmless because the deepest rule wins, and the
// persisted session list is deduplicated by sandbox.MergeExtraReadDirs.
func (r *ToolRegistry) AppendBaseReadPaths(paths ...string) (SandboxChange, error) {
	if len(paths) == 0 {
		return SandboxChange{}, nil
	}
	change, err := r.changeSandboxPolicy(func(policy sandboxPolicy) (sandboxPolicy, error) {
		prepared, err := sandbox.PrepareConfig(sandbox.Config{ReadPaths: append([]string(nil), paths...)})
		if err != nil {
			return sandboxPolicy{}, fmt.Errorf("prepare appended read paths: %w", err)
		}
		policy.base = policy.base.Merge(prepared)
		return policy, nil
	})
	if err != nil {
		return SandboxChange{}, err
	}
	r.mu.RLock()
	change.StaleServers = r.sandboxedServersLocked()
	r.mu.RUnlock()
	return change, nil
}

// changeSandboxPolicy applies change to the prepared policy and rebuilds the
// loaded and staged bash and shell tools here and in every derived registry
// under the result. Every replacement is built before any is installed,
// and a change the factory refuses for any of them
// is dropped whole, so the policy and the sandboxes of the loaded tools never
// disagree; a call already running finishes on the instance it started with.
// Callers serialize tool loads with policy changes: a tool whose load
// overlaps a change may be built under either policy. Without a sandbox
// factory, or under WithUnsafeNoSandbox, there is no policy and the call does
// nothing. A derived registry shares its parent's policy and a registry bound
// to an execution context has a fixed one; both refuse.
func (r *ToolRegistry) changeSandboxPolicy(change func(sandboxPolicy) (sandboxPolicy, error)) (SandboxChange, error) {
	if r.sandboxFactory == nil || r.unsafeNoSandbox {
		return SandboxChange{}, nil
	}
	if r.sandboxParent != nil {
		return SandboxChange{}, errors.New("a derived registry shares its parent's sandbox policy; change the parent's")
	}
	if r.executionPolicy != nil {
		return SandboxChange{}, errors.New("a registry bound to an execution context has a fixed sandbox policy")
	}
	r.sandboxConfigMu.Lock()
	defer r.sandboxConfigMu.Unlock()
	// baseSandboxPrepared is always true when a sandbox factory is
	// configured: WithSandboxFactory prepares the base at option time.
	if r.baseSandboxPrepareErr != nil {
		return SandboxChange{}, fmt.Errorf("prepare base sandbox config: %w", r.baseSandboxPrepareErr)
	}
	next, err := change(sandboxPolicy{base: r.baseSandboxCfg, layers: r.sandboxLayers})
	if err != nil {
		return SandboxChange{}, err
	}
	process, err := next.processConfig()
	if err != nil {
		return SandboxChange{}, err
	}
	// Construct the changed policy once, so the factory's refusal fails the
	// change even when no loaded tool needs a rebuild.
	if _, _, err := r.buildSandbox(process, "changed policy", nil); err != nil {
		return SandboxChange{}, err
	}
	rebuilds, err := r.rebuildProcessTools(process)
	if err != nil {
		return SandboxChange{}, err
	}
	for dependent := range r.sandboxDependents {
		replacements, err := dependent.rebuildProcessTools(process)
		if err != nil {
			return SandboxChange{}, err
		}
		rebuilds = append(rebuilds, replacements...)
	}
	var result SandboxChange
	for _, rebuild := range rebuilds {
		owner := rebuild.registry
		owner.mu.Lock()
		loaded := owner.tools
		if rebuild.pending {
			loaded = owner.pendingTools
		}
		if loaded[rebuild.name] == rebuild.old {
			loaded[rebuild.name] = rebuild.tool
			result.Rebuilt = append(result.Rebuilt, rebuild.name)
		}
		owner.mu.Unlock()
	}
	slices.Sort(result.Rebuilt)
	result.Rebuilt = slices.Compact(result.Rebuilt)
	r.baseSandboxCfg = next.base
	r.sandboxLayers = next.layers
	return result, nil
}

// processToolRebuild pairs a loaded or staged process tool with its replacement.
type processToolRebuild struct {
	registry  *ToolRegistry
	pending   bool
	name      string
	old, tool Tool
}

// rebuildProcessTools builds a replacement under policy for every loaded or
// staged tool that runs a sandboxed process of its own, in name order, and
// installs none of them.
func (r *ToolRegistry) rebuildProcessTools(policy sandbox.Config) ([]processToolRebuild, error) {
	r.mu.RLock()
	loaded := make([]processToolRebuild, 0, len(r.tools)+len(r.pendingTools))
	for name, tool := range r.tools {
		loaded = append(loaded, processToolRebuild{registry: r, name: name, old: tool})
	}
	for name, tool := range r.pendingTools {
		loaded = append(loaded, processToolRebuild{registry: r, pending: true, name: name, old: tool})
	}
	r.mu.RUnlock()
	sort.Slice(loaded, func(i, j int) bool { return loaded[i].name < loaded[j].name })
	var rebuilds []processToolRebuild
	for _, rebuild := range loaded {
		tool, err := r.rebuildProcessTool(policy, rebuild.old)
		if err != nil {
			return nil, fmt.Errorf("rebuild %s under the changed sandbox policy: %w", rebuild.name, err)
		}
		if tool != nil {
			rebuild.tool = tool
			rebuilds = append(rebuilds, rebuild)
		}
	}
	return rebuilds, nil
}

// rebuildProcessTool returns tool with a sandbox built under policy the way
// its first was built, or nil when tool runs no sandboxed process of its
// own.
func (r *ToolRegistry) rebuildProcessTool(policy sandbox.Config, tool Tool) (Tool, error) {
	switch t := tool.(type) {
	case *NamespacedTool:
		inner, err := r.rebuildProcessTool(policy, t.Tool)
		if inner == nil || err != nil {
			return nil, err
		}
		return &NamespacedTool{Tool: inner, namespacedName: t.namespacedName}, nil
	case *BashTool:
		if t.sandbox == nil {
			return nil, nil
		}
		sb, cfg, err := r.buildSandbox(policy, "bash", nil)
		if err != nil {
			return nil, err
		}
		return t.withSandboxConfig(sb, cfg), nil
	case *ShellTool:
		if t.sandbox == nil {
			return nil, nil
		}
		sb, cfg, err := r.buildSandbox(policy, t.Command, t.SandboxConfig(), t.Command)
		if err != nil {
			return nil, err
		}
		return t.withSandboxConfig(sb, cfg), nil
	}
	return nil, nil
}

// sandboxedServersLocked names, by tool namespace and in order, the
// sandboxed stdio MCP servers whose tools the registry holds, loaded or
// staged. Caller must hold r.mu.
func (r *ToolRegistry) sandboxedServersLocked() []string {
	var names []string
	for _, clients := range []map[string]*MCPClient{r.toolClients, r.pendingToolClients} {
		for name, client := range clients {
			if !client.sandboxed {
				continue
			}
			namespace, _, _ := strings.Cut(name, "__")
			if !slices.Contains(names, namespace) {
				names = append(names, namespace)
			}
		}
	}
	sort.Strings(names)
	return names
}

// newSandboxFor is NewSandbox with a tool identity for debug logging of the
// effective merged config (names and flags only, never env values).
// executables are the tool's own script, kept readable inside private roots.
func (r *ToolRegistry) newSandboxFor(name string, overlay *sandbox.Config, executables ...string) (sandbox.Sandbox, sandbox.Config, error) {
	if r.sandboxFactory == nil {
		return nil, sandbox.Config{}, fmt.Errorf("sandboxing not available")
	}
	policy, err := r.currentSandboxPolicy()
	if err != nil {
		return nil, sandbox.Config{}, err
	}
	process, err := policy.processConfig()
	if err != nil {
		return nil, sandbox.Config{}, err
	}
	return r.buildSandbox(process, name, overlay, executables...)
}

// newServerSandbox builds a stdio MCP server's sandbox from the base alone:
// a server outlives policy changes, so a layer that could later be narrowed
// or removed never reaches it. executables are the server's own binary.
func (r *ToolRegistry) newServerSandbox(name string, overlay *sandbox.Config, executables ...string) (sandbox.Sandbox, sandbox.Config, error) {
	if r.sandboxFactory == nil {
		return nil, sandbox.Config{}, fmt.Errorf("sandboxing not available")
	}
	base, err := r.preparedBaseSandboxConfig()
	if err != nil {
		return nil, sandbox.Config{}, fmt.Errorf("prepare base sandbox config: %w", err)
	}
	return r.buildSandbox(base, name, overlay, executables...)
}

// buildSandbox merges overlay over the prepared policy, keeps executables
// readable, prepares the result, and constructs its sandbox.
func (r *ToolRegistry) buildSandbox(policy sandbox.Config, name string, overlay *sandbox.Config, executables ...string) (sandbox.Sandbox, sandbox.Config, error) {
	cfg := policy
	if overlay != nil {
		cfg = cfg.Merge(*overlay)
	}
	var err error
	for _, executable := range executables {
		if cfg, err = exposeExecutable(cfg, executable); err != nil {
			return nil, sandbox.Config{}, fmt.Errorf("expose %s executable: %w", name, err)
		}
	}
	cfg, err = sandbox.PrepareConfig(cfg)
	if err != nil {
		return nil, sandbox.Config{}, fmt.Errorf("prepare effective sandbox config: %w", err)
	}
	slog.Debug("sandbox_config",
		"tool", name,
		"network", cfg.AllowNetwork,
		"deny_dns", cfg.DenyDNS,
		"deny_write", cfg.DenyWrite,
		"writable_paths", cfg.WritablePaths,
		"read_paths", cfg.ReadPaths,
		"deny_paths", cfg.DenyPaths,
		"allow_env", cfg.AllowEnv,
		"pass_env", cfg.PassEnv,
		"allow_unix_sockets", cfg.AllowUnixSockets)
	sb, err := r.constructPreparedSandbox(cfg)
	if err != nil {
		return nil, cfg, err
	}
	return sb, cfg, nil
}

// NewSandboxDirect creates a sandbox from an explicit config, ignoring the base config.
func (r *ToolRegistry) NewSandboxDirect(cfg sandbox.Config) (sandbox.Sandbox, error) {
	if _, err := r.preparedBaseSandboxConfig(); err != nil {
		return nil, fmt.Errorf("prepare base sandbox config: %w", err)
	}
	prepared, err := sandbox.PrepareConfig(cfg)
	if err != nil {
		return nil, err
	}
	return r.constructPreparedSandbox(prepared)
}

// newSchemaSandbox constructs a deliberately narrower policy for executable
// --schema discovery of script. Discovery never inherits workspace writes,
// network, credential exemptions, environment allowances or passthroughs, or
// Unix-socket grants from the normal execution policy. It does retain deny
// rules so a policy cannot become weaker while metadata is being inspected,
// and the read grants that keep interpreters under the private home usable.
func (r *ToolRegistry) newSchemaSandbox(script string) (sandbox.Sandbox, error) {
	baseCfg, err := r.preparedBaseSandboxConfig()
	if err != nil {
		return nil, fmt.Errorf("prepare base sandbox config: %w", err)
	}
	cfg := sandbox.DefaultConfig()
	cfg.DenyPaths = append([]string(nil), baseCfg.DenyPaths...)
	cfg.DenyWritePaths = append([]string(nil), baseCfg.DenyWritePaths...)
	cfg.ReadPaths = inheritableReadPaths(baseCfg, nil)
	cfg.DenyWrite = baseCfg.DenyWrite
	if cfg.DenyHostTemp = baseCfg.DenyHostTemp; cfg.DenyHostTemp {
		// The default policy names the host temp directory explicitly; a
		// withheld temp grant must not return through it.
		cfg.WritablePaths = nil
	}
	if cfg, err = exposeExecutable(cfg, script); err != nil {
		return nil, err
	}
	return r.NewSandboxDirect(cfg)
}

type stagedToolRecord struct {
	name       string
	tool       Tool
	client     *MCPClient
	serverSpec string
}

func closeStagedToolRecords(records []stagedToolRecord) {
	closed := make(map[*MCPClient]bool)
	for _, record := range records {
		if record.client != nil && !closed[record.client] {
			record.client.Close()
			closed[record.client] = true
		}
	}
}

// NewToolRegistry creates a registry. Process-backed tools are rejected unless
// the registry has a sandbox factory or the caller explicitly selects
// WithUnsafeNoSandbox; in-process tools can be registered without either.
func NewToolRegistry(tools []Tool, opts ...RegistryOption) *ToolRegistry {
	var o registryOptions
	for _, opt := range opts {
		opt(&o)
	}

	registry := newRegistry(o)
	for _, tool := range tools {
		registry.Register(tool)
	}
	return registry
}

// newRegistry builds an empty registry with the built-in native factories.
func newRegistry(o registryOptions) *ToolRegistry {
	registry := &ToolRegistry{
		environmentGate:       NewExecutionGate(),
		tools:                 make(map[string]Tool),
		nativeTools:           make(map[string]func() (Tool, error)),
		toolClients:           make(map[string]*MCPClient),
		serverTools:           make(map[string][]string),
		pendingTools:          make(map[string]Tool),
		pendingToolClients:    make(map[string]*MCPClient),
		pendingServerTools:    make(map[string][]string),
		alwaysAllowedTools:    make(map[string]bool),
		autoAllowedTools:      make(map[string]bool),
		pendingAutoAllowed:    make(map[string]bool),
		sandboxFactory:        o.sandboxFactory,
		baseSandboxCfg:        o.baseSandboxCfg,
		baseSandboxPrepared:   o.baseSandboxPrepared,
		baseSandboxPrepareErr: o.baseSandboxPrepareErr,
		sandboxLayers:         o.sandboxLayers,
		unsafeNoSandbox:       o.unsafeNoSandbox,
		changeTracker:         o.changeTracker,
	}

	registry.nativeTools["bash"] = func() (Tool, error) {
		bt := newBashTool(registry.executionRoot)
		bt.siblingLoaded = registry.hasVisibleTool
		bt.tracker = registry.ChangeTracker
		if err := registry.requireProcessSandbox("bash"); err != nil {
			return nil, err
		}
		if registry.sandboxFactory == nil {
			return bt, nil
		}
		// Fail closed: bash without its sandbox must not load.
		sb, cfg, err := registry.newSandboxFor("bash", nil)
		if err != nil {
			return nil, fmt.Errorf("sandbox for bash: %w", err)
		}
		return bt.withSandboxConfig(sb, cfg), nil
	}

	// read_file applies the base read policy in-process when sandboxing is
	// active and reads unrestricted otherwise, like view_image. The writing
	// tools fail closed without a sandbox: an in-process write grants the
	// model the caller's ambient host access exactly like an unsandboxed
	// command, so they require WithUnsafeNoSandbox to load without one.
	registry.nativeTools["read_file"] = func() (Tool, error) {
		return NewReadFileTool(registry), nil
	}
	registry.nativeTools["list_dir"] = func() (Tool, error) {
		return NewListDirTool(registry), nil
	}
	registry.nativeTools["write_file"] = func() (Tool, error) {
		if err := registry.requireProcessSandbox("write_file"); err != nil {
			return nil, err
		}
		return NewWriteFileTool(registry), nil
	}
	registry.nativeTools["edit_file"] = func() (Tool, error) {
		if err := registry.requireProcessSandbox("edit_file"); err != nil {
			return nil, err
		}
		return NewEditFileTool(registry), nil
	}

	return registry
}

// DeriveOption narrows a derived registry.
type DeriveOption func(*deriveOptions)

type deriveOptions struct {
	allow []string
	deny  []string
}

// AllowTools limits the tools a derived registry sees to those matching one
// of the patterns (filepath.Match globs, or exact names). It bounds the
// tools the derived registry loads itself as well as the parent's; only its
// always-allowed built-ins pass regardless. Repeated options accumulate.
func AllowTools(patterns ...string) DeriveOption {
	return func(o *deriveOptions) {
		o.allow = appendUniqueStrings(o.allow, patterns)
	}
}

// DenyTools hides the parent tools matching any of the patterns from a
// derived registry, after AllowTools. Repeated options accumulate.
func DenyTools(patterns ...string) DeriveOption {
	return func(o *deriveOptions) {
		o.deny = appendUniqueStrings(o.deny, patterns)
	}
}

func (o deriveOptions) filter() func(name string) bool {
	if len(o.allow) == 0 && len(o.deny) == 0 {
		return nil
	}
	return func(name string) bool {
		if len(o.allow) > 0 && !matchesAnyToolPattern(o.allow, name) {
			return false
		}
		return !matchesAnyToolPattern(o.deny, name)
	}
}

func matchesAnyToolPattern(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if MatchesToolPattern(pattern, name) {
			return true
		}
	}
	return false
}

// Derive returns a registry that sees this registry's tools through the
// options' allow-list and shares its MCP clients and sandbox policy, for a
// caller that needs a narrower or separately governed tool set (a subagent,
// say) without starting the servers again. The derived registry is a full
// registry of its own: tools it registers or loads are private to it and
// shadow the parent's, its skill policy and always-allowed set are its own,
// and closing it releases only what it loaded itself. A parent tool stays
// subject to the parent's policy as well, and the parent closing empties
// every registry derived from it. The allow-list bounds every tool but the
// derived registry's own always-allowed built-ins. The sandbox policy is the
// parent's own, not a copy: a later change to it (AppendBaseReadPaths or
// SetSandboxLayer) reaches the derived registry's policy checks and rebuilds
// its loaded and staged process tools as well as the parent's.
func (r *ToolRegistry) Derive(opts ...DeriveOption) *ToolRegistry {
	var o deriveOptions
	for _, opt := range opts {
		opt(&o)
	}
	derived := newRegistry(registryOptions{
		sandboxFactory:  r.sandboxFactory,
		unsafeNoSandbox: r.unsafeNoSandbox,
	})
	derived.parent = r
	derived.environmentGate = r.environmentGate
	derived.sandboxParent = r
	derived.executionRoot = r.executionRoot
	derived.executionSourceRoot = r.executionSourceRoot
	derived.executionPolicy = r.executionPolicy
	derived.executionSkills = r.executionSkills
	derived.viewAllowed = o.filter()
	return derived
}

// viewVisibleLocked reports whether the derived registry's allow-list lets
// name through; its own always-allowed tools pass regardless. Caller must
// hold r.mu.
func (r *ToolRegistry) viewVisibleLocked(name string) bool {
	return r.viewAllowed == nil || r.alwaysAllowedTools[name] || r.viewAllowed(name)
}

// hiddenByView returns, in order, the names among names that this
// registry's allow-list hides. Every name passes a registry without one.
func (r *ToolRegistry) hiddenByView(names []string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var hidden []string
	for _, name := range names {
		if !r.viewVisibleLocked(name) {
			hidden = append(hidden, name)
		}
	}
	return hidden
}

// lookupLocked finds a registered tool here or in the parent chain,
// regardless of policy. Caller must hold r.mu.
func (r *ToolRegistry) lookupLocked(name string) (Tool, bool) {
	if tool, ok := r.tools[name]; ok {
		return tool, true
	}
	if r.parent == nil {
		return nil, false
	}
	return r.parent.registeredTool(name)
}

// Register adds a tool to the registry
func (r *ToolRegistry) Register(tool Tool) {
	// A caller may register a process tool built elsewhere, before this
	// derived registry has constructed a sandbox or queried its policy.
	if owner := r.sandboxPolicyOwner(); owner != r {
		owner.sandboxConfigMu.Lock()
		owner.trackSandboxDependentLocked(r)
		owner.sandboxConfigMu.Unlock()
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	name := tool.GetName()
	if name == "" {
		// Fallback to schema title if GetName() returns empty
		if s := tool.GetSchema(); s != nil && s.Title() != "" {
			name = s.Title()
		}
	}

	if name != "" {
		slog.Debug("tool_registered", "tool_name", name)
		r.tools[name] = tool
	}
}

// registerSkillRuntimeTools publishes the skill built-ins and their bash
// dependency as one registry state transition. If bash is already registered,
// it remains authoritative; otherwise candidate is installed. A nil candidate
// leaves the registry untouched so its caller can construct one outside r.mu.
func (r *ToolRegistry) registerSkillRuntimeTools(activate *SkillActivateTool, readFile *SkillReadFileTool, candidate Tool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.lookupLocked("bash"); !exists {
		if candidate == nil {
			return false
		}
		r.tools["bash"] = candidate
		slog.Debug("tool_registered", "tool_name", "bash")
	}
	r.tools[activate.GetName()] = activate
	r.tools[readFile.GetName()] = readFile
	r.alwaysAllowedTools[activate.GetName()] = true
	r.alwaysAllowedTools[readFile.GetName()] = true
	slog.Debug("tool_registered", "tool_name", activate.GetName())
	slog.Debug("tool_registered", "tool_name", readFile.GetName())
	return true
}

func (r *ToolRegistry) registeredTool(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lookupLocked(name)
}

// hasVisibleTool reports whether a tool is registered and allowed — i.e. the
// model can see and call it. Bash consults this from GetSchema to steer file
// work toward dedicated tools, so it must never be called with r.mu held.
func (r *ToolRegistry) hasVisibleTool(name string) bool {
	_, _, allowed := r.GetIfAllowed(name)
	return allowed
}

// MarkAlwaysAllowed exempts a tool from active skill allowlist filtering.
func (r *ToolRegistry) MarkAlwaysAllowed(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alwaysAllowedTools[name] = true
}

// RegisterNative registers a native tool factory
func (r *ToolRegistry) RegisterNative(name string, factory func() Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.nativeTools[name] = func() (Tool, error) {
		tool := factory()
		if tool == nil {
			return nil, fmt.Errorf("native tool factory %s returned no tool", name)
		}
		return tool, nil
	}
	slog.Debug("native_factory_registered", "factory_name", name)
}

// Get retrieves a tool by name
func (r *ToolRegistry) Get(name string) (Tool, bool) {
	tool, _, allowed := r.GetIfAllowed(name)
	if !allowed {
		return nil, false
	}
	return tool, true
}

// GetIfAllowed retrieves a tool by name, returning existence and allowance
// in a single lock acquisition. A derived registry's own tools come first;
// a parent's tool must pass the parent's policy, then this registry's
// allow-list and policy.
func (r *ToolRegistry) GetIfAllowed(name string) (tool Tool, exists bool, allowed bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tool, exists = r.tools[name]
	if !exists {
		if r.parent == nil {
			return nil, false, false
		}
		// A parent's tool must pass the parent's policy first.
		if tool, exists, allowed = r.parent.GetIfAllowed(name); !allowed {
			return nil, exists, false
		}
	}
	if !r.viewVisibleLocked(name) || !r.isToolAllowedLocked(name) {
		return nil, true, false
	}
	return tool, true, true
}

// setToolLocked installs tool under name with the MCP client backing it (nil
// for non-MCP tools). If the entry it replaces was the last reference to a
// different client, that client is closed: a reloaded server or a restored
// filtered set must not strand the old transport or subprocess, which Close
// only ever visits through these maps.
func (r *ToolRegistry) setToolLocked(name string, tool Tool, client *MCPClient) {
	old := r.toolClients[name]
	r.tools[name] = tool
	if client != nil {
		r.toolClients[name] = client
	} else {
		delete(r.toolClients, name)
	}
	if old != nil && old != client {
		r.closeIfOrphanedLocked(old)
	}
}

// closeIfOrphanedLocked closes client once no live or pending tool refers to
// it.
func (r *ToolRegistry) closeIfOrphanedLocked(client *MCPClient) {
	for _, c := range r.toolClients {
		if c == client {
			return
		}
	}
	for _, c := range r.pendingToolClients {
		if c == client {
			return
		}
	}
	slog.Debug("mcp_client_closed", "reason", "no_remaining_tools")
	client.Close()
}

// dropServerToolsLocked removes a server's previous registration so a fresh
// load of the same spec replaces it rather than leaving stale tools behind,
// closing the previous client once nothing refers to it.
func (r *ToolRegistry) dropServerToolsLocked(serverSpec string) {
	names := r.serverTools[serverSpec]
	if len(names) == 0 {
		return
	}
	delete(r.serverTools, serverSpec)
	var clients []*MCPClient
	for _, name := range names {
		if c := r.toolClients[name]; c != nil {
			clients = append(clients, c)
		}
		delete(r.tools, name)
		delete(r.toolClients, name)
		slog.Debug("mcp_tool_removed", "tool_name", name)
	}
	for _, c := range clients {
		r.closeIfOrphanedLocked(c)
	}
}

// Remove removes a tool by namespaced name from the registry
func (r *ToolRegistry) Remove(namespacedName string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	tool, exists := r.tools[namespacedName]
	if !exists {
		return
	}

	// Get MCP client if this is an MCP tool
	client := r.toolClients[namespacedName]

	// Remove from registry
	delete(r.tools, namespacedName)
	delete(r.toolClients, namespacedName)
	slog.Debug("tool_removed", "tool_name", namespacedName)

	// Clean up MCP-specific tracking
	if client != nil {
		source := tool.GetSource()

		// Update serverTools list
		if tools := r.serverTools[source]; len(tools) > 0 {
			var remaining []string
			for _, name := range tools {
				if name != namespacedName {
					remaining = append(remaining, name)
				}
			}
			if len(remaining) > 0 {
				r.serverTools[source] = remaining
			} else {
				delete(r.serverTools, source)
			}
		}

		// Close client if no other tools use it
		r.closeIfOrphanedLocked(client)
	}
}

// Count reports how many tools the registry holds; a nil registry holds none.
func (r *ToolRegistry) Count() int {
	if r == nil {
		return 0
	}
	return len(r.All())
}

// All returns every tool the registry lets the model see, sorted by name.
func (r *ToolRegistry) All() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := r.allowedToolNamesLocked()
	tools := make([]Tool, 0, len(names))
	for _, name := range names {
		if tool, ok := r.lookupLocked(name); ok {
			tools = append(tools, tool)
		}
	}
	return tools
}

// GetSchemas returns all tool schemas. Schemas are built after the registry
// lock is released: a tool's GetSchema may consult the registry (bash
// cross-references the loaded file tools).
func (r *ToolRegistry) GetSchemas() []*schema.ToolSchema {
	tools := r.All()
	schemas := make([]*schema.ToolSchema, 0, len(tools))
	for _, tool := range tools {
		schemas = append(schemas, tool.GetSchema())
	}
	return schemas
}

func (r *ToolRegistry) allowedToolNamesLocked() []string {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		if r.viewVisibleLocked(name) && r.isToolAllowedLocked(name) {
			names = append(names, name)
		}
	}
	if r.parent != nil {
		for _, name := range r.parent.allowedToolNames() {
			if _, shadowed := r.tools[name]; shadowed {
				continue
			}
			if r.viewVisibleLocked(name) && r.isToolAllowedLocked(name) {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names
}

func (r *ToolRegistry) allowedToolNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.allowedToolNamesLocked()
}

func (r *ToolRegistry) isToolAllowedLocked(name string) bool {
	if r.alwaysAllowedTools[name] {
		return true
	}
	if !r.policyActive {
		return true
	}
	if r.autoAllowedTools[name] {
		return true
	}
	for _, pattern := range r.allowedPatterns {
		if MatchesToolPattern(pattern, name) {
			return true
		}
	}
	return false
}

// MatchesToolPattern reports whether a tool allow-list entry, a filepath.Match
// glob or an exact name, matches name.
func MatchesToolPattern(pattern, name string) bool {
	if pattern == "" {
		return false
	}
	matched, err := filepath.Match(pattern, name)
	if err == nil {
		return matched
	}
	return pattern == name
}

func appendUniqueStrings(dst []string, src []string) []string {
	seen := make(map[string]bool, len(dst))
	for _, item := range dst {
		seen[item] = true
	}
	for _, item := range src {
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		dst = append(dst, item)
	}
	return dst
}

// stageSkillAllowance queues allowed-tool patterns and auto-approved skill-owned tools.
func (r *ToolRegistry) stageSkillAllowance(patterns, autoAllowed []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(patterns) > 0 {
		r.pendingPolicyActive = true
		r.pendingAllowedPatterns = appendUniqueStrings(r.pendingAllowedPatterns, patterns)
	}
	for _, name := range autoAllowed {
		if name != "" {
			r.pendingAutoAllowed[name] = true
		}
	}
}

// CommitPendingChanges applies staged skill activations between agent turns.
func (r *ToolRegistry) CommitPendingChanges() {
	r.mu.RLock()
	empty := len(r.pendingTools) == 0 &&
		len(r.pendingToolClients) == 0 &&
		len(r.pendingServerTools) == 0 &&
		!r.pendingPolicyActive &&
		len(r.pendingAllowedPatterns) == 0 &&
		len(r.pendingAutoAllowed) == 0
	r.mu.RUnlock()
	if empty {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for name, tool := range r.pendingTools {
		r.setToolLocked(name, tool, r.pendingToolClients[name])
	}
	for serverSpec, toolNames := range r.pendingServerTools {
		r.serverTools[serverSpec] = appendUniqueStrings(r.serverTools[serverSpec], toolNames)
	}
	if r.pendingPolicyActive {
		r.policyActive = true
	}
	r.allowedPatterns = appendUniqueStrings(r.allowedPatterns, r.pendingAllowedPatterns)
	for name := range r.pendingAutoAllowed {
		r.autoAllowedTools[name] = true
	}

	r.pendingTools = make(map[string]Tool)
	r.pendingToolClients = make(map[string]*MCPClient)
	r.pendingServerTools = make(map[string][]string)
	r.pendingPolicyActive = false
	r.pendingAllowedPatterns = nil
	r.pendingAutoAllowed = make(map[string]bool)
}

// LoadMCPServer connects to an MCP server and registers its tools with namespace
// For multi-server configs (mcpServers format), loads ALL servers
func (r *ToolRegistry) LoadMCPServer(serverSpec string) (LoadResult, error) {
	return r.LoadMCPServerWithNamespacePrefix(serverSpec, "")
}

// LoadShellTool loads a single shell tool from a file path with the default namespace.
func (r *ToolRegistry) LoadShellTool(path string) (LoadResult, error) {
	return r.LoadShellToolWithNamespace(path, "")
}

// LoadShellToolWithNamespace loads a single shell tool from a file path with
// an explicit namespace; an empty namespace derives one from the path.
func (r *ToolRegistry) LoadShellToolWithNamespace(path, namespace string) (LoadResult, error) {
	record, result, err := r.prepareShellToolWithNamespace(path, namespace)
	if err != nil {
		return LoadResult{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.setToolLocked(record.name, record.tool, nil)
	slog.Debug("shell_tool_registered", "tool_name", record.name)

	return result, nil
}

// prepareShellToolWithNamespace discovers and sandboxes one shell tool without
// registering it. An empty namespace derives one from the path.
func (r *ToolRegistry) prepareShellToolWithNamespace(path, namespace string) (stagedToolRecord, LoadResult, error) {
	if err := r.requireProcessSandbox("shell tool"); err != nil {
		return stagedToolRecord{}, LoadResult{}, err
	}
	// Create a schema-loading sandbox (base config: no network, temp-only writes)
	// so that --schema execution cannot perform side effects.
	var schemaSB sandbox.Sandbox
	if r.sandboxFactory != nil {
		var err error
		schemaSB, err = r.newSchemaSandbox(path)
		if err != nil {
			return stagedToolRecord{}, LoadResult{}, fmt.Errorf("schema sandbox for shell tool %s: %w", path, err)
		}
	}
	shellTool, err := newShellTool(path, schemaSB)
	if err != nil {
		return stagedToolRecord{}, LoadResult{}, fmt.Errorf("failed to load shell tool %s: %w", path, err)
	}

	if shellTool.SandboxOptOut() && !r.unsafeNoSandbox {
		return stagedToolRecord{}, LoadResult{}, fmt.Errorf("shell tool %s requested sandbox:false; refusing without WithUnsafeNoSandbox", path)
	}
	if r.sandboxFactory != nil && !shellTool.SandboxOptOut() {
		// Fail closed: a tool that should be sandboxed but can't be must not
		// load, or it would silently run unsandboxed.
		sb, cfg, err := r.newSandboxFor(path, shellTool.SandboxConfig(), path)
		if err != nil {
			return stagedToolRecord{}, LoadResult{}, fmt.Errorf("sandbox for shell tool %s: %w", path, err)
		}
		shellTool = shellTool.withSandboxConfig(sb, cfg)
	}

	s := shellTool.GetSchema()
	if s == nil || s.Title() == "" {
		return stagedToolRecord{}, LoadResult{}, fmt.Errorf("shell tool %s has no name in schema", path)
	}
	if namespace == "" {
		namespace = extractNamespace(path)
	}

	namespacedName := fmt.Sprintf("%s__%s", namespace, s.Title())
	record := stagedToolRecord{
		name: namespacedName,
		tool: &NamespacedTool{
			Tool:           shellTool,
			namespacedName: namespacedName,
		},
	}

	return record, LoadResult{
		Type: "shell",
		Servers: []ServerResult{{
			Name:      namespace,
			ToolNames: []string{namespacedName},
		}},
	}, nil
}

func joinNamespacePrefix(prefix, name string) string {
	switch {
	case prefix == "":
		return name
	case name == "":
		return prefix
	default:
		return prefix + "-" + name
	}
}

func (r *ToolRegistry) stagePreparedTools(records []stagedToolRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, record := range records {
		r.pendingTools[record.name] = record.tool
		if record.client != nil {
			r.pendingToolClients[record.name] = record.client
		}
		if record.serverSpec != "" {
			r.pendingServerTools[record.serverSpec] = appendUniqueStrings(r.pendingServerTools[record.serverSpec], []string{record.name})
		}
		slog.Debug("tool_staged", "tool_name", record.name)
	}
}

// restrictiveSandboxConfig keeps a process tool's restrictions when rebinding
// it. Tool-local grants cannot enlarge the new execution context.
func restrictiveSandboxConfig(config sandbox.Config) sandbox.Config {
	return sandbox.Config{DenyPaths: config.DenyPaths, DenyWritePaths: config.DenyWritePaths, DenyWrite: config.DenyWrite, DenyHostTemp: config.DenyHostTemp, DenyDNS: config.DenyDNS}
}

func restrictiveSandboxOverlay(config *MCPConfig) json.RawMessage {
	if config.SandboxOptOut() {
		return nil
	}
	overlay, err := config.SandboxConfig()
	if err != nil || overlay == nil {
		return nil
	}
	restricted := restrictiveSandboxConfig(*overlay)
	data, err := json.Marshal(restricted)
	if err != nil {
		return nil
	}
	return data
}

func (r *ToolRegistry) prepareSingleMCPServerWithNamespace(jsonFile, serverName, namespace string, config *MCPConfig) ([]stagedToolRecord, []string, error) {
	config, err := r.contextMCPConfig(serverName, config)
	if err != nil {
		return nil, nil, err
	}
	serverSpec := fmt.Sprintf("%s#%s", jsonFile, serverName)
	return r.prepareMCPServerTools(config, serverName, namespace, serverSpec, nil)
}

// contextMCPConfig binds a server config to the registry's execution context,
// when it has one: the server runs in the context's root and its paths under
// the source root follow it there.
func (r *ToolRegistry) contextMCPConfig(serverName string, config *MCPConfig) (*MCPConfig, error) {
	if r.executionRoot == "" {
		return config, nil
	}
	if config.URL != "" && !config.ContextIndependent {
		return nil, fmt.Errorf("remote MCP server %s must declare contextIndependent to be shared with a worktree", serverName)
	}
	copy := *config
	config = &copy
	config.WorkDir = r.executionRoot
	config.Command = rebindSourcePath(config.Command, r.executionSourceRoot, r.executionRoot)
	config.Args = append([]string(nil), config.Args...)
	for i, arg := range config.Args {
		config.Args[i] = rebindSourcePath(arg, r.executionSourceRoot, r.executionRoot)
	}
	// Context binding is an upper bound: a server's own sandbox entry may
	// only narrow it. Its grants (and any opt-out) are dropped; the deny
	// rules and DNS block the parent honored for it still apply.
	config.Sandbox = restrictiveSandboxOverlay(config)
	return config, nil
}

// prepareMCPServerTools connects and wraps a resolved server configuration.
// Callers own config rebinding, source spelling, and registration semantics.
// A nil allowlist includes every tool; a non-nil empty one includes none.
func (r *ToolRegistry) prepareMCPServerTools(config *MCPConfig, serverName, namespace, serverSpec string, allowed map[string]bool) ([]stagedToolRecord, []string, error) {
	localProcess := config.Transport == "" || config.Transport == "stdio"
	if localProcess {
		if err := r.requireProcessSandbox("stdio MCP server"); err != nil {
			return nil, nil, err
		}
		if config.SandboxOptOut() && !r.unsafeNoSandbox {
			return nil, nil, fmt.Errorf("MCP server %s requested sandbox:false; refusing without WithUnsafeNoSandbox", serverName)
		}
	}
	var sb sandbox.Sandbox
	var sbCfg *sandbox.Config
	if localProcess && r.sandboxFactory != nil && !config.SandboxOptOut() {
		overlayCfg, err := config.SandboxConfig()
		if err != nil {
			return nil, nil, fmt.Errorf("invalid sandbox config for MCP server %s: %w", serverName, err)
		}
		var effective sandbox.Config
		var executables []string
		if resolved, err := exec.LookPath(config.Command); err == nil && filepath.IsAbs(resolved) {
			executables = append(executables, resolved)
		}
		sb, effective, err = r.newServerSandbox(serverName, overlayCfg, executables...)
		if err != nil {
			return nil, nil, fmt.Errorf("sandbox for MCP server %s: %w", serverName, err)
		}
		sbCfg = &effective
	}

	client, err := newMCPClientFromConfig(config, sb, sbCfg)
	if err != nil {
		return nil, nil, err
	}

	client.serverSpec = serverSpec

	serverTools, err := client.ListTools()
	if err != nil {
		client.Close()
		return nil, nil, err
	}

	var records []stagedToolRecord
	var toolNames []string
	for _, tool := range serverTools {
		s := tool.GetSchema()
		if s == nil || s.Title() == "" {
			continue
		}
		if allowed != nil && !allowed[s.Title()] {
			continue
		}

		namespacedName := fmt.Sprintf("%s__%s", namespace, s.Title())
		if mcpTool, ok := tool.(*MCPTool); ok {
			mcpTool.Source = serverSpec
		}

		wrappedTool := &NamespacedTool{
			Tool:           tool,
			namespacedName: namespacedName,
		}
		records = append(records, stagedToolRecord{
			name:       namespacedName,
			tool:       wrappedTool,
			client:     client,
			serverSpec: serverSpec,
		})
		toolNames = append(toolNames, namespacedName)
	}

	if len(records) == 0 {
		client.Close()
	}

	return records, toolNames, nil
}

func (r *ToolRegistry) prepareMCPServerWithNamespacePrefix(serverSpec, namespacePrefix string) ([]stagedToolRecord, LoadResult, error) {
	jsonFile, serverName := ParseServerSpec(serverSpec)

	configs, err := LoadMCPConfigFile(jsonFile)
	if err != nil {
		return nil, LoadResult{}, err
	}

	var records []stagedToolRecord
	result := LoadResult{Type: "mcp"}
	appendServer := func(name string, config MCPConfig) error {
		namespace := joinNamespacePrefix(namespacePrefix, name)
		serverRecords, toolNames, err := r.prepareSingleMCPServerWithNamespace(jsonFile, name, namespace, &config)
		if err != nil {
			return err
		}
		records = append(records, serverRecords...)
		result.Servers = append(result.Servers, ServerResult{Name: namespace, ToolNames: toolNames})
		return nil
	}

	if serverName != "" {
		config, ok := configs[serverName]
		if !ok {
			return nil, LoadResult{}, fmt.Errorf("server %q not found in config (available: %v)", serverName, mcpServerNames(configs))
		}
		if err := appendServer(serverName, config); err != nil {
			closeStagedToolRecords(records)
			return nil, LoadResult{}, err
		}
		return records, result, nil
	}

	for name, config := range configs {
		if err := appendServer(name, config); err != nil {
			closeStagedToolRecords(records)
			return nil, LoadResult{}, fmt.Errorf("server %s: %w", name, err)
		}
	}

	return records, result, nil
}

// LoadMCPServerWithNamespacePrefix loads all servers from a config file with an explicit namespace prefix.
func (r *ToolRegistry) LoadMCPServerWithNamespacePrefix(serverSpec, namespacePrefix string) (LoadResult, error) {
	records, result, err := r.prepareMCPServerWithNamespacePrefix(serverSpec, namespacePrefix)
	if err != nil {
		return LoadResult{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, record := range records {
		r.setToolLocked(record.name, record.tool, record.client)
		if record.serverSpec != "" {
			r.serverTools[record.serverSpec] = appendUniqueStrings(r.serverTools[record.serverSpec], []string{record.name})
		}
		slog.Debug("mcp_tool_registered", "tool_name", record.name)
	}

	return result, nil
}

// stageMCPServerWithNamespacePrefix queues MCP tools to be activated on the next turn.
func (r *ToolRegistry) stageMCPServerWithNamespacePrefix(serverSpec, namespacePrefix string) (LoadResult, error) {
	records, result, err := r.prepareMCPServerWithNamespacePrefix(serverSpec, namespacePrefix)
	if err != nil {
		return LoadResult{}, err
	}
	r.stagePreparedTools(records)
	return result, nil
}

// LoadToolAuto attempts to load a tool, auto-detecting if it's native, shell tool, or MCP server
func (r *ToolRegistry) LoadToolAuto(pathOrServer string) (LoadResult, error) {
	if factory, isNative := r.nativeFactory(pathOrServer); isNative {
		tool, err := factory()
		if err != nil {
			return LoadResult{}, err
		}
		r.Register(tool)
		return LoadResult{
			Type: "native",
			Servers: []ServerResult{{
				Name:      "native",
				ToolNames: []string{pathOrServer},
			}},
		}, nil
	}

	// Check if file exists
	info, err := os.Stat(pathOrServer)
	if os.IsNotExist(err) {
		return LoadResult{}, fmt.Errorf("file not found: %s", pathOrServer)
	}
	if err != nil {
		return LoadResult{}, fmt.Errorf("cannot access %s: %v", pathOrServer, err)
	}

	// JSON files are MCP configs; anything else must be an executable shell tool.
	if strings.HasSuffix(strings.ToLower(pathOrServer), ".json") {
		return r.LoadMCPServer(pathOrServer)
	}
	if info.Mode()&0111 == 0 {
		return LoadResult{}, fmt.Errorf("%s is not executable (for shell tools, run: chmod +x %s)", pathOrServer, pathOrServer)
	}
	return r.LoadShellTool(pathOrServer)
}

// GetActiveToolLoaders returns loader information for all tools
// This returns one entry per tool to allow selective loading
func (r *ToolRegistry) GetActiveToolLoaders() []ToolLoaderInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var loaders []ToolLoaderInfo

	for name, tool := range r.tools {
		loaders = append(loaders, ToolLoaderInfo{
			Name:   name,
			Type:   tool.GetType(),
			Source: tool.GetSource(),
		})
	}
	if r.parent != nil {
		for _, info := range r.parent.GetActiveToolLoaders() {
			if _, shadowed := r.tools[info.Name]; shadowed || !r.viewVisibleLocked(info.Name) {
				continue
			}
			loaders = append(loaders, info)
		}
	}

	return loaders
}

// RestartMCPServer starts the MCP server whose tools carry namespace again and
// swaps it in for the running one. A stdio server picks up the registry's
// current sandbox policy that way (a directory added with
// AppendBaseReadPaths, say), and a hung server gets a fresh process. The
// server's config is read again, and the registry keeps the tools it holds
// for the server: one the new process no longer offers is dropped, a new one
// is not added. The replacement starts before the running server stops, so
// when it cannot start, or offers none of the tools, the running server stays
// in place. Only a server this registry loaded itself restarts, and callers
// serialize a restart with calls to the server's tools.
func (r *ToolRegistry) RestartMCPServer(namespace string) (LoadResult, error) {
	spec, names, err := r.loadedMCPServer(namespace)
	if err != nil {
		return LoadResult{}, err
	}
	records, toolNames, err := r.prepareMCPServerRestart(spec, namespace, names)
	if err != nil {
		return LoadResult{}, fmt.Errorf("start MCP server %s again, keeping the running one: %w", namespace, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropServerToolsLocked(spec)
	for _, record := range records {
		r.setToolLocked(record.name, record.tool, record.client)
		slog.Debug("mcp_tool_registered", "tool_name", record.name)
	}
	r.serverTools[spec] = toolNames
	return LoadResult{Type: "mcp", Servers: []ServerResult{{Name: namespace, ToolNames: toolNames}}}, nil
}

// loadedMCPServer returns the spec of the one loaded server whose tools carry
// namespace, and those tools' names without it.
func (r *ToolRegistry) loadedMCPServer(namespace string) (string, []string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var specs, names []string
	for spec, toolNames := range r.serverTools {
		for _, name := range toolNames {
			if prefix, bare, ok := strings.Cut(name, "__"); ok && prefix == namespace {
				if !slices.Contains(specs, spec) {
					specs = append(specs, spec)
				}
				names = append(names, bare)
			}
		}
	}
	switch len(specs) {
	case 0:
		return "", nil, fmt.Errorf("no MCP server %q is loaded", namespace)
	case 1:
		return specs[0], names, nil
	}
	sort.Strings(specs)
	return "", nil, fmt.Errorf("MCP server %q is loaded from several configs: %s", namespace, strings.Join(specs, ", "))
}

// prepareMCPServerRestart starts the server spec names again under its
// current config and the registry's policy, offering only the tools named.
func (r *ToolRegistry) prepareMCPServerRestart(spec, namespace string, names []string) ([]stagedToolRecord, []string, error) {
	jsonFile, serverName := ParseServerSpec(spec)
	configs, err := LoadMCPConfigFile(jsonFile)
	if err != nil {
		return nil, nil, err
	}
	config, serverName, err := selectMCPServer(configs, jsonFile, serverName)
	if err != nil {
		return nil, nil, err
	}
	bound, err := r.contextMCPConfig(serverName, &config)
	if err != nil {
		return nil, nil, err
	}
	allowed := make(map[string]bool, len(names))
	for _, name := range names {
		allowed[name] = true
	}
	records, toolNames, err := r.prepareMCPServerTools(bound, serverName, namespace, spec, allowed)
	if err != nil {
		return nil, nil, err
	}
	if len(records) == 0 {
		return nil, nil, errors.New("the new process offers none of its tools")
	}
	return records, toolNames, nil
}

// LoadMCPServerWithFilter connects to an MCP server and only registers specified tools
// serverSpec format: "path/to/config.json#servername"
func (r *ToolRegistry) LoadMCPServerWithFilter(serverSpec string, allowedTools []string) error {
	jsonFile, serverName := ParseServerSpec(serverSpec)

	// Load config file
	configs, err := LoadMCPConfigFile(jsonFile)
	if err != nil {
		return err
	}

	config, namespace, err := selectMCPServer(configs, jsonFile, serverName)
	if err != nil {
		return err
	}
	// Create a set of allowed tools for quick lookup
	// Note: allowedTools contains namespaced names like "perp__perplexity_search_web"
	allowed := make(map[string]bool)
	for _, name := range allowedTools {
		// Strip namespace prefix if present (format: namespace__toolname)
		if _, bare, ok := strings.Cut(name, "__"); ok {
			name = bare
		}
		allowed[name] = true
	}

	records, toolNames, err := r.prepareMCPServerTools(&config, namespace, namespace, serverSpec, allowed)
	if err != nil {
		return err
	}

	// Register only allowed tools, replacing any earlier registration of the
	// same server so its client is not stranded.
	r.mu.Lock()
	defer r.mu.Unlock()

	r.dropServerToolsLocked(serverSpec)
	for _, record := range records {
		r.setToolLocked(record.name, record.tool, record.client)
		slog.Debug("mcp_tool_registered", "tool_name", record.name)
	}

	if len(toolNames) == 0 {
		slog.Debug("mcp_server_closed", "server_spec", serverSpec, "reason", "no_allowed_tools")
		return nil
	}

	// Track which tools came from this server
	r.serverTools[serverSpec] = toolNames

	return nil
}

// Close cleans up all resources
func (r *ToolRegistry) Close() error {
	owner := r.sandboxPolicyOwner()
	owner.sandboxConfigMu.Lock()
	defer owner.sandboxConfigMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()

	// Close all unique MCP clients
	closed := make(map[*MCPClient]bool)
	for _, clients := range []map[string]*MCPClient{r.toolClients, r.pendingToolClients} {
		for _, client := range clients {
			if !closed[client] {
				client.Close()
				closed[client] = true
			}
		}
	}

	// Clear maps
	r.tools = make(map[string]Tool)
	r.toolClients = make(map[string]*MCPClient)
	r.serverTools = make(map[string][]string)
	r.pendingTools = make(map[string]Tool)
	r.pendingToolClients = make(map[string]*MCPClient)
	r.pendingServerTools = make(map[string][]string)
	r.alwaysAllowedTools = make(map[string]bool)
	r.autoAllowedTools = make(map[string]bool)
	r.pendingAutoAllowed = make(map[string]bool)
	r.policyActive = false
	r.pendingPolicyActive = false
	r.allowedPatterns = nil
	r.pendingAllowedPatterns = nil
	// A closed derived registry serves nothing more; the parent is untouched.
	r.parent = nil
	r.viewAllowed = nil
	delete(owner.sandboxDependents, r)

	return nil
}

// extractNamespace extracts a namespace from a server spec
// e.g., "/path/to/filesystem.json" -> "filesystem"
// e.g., "/path/to/mcp.json#myserver" -> "myserver"
func extractNamespace(serverSpec string) string {
	jsonFile, serverName := ParseServerSpec(serverSpec)
	if serverName != "" {
		return serverName
	}
	base := filepath.Base(jsonFile)
	// Remove extension
	namespace := strings.TrimSuffix(base, filepath.Ext(base))
	return namespace
}
