package main

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestCLIRestoreSelectionPersistsAcrossResumeAndDefaults(t *testing.T) {
	for _, tc := range []struct {
		name        string
		selection   []string
		wantRestore bool
		legacy      bool
		loaders     []tools.ToolLoaderInfo
	}{
		{name: "defaults", wantRestore: true},
		{name: "restore only", selection: []string{llm.BuiltinRestoreArtifact}, wantRestore: true},
		{name: "combined", selection: []string{"read_file", llm.BuiltinRestoreArtifact}, wantRestore: true},
		{name: "duplicate restore", selection: []string{llm.BuiltinRestoreArtifact, llm.BuiltinRestoreArtifact}, wantRestore: true},
		{name: "read only", selection: []string{"read_file"}},
		{name: "legacy empty", legacy: true},
		{name: "legacy read only", legacy: true, loaders: []tools.ToolLoaderInfo{{Name: "read_file", Type: "native", Source: "builtin"}}},
		{name: "legacy restore only", legacy: true, loaders: []tools.ToolLoaderInfo{restoreToolLoader}, wantRestore: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := &Config{NoSandbox: true, NoSkills: true, Tools: tc.selection}
			metadata := metadataFromConfig(config)
			if tc.legacy {
				metadata = &sessions.Metadata{ActiveTools: tc.loaders}
			}
			store := testOpenMemoryStore(t, metadata)
			if tc.legacy {
				existing := testAcquireSession(t, store, "restore-selection")
				if err := existing.Close(); err != nil {
					t.Fatal(err)
				}
			}
			open := func(config *Config) *conversationState {
				state, err := (&conversationOpener{config: config, sessionStore: store, cmd: getCommand()}).openNew(context.Background(), "restore-selection", false)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = state.Close() })
				return state
			}
			check := func(state *conversationState) {
				t.Helper()
				metadata, err := state.session.GetMetadata(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				settings := state.settings
				settings.MaxIterations = 17
				settings.ToolTimeout = 3 * time.Second
				updated := state.agentConfig(settings)
				if updated.MaxIterations != 17 || updated.ToolTimeout != 3*time.Second {
					t.Fatalf("reconfigured defaults lost settings: %#v", updated)
				}
				// Swarm supplies the member store after taking these defaults.
				updated.ArtifactStore = state.artifactStore
				reconfigured := llm.NewAgent(nil, state.toolRegistry, updated)
				defer reconfigured.Close()
				if _, ok := state.effectiveTools().Get(llm.BuiltinRestoreArtifact); ok != tc.wantRestore {
					t.Errorf("restore visible=%v, want %v", ok, tc.wantRestore)
				}
				if _, ok := reconfigured.ToolRegistry().Get(llm.BuiltinRestoreArtifact); ok != tc.wantRestore {
					t.Errorf("reconfigured restore visible=%v, want %v", ok, tc.wantRestore)
				}
				if slices.Contains(updated.BuiltinTools(), llm.BuiltinRestoreArtifact) != tc.wantRestore {
					t.Errorf("reconfigured catalog %v does not match restore selection %v", updated.BuiltinTools(), tc.wantRestore)
				}
				if state.toolRegistry.HasNativeTool(llm.BuiltinRestoreArtifact) {
					t.Error("private restore installed as native constructor")
				}
				if _, ok := state.toolRegistry.Get(llm.BuiltinRestoreArtifact); ok {
					t.Error("private restore leaked into shared registry")
				}
				if slices.Contains(metadata.ActiveTools, restoreToolLoader) != tc.wantRestore {
					t.Errorf("persisted tool selection = %#v, want restore selection %v", metadata.ActiveTools, tc.wantRestore)
				}
				for _, name := range []string{llm.BuiltinListArtifacts, llm.BuiltinReadArtifact, llm.BuiltinReadTranscript} {
					if _, ok := state.effectiveTools().Get(name); !ok {
						t.Errorf("recall tool %s lost to mutation selection", name)
					}
					if _, ok := reconfigured.ToolRegistry().Get(name); !ok {
						t.Errorf("recall tool %s lost to reconfigured mutation selection", name)
					}
				}
				updateSwarmDefaults(state, &llm.CompletionRequest{}, settings)
			}
			state := open(config)
			check(state)
			if err := state.Close(); err != nil {
				t.Fatal(err)
			}
			// A resume without --tool must retain the saved private selection.
			state = open(&Config{NoSandbox: true, NoSkills: true})
			check(state)
		})
	}
}

func TestCreateContextPersistsExplicitRestoreSelection(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	config, _ := parseStorageTestConfig(t)
	config.NoSandbox = true
	config.Tools = []string{llm.BuiltinRestoreArtifact}
	if err := handleCreateContext(context.Background(), store, config, "created-restore"); err != nil {
		t.Fatal(err)
	}
	metadata, err := store.GetAllMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []tools.ToolLoaderInfo{restoreToolLoader}
	if got := metadata["created-restore"].ActiveTools; !slices.Equal(got, want) {
		t.Fatalf("created restore selection = %#v, want %#v", got, want)
	}
}
