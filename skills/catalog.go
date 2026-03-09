package skills

import (
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Message is a minimal chat message used by BuildMessages to avoid a circular
// dependency on the messages package. It is layout-compatible with
// messages.ChatMessage (Role and Content are the first two fields).
type Message struct {
	Role    string
	Content string
}

const skillFileName = "SKILL.md"
const maxSkillFileSize = 1 << 20

var skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

type frontmatter struct {
	Name          string         `yaml:"name"`
	Description   string         `yaml:"description"`
	License       string         `yaml:"license"`
	Compatibility string         `yaml:"compatibility"`
	AllowedTools  string         `yaml:"allowed-tools"`
	Metadata      map[string]any `yaml:"metadata"`
}

// Skill contains the parsed metadata and instructions for a discovered skill.
type Skill struct {
	Name          string
	Description   string
	License       string
	Compatibility string
	AllowedTools  string
	Metadata      map[string]any
	RootDir       string
	SkillFile     string
	Instructions  string
}

// Catalog stores the discovered skill set.
type Catalog struct {
	ordered []*Skill
	byName  map[string]*Skill
}

// Discover loads skills from either skill directories or container directories.
func Discover(paths []string) (*Catalog, error) {
	catalog := &Catalog{
		byName: make(map[string]*Skill),
	}

	for _, path := range paths {
		discovered, err := discoverPath(path)
		if err != nil {
			return nil, err
		}
		for _, skill := range discovered {
			if existing, ok := catalog.byName[skill.Name]; ok {
				return nil, fmt.Errorf("duplicate skill %q found at %s and %s", skill.Name, existing.RootDir, skill.RootDir)
			}
			catalog.byName[skill.Name] = skill
			catalog.ordered = append(catalog.ordered, skill)
		}
	}

	sort.Slice(catalog.ordered, func(i, j int) bool {
		return catalog.ordered[i].Name < catalog.ordered[j].Name
	})

	return catalog, nil
}

func discoverPath(path string) ([]*Skill, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("skill path %s: %w", path, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("skill path %s is not a directory", path)
	}

	if skill, ok, err := loadSkill(path); err != nil {
		return nil, err
	} else if ok {
		return []*Skill{skill}, nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("read skill directory %s: %w", path, err)
	}

	var discovered []*Skill
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		childPath := filepath.Join(path, entry.Name())
		skill, ok, err := loadSkill(childPath)
		if err != nil {
			return nil, err
		}
		if ok {
			discovered = append(discovered, skill)
		}
	}

	return discovered, nil
}

func loadSkill(root string) (*Skill, bool, error) {
	skillPath := filepath.Join(root, skillFileName)
	data, err := os.ReadFile(skillPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", skillPath, err)
	}

	meta, body, err := parseSkillMarkdown(string(data))
	if err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", skillPath, err)
	}

	root = filepath.Clean(root)
	if err := validateFrontmatter(meta, filepath.Base(root)); err != nil {
		return nil, false, fmt.Errorf("validate %s: %w", skillPath, err)
	}

	// Replace {baseDir} token used by OpenClaw-style skills.
	instructions := strings.TrimSpace(body)
	instructions = strings.ReplaceAll(instructions, "{baseDir}", root)

	skill := &Skill{
		Name:          meta.Name,
		Description:   meta.Description,
		License:       meta.License,
		Compatibility: meta.Compatibility,
		AllowedTools:  meta.AllowedTools,
		Metadata:      meta.Metadata,
		RootDir:       root,
		SkillFile:     skillPath,
		Instructions:  instructions,
	}

	if !checkMetadataGating(skill.Metadata) {
		return nil, false, nil
	}

	return skill, true, nil
}

// checkMetadataGating evaluates platform and binary requirements embedded in
// metadata (openclaw or clawdbot namespace). Returns true if the skill is
// eligible to run on this machine.
func checkMetadataGating(metadata map[string]any) bool {
	if len(metadata) == 0 {
		return true
	}

	// Try both known namespace keys.
	var gating map[string]any
	for _, key := range []string{"openclaw", "clawdbot"} {
		if v, ok := metadata[key]; ok {
			if m, ok := v.(map[string]any); ok {
				gating = m
				break
			}
		}
	}
	if gating == nil {
		return true
	}

	// "always: true" skips all checks.
	if v, ok := gating["always"].(bool); ok && v {
		return true
	}

	// OS filter.
	if osList, ok := gating["os"].([]any); ok && len(osList) > 0 {
		goOS := runtime.GOOS
		matched := false
		for _, entry := range osList {
			if s, ok := entry.(string); ok && normalizeOS(s) == goOS {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	// Binary requirements.
	if reqs, ok := gating["requires"].(map[string]any); ok {
		// All listed bins must exist.
		if bins, ok := reqs["bins"].([]any); ok {
			for _, b := range bins {
				if name, ok := b.(string); ok {
					if _, err := exec.LookPath(name); err != nil {
						return false
					}
				}
			}
		}
		// At least one of anyBins must exist.
		if anyBins, ok := reqs["anyBins"].([]any); ok && len(anyBins) > 0 {
			found := false
			for _, b := range anyBins {
				if name, ok := b.(string); ok {
					if _, err := exec.LookPath(name); err == nil {
						found = true
						break
					}
				}
			}
			if !found {
				return false
			}
		}
	}

	return true
}

// normalizeOS maps OpenClaw platform names to Go's runtime.GOOS values.
func normalizeOS(s string) string {
	switch strings.ToLower(s) {
	case "win32", "windows":
		return "windows"
	default:
		return strings.ToLower(s)
	}
}

func parseSkillMarkdown(content string) (*frontmatter, string, error) {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return nil, "", fmt.Errorf("missing YAML frontmatter")
	}

	rest := strings.TrimPrefix(normalized, "---\n")
	end := strings.Index(rest, "\n---\n")
	if end == -1 {
		return nil, "", fmt.Errorf("missing frontmatter terminator")
	}

	var meta frontmatter
	if err := yaml.Unmarshal([]byte(rest[:end]), &meta); err != nil {
		return nil, "", fmt.Errorf("invalid YAML frontmatter: %w", err)
	}

	body := rest[end+len("\n---\n"):]
	return &meta, body, nil
}

func validateFrontmatter(meta *frontmatter, dirName string) error {
	name := strings.TrimSpace(meta.Name)
	switch {
	case name == "":
		return fmt.Errorf("name is required")
	case len(name) > 64:
		return fmt.Errorf("name exceeds 64 characters")
	case !skillNamePattern.MatchString(name):
		return fmt.Errorf("name must contain only lowercase letters, numbers, and hyphens without consecutive hyphens")
	case name != dirName:
		return fmt.Errorf("name %q must match directory %q", name, dirName)
	}

	description := strings.TrimSpace(meta.Description)
	switch {
	case description == "":
		return fmt.Errorf("description is required")
	case len(description) > 1024:
		return fmt.Errorf("description exceeds 1024 characters")
	}

	if meta.Compatibility != "" && len(meta.Compatibility) > 500 {
		return fmt.Errorf("compatibility exceeds 500 characters")
	}

	return nil
}

// IsEmpty reports whether the catalog has any discovered skills.
func (c *Catalog) IsEmpty() bool {
	return c == nil || len(c.ordered) == 0
}

// List returns the discovered skills in a stable order.
func (c *Catalog) List() []*Skill {
	if c == nil {
		return nil
	}
	out := make([]*Skill, len(c.ordered))
	copy(out, c.ordered)
	return out
}

// Get finds a skill by name.
func (c *Catalog) Get(name string) (*Skill, bool) {
	if c == nil {
		return nil, false
	}
	skill, ok := c.byName[name]
	return skill, ok
}

// PromptXML returns the startup skill metadata block recommended by the spec.
func (c *Catalog) PromptXML() string {
	if c == nil || len(c.ordered) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("<available_skills>\n")
	for _, skill := range c.ordered {
		b.WriteString("  <skill>\n")
		b.WriteString("    <name>")
		b.WriteString(html.EscapeString(skill.Name))
		b.WriteString("</name>\n")
		b.WriteString("    <description>")
		b.WriteString(html.EscapeString(skill.Description))
		b.WriteString("</description>\n")
		b.WriteString("  </skill>\n")
	}
	b.WriteString("</available_skills>")
	return b.String()
}

// RuntimeSystemPrompt returns the standard system prompt augmentation for discovered skills.
func (c *Catalog) RuntimeSystemPrompt(baseSystemPrompt string) string {
	baseSystemPrompt = strings.TrimSpace(baseSystemPrompt)
	if c == nil || len(c.ordered) == 0 {
		return baseSystemPrompt
	}

	var sections []string
	if baseSystemPrompt != "" {
		sections = append(sections, baseSystemPrompt)
	}

	sections = append(sections, strings.TrimSpace(`
Agent Skills are available in this environment.
Do not assume a skill's instructions until you call the activate_skill tool.
Use read_skill_file to inspect files referenced by an activated skill.
If activation loads helper scripts or MCP servers, those tools will become available on the next turn.
If a skill declares allowed-tools, that allowlist is enforced on future turns after activation.
Allowed-tools policies are additive across skill activations; a later activation can widen tool access, but it does not revoke previously allowed tools.
`))
	sections = append(sections, c.PromptXML())

	return strings.Join(sections, "\n\n")
}

// BuildMessages returns a copy of msgs with the skill runtime system prompt
// injected. If the catalog is nil or empty, a plain copy of msgs is returned.
// The Message type is intentionally minimal to avoid importing the messages
// package (which would create a circular dependency through tools).
func (c *Catalog) BuildMessages(msgs []Message, baseSystemPrompt string) []Message {
	out := make([]Message, len(msgs))
	copy(out, msgs)

	if c == nil || c.IsEmpty() {
		return out
	}

	runtimeSystem := c.RuntimeSystemPrompt(baseSystemPrompt)
	if len(out) > 0 && out[0].Role == "system" {
		out[0].Content = runtimeSystem
		return out
	}

	return append([]Message{{
		Role:    "system",
		Content: runtimeSystem,
	}}, out...)
}

func absoluteCleanPath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func canonicalPathForValidation(path string) (string, error) {
	absPath, err := absoluteCleanPath(path)
	if err != nil {
		return "", err
	}

	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			parent := filepath.Dir(absPath)
			if parent == absPath {
				return absPath, nil
			}
			canonicalParent, parentErr := canonicalPathForValidation(parent)
			if parentErr != nil {
				return "", parentErr
			}
			return filepath.Join(canonicalParent, filepath.Base(absPath)), nil
		}
		return "", err
	}

	return absoluteCleanPath(resolved)
}

func pathWithinRoot(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

// ResolvePath resolves a skill-relative path while preventing directory escape.
func (s *Skill) ResolvePath(rel string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("skill is nil")
	}
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", fmt.Errorf("path is required")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("path must be relative to the skill root")
	}

	resolved := filepath.Clean(filepath.Join(s.RootDir, rel))
	root := filepath.Clean(s.RootDir)
	if resolved != root && !strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes the skill root", rel)
	}

	canonicalRoot, err := canonicalPathForValidation(root)
	if err != nil {
		return "", err
	}
	canonicalResolved, err := canonicalPathForValidation(resolved)
	if err != nil {
		return "", err
	}
	if !pathWithinRoot(canonicalRoot, canonicalResolved) {
		return "", fmt.Errorf("path %q escapes the skill root", rel)
	}

	return resolved, nil
}

// ReadFile returns the contents of a skill-relative file.
func (s *Skill) ReadFile(rel string) (string, error) {
	path, err := s.ResolvePath(rel)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", rel)
	}

	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxSkillFileSize+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > maxSkillFileSize {
		return "", fmt.Errorf("%s exceeds the %d byte limit", rel, maxSkillFileSize)
	}

	return string(data), nil
}

// ListFiles returns all files under a skill subdirectory in a stable order.
func (s *Skill) ListFiles(subdir string) ([]string, error) {
	path, err := s.ResolvePath(subdir)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", subdir)
	}

	var files []string
	err = filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(s.RootDir, current)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(files)
	return files, nil
}
