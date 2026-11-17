package main

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestPreferenceShortcutsAreCompatibilityOnly(t *testing.T) {
	for _, name := range []string{"/effort", "/fast", "/keys", "/model"} {
		if _, ok := defaultReplCommands.get(name); !ok {
			t.Fatalf("compatibility command %s no longer dispatches", name)
		}
		if slices.Contains(defaultReplCommands.commandNames(), name) {
			t.Fatalf("compatibility command %s is advertised", name)
		}
		for _, markup := range []bool{false, true} {
			if help := strings.Join(defaultReplCommands.helpLinesStyled(markup), "\n"); strings.Contains(help, name) {
				t.Fatalf("help advertises %s: %q", name, help)
			}
		}
	}
	openedKeys := false
	dispatchDefaultCommandForTest(t, "/keys", &replCommandContext{openKeyManager: func() { openedKeys = true }})
	if !openedKeys {
		t.Fatal("/keys no longer opens the setup form at the key override")
	}
	openedSetup := false
	dispatchDefaultCommandForTest(t, "/model", &replCommandContext{openSetup: func() { openedSetup = true }})
	if !openedSetup {
		t.Fatal("/model no longer routes through /setup")
	}
}

func TestSetupFormAndSetPersistTheSamePreferences(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := testOpenMemoryStore(t, nil)
	initial := Settings{Model: "openai/gpt-5.4", Temperature: 0.7, MaxHistoryTokens: 32000, ThinkingEffort: "high"}
	setupREPL := newManagedREPL(&Config{}, "setup-settings", 0, 0)
	setupREPL.state = &conversationState{session: testAcquireSession(t, store, "setup-settings"), settings: initial}
	text := newManagedREPL(&Config{}, "text-settings", 0, 0)
	text.state = &conversationState{session: testAcquireSession(t, store, "text-settings"), settings: initial}

	budget := "64000"
	if err := setupREPL.applySetupSessionSettings("openrouter/openai/gpt-5.4", "example/turbo", "medium", 128000, &budget); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"/set model openrouter/openai/gpt-5.4", "/set modelhost example/turbo", "/set maxcontext 64000", "/set effort medium"} {
		text.runCommand(command)
	}
	if !reflect.DeepEqual(setupREPL.state.settings, text.state.settings) {
		t.Fatalf("setup settings %+v differ from text settings %+v", setupREPL.state.settings, text.state.settings)
	}
	setupMeta, err := setupREPL.state.session.GetMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	textMeta, err := text.state.session.GetMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range settingSpecs {
		if spec.parse != nil {
			var fromSetup, fromText Settings
			spec.fromMeta(&fromSetup, setupMeta)
			spec.fromMeta(&fromText, textMeta)
			if !reflect.DeepEqual(fromSetup, fromText) {
				t.Fatalf("%s persisted differently: setup %+v, text %+v", spec.key, fromSetup, fromText)
			}
		}
	}
	if got := strings.Join(transcriptTexts(text.model), "\n"); !strings.Contains(got, "saved to this session") {
		t.Fatalf("missing save scope: %q", got)
	}
}

func TestPreferenceChangesRejectInvalidValuesAndFailedSaves(t *testing.T) {
	for _, viaSetup := range []bool{false, true} {
		t.Run(map[bool]string{false: "set", true: "setup"}[viaSetup], func(t *testing.T) {
			store := testOpenMemoryStore(t, nil)
			r := newManagedREPL(&Config{}, "settings-failure", 0, 0)
			initial := Settings{Model: "openrouter/openai/gpt-5.4", ModelHost: "example/turbo", MaxHistoryTokens: 32000, ThinkingEffort: "high"}
			r.state = &conversationState{session: testAcquireSession(t, store, "settings-failure"), settings: initial}
			ctx := newManagedReplCommandContext(r)
			applied := 0
			ctx.settingsApplied = func(bool) { applied++ }
			var err error
			if viaSetup {
				// The valid draft model must not land if the host is invalid.
				err = r.applySetupSessionSettings("openai/gpt-5.4", "example/turbo", "high", 0, nil)
			} else {
				_, err = applyAndPersistSetting(ctx, "modelhost", "invalid host")
			}
			if err == nil || !reflect.DeepEqual(r.state.settings, initial) || applied != 0 {
				t.Fatalf("invalid change: err=%v settings=%+v applied=%d", err, r.state.settings, applied)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if viaSetup {
				err = r.applySetupSessionSettings("openai/gpt-5.4", "", "high", 0, nil)
			} else {
				_, err = applyAndPersistSetting(ctx, "model", "openai/gpt-5.4")
			}
			if err == nil || !strings.Contains(err.Error(), "settings unchanged: saving to this session failed") || !reflect.DeepEqual(r.state.settings, initial) || applied != 0 {
				t.Fatalf("failed save: err=%v settings=%+v applied=%d", err, r.state.settings, applied)
			}
			if got := strings.Join(transcriptTexts(r.model), "\n"); strings.Contains(got, "saved to this session") {
				t.Fatalf("failed save reported success: %q", got)
			}
		})
	}
}

func TestSetWithoutStorageDoesNotClaimASave(t *testing.T) {
	ctx := &replCommandContext{settings: &Settings{}}
	got := strings.Join(dispatchDefaultCommandForTest(t, "/set temp 1.0", ctx), "\n")
	if ctx.settings.Temperature != 1 || !strings.Contains(got, "applied for this run; no session to save") {
		t.Fatalf("unstored change scope: settings=%+v reply=%q", ctx.settings, got)
	}
}
