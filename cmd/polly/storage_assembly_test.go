package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
	"github.com/alexschlessinger/pollytool/tools"
)

type assemblyStorage struct {
	sessions.SessionStore
	path      string
	locations int
	promoted  []string
}

func (s *assemblyStorage) Location() (sessions.StoreMode, string) {
	s.locations++
	return sessions.ModeDisk, s.path
}
func (s *assemblyStorage) Promote(_ context.Context, path string) error {
	s.promoted = append(s.promoted, path)
	return nil
}

func TestStorageAssemblySharesExclusionsWithSwarm(t *testing.T) {
	skipIfWindows(t)
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	t.Chdir(root)
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-qm", "base"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
	}
	private := filepath.Join(root, "private.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.WriteFile(private+suffix, []byte("PRIVATE_STORAGE_CONTENT"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	store := &assemblyStorage{SessionStore: testOpenMemoryStore(t, nil), path: private}
	storage, err := newSessionStorage(store)
	if err != nil {
		t.Fatal(err)
	}
	promotion, err := defaultStorePath()
	if err != nil {
		t.Fatal(err)
	}
	for _, database := range []string{private, promotion} {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if !slices.Contains(storage.privatePaths, database+suffix) {
				t.Fatalf("missing storage exclusion: %s", database+suffix)
			}
		}
	}
	// Changing discovery after setup must not change the frozen configuration
	// used by the registry, the swarm or the workspace constructor.
	store.path = filepath.Join(root, "later.db")
	parent, err := store.Acquire(context.Background(), "assembly", sessions.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	registry := tools.NewToolRegistry(nil, tools.WithNativeTools(), tools.WithUnsafeNoSandbox())
	defer registry.Close()
	if _, err := registry.LoadToolAuto("read_file"); err != nil {
		t.Fatal(err)
	}
	state := &conversationState{sessionStore: store, storage: storage, session: parent, toolRegistry: registry,
		settings: Settings{Model: "test/model", MaxTokens: 128, MaxIterations: 3}}
	var result string
	model := integrationModel(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		for _, m := range req.Messages {
			if m.Role == messages.MessageRoleTool {
				result = m.Content
				return spawnTestReply("done")
			}
		}
		return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
			ToolCalls: []messages.ChatMessageToolCall{{ID: "private-read", Name: "read_file", Arguments: tools.Result(map[string]any{"path": private})}}}
	})
	if err := registerSwarm(state, &Config{SwarmDirectory: filepath.Join(t.TempDir(), "runtime")}, model); err != nil {
		t.Fatal(err)
	}
	defer state.swarm.Close()
	if store.locations != 1 || len(store.promoted) != 0 {
		t.Fatal("swarm rediscovered or eagerly promoted storage")
	}
	a, err := state.swarm.Agent(context.Background(), "", swarm.AgentRequest{Label: "Research", Task: "read private storage", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "blocked") || strings.Contains(result, "PRIVATE_STORAGE_CONTENT") {
		t.Fatalf("member was not bound to resolved exclusions: %s", result)
	}
	s, err := state.swarm.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c := s.Contexts[a.Context]
	if c == nil || c.Checkout == nil {
		t.Fatal("member did not receive a Git workspace")
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(c.Root, "private.db") + suffix); !os.IsNotExist(err) {
			t.Fatalf("workspace copied private storage%s: %v", suffix, err)
		}
	}
	if store.locations != 1 || !slices.Equal(store.promoted, []string{promotion}) {
		t.Fatalf("storage ownership changed: reads=%d promotions=%v", store.locations, store.promoted)
	}
}
