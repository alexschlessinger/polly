package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/alexschlessinger/pollytool/skills"
	"github.com/google/jsonschema-go/jsonschema"
)

// SkillActivateTool loads a skill's instructions and registers any executable scripts.
type SkillActivateTool struct {
	catalog   *skills.Catalog
	registry  *ToolRegistry
	mu        sync.Mutex
	activated map[string]bool
}

// NewSkillActivateTool creates the skill activation tool.
func NewSkillActivateTool(catalog *skills.Catalog, registry *ToolRegistry) *SkillActivateTool {
	return &SkillActivateTool{
		catalog:   catalog,
		registry:  registry,
		activated: make(map[string]bool),
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

func (t *SkillActivateTool) GetSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Title:       "activate_skill",
		Description: "Load a skill's instructions and list any bundled scripts for this run.",
		Type:        "object",
		Properties: map[string]*jsonschema.Schema{
			"name": {
				Type:        "string",
				Description: "The skill name from the available_skills list.",
			},
		},
		Required: []string{"name"},
	}
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
	t.mu.Unlock()

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
		t.mu.Unlock()
	}
	sort.Strings(loadedTools)
	sort.Strings(loadedMCPServers)

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

	if len(allowedPatterns) > 0 {
		b.WriteString("Allowed tool patterns now active for future turns:\n")
		for _, pattern := range allowedPatterns {
			b.WriteString("- ")
			b.WriteString(pattern)
			b.WriteString("\n")
		}
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

func parseAllowedToolPatterns(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})
}

// SkillReadFileTool returns the contents of a file inside a discovered skill.
type SkillReadFileTool struct {
	catalog *skills.Catalog
}

// NewSkillReadFileTool creates the skill file reader tool.
func NewSkillReadFileTool(catalog *skills.Catalog) *SkillReadFileTool {
	return &SkillReadFileTool{catalog: catalog}
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

func (t *SkillReadFileTool) GetSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Title:       "read_skill_file",
		Description: "Read a file relative to a discovered skill root.",
		Type:        "object",
		Properties: map[string]*jsonschema.Schema{
			"skill": {
				Type:        "string",
				Description: "The skill name.",
			},
			"path": {
				Type:        "string",
				Description: "The relative file path within the skill directory.",
			},
		},
		Required: []string{"skill", "path"},
	}
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

	content, err := skill.ReadFile(relPath)
	if err != nil {
		return "", err
	}

	cleanRel := filepath.ToSlash(strings.TrimSpace(relPath))
	return fmt.Sprintf("File: %s\n%s", cleanRel, content), nil
}
