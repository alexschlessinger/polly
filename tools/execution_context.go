package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	Sandbox      sandbox.Config `json:"-"`
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
func (r *ToolRegistry) ExecutionPolicy(root string, readOnly bool, deniedReads, deniedWrites []string) (ExecutionContext, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return ExecutionContext{}, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return ExecutionContext{}, err
	}
	base, err := r.preparedBaseSandboxConfig()
	if err != nil {
		return ExecutionContext{}, err
	}
	cfg := sandbox.DefaultConfig()
	cfg.AllowNetwork = base.AllowNetwork
	cfg.AllowEnv = append([]string(nil), base.AllowEnv...)
	cfg.PassEnv = append([]string(nil), base.PassEnv...)
	cfg.DenyPaths = append(cfg.DenyPaths, base.DenyPaths...)
	cfg.DenyWritePaths = append(cfg.DenyWritePaths, base.DenyWritePaths...)
	cfg.DenyPaths = append(cfg.DenyPaths, deniedReads...)
	cfg.DenyWritePaths = append(cfg.DenyWritePaths, deniedWrites...)
	cfg.WritablePaths = []string{abs}
	cfg.DenyWrite = readOnly || base.DenyWrite
	return ExecutionContext{Root: abs, ReadOnly: cfg.DenyWrite, Sandbox: cfg}, nil
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
			var sb sandbox.Sandbox
			if bound.HasSandbox() {
				sb, err = bound.NewSandboxDirect(ec.Sandbox)
			} else {
				err = bound.requireProcessSandbox("shell tool")
			}
			if err == nil {
				clone := t.withSandboxConfig(sb, ec.Sandbox)
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
	rel, err := filepath.Rel(source, path)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
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
