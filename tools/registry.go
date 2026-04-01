package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"go.uber.org/zap"
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

// GetMeta forwards metadata for wrapped MetaTools.
func (n *NamespacedTool) GetMeta() map[string]string {
	if mt, ok := n.Tool.(MetaTool); ok {
		return mt.GetMeta()
	}
	return nil
}

// ToolRegistry manages available tools
type ToolRegistry struct {
	mu    sync.RWMutex
	tools map[string]Tool

	// Native tool factories
	nativeTools map[string]func() Tool // toolName -> factory

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
	sandboxFactory func(sandbox.Config) (sandbox.Sandbox, error)
	baseSandboxCfg sandbox.Config
}

type registryOptions struct {
	sandboxFactory func(sandbox.Config) (sandbox.Sandbox, error)
	baseSandboxCfg sandbox.Config
}

// RegistryOption configures a ToolRegistry.
type RegistryOption func(*registryOptions)

// WithSandboxFactory sets the sandbox factory and base config for the registry.
func WithSandboxFactory(factory func(sandbox.Config) (sandbox.Sandbox, error), baseCfg sandbox.Config) RegistryOption {
	return func(o *registryOptions) {
		o.sandboxFactory = factory
		o.baseSandboxCfg = baseCfg
	}
}

// HasSandbox reports whether sandboxing is available.
func (r *ToolRegistry) HasSandbox() bool {
	return r.sandboxFactory != nil
}

// NewSandbox creates a sandbox with the base config merged with optional per-tool overrides.
func (r *ToolRegistry) NewSandbox(overlay *sandbox.Config) (sandbox.Sandbox, error) {
	if r.sandboxFactory == nil {
		return nil, fmt.Errorf("sandboxing not available")
	}
	cfg := r.baseSandboxCfg
	if overlay != nil {
		cfg = cfg.Merge(*overlay)
	}
	return r.sandboxFactory(cfg)
}

// NewSandboxDirect creates a sandbox from an explicit config, ignoring the base config.
func (r *ToolRegistry) NewSandboxDirect(cfg sandbox.Config) (sandbox.Sandbox, error) {
	if r.sandboxFactory == nil {
		return nil, fmt.Errorf("sandboxing not available")
	}
	return r.sandboxFactory(cfg)
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

// NewToolRegistry creates a new tool registry from a list of tools
func NewToolRegistry(tools []Tool, opts ...RegistryOption) *ToolRegistry {
	var o registryOptions
	for _, opt := range opts {
		opt(&o)
	}

	registry := &ToolRegistry{
		tools:              make(map[string]Tool),
		nativeTools:        make(map[string]func() Tool),
		toolClients:        make(map[string]*MCPClient),
		serverTools:        make(map[string][]string),
		pendingTools:       make(map[string]Tool),
		pendingToolClients: make(map[string]*MCPClient),
		pendingServerTools: make(map[string][]string),
		alwaysAllowedTools: make(map[string]bool),
		autoAllowedTools:   make(map[string]bool),
		pendingAutoAllowed: make(map[string]bool),
		sandboxFactory:     o.sandboxFactory,
		baseSandboxCfg:     o.baseSandboxCfg,
	}

	registry.nativeTools["bash"] = func() Tool {
		bt := NewBashTool("")
		if registry.sandboxFactory != nil {
			if sb, err := registry.sandboxFactory(registry.baseSandboxCfg); err == nil {
				bt = bt.WithSandbox(sb)
			}
		}
		return bt
	}

	for _, tool := range tools {
		registry.Register(tool)
	}

	return registry
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
		zap.S().Debugw("tool_registered", "tool_name", name)
		r.tools[name] = tool
	}
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

	r.nativeTools[name] = factory
	zap.S().Debugw("native_factory_registered", "factory_name", name)
}

// Get retrieves a tool by name
func (r *ToolRegistry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tool, ok := r.tools[name]
	if !ok || !r.isToolAllowedLocked(name) {
		return nil, false
	}
	return tool, true
}

// GetIfAllowed retrieves a tool by name, returning existence and allowance in a single lock acquisition.
func (r *ToolRegistry) GetIfAllowed(name string) (tool Tool, exists bool, allowed bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tool, exists = r.tools[name]
	if !exists {
		return nil, false, false
	}
	allowed = r.isToolAllowedLocked(name)
	if !allowed {
		return nil, true, false
	}
	return tool, true, true
}

// GetMeta returns metadata for a tool if it implements MetaTool, otherwise nil.
func (r *ToolRegistry) GetMeta(name string) map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tool, ok := r.tools[name]
	if !ok {
		return nil
	}
	if mt, ok := tool.(MetaTool); ok {
		return mt.GetMeta()
	}
	return nil
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
	zap.S().Debugw("tool_removed", "tool_name", namespacedName)

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
		stillInUse := false
		for _, c := range r.toolClients {
			if c == client {
				stillInUse = true
				break
			}
		}
		if !stillInUse {
			zap.S().Debugw("mcp_client_closed", "reason", "no_remaining_tools")
			client.Close()
		}
	}
}

// All returns all tools in the registry
func (r *ToolRegistry) All() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tools := make([]Tool, 0, len(r.tools))
	for name, tool := range r.tools {
		if !r.isToolAllowedLocked(name) {
			continue
		}
		tools = append(tools, tool)
	}
	return tools
}

// GetSchemas returns all tool schemas
func (r *ToolRegistry) GetSchemas() []*schema.ToolSchema {
	r.mu.RLock()
	defer r.mu.RUnlock()

	schemas := make([]*schema.ToolSchema, 0, len(r.tools))
	for name, tool := range r.tools {
		if !r.isToolAllowedLocked(name) {
			continue
		}
		schemas = append(schemas, tool.GetSchema())
	}
	return schemas
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
		r.tools[name] = tool
	}
	for name, client := range r.pendingToolClients {
		r.toolClients[name] = client
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
		zap.S().Debugw("mcp_tool_removed", "tool_name", name)
	}

	// Close client
	if client != nil {
		client.Close()
		zap.S().Debugw("mcp_server_closed", "server_name", GetMCPDisplayName(serverSpec))
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
		r.tools[record.name] = record.tool
		zap.S().Debugw("shell_tool_registered", "tool_name", record.name)
	}

	return result, nil
}

func (r *ToolRegistry) prepareShellToolWithNamespace(path, namespace string) ([]stagedToolRecord, LoadResult, error) {
	// Create a schema-loading sandbox (base config: no network, temp-only writes)
	// so that --schema execution cannot perform side effects.
	var schemaSB sandbox.Sandbox
	if r.sandboxFactory != nil {
		schemaSB, _ = r.sandboxFactory(r.baseSandboxCfg)
	}
	shellTool, err := NewShellTool(path, schemaSB)
	if err != nil {
		return nil, LoadResult{}, fmt.Errorf("failed to load shell tool %s: %w", path, err)
	}

	if r.sandboxFactory != nil && !shellTool.SandboxOptOut() {
		sb, err := r.NewSandbox(shellTool.SandboxConfig())
		if err == nil {
			shellTool = shellTool.WithSandbox(sb)
		}
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
		zap.S().Debugw("tool_staged", "tool_name", record.name)
	}
}

func (r *ToolRegistry) prepareSingleMCPServerWithNamespace(jsonFile, serverName, namespace string, config *MCPConfig) ([]stagedToolRecord, []string, error) {
	var sb sandbox.Sandbox
	if r.sandboxFactory != nil && !config.SandboxOptOut() {
		overlayCfg, err := config.SandboxConfig()
		if err != nil {
			return nil, nil, fmt.Errorf("invalid sandbox config for MCP server %s: %w", serverName, err)
		}
		sb, err = r.NewSandbox(overlayCfg)
		if err != nil {
			return nil, nil, fmt.Errorf("sandbox for MCP server %s: %w", serverName, err)
		}
	}

	client, err := NewMCPClientFromConfig(config, sb)
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
		r.tools[record.name] = record.tool
		if record.client != nil {
			r.toolClients[record.name] = record.client
		}
		if record.serverSpec != "" {
			r.serverTools[record.serverSpec] = appendUniqueStrings(r.serverTools[record.serverSpec], []string{record.name})
		}
		zap.S().Debugw("mcp_tool_registered", "tool_name", record.name)
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
		tool := factory()
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

// LoadMCPServers batch loads multiple servers
func (r *ToolRegistry) LoadMCPServers(serverSpecs []string) error {
	for _, spec := range serverSpecs {
		if _, err := r.LoadMCPServer(spec); err != nil {
			return fmt.Errorf("failed to load MCP server %s: %w", spec, err)
		}
	}
	return nil
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

	// Create sandbox unless user opted out
	var sb sandbox.Sandbox
	if r.sandboxFactory != nil && !config.SandboxOptOut() {
		overlayCfg, specErr := config.SandboxConfig()
		if specErr != nil {
			return fmt.Errorf("invalid sandbox config for MCP server %s: %w", namespace, specErr)
		}
		sb, err = r.NewSandbox(overlayCfg)
		if err != nil {
			return fmt.Errorf("sandbox for MCP server %s: %w", namespace, err)
		}
	}

	// Create client
	client, err := NewMCPClientFromConfig(&config, sb)
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

	// Register only allowed tools
	r.mu.Lock()
	defer r.mu.Unlock()

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
				r.tools[namespacedName] = wrappedTool
				r.toolClients[namespacedName] = client
				toolNames = append(toolNames, namespacedName)
				zap.S().Debugw("mcp_tool_registered", "tool_name", namespacedName)
			}
		}
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
