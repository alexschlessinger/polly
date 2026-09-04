package main

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

func TestGenerateContextName(t *testing.T) {
	name, err := generateContextName(func(string) (bool, error) { return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[a-z]+-[a-z]+$`).MatchString(name) {
		t.Fatalf("unexpected name shape: %q", name)
	}

	// With every petname taken, the numbered fallback still terminates.
	name, err = generateContextName(func(n string) (bool, error) { return !strings.HasSuffix(n, "-3"), nil })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(name, "-3") {
		t.Fatalf("expected numbered fallback, got %q", name)
	}
}

// A store that cannot answer must fail the generation, not spin: the
// numbered fallback only exits on a free name, so an error read as "taken"
// never terminates.
func TestGenerateContextNameReturnsStoreError(t *testing.T) {
	want := errors.New("store closed")
	calls := 0
	_, err := generateContextName(func(string) (bool, error) {
		calls++
		return false, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if calls != 1 {
		t.Fatalf("exists called %d times, want 1", calls)
	}
}

func TestWantsAutoREPLContext(t *testing.T) {
	if wantsAutoREPLContext(&Config{PromptSet: true}) {
		t.Fatal("one-shot prompt run must not auto-create a context")
	}
	if wantsAutoREPLContext(&Config{ListContexts: true}) {
		t.Fatal("context management flags must not auto-create a context")
	}
	if wantsAutoREPLContext(&Config{AddToContext: true}) {
		t.Fatal("--add must not auto-create a context")
	}
	if wantsAutoREPLContext(&Config{Files: []string{"x"}}) {
		t.Fatal("a REPL-incompatible flag set must not auto-create a context")
	}
	// The positive case depends on stdin being a terminal/char device, which
	// holds under plain `go test` but not when the harness pipes stdin.
	if !hasStdinData() && !wantsAutoREPLContext(&Config{}) {
		t.Fatal("a bare interactive run should auto-create a context")
	}
}

func TestDiscardUnusedAutoContext(t *testing.T) {
	store := testOpenDiskStore(t, filepath.Join(t.TempDir(), "polly.db"), nil)

	idle := testAcquireAutoSession(t, store, "idle-fox")
	// Production derives the run context from the session. Closing the session
	// cancels that context, so cleanup must not attempt a subsequent store call
	// with it.
	if err := discardUnusedAutoContext(idle.Context(), &conversationState{session: idle}); err != nil {
		t.Fatalf("discard idle context: %v", err)
	}
	if testStoreExists(t, store, "idle-fox") {
		t.Fatal("turn-less auto context should be deleted on exit")
	}

	used := testAcquireAutoSession(t, store, "busy-owl")
	if err := used.AddMessage(context.Background(), messages.ChatMessage{Role: messages.MessageRoleUser, Content: "hi"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := discardUnusedAutoContext(used.Context(), &conversationState{session: used}); err != nil {
		t.Fatalf("discard used context: %v", err)
	}
	if !testStoreExists(t, store, "busy-owl") {
		t.Fatal("auto context with a turn must survive exit")
	}

	renamed := testAcquireAutoSession(t, store, "misty-vole")
	if err := renamed.Rename(context.Background(), "kept-session"); err != nil {
		t.Fatalf("rename idle auto context: %v", err)
	}
	if err := discardUnusedAutoContext(renamed.Context(), &conversationState{session: renamed}); err != nil {
		t.Fatalf("close renamed context: %v", err)
	}
	if !testStoreExists(t, store, "kept-session") || testStoreExists(t, store, "misty-vole") {
		t.Fatal("renaming must promote and preserve an otherwise-unused auto context")
	}
}

func TestReplRenameCommand(t *testing.T) {
	store := testOpenDiskStore(t, filepath.Join(t.TempDir(), "polly.db"), nil)
	session := testAcquireAutoSession(t, store, "misty-vole")

	var replies []string
	var uiName string
	ctx := &replCommandContext{
		config:         &Config{},
		state:          &conversationState{session: session},
		registry:       defaultReplCommands,
		reply:          func(line string) error { replies = append(replies, line); return nil },
		setContextName: func(name string) { uiName = name },
	}

	handled, quit, err := defaultReplCommands.dispatch("/rename my-project", ctx)
	if !handled || quit || err != nil {
		t.Fatalf("dispatch: handled=%v quit=%v err=%v", handled, quit, err)
	}
	name, err := session.GetName(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if name != "my-project" {
		t.Fatalf("session name = %q, want my-project", name)
	}
	if uiName != "my-project" {
		t.Fatalf("UI name = %q, want my-project", uiName)
	}
	if !testStoreExists(t, store, "my-project") || testStoreExists(t, store, "misty-vole") {
		t.Fatal("rename should update the SQLite catalog")
	}
	if len(replies) == 0 || !strings.Contains(replies[len(replies)-1], "my-project") {
		t.Fatalf("expected a confirmation reply, got %v", replies)
	}

	// Memory and disk sessions share the same rename behavior.
	memStore := testOpenMemoryStore(t, nil)
	memSession := testAcquireAutoSession(t, memStore, "default")
	ctx.state = &conversationState{session: memSession}
	replies = nil
	if handled, _, err := defaultReplCommands.dispatch("/rename memory-name", ctx); !handled || err != nil {
		t.Fatalf("mem dispatch: handled=%v err=%v", handled, err)
	}
	if !testStoreExists(t, memStore, "memory-name") || testStoreExists(t, memStore, "default") {
		t.Fatal("memory rename should update the same SQLite catalog")
	}
	if len(replies) == 0 || !strings.Contains(replies[0], "memory-name") {
		t.Fatalf("expected rename confirmation, got %v", replies)
	}
}
