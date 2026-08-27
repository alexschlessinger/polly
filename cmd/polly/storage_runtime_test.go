package main

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/urfave/cli/v3"
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

func TestFreshNamedContextSeedsResolvedDefaults(t *testing.T) {
	for _, tt := range []struct {
		name          string
		modelOverride string
	}{
		{name: "built-in model"},
		{name: "only model overridden", modelOverride: "openrouter/z-ai/glm-5.3-flash"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			args := []string(nil)
			if tt.modelOverride != "" {
				args = []string{"--model", tt.modelOverride}
			}
			config, cmd := parseStorageTestConfig(t, args...)

			store, err := setupSessionStore(config, "fresh", false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			if _, err := checkAndPromptForMissingContext(context.Background(), store, "fresh"); err != nil {
				t.Fatal(err)
			}
			if _, _, err := initializeConversation(context.Background(), config, store, "fresh", cmd); err != nil {
				t.Fatal(err)
			}

			assertResolvedConfig(t, config, tt.modelOverride)
			metadata, err := store.GetAllMetadata(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			assertResolvedMetadata(t, metadata["fresh"], tt.modelOverride)
		})
	}
}

func TestFreshManagedREPLContextSeedsEffectiveSystemPrompt(t *testing.T) {
	config, cmd := parseStorageTestConfig(t)
	applyConversationModeDefaults(config, conversationModeREPL, cmd.IsSet("system"), true)

	store, err := setupSessionStore(config, "", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	session := testAcquireSession(t, store, "fresh-repl")

	metadata, err := session.GetMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metadata.SystemPrompt != defaultREPLSystemPrompt {
		t.Fatalf("stored system prompt = %q, want managed REPL default %q", metadata.SystemPrompt, defaultREPLSystemPrompt)
	}
	history := testSessionHistory(t, session)
	want := []messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: defaultREPLSystemPrompt}}
	if !reflect.DeepEqual(history, want) {
		t.Fatalf("fresh managed REPL history = %#v, want %#v", history, want)
	}
}

func TestCreateContextStoresResolvedSettings(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	config, _ := parseStorageTestConfig(t)
	if err := handleCreateContext(context.Background(), store, config, "created"); err != nil {
		t.Fatal(err)
	}
	metadata, err := store.GetAllMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertResolvedMetadata(t, metadata["created"], "")
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

func TestHandleResetContextRebuildsHistoryFromSystemOverride(t *testing.T) {
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
			withStdin(t, "y\n", func() {
				if err := handleResetContext(context.Background(), store, config, cmd, "prompt-reset"); err != nil {
					t.Fatal(err)
				}
			})

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
				t.Fatalf("history after management reset = %#v, want %#v", history, want)
			}
		})
	}
}

func parseStorageTestConfig(t *testing.T, args ...string) (*Config, *cli.Command) {
	t.Helper()
	for _, key := range []string{
		"POLLYTOOL_MODEL", "POLLYTOOL_TEMP", "POLLYTOOL_MAXTOKENS", "POLLYTOOL_MAXITERATIONS",
		"POLLYTOOL_THINKING", "POLLYTOOL_SYSTEM", "POLLYTOOL_TOOLTIMEOUT", "POLLYTOOL_SKILLDIR",
	} {
		value, existed := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(key, value)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}

	flags, groups := defineFlagsWithGroups()
	var config *Config
	var parsedCommand *cli.Command
	command := &cli.Command{
		Name:                   "polly",
		Flags:                  flags,
		MutuallyExclusiveFlags: groups,
		Action: func(_ context.Context, cmd *cli.Command) error {
			config = parseConfig(cmd)
			parsedCommand = cmd
			return nil
		},
	}
	if err := command.Run(context.Background(), append([]string{"polly"}, args...)); err != nil {
		t.Fatal(err)
	}
	return config, parsedCommand
}

func assertResolvedConfig(t *testing.T, config *Config, modelOverride string) {
	t.Helper()
	wantModel := "anthropic/claude-sonnet-4-6"
	if modelOverride != "" {
		wantModel = modelOverride
	}
	if config.Model != wantModel || config.Temperature != 1 || config.MaxTokens != 64_000 ||
		config.MaxHistoryTokens != 256_000 || config.ThinkingEffort != "off" ||
		config.SystemPrompt != defaultSystemPrompt || config.ToolTimeout != 30*time.Second ||
		config.MaxIterations != 250 || len(config.SkillDirs) != 0 {
		t.Fatalf("resolved settings = %+v", config)
	}
}

func assertResolvedMetadata(t *testing.T, metadata *sessions.Metadata, modelOverride string) {
	t.Helper()
	if metadata == nil {
		t.Fatal("metadata is nil")
	}
	wantModel := "anthropic/claude-sonnet-4-6"
	if modelOverride != "" {
		wantModel = modelOverride
	}
	if metadata.Model != wantModel || metadata.Temperature != 1 || metadata.MaxTokens != 64_000 ||
		metadata.MaxHistoryTokens != 256_000 || metadata.ThinkingEffort != "off" ||
		metadata.SystemPrompt != defaultSystemPrompt || metadata.ToolTimeout != 30*time.Second ||
		metadata.MaxIterations != 250 || len(metadata.SkillDirs) != 0 {
		t.Fatalf("stored settings = %+v", metadata)
	}
}

func withStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	original := os.Stdin
	os.Stdin = reader
	defer func() {
		os.Stdin = original
		_ = reader.Close()
	}()
	fn()
}
