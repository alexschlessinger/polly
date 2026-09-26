package main

import (
	"context"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

func TestParseFastMode(t *testing.T) {
	for value, want := range map[string]bool{"on": true, "ON": true, "true": true, "yes": true, "1": true, "off": false, "false": false, "no": false, "0": false, " off ": false} {
		if got, err := parseFastMode(value); err != nil || got != want {
			t.Errorf("parseFastMode(%q) = %v, %v; want %v", value, got, err, want)
		}
	}
	for _, value := range []string{"", "maybe", "2"} {
		if _, err := parseFastMode(value); err == nil || !strings.Contains(err.Error(), "fast must be on or off") {
			t.Errorf("parseFastMode(%q) = %v, want a refusal", value, err)
		}
	}
}

func TestFastCommand(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "fast-test")
	settings := &Settings{Model: "openai/gpt-5.4", ThinkingEffort: "low"}
	applied := 0
	ctx := &replCommandContext{
		settings:        settings,
		state:           &conversationState{session: session},
		settingsApplied: func() { applied++ },
	}
	if got := strings.Join(dispatchDefaultCommandForTest(t, "/fast", ctx), "\n"); got != "fast: off" {
		t.Fatalf("current fast mode: %q", got)
	}
	dispatchDefaultCommandForTest(t, "/fast on", ctx)
	md, err := session.GetMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !settings.Fast || !md.Fast || applied != 1 {
		t.Fatalf("fast mode not applied and persisted: settings=%+v metadata=%+v applied=%d", settings, md, applied)
	}
	if got := strings.Join(dispatchDefaultCommandForTest(t, "/fast", ctx), "\n"); got != "fast: on" {
		t.Fatalf("fast mode after /fast on: %q", got)
	}
	for _, input := range []string{"/fast maybe", "/fast off extra"} {
		dispatchDefaultCommandForTest(t, input, ctx)
		if !settings.Fast || applied != 1 {
			t.Fatalf("invalid input changed fast mode: %s", input)
		}
	}
	dispatchDefaultCommandForTest(t, "/fast off", ctx)
	if settings.Fast || applied != 2 {
		t.Fatalf("fast mode not turned off: settings=%+v applied=%d", settings, applied)
	}

	// A provider without a fast tier refuses the setting and shows why a
	// saved one is not in effect.
	settings.Model = "anthropic/claude-sonnet-4-6"
	replies := dispatchDefaultCommandForTest(t, "/fast on", ctx)
	if settings.Fast || applied != 2 || !strings.Contains(strings.Join(replies, "\n"), "fast mode is not available for anthropic/ models") {
		t.Fatalf("anthropic accepted fast mode: settings=%+v applied=%d replies=%q", settings, applied, replies)
	}
	settings.Fast = true
	if got := strings.Join(dispatchDefaultCommandForTest(t, "/fast", ctx), "\n"); got != "fast: on (fast mode is not available for anthropic/ models)" {
		t.Fatalf("saved fast mode on anthropic: %q", got)
	}
}

func TestFastFlagSources(t *testing.T) {
	themeTestUnsetEnv(t, "POLLYTOOL_FAST")
	run := func(args ...string) (given, on bool) {
		t.Helper()
		cmd := getCommand()
		cmd.Action = func(ctx context.Context, cmd *cli.Command) error {
			given, on = flagGiven(cmd, "fast"), cmd.Bool("fast")
			return nil
		}
		if err := cmd.Run(context.Background(), append([]string{"polly"}, args...)); err != nil {
			t.Fatal(err)
		}
		return given, on
	}
	if given, on := run(); given || on {
		t.Fatalf("bare launch: given=%v on=%v", given, on)
	}
	if given, on := run("--fast"); !given || !on {
		t.Fatalf("--fast: given=%v on=%v", given, on)
	}
	t.Setenv("POLLYTOOL_FAST", "1")
	if given, on := run(); given || !on {
		t.Fatalf("POLLYTOOL_FAST=1: given=%v on=%v", given, on)
	}
}
