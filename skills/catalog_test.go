package skills

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDiscoverContainerDirectory(t *testing.T) {
	root := t.TempDir()
	createTestSkill(t, root, "build-helper", "Help with builds")
	createTestSkill(t, root, "test-helper", "Help with tests")

	catalog, err := Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	if catalog.IsEmpty() {
		t.Fatal("expected skills to be discovered")
	}

	got := catalog.List()
	if len(got) != 2 {
		t.Fatalf("len(List()) = %d, want 2", len(got))
	}
	if got[0].Name != "build-helper" || got[1].Name != "test-helper" {
		t.Fatalf("unexpected skill order: %q, %q", got[0].Name, got[1].Name)
	}

	prompt := catalog.PromptXML()
	if !strings.Contains(prompt, "<name>build-helper</name>") {
		t.Fatalf("PromptXML() missing skill name: %s", prompt)
	}
	if !strings.Contains(prompt, "<description>Help with builds</description>") {
		t.Fatalf("PromptXML() missing description: %s", prompt)
	}
}

func TestRuntimeSystemPromptIncludesSkillGuidance(t *testing.T) {
	root := t.TempDir()
	createTestSkill(t, root, "build-helper", "Help with builds")

	catalog, err := Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	prompt := catalog.RuntimeSystemPrompt("Base system prompt")
	if !strings.Contains(prompt, "Base system prompt") {
		t.Fatalf("RuntimeSystemPrompt() missing base prompt: %s", prompt)
	}
	if !strings.Contains(prompt, "activate_skill") {
		t.Fatalf("RuntimeSystemPrompt() missing activation guidance: %s", prompt)
	}
	if !strings.Contains(prompt, "<available_skills>") {
		t.Fatalf("RuntimeSystemPrompt() missing skills XML: %s", prompt)
	}
}

func TestDiscoverRejectsMismatchedDirectoryName(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "wrong-dir")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	content := `---
name: right-name
description: mismatch
---
Use this skill.`
	if err := os.WriteFile(filepath.Join(skillDir, skillFileName), []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, err := Discover([]string{root})
	if err == nil || !strings.Contains(err.Error(), "must match directory") {
		t.Fatalf("Discover() error = %v, want mismatched directory error", err)
	}
}

func TestSkillReadFilePreventsEscape(t *testing.T) {
	root := t.TempDir()
	createTestSkill(t, root, "safe-reader", "Read files safely")

	catalog, err := Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	skill, ok := catalog.Get("safe-reader")
	if !ok {
		t.Fatal("expected discovered skill")
	}

	content, err := skill.ReadFile("references/guide.md")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(content, "reference data") {
		t.Fatalf("ReadFile() = %q, want reference file content", content)
	}

	if _, err := skill.ReadFile("../outside.txt"); err == nil {
		t.Fatal("ReadFile() expected escape prevention error")
	}
}

func TestSkillReadFileRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	createTestSkill(t, root, "safe-reader", "Read files safely")

	outsideFile := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0644); err != nil {
		t.Fatalf("WriteFile(outside) error = %v", err)
	}

	leakPath := filepath.Join(root, "safe-reader", "references", "leak.txt")
	if err := os.Symlink(outsideFile, leakPath); err != nil {
		t.Skipf("Symlink() unavailable: %v", err)
	}

	catalog, err := Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	skill, ok := catalog.Get("safe-reader")
	if !ok {
		t.Fatal("expected discovered skill")
	}

	if _, err := skill.ReadFile("references/leak.txt"); err == nil || !strings.Contains(err.Error(), "escapes the skill root") {
		t.Fatalf("ReadFile() error = %v, want symlink escape error", err)
	}
}

func TestSkillReadFileRejectsOversizedFile(t *testing.T) {
	root := t.TempDir()
	createTestSkill(t, root, "safe-reader", "Read files safely")

	largeFile := filepath.Join(root, "safe-reader", "references", "large.txt")
	data := strings.Repeat("a", maxSkillFileSize+1)
	if err := os.WriteFile(largeFile, []byte(data), 0644); err != nil {
		t.Fatalf("WriteFile(large) error = %v", err)
	}

	catalog, err := Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	skill, ok := catalog.Get("safe-reader")
	if !ok {
		t.Fatal("expected discovered skill")
	}

	if _, err := skill.ReadFile("references/large.txt"); err == nil || !strings.Contains(err.Error(), "exceeds the") {
		t.Fatalf("ReadFile() error = %v, want size limit error", err)
	}
}

func TestDiscoverOpenClawSkillWithNestedMetadata(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "oc-skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	content := `---
name: oc-skill
description: An OpenClaw-style skill
homepage: https://example.com
user-invocable: true
metadata:
  openclaw:
    always: true
    os:
      - darwin
      - linux
    requires:
      bins:
        - git
---
Run the helper at {baseDir}/scripts/run.sh to get started.
`
	if err := os.WriteFile(filepath.Join(skillDir, skillFileName), []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile(SKILL.md) error = %v", err)
	}

	catalog, err := Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	skill, ok := catalog.Get("oc-skill")
	if !ok {
		t.Fatal("expected skill to be discovered")
	}

	// Nested metadata should be parsed without error.
	oc, ok := skill.Metadata["openclaw"]
	if !ok {
		t.Fatal("metadata.openclaw missing")
	}
	ocMap, ok := oc.(map[string]any)
	if !ok {
		t.Fatalf("metadata.openclaw is %T, want map[string]any", oc)
	}
	if ocMap["always"] != true {
		t.Fatalf("metadata.openclaw.always = %v, want true", ocMap["always"])
	}

	// {baseDir} should be replaced with the skill root path.
	if strings.Contains(skill.Instructions, "{baseDir}") {
		t.Fatalf("Instructions still contain {baseDir}: %s", skill.Instructions)
	}
	if !strings.Contains(skill.Instructions, skillDir) {
		t.Fatalf("Instructions missing resolved baseDir path %q: %s", skillDir, skill.Instructions)
	}
}

func TestMetadataGatingOS(t *testing.T) {
	root := t.TempDir()

	// Skill matching current OS — should be discovered.
	writeGatedSkill(t, root, "native-skill", runtime.GOOS)

	// Skill requiring a different OS — should be skipped.
	fakeOS := "win32"
	if runtime.GOOS == "windows" {
		fakeOS = "linux"
	}
	writeGatedSkill(t, root, "foreign-skill", fakeOS)

	catalog, err := Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	if _, ok := catalog.Get("native-skill"); !ok {
		t.Error("native-skill should be discovered on this OS")
	}
	if _, ok := catalog.Get("foreign-skill"); ok {
		t.Error("foreign-skill should be skipped on this OS")
	}
}

func TestMetadataGatingBins(t *testing.T) {
	root := t.TempDir()

	// "go" should exist on PATH during tests.
	writeGatedSkillBins(t, root, "has-bins", []string{"go"}, nil)

	// A binary that definitely doesn't exist.
	writeGatedSkillBins(t, root, "missing-bins", []string{"__polly_nonexistent_binary_xyz__"}, nil)

	// anyBins: at least one must exist.
	writeGatedSkillBins(t, root, "any-bins-ok", nil, []string{"__nope__", "go"})
	writeGatedSkillBins(t, root, "any-bins-fail", nil, []string{"__nope1__", "__nope2__"})

	catalog, err := Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	for _, tc := range []struct {
		name string
		want bool
	}{
		{"has-bins", true},
		{"missing-bins", false},
		{"any-bins-ok", true},
		{"any-bins-fail", false},
	} {
		_, ok := catalog.Get(tc.name)
		if ok != tc.want {
			t.Errorf("%s: discovered=%v, want %v", tc.name, ok, tc.want)
		}
	}
}

func TestMetadataGatingAlwaysSkipsChecks(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "always-skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Requires an impossible OS and missing binary, but always: true.
	content := `---
name: always-skill
description: always passes
metadata:
  openclaw:
    always: true
    os:
      - impossible-os
    requires:
      bins:
        - __nonexistent__
---
Instructions.
`
	if err := os.WriteFile(filepath.Join(skillDir, skillFileName), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	catalog, err := Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if _, ok := catalog.Get("always-skill"); !ok {
		t.Error("always-skill should be discovered despite impossible requirements")
	}
}

func TestMetadataGatingNoMetadata(t *testing.T) {
	root := t.TempDir()
	createTestSkill(t, root, "plain-skill", "no metadata gating")

	catalog, err := Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if _, ok := catalog.Get("plain-skill"); !ok {
		t.Error("skill without metadata should always be discovered")
	}
}

func writeGatedSkill(t *testing.T, root, name, osName string) {
	t.Helper()
	skillDir := filepath.Join(root, name)
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: gated\nmetadata:\n  openclaw:\n    os:\n      - " + osName + "\n---\nInstructions.\n"
	if err := os.WriteFile(filepath.Join(skillDir, skillFileName), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func writeGatedSkillBins(t *testing.T, root, name string, bins, anyBins []string) {
	t.Helper()
	skillDir := filepath.Join(root, name)
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}

	var reqLines []string
	if len(bins) > 0 {
		reqLines = append(reqLines, "      bins:")
		for _, b := range bins {
			reqLines = append(reqLines, "        - "+b)
		}
	}
	if len(anyBins) > 0 {
		reqLines = append(reqLines, "      anyBins:")
		for _, b := range anyBins {
			reqLines = append(reqLines, "        - "+b)
		}
	}

	content := "---\nname: " + name + "\ndescription: bin-gated\nmetadata:\n  openclaw:\n    requires:\n" + strings.Join(reqLines, "\n") + "\n---\nInstructions.\n"
	if err := os.WriteFile(filepath.Join(skillDir, skillFileName), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildMessagesInjectsSkillPromptWithoutMutatingInput(t *testing.T) {
	root := t.TempDir()
	createTestSkill(t, root, "formatter", "Formats generated text")

	catalog, err := Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	history := []Message{
		{Role: "system", Content: "Base system prompt"},
		{Role: "user", Content: "hello"},
	}
	msgs := catalog.BuildMessages(history, "Base system prompt")

	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}
	if !strings.Contains(msgs[0].Content, "<available_skills>") {
		t.Fatalf("system prompt missing skill block: %s", msgs[0].Content)
	}
	if history[0].Content != "Base system prompt" {
		t.Fatalf("history mutated: %s", history[0].Content)
	}
}

func TestBuildMessagesPrependsSystemPromptWhenMissing(t *testing.T) {
	root := t.TempDir()
	createTestSkill(t, root, "reviewer", "Reviews code changes")

	catalog, err := Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	history := []Message{
		{Role: "user", Content: "hello"},
	}
	msgs := catalog.BuildMessages(history, "")

	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Fatalf("first role = %s, want system", msgs[0].Role)
	}
	if !strings.Contains(msgs[0].Content, "activate_skill") {
		t.Fatalf("system prompt missing runtime guidance: %s", msgs[0].Content)
	}
}

func TestBuildMessagesNilCatalog(t *testing.T) {
	history := []Message{
		{Role: "user", Content: "hello"},
	}
	var catalog *Catalog
	msgs := catalog.BuildMessages(history, "base")

	if len(msgs) != 1 || msgs[0].Content != "hello" {
		t.Fatalf("nil catalog should return copy of input, got %v", msgs)
	}
}

func createTestSkill(t *testing.T, root, name, description string) string {
	t.Helper()

	skillDir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(skillDir, "references"), 0755); err != nil {
		t.Fatalf("MkdirAll(references) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(skillDir, "scripts"), 0755); err != nil {
		t.Fatalf("MkdirAll(scripts) error = %v", err)
	}

	content := `---
name: ` + name + `
description: ` + description + `
compatibility: polly >= 0.1
allowed-tools: activate_skill,read_skill_file
---
# ` + name + `

Follow these instructions carefully.
`
	if err := os.WriteFile(filepath.Join(skillDir, skillFileName), []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile(SKILL.md) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "references", "guide.md"), []byte("reference data"), 0644); err != nil {
		t.Fatalf("WriteFile(reference) error = %v", err)
	}

	return skillDir
}
