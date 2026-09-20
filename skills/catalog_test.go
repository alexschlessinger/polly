package skills

import (
	"errors"
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
	writeSkillFile(t, root, "wrong-dir", `---
name: right-name
description: mismatch
---
Use this skill.`)

	_, err := Discover([]string{root})
	if err == nil || !strings.Contains(err.Error(), "must match directory") {
		t.Fatalf("Discover() error = %v, want mismatched directory error", err)
	}
}

func TestSkillReadFilePreventsEscape(t *testing.T) {
	root := t.TempDir()
	createTestSkill(t, root, "safe-reader", "Read files safely")
	skill := discoverSkill(t, root, "safe-reader")

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

	skill := discoverSkill(t, root, "safe-reader")
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

	skill := discoverSkill(t, root, "safe-reader")
	if _, err := skill.ReadFile("references/large.txt"); err == nil || !strings.Contains(err.Error(), "exceeds the") {
		t.Fatalf("ReadFile() error = %v, want size limit error", err)
	}
}

func TestDiscoverOpenClawSkillWithNestedMetadata(t *testing.T) {
	root := t.TempDir()
	skillDir := writeSkillFile(t, root, "oc-skill", `---
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
`)

	skill := discoverSkill(t, root, "oc-skill")

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
	// Requires an impossible OS and missing binary, but always: true.
	writeSkillFile(t, root, "always-skill", `---
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
`)

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
	writeSkillFile(t, root, name, "---\nname: "+name+"\ndescription: gated\nmetadata:\n  openclaw:\n    os:\n      - "+osName+"\n---\nInstructions.\n")
}

func writeGatedSkillBins(t *testing.T, root, name string, bins, anyBins []string) {
	t.Helper()
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
	writeSkillFile(t, root, name, "---\nname: "+name+"\ndescription: bin-gated\nmetadata:\n  openclaw:\n    requires:\n"+strings.Join(reqLines, "\n")+"\n---\nInstructions.\n")
}

// writeSkillFile creates root/name/SKILL.md with content and returns the
// skill directory.
func writeSkillFile(t *testing.T, root, name, content string) string {
	t.Helper()
	skillDir := filepath.Join(root, name)
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, skillFileName), []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile(SKILL.md) error = %v", err)
	}
	return skillDir
}

// createTestSkill writes a complete skill with a references file and an empty
// scripts directory, and returns the skill directory.
func createTestSkill(t *testing.T, root, name, description string) string {
	t.Helper()
	skillDir := writeSkillFile(t, root, name, `---
name: `+name+`
description: `+description+`
compatibility: polly >= 0.1
allowed-tools: activate_skill,read_skill_file
---
# `+name+`

Follow these instructions carefully.
`)
	if err := os.MkdirAll(filepath.Join(skillDir, "scripts"), 0755); err != nil {
		t.Fatalf("MkdirAll(scripts) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(skillDir, "references"), 0755); err != nil {
		t.Fatalf("MkdirAll(references) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "references", "guide.md"), []byte("reference data"), 0644); err != nil {
		t.Fatalf("WriteFile(reference) error = %v", err)
	}
	return skillDir
}

// discoverSkill discovers root and returns the named skill.
func discoverSkill(t *testing.T, root, name string) *Skill {
	t.Helper()
	catalog, err := Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	skill, ok := catalog.Get(name)
	if !ok {
		t.Fatalf("%s not discovered", name)
	}
	return skill
}

func TestSkillReadFileRejectsRootReplacedBySymlinkAfterDiscovery(t *testing.T) {
	root := t.TempDir()
	skillDir := createTestSkill(t, root, "safe-reader", "Read files safely")
	skill := discoverSkill(t, root, "safe-reader")

	// Swap the skill directory for a symlink to a tree holding a secret, as
	// a writable skill directory allows after discovery.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "id_rsa"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(skillDir, skillDir+".orig"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, skillDir); err != nil {
		t.Skipf("Symlink() unavailable: %v", err)
	}

	if _, err := skill.ReadFile("id_rsa"); err == nil || !strings.Contains(err.Error(), "escapes the skill root") {
		t.Fatalf("ReadFile() through swapped root error = %v, want escape error", err)
	}
}

func TestSkillReadFileCheckedSeesCanonicalPath(t *testing.T) {
	root := t.TempDir()
	createTestSkill(t, root, "safe-reader", "Read files safely")
	skill := discoverSkill(t, root, "safe-reader")

	var seen string
	_, err := skill.ReadFileChecked("references/guide.md", func(canonical string) error {
		seen = canonical
		return errors.New("policy says no")
	})
	if err == nil || !strings.Contains(err.Error(), "policy says no") {
		t.Fatalf("ReadFileChecked() error = %v, want policy error", err)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(root, "safe-reader", "references", "guide.md"))
	if seen != want {
		t.Fatalf("policy hook saw %q, want canonical %q", seen, want)
	}
}

// TestDiscoverReadsTheActivatingCommand covers the frontmatter a skill uses
// when its instructions only work after a slash command has set things up:
// the host reads Command to offer that command instead of the skill's own
// bare spelling, so a spelling that is not a slash command is refused.
func TestDiscoverReadsTheActivatingCommand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		want    string
	}{
		{"command-skill", "/sandbox-init", "/sandbox-init"},
		{"plain-skill", "", ""},
		{"missing-slash", "sandbox-init", "error"},
		{"not-a-name", "/Sandbox Init", "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			content := "---\nname: " + tc.name + "\ndescription: Set the workspace up\n"
			if tc.command != "" {
				content += "command: " + tc.command + "\n"
			}
			content += "---\nPrepare it.\n"
			writeSkillFile(t, root, tc.name, content)
			catalog, err := Discover([]string{root})
			if tc.want == "error" {
				if err == nil || !strings.Contains(err.Error(), "must be a slash command") {
					t.Fatalf("Discover() error = %v, want a rejected command", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Discover() error = %v", err)
			}
			skill, ok := catalog.Get(tc.name)
			if !ok || skill.Command != tc.want {
				t.Fatalf("command = %q, %t, want %q", skill.Command, ok, tc.want)
			}
		})
	}
}
