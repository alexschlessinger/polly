package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// SkillActivateTool loads a skill's instructions and registers any executable scripts.
type SkillActivateTool struct {
	catalog   *skills.Catalog
	registry  *ToolRegistry
	mu        sync.Mutex
	activated map[string]bool
	// skillTools records the tools each activation loaded, so a derived
	// runtime can tell whether its registry's allow-list still shows them.
	skillTools map[string][]string
	// unavailable names the skills a derived runtime may not activate: the
	// parent loaded their tools, and this registry's allow-list hides the
	// ones listed. Activating one is refused rather than delivering
	// instructions for tools the model cannot call.
	unavailable map[string][]string
}

// NewSkillActivateTool creates the skill activation tool.
func NewSkillActivateTool(catalog *skills.Catalog, registry *ToolRegistry) *SkillActivateTool {
	return &SkillActivateTool{
		catalog:     catalog,
		registry:    registry,
		activated:   make(map[string]bool),
		skillTools:  make(map[string][]string),
		unavailable: make(map[string][]string),
	}
}

func (t *SkillActivateTool) GetName() string {
	return "activate_skill"
}

func (t *SkillActivateTool) GetType() string {
	return "native"
}

func (t *SkillActivateTool) GetSource() string {
	return "builtin"
}

func (t *SkillActivateTool) GetSchema() *schema.ToolSchema {
	return schema.Tool("activate_skill", "Load a skill's instructions and list any bundled scripts for this run.",
		schema.Params{"name": schema.S("The skill name from the available_skills list.")},
		"name",
	)
}

func (t *SkillActivateTool) bashWritablePaths() []string {
	if t.registry == nil {
		return nil
	}
	bash, ok := t.registry.registeredTool("bash")
	if !ok {
		return nil
	}
	info := SandboxDetails(bash)
	if !info.Active || info.Config == nil {
		return nil
	}
	return append([]string(nil), info.Config.WritablePaths...)
}

func (t *SkillActivateTool) activate(name string) (string, error) {
	skillName := strings.TrimSpace(name)
	if skillName == "" {
		return "", fmt.Errorf("name must be a non-empty string")
	}

	skill, ok := t.catalog.Get(skillName)
	if !ok {
		return "", fmt.Errorf("skill %q not found", name)
	}

	t.mu.Lock()
	alreadyActivated := t.activated[skill.Name]
	withheld := t.unavailable[skill.Name]
	t.mu.Unlock()
	if len(withheld) > 0 {
		return "", fmt.Errorf("skill %q is unavailable to this agent: its tools %s are excluded by the agent's tool allow list", skill.Name, strings.Join(withheld, ", "))
	}

	// Always list scripts (just file paths, no side effects)
	scriptFiles, err := skill.ListFiles("scripts")
	if err != nil {
		return "", err
	}
	var scriptPaths []string
	for _, rel := range scriptFiles {
		scriptPath, err := skill.ResolvePath(rel)
		if err != nil {
			return "", err
		}
		scriptPaths = append(scriptPaths, scriptPath)
	}

	var loadedTools []string
	var loadedMCPServers []string
	allowedPatterns := parseAllowedToolPatterns(skill.AllowedTools)
	if !alreadyActivated {
		var records []stagedToolRecord

		mcpFiles, err := skill.ListFiles("mcp")
		if err != nil {
			return "", err
		}
		for _, rel := range mcpFiles {
			if strings.ToLower(filepath.Ext(rel)) != ".json" {
				continue
			}
			configPath, err := skill.ResolvePath(rel)
			if err != nil {
				return "", err
			}

			prepared, result, err := t.registry.prepareMCPServerWithNamespacePrefix(configPath, skill.Name)
			if err != nil {
				closeStagedToolRecords(records)
				return "", fmt.Errorf("load skill MCP config %s: %w", rel, err)
			}
			records = append(records, prepared...)
			for _, server := range result.Servers {
				loadedMCPServers = append(loadedMCPServers, server.Name)
				loadedTools = append(loadedTools, server.ToolNames...)
			}
		}

		if len(records) > 0 {
			t.registry.stagePreparedTools(records)
		}
		t.registry.stageSkillAllowance(allowedPatterns, loadedTools)

		t.mu.Lock()
		t.activated[skill.Name] = true
		t.skillTools[skill.Name] = append([]string(nil), loadedTools...)
		t.mu.Unlock()
	}
	sort.Strings(loadedTools)
	sort.Strings(loadedMCPServers)
	// A derived registry's allow-list can hide tools the skill just loaded;
	// they are registered but never callable, so say so instead of listing
	// them as loaded.
	hiddenTools := t.registry.hiddenByView(loadedTools)
	loadedTools = withoutStrings(loadedTools, hiddenTools)

	allFiles, err := skill.ListFiles(".")
	if err != nil {
		return "", err
	}
	var skillFiles []string
	for _, f := range allFiles {
		base := filepath.Base(f)
		if strings.EqualFold(base, "SKILL.md") || strings.EqualFold(base, "LICENSE.txt") {
			continue
		}
		if strings.HasPrefix(f, "scripts/") {
			continue
		}
		if strings.HasPrefix(f, "mcp/") && strings.ToLower(filepath.Ext(f)) == ".json" {
			continue
		}
		skillFiles = append(skillFiles, f)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Skill activated: %s\n", skill.Name)
	fmt.Fprintf(&b, "Description: %s\n", skill.Description)
	if skill.Compatibility != "" {
		fmt.Fprintf(&b, "Compatibility: %s\n", skill.Compatibility)
	}
	if skill.AllowedTools != "" {
		fmt.Fprintf(&b, "Allowed tools: %s\n", skill.AllowedTools)
	}
	b.WriteString("Instructions:\n")
	b.WriteString(skill.Instructions)
	b.WriteString("\n")

	if len(scriptPaths) > 0 {
		b.WriteString("Available scripts (run via the bash tool):\n")
		for _, p := range scriptPaths {
			b.WriteString("- ")
			b.WriteString(p)
			b.WriteString("\n")
		}
	}

	if len(skillFiles) > 0 {
		b.WriteString("Skill files (use read_skill_file to read):\n")
		for _, f := range skillFiles {
			b.WriteString("- ")
			b.WriteString(f)
			b.WriteString("\n")
		}
	}

	if len(loadedMCPServers) > 0 {
		b.WriteString("Loaded MCP servers:\n")
		for _, server := range loadedMCPServers {
			b.WriteString("- ")
			b.WriteString(server)
			b.WriteString("\n")
		}
	}

	if len(loadedTools) > 0 {
		b.WriteString("Loaded tools:\n")
		for _, toolName := range loadedTools {
			b.WriteString("- ")
			b.WriteString(toolName)
			b.WriteString("\n")
		}
	} else if alreadyActivated {
		b.WriteString("(skill already active for this run)\n")
	}
	if len(hiddenTools) > 0 {
		b.WriteString("Tools excluded by this agent's tool allow list (not callable):\n")
		for _, toolName := range hiddenTools {
			b.WriteString("- ")
			b.WriteString(toolName)
			b.WriteString("\n")
		}
	}

	if len(allowedPatterns) > 0 {
		b.WriteString("Allowed tool patterns now active for future turns:\n")
		for _, pattern := range allowedPatterns {
			b.WriteString("- ")
			b.WriteString(pattern)
			b.WriteString("\n")
		}
	}

	if writablePaths := t.bashWritablePaths(); len(writablePaths) > 0 {
		b.WriteString("Sandbox: bash commands can only write to: ")
		b.WriteString(strings.Join(writablePaths, ", "))
		b.WriteString("\n")
	}

	return strings.TrimSpace(b.String()), nil
}

func (t *SkillActivateTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	_ = ctx

	name, ok := args["name"].(string)
	if !ok || strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("name must be a non-empty string")
	}

	return t.activate(name)
}

// ActivatedSkills returns the activated skill names in stable order.
func (t *SkillActivateTool) ActivatedSkills() []string {
	t.mu.Lock()
	defer t.mu.Unlock()

	skills := make([]string, 0, len(t.activated))
	for name := range t.activated {
		skills = append(skills, name)
	}
	sort.Strings(skills)
	return skills
}

// toolsLoadedBy returns the tools the named skill's activation loaded here.
func (t *SkillActivateTool) toolsLoadedBy(skill string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.skillTools[skill]...)
}

// withoutStrings returns names without the entries in drop, keeping order.
func withoutStrings(names, drop []string) []string {
	if len(drop) == 0 {
		return names
	}
	dropped := make(map[string]bool, len(drop))
	for _, name := range drop {
		dropped[name] = true
	}
	kept := names[:0:0]
	for _, name := range names {
		if !dropped[name] {
			kept = append(kept, name)
		}
	}
	return kept
}

func parseAllowedToolPatterns(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})
}

// SkillReadFileTool returns the contents of a file inside a discovered skill.
// Reads honor the registry's base sandbox read policy so the tool cannot see
// what a sandboxed command could not.
type SkillReadFileTool struct {
	catalog  *skills.Catalog
	registry *ToolRegistry
}

// NewSkillReadFileTool creates the skill file reader tool bound to registry's
// sandbox policy. A nil registry applies no policy.
func NewSkillReadFileTool(catalog *skills.Catalog, registry *ToolRegistry) *SkillReadFileTool {
	return &SkillReadFileTool{catalog: catalog, registry: registry}
}

func (t *SkillReadFileTool) GetName() string {
	return "read_skill_file"
}

func (t *SkillReadFileTool) GetType() string {
	return "native"
}

func (t *SkillReadFileTool) GetSource() string {
	return "builtin"
}

func (t *SkillReadFileTool) GetSchema() *schema.ToolSchema {
	return schema.Tool("read_skill_file", "Read a file relative to a discovered skill root.",
		schema.Params{
			"skill": schema.S("The skill name."),
			"path":  schema.S("The relative file path within the skill directory."),
		},
		"skill", "path",
	)
}

// readPolicy applies the registry's sandbox read policy to the canonical path
// a skill read is about to open.
func (t *SkillReadFileTool) readPolicy(canonical string) error {
	if t.registry == nil {
		return nil
	}
	cfg, active, err := t.registry.SandboxReadPolicy()
	if err != nil {
		return fmt.Errorf("resolve sandbox policy: %w", err)
	}
	if !active {
		return nil
	}
	return sandbox.ReadAllowed(cfg, canonical)
}

func (t *SkillReadFileTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	_ = ctx

	skillName, ok := args["skill"].(string)
	if !ok || strings.TrimSpace(skillName) == "" {
		return "", fmt.Errorf("skill must be a non-empty string")
	}
	relPath, ok := args["path"].(string)
	if !ok || strings.TrimSpace(relPath) == "" {
		return "", fmt.Errorf("path must be a non-empty string")
	}

	skill, ok := t.catalog.Get(strings.TrimSpace(skillName))
	if !ok {
		return "", fmt.Errorf("skill %q not found", skillName)
	}

	content, err := skill.ReadFileChecked(relPath, t.readPolicy)
	if err != nil {
		return "", err
	}

	cleanRel := filepath.ToSlash(strings.TrimSpace(relPath))
	return fmt.Sprintf("File: %s\n%s", cleanRel, content), nil
}
