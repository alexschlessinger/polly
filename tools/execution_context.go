package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"errors"
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
}

// ExecutionGrant is the authority a context receives beyond reading its root.
type ExecutionGrant struct {
	// ReadOnly denies writes to the root. With a Scratch the context may write
	// only there; without one it denies every write.
	ReadOnly     bool
	DeniedReads  []string
	DeniedWrites []string
	// Scratch is an existing directory outside the root, exported to the
	// context's processes as TMPDIR and the Go cache root. It is the only
	// writable path of a read-only context. A missing or nested scratch fails
	// closed.
	Scratch string
}

// scratchEnv points a context's processes at its scratch: temp files, Go's
// work directory and build cache land there, and module lookups fail fast
// instead of dialing, since the module cache is never writable in a context.
func scratchEnv(scratch string) map[string]string {
	return map[string]string{"TMPDIR": scratch, "TMP": scratch, "TEMP": scratch, "GOTMPDIR": scratch, "GOCACHE": filepath.Join(scratch, "go-build"), "GOPROXY": "off"}
}

// ContextTool explicitly binds a custom tool to a new execution context.
// Unknown tools are omitted rather than retaining authority over another cwd.
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

func (r *ToolRegistry) ResolvePath(path string) (string, error) {
	if r != nil && r.executionRoot != "" && !filepath.IsAbs(path) && !strings.HasPrefix(path, "~") {
		path = filepath.Join(r.executionRoot, path)
	}
	return resolveLocalPath(path)
}

// ExecutionPolicy narrows the parent's grants. Workspace-dependent write
// roots are replaced; inherited deny rules and network policy are retained.
// A read-only grant with a scratch writes only there; without one it keeps
// the all-writes-denied policy. An operator's denyWrite base still wins.
func (r *ToolRegistry) ExecutionPolicy(root string, grant ExecutionGrant) (ExecutionContext, error) {
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
	}
	base, err := r.preparedBaseSandboxConfig()
	if err != nil {
		return ExecutionContext{}, err
	}
	cfg := sandbox.DefaultConfig()
	cfg.AllowNetwork = base.AllowNetwork
	cfg.DenyDNS = base.DenyDNS
	cfg.AllowEnv = append([]string(nil), base.AllowEnv...)
	cfg.PassEnv = append([]string(nil), base.PassEnv...)
	cfg.DenyPaths = append(cfg.DenyPaths, base.DenyPaths...)
	cfg.DenyWritePaths = append(cfg.DenyWritePaths, base.DenyWritePaths...)
	cfg.DenyPaths = append(cfg.DenyPaths, grant.DeniedReads...)
	cfg.DenyWritePaths = append(cfg.DenyWritePaths, grant.DeniedWrites...)
	cfg.DenyWrite = base.DenyWrite
	cfg.DenyHostTemp = base.DenyHostTemp
	switch {
	case grant.ReadOnly && scratch != "":
		// Read-only research writes only in its scratch: the root is an
		// explicit read-only island and the shared host temp is withheld.
		cfg.WritablePaths = []string{scratch}
		cfg.DenyWritePaths = append(cfg.DenyWritePaths, abs)
		cfg.DenyHostTemp = true
	case grant.ReadOnly:
		cfg.WritablePaths = []string{abs}
		cfg.DenyWrite = true
	default:
		cfg.WritablePaths = []string{abs}
		if scratch != "" {
			cfg.WritablePaths = append(cfg.WritablePaths, scratch)
		}
	}
	if scratch != "" && !cfg.DenyWrite {
		cfg.Env = scratchEnv(scratch)
	}
	if grant.ReadOnly || cfg.DenyWrite {
		// Keep the checkout visible inside Linux private temp; not a write grant.
		cfg, err = sandbox.ExposeReadOnlyPaths(cfg, abs)
		if err != nil {
			return ExecutionContext{}, err
		}
	}
	return ExecutionContext{Root: abs, ReadOnly: grant.ReadOnly || cfg.DenyWrite, Scratch: scratch, Sandbox: cfg}, nil
}

// BindExecutionContext owns fresh native tools and local MCP servers. It
// never derives a live view of another member's registry. Required tool
// patterns fail launch when no compatible tool can satisfy them.
func (r *ToolRegistry) BindExecutionContext(ec ExecutionContext, allow []string) (*ToolRegistry, []string, error) {
	if ec.Root == "" || !filepath.IsAbs(ec.Root) {
		return nil, nil, fmt.Errorf("execution root must be absolute")
	}
	opts := []RegistryOption{}
	if r.sandboxFactory != nil {
		opts = append(opts, WithSandboxFactory(r.sandboxFactory, ec.Sandbox))
	}
	if r.unsafeNoSandbox {
		opts = append(opts, WithUnsafeNoSandbox())
	}
	bound := NewToolRegistry(nil, opts...)
	bound.executionRoot = ec.Root
	bound.executionSourceRoot = ec.SourceRoot
	prepared, err := sandbox.PrepareConfig(ec.Sandbox)
	if err != nil {
		bound.Close()
		return nil, nil, err
	}
	bound.executionPolicy = &prepared
	omitted := []string{}
	visible := map[string]bool{}
	for _, tool := range r.All() {
		_, exists, allowed := r.GetIfAllowed(tool.GetName())
		visible[tool.GetName()] = exists && allowed
	}
	loadedMCP := map[string]bool{}
	var skillRuntime *SkillRuntime
	for _, original := range r.All() {
		name := original.GetName()
		if !visible[name] {
			continue
		}
		if allow != nil && !matchesAnyToolPattern(allow, name) {
			continue
		}
		if name == "spawn_agent" || name == "zvec_grep_search" || strings.HasPrefix(name, "swarm_") || strings.HasPrefix(name, "workflow_") {
			omitted = append(omitted, name)
			continue
		}
		var tool Tool
		var err error
		bare := unwrapTool(original)
		switch t := bare.(type) {
		case *SkillActivateTool, *SkillReadFileTool:
			var catalog *skills.Catalog
			if activation, ok := t.(*SkillActivateTool); ok {
				catalog = activation.catalog
			} else {
				catalog = t.(*SkillReadFileTool).catalog
			}
			if skillRuntime == nil {
				catalog, err = rebindSkillCatalog(catalog, bound, ec)
				if err == nil {
					skillRuntime, err = NewSkillRuntime(catalog, bound)
				}
				if err == nil {
					bound.executionSkills = catalog
				}
			}
			if err == nil {
				tool, _ = bound.Get(name)
			}
		case *BashTool, *readFileTool, *writeFileTool, *editFileTool, *listDirTool:
			if factory, ok := bound.nativeTools[bare.GetName()]; ok {
				tool, err = factory()
			}
		case *viewImageTool:
			tool = NewViewImageTool(bound)
		case *ShellTool:
			cfg := ec.Sandbox
			if overlay := t.SandboxConfig(); overlay != nil {
				cfg = cfg.Merge(restrictiveSandboxConfig(*overlay))
			}
			if cfg.DenyWrite {
				// A tool-local denial can turn an editing context read-only.
				// Keep its selected checkout visible inside Linux private temp.
				cfg, err = sandbox.ExposeReadOnlyPaths(cfg, ec.Root)
				if err != nil {
					break
				}
			}
			var sb sandbox.Sandbox
			if bound.HasSandbox() {
				sb, err = bound.NewSandboxDirect(cfg)
			} else {
				err = bound.requireProcessSandbox("shell tool")
			}
			if err == nil {
				clone := t.withSandboxConfig(sb, cfg)
				clone.workDir = ec.Root
				if filepath.IsAbs(clone.Command) && ec.SourceRoot != "" {
					if rel, e := filepath.Rel(ec.SourceRoot, clone.Command); e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
						clone.Command = filepath.Join(ec.Root, rel)
					}
				}
				if e := checkReadPolicy(bound, clone.Command); e != nil {
					err = e
					break
				}
				tool = clone
			}
		case *MCPTool:
			if !loadedMCP[t.Source] {
				loadedMCP[t.Source] = true
				_, err = bound.LoadMCPServer(rebindSourcePath(t.Source, ec.SourceRoot, ec.Root))
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
	}
	if allow != nil {
		for _, pattern := range allow {
			found := false
			for _, builtin := range ec.BuiltinTools {
				if MatchesToolPattern(pattern, builtin) {
					found = true
				}
			}
			for _, t := range bound.All() {
				if MatchesToolPattern(pattern, t.GetName()) {
					found = true
					break
				}
			}
			if !found {
				bound.Close()
				return nil, omitted, fmt.Errorf("required tool %q cannot honor execution context", pattern)
			}
		}
		// A relaunched MCP server may advertise more tools than requested.
		for _, t := range bound.All() {
			if !matchesAnyToolPattern(allow, t.GetName()) {
				delete(bound.tools, t.GetName())
			}
		}
	}
	for _, tool := range bound.All() {
		if !visible[tool.GetName()] {
			delete(bound.tools, tool.GetName())
		}
	}
	// This filter also bounds later skill activation and private built-ins.
	bound.viewAllowed = func(name string) bool {
		if name == "spawn_agent" || strings.HasPrefix(name, "workflow_") || name == "zvec_grep_search" {
			return false
		}
		return visible[name] && (allow == nil || matchesAnyToolPattern(allow, name)) || strings.HasPrefix(name, "swarm_") || name == "list_agents" || name == "send_message" || name == "read_messages"
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
	var paths []string
	for _, skill := range catalog.List() {
		path := skill.RootDir
		if rel, err := filepath.Rel(parentRoot, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			path = filepath.Join(ec.Root, rel)
		}
		if err := checkReadPolicy(registry, filepath.Join(path, "SKILL.md")); err != nil {
			return nil, err
		}
		if _, err := os.Stat(filepath.Join(path, "SKILL.md")); err != nil {
			return nil, fmt.Errorf("skill unavailable in execution context: %s: %w", skill.Name, err)
		}
		paths = append(paths, path)
	}
	return skills.Discover(paths)
}
