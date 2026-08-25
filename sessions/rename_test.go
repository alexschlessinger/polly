package sessions

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

func TestFileSessionRename(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileSessionStore(dir, nil)
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	session, err := store.Get("scratch")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := session.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "hi"}); err != nil {
		t.Fatalf("add: %v", err)
	}

	fs, ok := session.(*FileSession)
	if !ok {
		t.Fatalf("expected *FileSession, got %T", session)
	}
	if err := fs.Rename("keeper"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	if fs.GetName() != "keeper" {
		t.Fatalf("GetName = %q, want keeper", fs.GetName())
	}
	if _, err := os.Stat(filepath.Join(dir, "scratch.json")); !os.IsNotExist(err) {
		t.Fatalf("old file should be gone, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "keeper.json")); err != nil {
		t.Fatalf("new file missing: %v", err)
	}

	// Writes after the rename land in the new file.
	if err := session.AddMessage(messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "hello"}); err != nil {
		t.Fatalf("add after rename: %v", err)
	}
	session.Close()

	reopened, err := store.Get("keeper")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	history := reopened.GetHistory()
	if len(history) != 2 || history[0].Content != "hi" || history[1].Content != "hello" {
		t.Fatalf("history did not survive rename: %+v", history)
	}
	if reopened.GetMetadata().Name != "keeper" {
		t.Fatalf("metadata name = %q, want keeper", reopened.GetMetadata().Name)
	}
}

func TestFileSessionRenameRejectsExisting(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileSessionStore(dir, nil)
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	taken, err := store.Get("taken")
	if err != nil {
		t.Fatalf("get taken: %v", err)
	}
	taken.Close()

	session, err := store.Get("scratch")
	if err != nil {
		t.Fatalf("get scratch: %v", err)
	}
	defer session.Close()
	fs := session.(*FileSession)

	if err := fs.Rename("taken"); err == nil {
		t.Fatal("rename onto existing context should fail")
	}
	if err := fs.Rename("bad/name"); err == nil {
		t.Fatal("rename to invalid name should fail")
	}
	if err := fs.Rename("scratch"); err != nil {
		t.Fatalf("rename to same name should be a no-op, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "scratch.json")); err != nil {
		t.Fatalf("failed renames must leave the original file intact: %v", err)
	}
}
