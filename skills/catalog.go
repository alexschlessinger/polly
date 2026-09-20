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
	"slices"
	"strings"

	"github.com/alexschlessinger/pollytool/internal/safefile"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"go.yaml.in/yaml/v3"
)

const skillFileName = "SKILL.md"
const maxSkillFileSize = 1 << 20

var skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

type frontmatter struct {
	Name          string         `yaml:"name"`
	Description   string         `yaml:"description"`
	License       string         `yaml:"license"`
	Compatibility string         `yaml:"compatibility"`
	AllowedTools  string         `yaml:"allowed-tools"`
	Command       string         `yaml:"command"`
	Metadata      map[string]any `yaml:"metadata"`
}

// Skill contains the parsed metadata and instructions for a discovered skill.
type Skill struct {
	Name          string
	Description   string
	License       string
	Compatibility string
	AllowedTools  string

	// Command is the slash command that activates this skill, empty for the
	// usual skill a user activates by name. A skill whose work needs what
	// only a command sets up names it here, and the host then offers the
	// command instead of the skill's own bare spelling.
	Command string

	Metadata     map[string]any
	RootDir      string
	Instructions string

	// canonicalRoot is RootDir with symlinks resolved at discovery time. Reads
	// are contained within it rather than within whatever RootDir resolves to
	// later, so replacing the skill directory with a symlink after discovery
	// cannot redirect them. A Skill without one was not discovered and
	// resolves no paths at all.
	canonicalRoot string
}

// Catalog stores the discovered skill set.
type Catalog struct {
	ordered []*Skill
	byName  map[string]*Skill
}

func newCatalog() *Catalog {
	return &Catalog{byName: make(map[string]*Skill)}
}

// add records a skill whose name is not yet in the catalog. The caller sorts
// once it has added everything.
func (c *Catalog) add(skill *Skill) {
	c.byName[skill.Name] = skill
	c.ordered = append(c.ordered, skill)
}

func (c *Catalog) sortByName() {
	slices.SortFunc(c.ordered, func(a, b *Skill) int { return strings.Compare(a.Name, b.Name) })
}

// Discover loads skills from either skill directories or container directories.
func Discover(paths []string) (*Catalog, error) {
	catalog := newCatalog()
	for _, path := range paths {
		discovered, err := discoverPath(path)
		if err != nil {
			return nil, err
		}
		for _, skill := range discovered {
			if existing, ok := catalog.byName[skill.Name]; ok {
				return nil, fmt.Errorf("duplicate skill %q found at %s and %s", skill.Name, existing.RootDir, skill.RootDir)
			}
			catalog.add(skill)
		}
	}
	catalog.sortByName()
	return catalog, nil
}

// Merge adds every skill from other that is not already present. Existing
// skills win, so user-installed skills shadow builtin skills of the same
// name instead of tripping the duplicate-name error.
func (c *Catalog) Merge(other *Catalog) {
	if c == nil || other == nil {
		return
	}
	for _, skill := range other.ordered {
		if _, ok := c.byName[skill.Name]; !ok {
			c.add(skill)
		}
	}
	c.sortByName()
}

func discoverPath(path string) ([]*Skill, error) {
	if err := requireDir(path); err != nil {
		return nil, err
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
	meta, body, err := readSkillMarkdown(skillPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}

	root = filepath.Clean(root)
	if err := validateFrontmatter(meta, filepath.Base(root)); err != nil {
		return nil, false, fmt.Errorf("validate %s: %w", skillPath, err)
	}
	canonicalRoot, err := canonicalPath(root)
	if err != nil {
		return nil, false, fmt.Errorf("resolve %s: %w", root, err)
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
		Command:       strings.TrimSpace(meta.Command),
		Metadata:      meta.Metadata,
		RootDir:       root,
		Instructions:  instructions,
		canonicalRoot: canonicalRoot,
	}

	if !checkMetadataGating(skill.Metadata) {
		return nil, false, nil
	}

	return skill, true, nil
}

// readSkillMarkdown reads and parses a SKILL.md. A missing file is reported
// with os.ErrNotExist so callers can treat its directory as not a skill.
func readSkillMarkdown(path string) (*frontmatter, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", path, err)
	}
	meta, body, err := parseSkillMarkdown(string(data))
	if err != nil {
		return nil, "", fmt.Errorf("parse %s: %w", path, err)
	}
	return meta, body, nil
}

// checkMetadataGating evaluates platform and binary requirements embedded in
// metadata (openclaw or clawdbot namespace). Returns true if the skill is
// eligible to run on this machine.
func checkMetadataGating(metadata map[string]any) bool {
	var gating map[string]any
	for _, key := range []string{"openclaw", "clawdbot"} {
		if m, ok := metadata[key].(map[string]any); ok {
			gating = m
			break
		}
	}
	// "always: true" skips all checks.
	if gating == nil || gating["always"] == true {
		return true
	}
	if osList, _ := gating["os"].([]any); len(osList) > 0 && !anyString(osList, func(s string) bool { return normalizeOS(s) == runtime.GOOS }) {
		return false
	}
	reqs, _ := gating["requires"].(map[string]any)
	// All listed bins must exist.
	if bins, _ := reqs["bins"].([]any); anyString(bins, func(name string) bool { return !onPath(name) }) {
		return false
	}
	// At least one of anyBins must exist.
	if anyBins, _ := reqs["anyBins"].([]any); len(anyBins) > 0 && !anyString(anyBins, onPath) {
		return false
	}
	return true
}

// anyString reports whether some string element of a YAML list satisfies
// pred; elements of other types are ignored.
func anyString(list []any, pred func(string) bool) bool {
	return slices.ContainsFunc(list, func(entry any) bool {
		s, ok := entry.(string)
		return ok && pred(s)
	})
}

func onPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// normalizeOS maps OpenClaw platform names to Go's runtime.GOOS values.
func normalizeOS(s string) string {
	s = strings.ToLower(s)
	if s == "win32" {
		return "windows"
	}
	return s
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

// validateSkillName enforces the name rule shared by discovery and by the
// remote fetch that names a cache entry after the skill.
func validateSkillName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("name is required")
	case len(name) > 64:
		return fmt.Errorf("name exceeds 64 characters")
	case !skillNamePattern.MatchString(name):
		return fmt.Errorf("name must contain only lowercase letters, numbers, and hyphens without consecutive hyphens")
	}
	return nil
}

func validateFrontmatter(meta *frontmatter, dirName string) error {
	name := strings.TrimSpace(meta.Name)
	if err := validateSkillName(name); err != nil {
		return err
	}
	if name != dirName {
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

	if command := strings.TrimSpace(meta.Command); command != "" {
		rest, ok := strings.CutPrefix(command, "/")
		if !ok || !skillNamePattern.MatchString(rest) {
			return fmt.Errorf("command %q must be a slash command such as /sandbox-init", meta.Command)
		}
	}

	return nil
}

// Count reports how many skills the catalog holds; a nil catalog holds none.
func (c *Catalog) Count() int {
	if c == nil {
		return 0
	}
	return len(c.ordered)
}

// IsEmpty reports whether the catalog has any discovered skills.
func (c *Catalog) IsEmpty() bool {
	return c.Count() == 0
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
	if c.IsEmpty() {
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
	if c.IsEmpty() {
		return baseSystemPrompt
	}

	var sections []string
	if baseSystemPrompt != "" {
		sections = append(sections, baseSystemPrompt)
	}

	sections = append(sections, strings.TrimSpace(`
Agent Skills are available in this environment.
Do not assume a skill's instructions until you call activate_skill or receive explicitly requested skill instructions activated by Polly. Host-activated instructions are already loaded; do not activate the same skill again merely to load them.
Use read_skill_file to inspect files referenced by an activated skill.
If activation loads helper scripts or MCP servers, those tools will become available on the next turn.
If a skill declares allowed-tools, that allowlist is enforced on future turns after activation.
Allowed-tools policies are additive across skill activations; a later activation can widen tool access, but it does not revoke previously allowed tools.
`))
	sections = append(sections, c.PromptXML())

	return strings.Join(sections, "\n\n")
}

// canonicalPath resolves symlinks along path as far as it exists and appends
// the rest lexically: the spelling the sandbox's read policy and a no-follow
// open agree on.
func canonicalPath(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return sandbox.ResolveExistingPathPrefix(absPath)
}

// ResolvePath resolves a skill-relative path while preventing directory escape.
func (s *Skill) ResolvePath(rel string) (string, error) {
	lexical, _, err := s.resolve(rel)
	return lexical, err
}

// resolve returns the lexical path of rel under the skill root and the
// canonical spelling that a read must open. Containment is judged against the
// canonical root recorded at discovery, so a skill directory that is later
// replaced by a symlink cannot redirect skill reads outside the tree that was
// discovered.
func (s *Skill) resolve(rel string) (lexical, canonical string, err error) {
	if s == nil {
		return "", "", fmt.Errorf("skill is nil")
	}
	if s.canonicalRoot == "" {
		return "", "", fmt.Errorf("skill %q was not loaded by discovery", s.Name)
	}
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", "", fmt.Errorf("path is required")
	}
	if filepath.IsAbs(rel) {
		return "", "", fmt.Errorf("path must be relative to the skill root")
	}

	resolved := filepath.Clean(filepath.Join(s.RootDir, rel))
	if !sandbox.PathWithin(resolved, s.RootDir) {
		return "", "", fmt.Errorf("path %q escapes the skill root", rel)
	}
	canonicalResolved, err := canonicalPath(resolved)
	if err != nil {
		return "", "", err
	}
	if !sandbox.PathWithin(canonicalResolved, s.canonicalRoot) {
		return "", "", fmt.Errorf("path %q escapes the skill root", rel)
	}

	return resolved, canonicalResolved, nil
}

// ReadFile returns the contents of a skill-relative file.
func (s *Skill) ReadFile(rel string) (string, error) {
	return s.ReadFileChecked(rel, nil)
}

// ReadFileChecked is ReadFile with a policy hook that sees the canonical path
// about to be opened; a non-nil error from the hook aborts the read. The file
// is opened without following symlinks, so the object read is the one the
// hook approved even if a link is rewritten concurrently.
func (s *Skill) ReadFileChecked(rel string, check func(canonical string) error) (string, error) {
	_, canonical, err := s.resolve(rel)
	if err != nil {
		return "", err
	}
	if check != nil {
		if err := check(canonical); err != nil {
			return "", err
		}
	}

	file, err := safefile.OpenRegular(canonical, os.O_RDONLY, 0)
	if err != nil {
		var notRegular *safefile.NotRegularError
		if errors.As(err, &notRegular) {
			if notRegular.Mode.IsDir() {
				return "", fmt.Errorf("%s is a directory", rel)
			}
			return "", fmt.Errorf("%s is not a regular file", rel)
		}
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

	slices.Sort(files)
	return files, nil
}
