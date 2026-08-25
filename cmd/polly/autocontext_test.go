package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func TestGenerateContextName(t *testing.T) {
	name := generateContextName(func(string) bool { return false })
	if !regexp.MustCompile(`^[a-z]+-[a-z]+$`).MatchString(name) {
		t.Fatalf("unexpected name shape: %q", name)
	}

	// With every petname taken, the numbered fallback still terminates.
	name = generateContextName(func(n string) bool { return !strings.HasSuffix(n, "-3") })
	if !strings.HasSuffix(name, "-3") {
		t.Fatalf("expected numbered fallback, got %q", name)
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
	store, err := sessions.NewFileSessionStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	idle, err := store.Get("idle-fox")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	discardUnusedAutoContext(&conversationState{session: idle}, store, "idle-fox")
	if store.Exists("idle-fox") {
		t.Fatal("turn-less auto context should be deleted on exit")
	}

	used, err := store.Get("busy-owl")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := used.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "hi"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	discardUnusedAutoContext(&conversationState{session: used}, store, "busy-owl")
	if !store.Exists("busy-owl") {
		t.Fatal("auto context with a turn must survive exit")
	}
}

func TestReplRenameCommand(t *testing.T) {
	store, err := sessions.NewFileSessionStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	session, err := store.Get("misty-vole")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer session.Close()

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
	if session.GetName() != "my-project" {
		t.Fatalf("session name = %q, want my-project", session.GetName())
	}
	if uiName != "my-project" {
		t.Fatalf("UI name = %q, want my-project", uiName)
	}
	if !store.Exists("my-project") || store.Exists("misty-vole") {
		t.Fatal("rename should move the backing file")
	}
	if len(replies) == 0 || !strings.Contains(replies[len(replies)-1], "my-project") {
		t.Fatalf("expected a confirmation reply, got %v", replies)
	}

	// Renaming an in-memory session reports the limitation instead of failing.
	memStore := sessions.NewSyncMapSessionStore(nil)
	memSession, err := memStore.Get("default")
	if err != nil {
		t.Fatalf("mem get: %v", err)
	}
	ctx.state = &conversationState{session: memSession}
	replies = nil
	if handled, _, err := defaultReplCommands.dispatch("/rename nope", ctx); !handled || err != nil {
		t.Fatalf("mem dispatch: handled=%v err=%v", handled, err)
	}
	if len(replies) == 0 || !strings.Contains(replies[0], "in-memory") {
		t.Fatalf("expected in-memory notice, got %v", replies)
	}
}
