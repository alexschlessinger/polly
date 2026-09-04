package tools

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/alexschlessinger/pollytool/schema"
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
	mu    sync.RWMutex
	tools map[string]Tool

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

	// parent makes this a derived registry (see Derive): a lookup that misses
	// the registry's own tools continues in the parent, whose tools and MCP
	// clients are shared rather than loaded again, and narrowed by
	// viewAllowed. Both are fixed at Derive and cleared by Close. A parent
	// never reaches into a derived registry, so a derived registry may call
	// its parent while holding its own lock.
	parent      *ToolRegistry
	viewAllowed func(name string) bool
}

type registryOptions struct {
	sandboxFactory        func(sandbox.Config) (sandbox.Sandbox, error)
	baseSandboxCfg        sandbox.Config
	baseSandboxPrepared   bool
	baseSandboxPrepareErr error
	unsafeNoSandbox       bool
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

// WithUnsafeNoSandbox explicitly permits process-backed tools to run without
// an OS sandbox. Without this option, registries that lack a sandbox factory
// reject bash, shell tools, and stdio MCP servers instead of silently running
// them with ambient host access.
func WithUnsafeNoSandbox() RegistryOption {
	return func(o *registryOptions) {
		o.unsafeNoSandbox = true
	}
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

// NewSandbox creates a sandbox with the base config merged with optional per-tool overrides.
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

func (r *ToolRegistry) constructSandbox(cfg sandbox.Config) (sandbox.Sandbox, error) {
	prepared, err := sandbox.PrepareConfig(cfg)
	if err != nil {
		return nil, err
	}
	return r.constructPreparedSandbox(prepared)
}

// preparedBaseSandboxConfig freezes the caller-approved base authority once
// for the lifetime of the registry. Backends are constructed lazily, so
// re-preparing the original path spellings for each tool would let an earlier
// sandbox retarget a symlink before a later tool is loaded.
func (r *ToolRegistry) preparedBaseSandboxConfig() (sandbox.Config, error) {
	r.sandboxConfigMu.Lock()
	defer r.sandboxConfigMu.Unlock()
	if !r.baseSandboxPrepared {
		r.baseSandboxCfg, r.baseSandboxPrepareErr = sandbox.PrepareConfig(r.baseSandboxCfg)
		r.baseSandboxPrepared = true
	}
	return r.baseSandboxCfg, r.baseSandboxPrepareErr
}

// SandboxReadPolicy returns the prepared base sandbox config when process
// sandboxing is active. active is false when no sandbox factory is configured,
// in which case in-process reads are unrestricted just like wrapped commands.
func (r *ToolRegistry) SandboxReadPolicy() (cfg sandbox.Config, active bool, err error) {
	if r.sandboxFactory == nil {
		return sandbox.Config{}, false, nil
	}
	cfg, err = r.preparedBaseSandboxConfig()
	return cfg, true, err
}

// SandboxWritePolicy returns the prepared base sandbox config when process
// sandboxing is active, for checking in-process writes via
// sandbox.WriteAllowed. active is false when no sandbox factory is
// configured, in which case in-process writes are unrestricted just like
// wrapped commands.
func (r *ToolRegistry) SandboxWritePolicy() (cfg sandbox.Config, active bool, err error) {
	return r.SandboxReadPolicy()
}

// newSandboxFor is NewSandbox with a tool/server identity for debug logging
// of the effective merged config (names and flags only, never env values).
func (r *ToolRegistry) newSandboxFor(name string, overlay *sandbox.Config) (sandbox.Sandbox, sandbox.Config, error) {
	if r.sandboxFactory == nil {
		return nil, sandbox.Config{}, fmt.Errorf("sandboxing not available")
	}
	cfg, err := r.preparedBaseSandboxConfig()
	if err != nil {
		return nil, sandbox.Config{}, fmt.Errorf("prepare base sandbox config: %w", err)
	}
	if overlay != nil {
		cfg = cfg.Merge(*overlay)
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
	return r.constructSandbox(cfg)
}

// newSchemaSandbox constructs a deliberately narrower policy for executable
// --schema discovery. Discovery never inherits workspace writes, network, read
// exceptions, environment allowances or passthroughs, or Unix-socket grants
// from the normal execution policy. It does retain deny rules so a policy
// cannot become weaker while metadata is being inspected.
func (r *ToolRegistry) newSchemaSandbox() (sandbox.Sandbox, error) {
	baseCfg, err := r.preparedBaseSandboxConfig()
	if err != nil {
		return nil, fmt.Errorf("prepare base sandbox config: %w", err)
	}
	cfg := sandbox.DefaultConfig()
	cfg.DenyPaths = append([]string(nil), baseCfg.DenyPaths...)
	cfg.DenyWritePaths = append([]string(nil), baseCfg.DenyWritePaths...)
	cfg.DenyWrite = baseCfg.DenyWrite
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
		unsafeNoSandbox:       o.unsafeNoSandbox,
	}

	registry.nativeTools["bash"] = func() (Tool, error) {
		bt := newBashTool("")
		bt.siblingLoaded = registry.hasVisibleTool
		if registry.sandboxFactory == nil {
			if err := registry.requireProcessSandbox("bash"); err != nil {
				return nil, err
			}
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
	registry.nativeTools["search_files"] = func() (Tool, error) {
		return NewSearchFilesTool(registry), nil
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

// AllowTools limits the parent tools a derived registry sees to those
// matching one of the patterns (filepath.Match globs, or exact names).
// Tools the derived registry registers itself are not affected. Repeated
// options accumulate.
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
		if matchesToolPattern(pattern, name) {
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
// derived registry's own always-allowed built-ins.
func (r *ToolRegistry) Derive(opts ...DeriveOption) *ToolRegistry {
	var o deriveOptions
	for _, opt := range opts {
		opt(&o)
	}
	baseCfg, prepareErr := r.preparedBaseSandboxConfig()
	derived := newRegistry(registryOptions{
		sandboxFactory:        r.sandboxFactory,
		baseSandboxCfg:        baseCfg.Merge(sandbox.Config{}),
		baseSandboxPrepared:   true,
		baseSandboxPrepareErr: prepareErr,
		unsafeNoSandbox:       r.unsafeNoSandbox,
	})
	derived.parent = r
	derived.viewAllowed = o.filter()
	return derived
}

// viewVisibleLocked reports whether the derived registry's allow-list lets
// name through; its own always-allowed tools pass regardless. Caller must
// hold r.mu.
func (r *ToolRegistry) viewVisibleLocked(name string) bool {
	return r.viewAllowed == nil || r.alwaysAllowedTools[name] || r.viewAllowed(name)
}

// inheritedLocked resolves name in the parent chain: the tool, whether it
// exists there, and whether the parent lets this registry use it. Caller
// must hold r.mu.
func (r *ToolRegistry) inheritedLocked(name string) (tool Tool, exists bool, allowed bool) {
	if r.parent == nil {
		return nil, false, false
	}
	return r.parent.GetIfAllowed(name)
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
func (r *ToolRegistry) registerSkillRuntimeTools(activate *SkillActivateTool, readFile *SkillReadFileTool, candidate *BashTool) bool {
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
		tool, exists, allowed = r.inheritedLocked(name)
		if !exists || !allowed {
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

// All returns all tools in the registry
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
		if matchesToolPattern(pattern, name) {
			return true
		}
	}
	return false
}

func matchesToolPattern(pattern, name string) bool {
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

func (r *ToolRegistry) stageTool(name string, tool Tool, client *MCPClient) {
	r.pendingTools[name] = tool
	if client != nil {
		r.pendingToolClients[name] = client
	}
}

func (r *ToolRegistry) stageServerTools(serverSpec string, toolNames []string) {
	if len(toolNames) == 0 {
		r.pendingServerTools[serverSpec] = nil
		return
	}
	r.pendingServerTools[serverSpec] = appendUniqueStrings(r.pendingServerTools[serverSpec], toolNames)
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

// UnloadMCPServer removes all tools from a server and closes it
func (r *ToolRegistry) UnloadMCPServer(serverSpec string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	toolNames, exists := r.serverTools[serverSpec]
	if !exists {
		return fmt.Errorf("MCP server not loaded: %s", GetMCPDisplayName(serverSpec))
	}

	// Get the client from first tool (all tools share same client)
	var client *MCPClient
	if len(toolNames) > 0 {
		client = r.toolClients[toolNames[0]]
	}

	// Remove all tools
	for _, name := range toolNames {
		delete(r.tools, name)
		delete(r.toolClients, name)
		slog.Debug("mcp_tool_removed", "tool_name", name)
	}

	// Close client
	if client != nil {
		client.Close()
		slog.Debug("mcp_server_closed", "server_name", GetMCPDisplayName(serverSpec))
	}

	// Clean up tracking
	delete(r.serverTools, serverSpec)

	return nil
}

func (r *ToolRegistry) loadShellToolWithNamespace(path, namespace string) (LoadResult, error) {
	records, result, err := r.prepareShellToolWithNamespace(path, namespace)
	if err != nil {
		return LoadResult{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, record := range records {
		r.setToolLocked(record.name, record.tool, nil)
		slog.Debug("shell_tool_registered", "tool_name", record.name)
	}

	return result, nil
}

func (r *ToolRegistry) prepareShellToolWithNamespace(path, namespace string) ([]stagedToolRecord, LoadResult, error) {
	if err := r.requireProcessSandbox("shell tool"); err != nil {
		return nil, LoadResult{}, err
	}
	// Create a schema-loading sandbox (base config: no network, temp-only writes)
	// so that --schema execution cannot perform side effects.
	var schemaSB sandbox.Sandbox
	if r.sandboxFactory != nil {
		var err error
		schemaSB, err = r.newSchemaSandbox()
		if err != nil {
			return nil, LoadResult{}, fmt.Errorf("schema sandbox for shell tool %s: %w", path, err)
		}
	}
	shellTool, err := newShellTool(path, schemaSB)
	if err != nil {
		return nil, LoadResult{}, fmt.Errorf("failed to load shell tool %s: %w", path, err)
	}

	if shellTool.SandboxOptOut() && !r.unsafeNoSandbox {
		return nil, LoadResult{}, fmt.Errorf("shell tool %s requested sandbox:false; refusing without WithUnsafeNoSandbox", path)
	}
	if r.sandboxFactory != nil && !shellTool.SandboxOptOut() {
		// Fail closed: a tool that should be sandboxed but can't be must not
		// load, or it would silently run unsandboxed.
		sb, cfg, err := r.newSandboxFor(path, shellTool.SandboxConfig())
		if err != nil {
			return nil, LoadResult{}, fmt.Errorf("sandbox for shell tool %s: %w", path, err)
		}
		shellTool = shellTool.withSandboxConfig(sb, cfg)
	}

	s := shellTool.GetSchema()
	if s == nil || s.Title() == "" {
		return nil, LoadResult{}, fmt.Errorf("shell tool %s has no name in schema", path)
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

	return []stagedToolRecord{record}, LoadResult{
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
		r.stageTool(record.name, record.tool, record.client)
		if record.serverSpec != "" {
			r.stageServerTools(record.serverSpec, []string{record.name})
		}
		slog.Debug("tool_staged", "tool_name", record.name)
	}
}

func (r *ToolRegistry) prepareSingleMCPServerWithNamespace(jsonFile, serverName, namespace string, config *MCPConfig) ([]stagedToolRecord, []string, error) {
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
	var sbCfg sandbox.Config
	var hasSandboxCfg bool
	if localProcess && r.sandboxFactory != nil && !config.SandboxOptOut() {
		overlayCfg, err := config.SandboxConfig()
		if err != nil {
			return nil, nil, fmt.Errorf("invalid sandbox config for MCP server %s: %w", serverName, err)
		}
		sb, sbCfg, err = r.newSandboxFor(serverName, overlayCfg)
		if err != nil {
			return nil, nil, fmt.Errorf("sandbox for MCP server %s: %w", serverName, err)
		}
		hasSandboxCfg = true
	}

	var client *MCPClient
	var err error
	if hasSandboxCfg {
		client, err = newMCPClientFromConfig(config, sb, sbCfg)
	} else {
		client, err = newMCPClientFromConfig(config, sb)
	}
	if err != nil {
		return nil, nil, err
	}

	serverSpec := fmt.Sprintf("%s#%s", jsonFile, serverName)
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
			var available []string
			for name := range configs {
				available = append(available, name)
			}
			return nil, LoadResult{}, fmt.Errorf("server %q not found in config (available: %v)", serverName, available)
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

// LoadShellTool loads a single shell tool from a file path with the default namespace.
func (r *ToolRegistry) LoadShellTool(path string) (LoadResult, error) {
	return r.loadShellToolWithNamespace(path, extractNamespace(path))
}

// LoadShellToolWithNamespace loads a single shell tool from a file path with an explicit namespace.
func (r *ToolRegistry) LoadShellToolWithNamespace(path, namespace string) (LoadResult, error) {
	if namespace == "" {
		namespace = extractNamespace(path)
	}
	return r.loadShellToolWithNamespace(path, namespace)
}

// LoadToolAuto attempts to load a tool, auto-detecting if it's native, shell tool, or MCP server
func (r *ToolRegistry) LoadToolAuto(pathOrServer string) (LoadResult, error) {
	// First check if it's a registered native tool
	r.mu.RLock()
	factory, isNative := r.nativeTools[pathOrServer]
	r.mu.RUnlock()

	if isNative {
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

	// Determine type based on extension and try appropriate loader
	isJSON := strings.HasSuffix(strings.ToLower(pathOrServer), ".json")

	if isJSON {
		// Try MCP for JSON files
		mcpResult, mcpErr := r.LoadMCPServer(pathOrServer)
		if mcpErr == nil {
			return mcpResult, nil
		}
		return LoadResult{}, mcpErr
	}

	// For non-JSON, try as shell tool
	// Check if executable
	if info.Mode()&0111 == 0 {
		return LoadResult{}, fmt.Errorf("%s is not executable (for shell tools, run: chmod +x %s)", pathOrServer, pathOrServer)
	}

	shellResult, shellErr := r.LoadShellTool(pathOrServer)
	if shellErr == nil {
		return shellResult, nil
	}
	return LoadResult{}, shellErr
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

// LoadMCPServerWithFilter connects to an MCP server and only registers specified tools
// serverSpec format: "path/to/config.json#servername"
func (r *ToolRegistry) LoadMCPServerWithFilter(serverSpec string, allowedTools []string) error {
	jsonFile, serverName := ParseServerSpec(serverSpec)

	// Load config file
	configs, err := LoadMCPConfigFile(jsonFile)
	if err != nil {
		return err
	}

	// Find the right config
	var config MCPConfig
	var namespace string

	if serverName != "" {
		cfg, ok := configs[serverName]
		if !ok {
			return fmt.Errorf("server %q not found in config", serverName)
		}
		config = cfg
		namespace = serverName
	} else if len(configs) == 1 {
		for name, cfg := range configs {
			config = cfg
			namespace = name
			break
		}
	} else {
		return fmt.Errorf("config has multiple servers, need specific server in spec")
	}
	localProcess := config.Transport == "" || config.Transport == "stdio"
	if localProcess {
		if err := r.requireProcessSandbox("stdio MCP server"); err != nil {
			return err
		}
		if config.SandboxOptOut() && !r.unsafeNoSandbox {
			return fmt.Errorf("MCP server %s requested sandbox:false; refusing without WithUnsafeNoSandbox", namespace)
		}
	}

	// Create sandbox unless user opted out
	var sb sandbox.Sandbox
	var sbCfg sandbox.Config
	var hasSandboxCfg bool
	if localProcess && r.sandboxFactory != nil && !config.SandboxOptOut() {
		overlayCfg, specErr := config.SandboxConfig()
		if specErr != nil {
			return fmt.Errorf("invalid sandbox config for MCP server %s: %w", namespace, specErr)
		}
		sb, sbCfg, err = r.newSandboxFor(namespace, overlayCfg)
		if err != nil {
			return fmt.Errorf("sandbox for MCP server %s: %w", namespace, err)
		}
		hasSandboxCfg = true
	}

	// Create client
	var client *MCPClient
	if hasSandboxCfg {
		client, err = newMCPClientFromConfig(&config, sb, sbCfg)
	} else {
		client, err = newMCPClientFromConfig(&config, sb)
	}
	if err != nil {
		return err
	}
	client.serverSpec = serverSpec

	// Get all tools from server
	tools, err := client.ListTools()
	if err != nil {
		client.Close()
		return err
	}

	// Create a set of allowed tools for quick lookup
	// Note: allowedTools contains namespaced names like "perp__perplexity_search_web"
	allowed := make(map[string]bool)
	for _, name := range allowedTools {
		// Strip namespace prefix if present (format: namespace__toolname)
		if idx := strings.Index(name, "__"); idx != -1 {
			bareToolName := name[idx+2:]
			allowed[bareToolName] = true
		} else {
			allowed[name] = true
		}
	}

	// Register only allowed tools, replacing any earlier registration of the
	// same server so its client is not stranded.
	r.mu.Lock()
	defer r.mu.Unlock()

	r.dropServerToolsLocked(serverSpec)

	var toolNames []string
	for _, tool := range tools {
		s := tool.GetSchema()
		if s != nil && s.Title() != "" {
			// Only register if this tool is in the allowed list
			if allowed[s.Title()] {
				// Create namespaced name
				namespacedName := fmt.Sprintf("%s__%s", namespace, s.Title())

				// Set source on the tool for persistence
				if mcpTool, ok := tool.(*MCPTool); ok {
					mcpTool.Source = serverSpec
				}

				// Wrap the tool to provide namespaced schema
				wrappedTool := &NamespacedTool{
					Tool:           tool,
					namespacedName: namespacedName,
				}
				r.setToolLocked(namespacedName, wrappedTool, client)
				toolNames = append(toolNames, namespacedName)
				slog.Debug("mcp_tool_registered", "tool_name", namespacedName)
			}
		}
	}

	if len(toolNames) == 0 {
		// Nothing refers to the client, so nothing would ever close it.
		client.Close()
		slog.Debug("mcp_server_closed", "server_name", GetMCPDisplayName(serverSpec), "reason", "no_allowed_tools")
		return nil
	}

	// Track which tools came from this server
	r.serverTools[serverSpec] = toolNames

	return nil
}

// GetLoadedMCPServers returns list of loaded server specs
func (r *ToolRegistry) GetLoadedMCPServers() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	servers := make([]string, 0, len(r.serverTools))
	for spec := range r.serverTools {
		servers = append(servers, spec)
	}
	if r.parent != nil {
		servers = appendUniqueStrings(servers, r.parent.GetLoadedMCPServers())
	}
	return servers
}

// Close cleans up all resources
func (r *ToolRegistry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Close all unique MCP clients
	closed := make(map[*MCPClient]bool)
	for _, client := range r.toolClients {
		if !closed[client] {
			client.Close()
			closed[client] = true
		}
	}
	for _, client := range r.pendingToolClients {
		if !closed[client] {
			client.Close()
			closed[client] = true
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
