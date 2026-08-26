package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func TestDeleteContextReturnsSessionInUse(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	_ = testAcquireSession(t, store, "busy")

	err := deleteContext(context.Background(), store, "busy")
	if !errors.Is(err, sessions.ErrSessionInUse) {
		t.Fatalf("delete active context error = %v, want ErrSessionInUse", err)
	}
	if !testStoreExists(t, store, "busy") {
		t.Fatal("active context was deleted")
	}
}

func TestPurgeContextsAttemptsAllAndReturnsFailures(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	_ = testAcquireSession(t, store, "busy")
	free := testAcquireSession(t, store, "free")
	if err := free.Close(); err != nil {
		t.Fatal(err)
	}

	err := purgeContexts(context.Background(), store, []string{"busy", "free"})
	if !errors.Is(err, sessions.ErrSessionInUse) {
		t.Fatalf("purge error = %v, want ErrSessionInUse", err)
	}
	if !testStoreExists(t, store, "busy") {
		t.Fatal("purge deleted the active context")
	}
	if testStoreExists(t, store, "free") {
		t.Fatal("purge stopped before deleting an available context")
	}
}

func TestInitializeConversationRestoresAuthoritativeZeroSettings(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "zero-settings")
	metadata, err := session.GetMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	metadata.Model = ""
	metadata.Temperature = 0
	metadata.MaxTokens = 0
	metadata.MaxHistoryTokens = 0
	metadata.SystemPrompt = ""
	metadata.ThinkingEffort = "off"
	metadata.ToolTimeout = 0
	metadata.MaxIterations = 0
	metadata.SkillDirs = nil
	if err := session.SetMetadata(context.Background(), metadata); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	config := &Config{
		Settings: Settings{
			Model:            "default/model",
			Temperature:      1,
			MaxTokens:        64_000,
			MaxHistoryTokens: 256_000,
			SystemPrompt:     "default system",
			ThinkingEffort:   "high",
			ToolTimeout:      30 * time.Second,
			SkillDirs:        []string{"/default/skills"},
		},
		MaxIterations: 250,
	}
	contextID, _, err := initializeConversation(context.Background(), config, store, "zero-settings", getCommand())
	if err != nil {
		t.Fatal(err)
	}
	if contextID != "zero-settings" {
		t.Fatalf("context ID = %q", contextID)
	}
	if config.Model != "" || config.Temperature != 0 || config.MaxTokens != 0 || config.MaxHistoryTokens != 0 ||
		config.SystemPrompt != "" || config.ThinkingEffort != "off" || config.ToolTimeout != 0 || config.MaxIterations != 0 ||
		len(config.SkillDirs) != 0 {
		t.Fatalf("resolved settings did not preserve stored zero values: %+v", config)
	}
}

func TestInitializeConversationStoresChangedSystemPromptBeforeClear(t *testing.T) {
	for _, prompt := range []string{"new system", ""} {
		name := "nonempty"
		if prompt == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			store := testOpenMemoryStore(t, &sessions.Metadata{SystemPrompt: "old system"})
			session := testAcquireSession(t, store, "prompt-reset")
			testAddMessage(t, session, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "old turn"})
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}

			cmd := getCommand()
			if err := cmd.Set("system", prompt); err != nil {
				t.Fatal(err)
			}
			config := &Config{Settings: Settings{SystemPrompt: prompt}}
			if _, _, err := initializeConversation(context.Background(), config, store, "prompt-reset", cmd); err != nil {
				t.Fatal(err)
			}

			reopened := testAcquireSession(t, store, "prompt-reset")
			metadata, err := reopened.GetMetadata(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if metadata.SystemPrompt != prompt {
				t.Fatalf("stored system prompt = %q, want %q", metadata.SystemPrompt, prompt)
			}
			history := testSessionHistory(t, reopened)
			var want []messages.ChatMessage
			if prompt != "" {
				want = []messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: prompt}}
			}
			if len(history) != len(want) || (len(want) > 0 && !reflect.DeepEqual(history, want)) {
				t.Fatalf("history after prompt reset = %#v, want %#v", history, want)
			}
		})
	}
}
