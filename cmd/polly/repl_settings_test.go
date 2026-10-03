package main

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestPreferenceShortcutsAreCompatibilityOnly(t *testing.T) {
	for _, name := range []string{"/effort", "/fast", "/keys"} {
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
	opened := false
	dispatchDefaultCommandForTest(t, "/keys", &replCommandContext{openKeyManager: func() { opened = true }})
	if !opened {
		t.Fatal("/keys no longer opens the model form at the key override")
	}
}

func TestModelPickerAndSetPersistTheSamePreferences(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	initial := Settings{Model: "openai/gpt-5.4", Temperature: 0.7, MaxHistoryTokens: 32000}
	picker := newManagedREPL(&Config{}, "picker-settings", 0, 0)
	picker.state = &conversationState{session: testAcquireSession(t, store, "picker-settings"), settings: initial}
	text := newManagedREPL(&Config{}, "text-settings", 0, 0)
	text.state = &conversationState{session: testAcquireSession(t, store, "text-settings"), settings: initial}
	budget := "64000"
	if err := picker.applySelectedModelHost("openrouter/openai/gpt-5.4", "example/turbo", 128000, &budget); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"/set model openrouter/openai/gpt-5.4", "/set modelhost example/turbo", "/set maxcontext 64000"} {
		text.runCommand(command)
	}
	if !reflect.DeepEqual(picker.state.settings, text.state.settings) {
		t.Fatalf("picker settings %+v differ from text settings %+v", picker.state.settings, text.state.settings)
	}
	pickerMeta, err := picker.state.session.GetMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	textMeta, err := text.state.session.GetMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range settingSpecs {
		if spec.parse != nil {
			var fromPicker, fromText Settings
			spec.fromMeta(&fromPicker, pickerMeta)
			spec.fromMeta(&fromText, textMeta)
			if !reflect.DeepEqual(fromPicker, fromText) {
				t.Fatalf("%s persisted differently: picker %+v, text %+v", spec.key, fromPicker, fromText)
			}
		}
	}
	for _, r := range []*managedREPL{picker, text} {
		if got := strings.Join(transcriptTexts(r.model), "\n"); !strings.Contains(got, "saved to this session") {
			t.Fatalf("missing save scope: %q", got)
		}
	}
}

func TestPreferenceChangesRejectInvalidValuesAndFailedSaves(t *testing.T) {
	for _, viaPicker := range []bool{false, true} {
		t.Run(map[bool]string{false: "set", true: "picker"}[viaPicker], func(t *testing.T) {
			store := testOpenMemoryStore(t, nil)
			r := newManagedREPL(&Config{}, "settings-failure", 0, 0)
			initial := Settings{Model: "openrouter/openai/gpt-5.4", ModelHost: "example/turbo", MaxHistoryTokens: 32000}
			r.state = &conversationState{session: testAcquireSession(t, store, "settings-failure"), settings: initial}
			ctx := newManagedReplCommandContext(r)
			applied := 0
			ctx.settingsApplied = func(bool) { applied++ }
			var err error
			if viaPicker {
				// The valid draft model must not land if the host is invalid.
				err = r.applySelectedModelHost("openai/gpt-5.4", "example/turbo", 0, nil)
			} else {
				_, err = applyAndPersistSetting(ctx, "modelhost", "invalid host")
			}
			if err == nil || !reflect.DeepEqual(r.state.settings, initial) || applied != 0 {
				t.Fatalf("invalid change: err=%v settings=%+v applied=%d", err, r.state.settings, applied)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if viaPicker {
				err = r.applySelectedModelHost("openai/gpt-5.4", "", 0, nil)
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
