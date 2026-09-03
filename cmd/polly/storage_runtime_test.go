package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/urfave/cli/v3"
)

func TestMetadataFromConfigSeedsDefaultNativeTools(t *testing.T) {
	want := make([]tools.ToolLoaderInfo, 0, len(defaultNativeToolNames))
	for _, name := range defaultNativeToolNames {
		want = append(want, tools.ToolLoaderInfo{Name: name, Type: "native", Source: "builtin"})
	}

	metadata := metadataFromConfig(&Config{})
	if !reflect.DeepEqual(metadata.ActiveTools, want) {
		t.Fatalf("default active tools = %#v, want %#v", metadata.ActiveTools, want)
	}

	metadata = metadataFromConfig(&Config{Tools: []string{"custom.json"}})
	if metadata.ActiveTools != nil {
		t.Fatalf("explicit tool metadata was preseeded with defaults: %#v", metadata.ActiveTools)
	}
}

func TestInitializeSessionAppliesNativeToolDefaultsOnlyToFreshContexts(t *testing.T) {
	t.Run("fresh context", func(t *testing.T) {
		config := &Config{NoSandbox: true, NoSkills: true}
		store := testOpenMemoryStore(t, metadataFromConfig(config))
		session, registry := initializeToolDefaultsTestSession(t, config, store, "fresh")
		defer func() { _ = session.Close() }()
		defer func() { _ = registry.Close() }()

		for _, name := range defaultNativeToolNames {
			if _, ok := registry.Get(name); !ok {
				t.Errorf("fresh context did not load default tool %q", name)
			}
		}
	})

	t.Run("existing empty context", func(t *testing.T) {
		store := testOpenMemoryStore(t, nil)
		existing := testAcquireSession(t, store, "legacy-empty")
		if err := existing.Close(); err != nil {
			t.Fatal(err)
		}

		config := &Config{NoSandbox: true, NoSkills: true}
		session, registry := initializeToolDefaultsTestSession(t, config, store, "legacy-empty")
		defer func() { _ = session.Close() }()
		defer func() { _ = registry.Close() }()

		for _, name := range defaultNativeToolNames {
			if _, ok := registry.Get(name); ok {
				t.Errorf("existing empty context unexpectedly enabled default tool %q", name)
			}
		}
	})

	t.Run("explicit tools replace defaults", func(t *testing.T) {
		config := &Config{NoSandbox: true, NoSkills: true, Tools: []string{"read_file"}}
		store := testOpenMemoryStore(t, metadataFromConfig(config))
		session, registry := initializeToolDefaultsTestSession(t, config, store, "explicit")
		defer func() { _ = session.Close() }()
		defer func() { _ = registry.Close() }()

		if _, ok := registry.Get("read_file"); !ok {
			t.Error("explicit read_file tool was not loaded")
		}
		for _, name := range defaultNativeToolNames {
			if name == "read_file" {
				continue
			}
			if _, ok := registry.Get(name); ok {
				t.Errorf("explicit tool selection unexpectedly retained default tool %q", name)
			}
		}
	})
}

func initializeToolDefaultsTestSession(t *testing.T, config *Config, store sessions.SessionStore, name string) (sessions.Session, *tools.ToolRegistry) {
	t.Helper()
	state, err := newConversationState(context.Background(), config, nil, store, name, false, getCommand(), nil)
	if err != nil {
		t.Fatalf("newConversationState(%q): %v", name, err)
	}
	return state.session, state.toolRegistry
}

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
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home) // Windows os.UserHomeDir
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

func TestFreshContextSeedsPersonaOnly(t *testing.T) {
	// No -s: the display contract is composed at send time, so nothing is
	// stored and no system message is seeded.
	t.Run("default", func(t *testing.T) {
		config, _ := parseStorageTestConfig(t)
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
		if metadata.SystemPrompt != "" {
			t.Fatalf("stored system prompt = %q, want empty", metadata.SystemPrompt)
		}
		if history := testSessionHistory(t, session); len(history) != 0 {
			t.Fatalf("fresh history = %#v, want empty", history)
		}
	})

	// An explicit persona is still seeded into the transcript as message 0.
	t.Run("persona", func(t *testing.T) {
		config, _ := parseStorageTestConfig(t, "--system", "be a pirate")
		store, err := setupSessionStore(config, "", false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		session := testAcquireSession(t, store, "fresh-persona")

		history := testSessionHistory(t, session)
		want := []messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: "be a pirate"}}
		if !reflect.DeepEqual(history, want) {
			t.Fatalf("fresh persona history = %#v, want %#v", history, want)
		}
	})
}

// TestUpdateContextInfoPreservesStoredSettingsWithoutFlags proves the startup
// write-back is a no-op for an untouched existing context: every flagged row
// restores from metadata first, so copying it back stores the same value.
func TestUpdateContextInfoPreservesStoredSettingsWithoutFlags(t *testing.T) {
	store := testOpenMemoryStore(t, &sessions.Metadata{
		Model:          "openai/gpt-5.4",
		SystemPrompt:   "be a pirate",
		ThinkingEffort: "",
	})
	session := testAcquireSession(t, store, "untouched")
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	config := &Config{Settings: Settings{
		Model:            "default/model",
		MaxHistoryTokens: 256_000,
		ThinkingEffort:   "high",
		SystemPrompt:     "default system",
	}}
	cmd := getCommand()
	if _, _, err := initializeConversation(context.Background(), config, store, "untouched", cmd); err != nil {
		t.Fatal(err)
	}
	if config.Model != "openai/gpt-5.4" || config.MaxHistoryTokens != 0 || config.ThinkingEffort != "" || config.SystemPrompt != "be a pirate" {
		t.Fatalf("resolved settings did not restore the stored values: %+v", config.Settings)
	}

	session = testAcquireSession(t, store, "untouched")
	md, err := session.GetMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := updateContextInfo(context.Background(), session, md, config, cmd); err != nil {
		t.Fatal(err)
	}
	stored, err := session.GetMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stored.Model != "openai/gpt-5.4" || stored.MaxHistoryTokens != 0 || stored.ThinkingEffort != "" ||
		stored.SystemPrompt != "be a pirate" {
		t.Fatalf("startup write-back rewrote stored settings: %+v", stored)
	}
}

// legacySystemPromptDefault is the pre-display-contract default prompt that
// databases written before the split stored verbatim; the session store's
// schema v2 migration strips it on open.
const legacySystemPromptDefault = "Your output will be displayed in a unix terminal. Be terse, 512 characters max. Do not use markdown."

// A context carrying the legacy default in a schema-v1 database is migrated
// on open: it resolves as persona-less, keeps its history, and -s "" against
// it does not trigger the prompt-change reset that would wipe that history.
func TestInitializeConversationResumesMigratedLegacyContext(t *testing.T) {
	for _, tt := range []struct {
		name        string
		setEmptyArg bool
	}{
		{name: "resume without -s"},
		{name: "explicit empty -s", setEmptyArg: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.db")
			store := testOpenDiskStore(t, path, &sessions.Metadata{SystemPrompt: legacySystemPromptDefault})
			session := testAcquireSession(t, store, "legacy")
			testAddMessage(t, session, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "old turn"})
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			// Rewind the schema version so the reopen migrates, exactly as a
			// database written before the display-contract split would.
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec("PRAGMA user_version = 1"); err != nil {
				t.Fatal(err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			store = testOpenDiskStore(t, path, nil)

			cmd := getCommand()
			if tt.setEmptyArg {
				if err := cmd.Set("system", ""); err != nil {
					t.Fatal(err)
				}
			}
			config := &Config{}
			if _, _, err := initializeConversation(context.Background(), config, store, "legacy", cmd); err != nil {
				t.Fatal(err)
			}
			if config.Settings.SystemPrompt != "" {
				t.Fatalf("resolved system prompt = %q, want empty", config.Settings.SystemPrompt)
			}

			reopened := testAcquireSession(t, store, "legacy")
			history := testSessionHistory(t, reopened)
			if len(history) != 1 || history[0].Content != "old turn" {
				t.Fatalf("history after migrated resume = %#v, want the user turn alone", history)
			}
		})
	}
}

// --add stores a text file as one text part carrying FileName, with the
// filename boundary kept in the provider-visible text, so the resumed REPL
// compacts it to "[attached: name]" without parsing the header.
func TestHandleAddToContextStoresTextFileAsPart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("SECRET BODY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := testOpenMemoryStore(t, nil)
	config := &Config{Files: []string{path}}
	withStdin(t, "from stdin\n", func() {
		if err := handleAddToContext(context.Background(), store, config, "imported"); err != nil {
			t.Fatal(err)
		}
	})

	history := testSessionHistory(t, testAcquireSession(t, store, "imported"))
	if len(history) != 2 {
		t.Fatalf("imported history = %#v, want the stdin note and the file", history)
	}
	file := history[1]
	want := []messages.ContentPart{{Type: "text", Text: "=== notes.txt ===\nSECRET BODY\n", FileName: "notes.txt"}}
	if file.Content != "" || !reflect.DeepEqual(file.Parts, want) {
		t.Fatalf("imported file message = %#v, want parts %#v", file, want)
	}
	if imported, _ := file.Metadata[messages.MetadataKeyContextImport].(bool); !imported {
		t.Fatalf("imported file message lacks the context-import flag: %#v", file.Metadata)
	}
	if display, restorable, contextOnly := historyUserSummary(file); display != "[attached: notes.txt]" || restorable || !contextOnly {
		t.Fatalf("hydrated summary = %q restorable=%v contextOnly=%v", display, restorable, contextOnly)
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
		config.SystemPrompt != "" || config.ToolTimeout != 5*time.Minute ||
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
		metadata.SystemPrompt != "" || metadata.ToolTimeout != 5*time.Minute ||
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
