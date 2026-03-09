package llm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/skills"
)

func TestResolvedMessagesNilSkills(t *testing.T) {
	req := &CompletionRequest{
		Messages: []messages.ChatMessage{
			{Role: messages.MessageRoleUser, Content: "hello"},
		},
	}
	got := req.ResolvedMessages()
	if len(got) != 1 || got[0].Content != "hello" {
		t.Fatalf("expected unchanged copy, got %v", got)
	}
}

func TestResolvedMessagesEmptySkills(t *testing.T) {
	catalog, err := skills.Discover([]string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	req := &CompletionRequest{
		Messages: []messages.ChatMessage{
			{Role: messages.MessageRoleUser, Content: "hello"},
		},
		Skills: catalog,
	}
	got := req.ResolvedMessages()
	if len(got) != 1 || got[0].Content != "hello" {
		t.Fatalf("expected unchanged copy, got %v", got)
	}
}

func TestResolvedMessagesReplacesSystemMessage(t *testing.T) {
	catalog := createTestCatalog(t)
	req := &CompletionRequest{
		Messages: []messages.ChatMessage{
			{Role: messages.MessageRoleSystem, Content: "Base prompt"},
			{Role: messages.MessageRoleUser, Content: "hello"},
		},
		Skills: catalog,
	}
	got := req.ResolvedMessages()
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if !strings.Contains(got[0].Content, "<available_skills>") {
		t.Fatalf("system prompt missing skill block: %s", got[0].Content)
	}
	if !strings.Contains(got[0].Content, "Base prompt") {
		t.Fatalf("system prompt missing base prompt: %s", got[0].Content)
	}
}

func TestResolvedMessagesPrependsSystemMessage(t *testing.T) {
	catalog := createTestCatalog(t)
	req := &CompletionRequest{
		Messages: []messages.ChatMessage{
			{Role: messages.MessageRoleUser, Content: "hello"},
		},
		Skills: catalog,
	}
	got := req.ResolvedMessages()
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Role != messages.MessageRoleSystem {
		t.Fatalf("first role = %s, want system", got[0].Role)
	}
	if !strings.Contains(got[0].Content, "activate_skill") {
		t.Fatalf("system prompt missing runtime guidance: %s", got[0].Content)
	}
	if got[1].Content != "hello" {
		t.Fatalf("user message shifted: %s", got[1].Content)
	}
}

func TestResolvedMessagesDoesNotMutateInput(t *testing.T) {
	catalog := createTestCatalog(t)
	original := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "Base prompt"},
		{Role: messages.MessageRoleUser, Content: "hello"},
	}
	req := &CompletionRequest{
		Messages: original,
		Skills:   catalog,
	}
	_ = req.ResolvedMessages()
	if original[0].Content != "Base prompt" {
		t.Fatalf("input mutated: %s", original[0].Content)
	}
}

func createTestCatalog(t *testing.T) *skills.Catalog {
	t.Helper()
	root := t.TempDir()
	skillDir := filepath.Join(root, "test-skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: test-skill\ndescription: a test skill\n---\nInstructions.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	catalog, err := skills.Discover([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}
