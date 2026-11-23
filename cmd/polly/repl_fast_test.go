package main

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/urfave/cli/v3"
)

func TestParseSpeed(t *testing.T) {
	for value, want := range map[string]string{
		"fast": "priority", "on": "priority", "ON": "priority", "true": "priority", "yes": "priority", "1": "priority", "T": "priority",
		"default": "", "off": "", "false": "", "no": "", "0": "", " off ": "", "F": "",
		"ultrafast": "ultrafast", " UltraFast ": "ultrafast",
	} {
		if got, err := parseSpeed(value); err != nil || got != want {
			t.Errorf("parseSpeed(%q) = %q, %v; want %q", value, got, err, want)
		}
	}
	for _, value := range []string{"", "maybe", "2"} {
		if _, err := parseSpeed(value); err == nil || !strings.Contains(err.Error(), "default, fast, or ultrafast") {
			t.Errorf("parseSpeed(%q) = %v, want a refusal", value, err)
		}
	}
}

func TestSpeedCommand(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "fast-test")
	settings := &Settings{Model: "openai/gpt-6.2-astra", ThinkingEffort: "low"}
	applied := 0
	ctx := &replCommandContext{
		settings:        settings,
		state:           &conversationState{session: session},
		settingsApplied: func(bool) { applied++ },
	}
	run := func(command string) string {
		t.Helper()
		return strings.Join(dispatchDefaultCommandForTest(t, command, ctx), "\n")
	}
	if got := run("/speed"); got != "speed: Default" {
		t.Fatalf("current speed: %q", got)
	}
	for i, step := range []struct{ command, tier, display string }{
		{"/speed fast", llm.ServiceTierPriority, "Fast"},
		{"/set speed ultrafast", llm.ServiceTierUltrafast, "Ultrafast"},
		{"/speed default", "", "Default"},
		{"/fast on", llm.ServiceTierPriority, "Fast"},
		{"/set fast off", "", "Default"},
		{"/speed ultrafast", llm.ServiceTierUltrafast, "Ultrafast"},
	} {
		if got := run(step.command); got != "speed: "+step.display+" (saved to this session)" {
			t.Fatalf("after %q: reply=%q", step.command, got)
		}
		if got := run("/set speed"); got != "speed: "+step.display || settings.ServiceTier != step.tier || applied != i+1 {
			t.Fatalf("after %q: display=%q tier=%q applied=%d", step.command, got, settings.ServiceTier, applied)
		}
		md, err := session.GetMetadata(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		wantFast := step.tier == llm.ServiceTierPriority
		wantNamed := step.tier
		if wantFast {
			wantNamed = ""
		}
		if md.Fast != wantFast || md.ServiceTier != wantNamed {
			t.Fatalf("after %q: persisted fast=%v serviceTier=%q", step.command, md.Fast, md.ServiceTier)
		}
	}
	for _, input := range []string{"/speed maybe", "/speed default extra"} {
		run(input)
		if settings.ServiceTier != llm.ServiceTierUltrafast || applied != 6 {
			t.Fatalf("invalid input changed speed: %s", input)
		}
	}
	if !defaultReplCommands.busySafeCommand("/speed") || defaultReplCommands.busySafeCommand("/speed fast") {
		t.Fatal("only a read of Speed should be allowed during a turn")
	}
	if got := run("/set"); !strings.Contains(got, "  speed: Ultrafast") || strings.Contains(got, "  fast:") {
		t.Fatalf("preferences should list Speed once: %q", got)
	}

	// Unsupported routes refuse explicit edits, without erasing the saved tier.
	for _, target := range []struct{ model, endpoint, choice, reason string }{
		{"openai/gpt-5.4", "", "ultrafast", "not available for openai/gpt-5.4"},
		{"openai/gpt-6.2-astra", "https://custom.example/v1", "ultrafast", "Chat Completions endpoints"},
		{"anthropic/claude-sonnet-4-6", "", "fast", "not available for anthropic/ models"},
	} {
		settings.Model, ctx.config = target.model, &Config{BaseURL: target.endpoint}
		if msg := run("/speed " + target.choice); settings.ServiceTier != llm.ServiceTierUltrafast || applied != 6 || !strings.Contains(msg, target.reason) {
			t.Fatalf("%+v accepted the tier or failed silently: %q", target, msg)
		}
		if got := run("/speed"); got != "speed: Default · Ultrafast saved, unavailable for this model or endpoint" {
			t.Fatalf("saved Ultrafast on %+v: %q", target, got)
		}
	}
}

func TestSpeedCompletionUsesTheCurrentRoute(t *testing.T) {
	for _, tc := range []struct {
		model, endpoint string
		want            []string
	}{
		{"openai/gpt-6.2-astra", "", []string{"default", "fast", "ultrafast"}},
		{"openai/gpt-5.4", "", []string{"default", "fast"}},
		{"openai/gpt-6.2-astra", "https://custom.example/v1", []string{"default", "fast"}},
		{"anthropic/claude-sonnet-4-6", "", []string{"default"}},
		{"codex/cold-catalog", "", []string{"default", "fast"}},
	} {
		ctx := &replCommandContext{settings: &Settings{Model: tc.model}, config: &Config{BaseURL: tc.endpoint}}
		if got := completeSpeedCommand(ctx, []string{"/speed"}, ""); !slices.Equal(got, tc.want) {
			t.Errorf("/speed completion for %s at %q = %q, want %q", tc.model, tc.endpoint, got, tc.want)
		}
		wantPrefix := []string(nil)
		if slices.Contains(tc.want, "ultrafast") {
			wantPrefix = []string{"ultrafast"}
		}
		if got := completeSetCommand(ctx, []string{"/set", "speed", "u"}, "u"); !slices.Equal(got, wantPrefix) {
			t.Errorf("/set speed u completion for %s at %q = %q, want %q", tc.model, tc.endpoint, got, wantPrefix)
		}
	}
}

type codexTierFixture struct {
	captureCompletionLLM
	reads int
}

func (m *codexTierFixture) GetModelInfo(_ context.Context, target llm.ModelTarget) (*llm.ModelInfo, error) {
	m.reads++
	return &llm.ModelInfo{ID: target.Model, ModelCapabilities: llm.ModelCapabilities{ServiceTiers: []string{llm.ServiceTierUltrafast}}}, nil
}

func TestSpeedEditChecksColdCodexCatalogButReadsDoNot(t *testing.T) {
	fixture := &codexTierFixture{}
	agent := llm.NewAgent(fixture, nil, llm.AgentConfig{})
	defer agent.Close()
	ctx := &replCommandContext{settings: &Settings{Model: "codex/plan-model", ServiceTier: llm.ServiceTierUltrafast}, state: &conversationState{agent: agent}}
	if got := speedWordsFor(ctx); slices.Contains(got, "ultrafast") || fixture.reads != 0 {
		t.Fatalf("cold completions = %q; catalog reads = %d", got, fixture.reads)
	}
	if got := speedDisplay(ctx, ctx.settings); got != "Ultrafast saved · availability not yet confirmed for this account" || fixture.reads != 0 {
		t.Fatalf("cold display = %q; catalog reads = %d", got, fixture.reads)
	}
	if err := validateSpeed(ctx, "ultrafast"); err != nil || fixture.reads != 1 {
		t.Fatalf("cold catalog edit: reads=%d, err=%v", fixture.reads, err)
	}
}

func TestSpeedFlagSources(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	themeTestUnsetEnv(t, "POLLYTOOL_FAST")
	themeTestUnsetEnv(t, "POLLYTOOL_SPEED")
	registry := tools.NewToolRegistry(nil)
	defer registry.Close()
	check := func(wantTier string, wantGiven bool, flags ...string) {
		t.Helper()
		config, cmd := parseEnvTestConfig(t, append(flags, "positional prompt")...)
		if args := cmd.Args().Slice(); !slices.Equal(args, []string{"positional prompt"}) {
			t.Errorf("%q left positional args %q", flags, args)
		}
		req := createCompletionRequest(config, &config.Launch, nil, registry, nil, nil)
		spec, _ := settingSpecFor("fast")
		if given := spec.flagSet(cmd); req.ServiceTier != wantTier || given != wantGiven {
			t.Errorf("%q: tier=%q given=%v; want %q, %v", flags, req.ServiceTier, given, wantTier, wantGiven)
		}
	}
	check("", false)
	check("priority", true, "--speed", "fast")
	check("ultrafast", true, "--speed=ultrafast")
	check("", true, "--speed", "default")
	check("priority", true, "--fast")
	check("ultrafast", true, "--fast=ultrafast")
	for _, env := range []string{"1", "T", "true"} {
		t.Setenv("POLLYTOOL_FAST", env)
		check("priority", false)
	}
	for _, env := range []string{"0", "F", "false", ""} {
		t.Setenv("POLLYTOOL_FAST", env)
		check("", false)
	}
	t.Setenv("POLLYTOOL_FAST", "ultrafast")
	check("ultrafast", false)
	check("priority", true, "--fast")
	check("", true, "--fast=false")

	t.Setenv("POLLYTOOL_FAST", "invalid")
	check("priority", true, "--fast") // explicit arguments ignore invalid defaults
	check("ultrafast", true, "--speed", "ultrafast")
	for _, args := range [][]string{{"polly"}, {"polly", "--fast=invalid"}, {"polly", "--speed=invalid"}, {"polly", "--speed="}} {
		cmd := getCommand()
		cmd.Action = func(context.Context, *cli.Command) error { return nil }
		if err := cmd.Run(context.Background(), args); err == nil || !strings.Contains(err.Error(), "default, fast, or ultrafast") {
			t.Fatalf("%q: invalid tier not refused: %v", args, err)
		}
	}

	if err := writeUserConfig(filepath.Join(home, userConfigDirName, userConfigFileName), map[string]string{"POLLYTOOL_SPEED": "ultrafast"}); err != nil {
		t.Fatal(err)
	}
	check("ultrafast", false)
	t.Setenv("POLLYTOOL_SPEED", "default")
	check("", false)
	check("priority", true, "--fast")
	check("ultrafast", true, "--speed", "ultrafast")
}

func TestSpeedAndLegacyFlagsConflict(t *testing.T) {
	for _, flags := range [][]string{{"--fast", "--speed=ultrafast"}, {"--speed", "ultrafast", "--fast"}, {"--fast=false", "--speed=default"}} {
		cmd := getCommand()
		cmd.Action = func(context.Context, *cli.Command) error { return nil }
		if err := cmd.Run(context.Background(), append([]string{"polly"}, flags...)); err == nil || !strings.Contains(err.Error(), "--speed and --fast cannot be used together") {
			t.Fatalf("%q: expected conflicting flags error, got %v", flags, err)
		}
	}
}

func TestSpeedStatusFollowsModelAndPreference(t *testing.T) {
	const supported = "openai/gpt-6.2-astra"
	r := newManagedREPL(&Config{Launch: Settings{Model: supported}}, "speed-status", 0, 0)
	r.state = &conversationState{settings: r.config.Launch}
	for _, step := range []struct{ command, modelField string }{
		{"/speed fast", "gpt-6.2-astra [Fast]"},
		{"/speed ultrafast", "gpt-6.2-astra [Ultrafast]"},
		{"/set model openai/gpt-5.4", "gpt-5.4"},
		{"/set model " + supported, "gpt-6.2-astra [Ultrafast]"},
		{"/speed default", "gpt-6.2-astra"},
	} {
		r.runCommand(step.command)
		r.refreshStatusSpeed()
		row := []rune(plainStyledText(r.model.statusRow(100)))
		placement := r.model.status.modelField
		if got := string(row[placement.X : placement.X+placement.Cols]); got != step.modelField {
			t.Fatalf("%s: model field = %q, want %q", step.command, got, step.modelField)
		}
	}
}

func TestSpeedFlagsOnLegacySession(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "legacy")
	ctx := context.Background()
	if err := updateMetadata(ctx, session, func(md *sessions.Metadata) { md.Fast = true }); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POLLYTOOL_SPEED", "ultrafast")
	for _, tc := range []struct{ flag, want string }{
		{"", llm.ServiceTierPriority}, // environment doesn't override a saved bool
		{"--speed=ultrafast", llm.ServiceTierUltrafast},
		{"--speed=default", ""},
		{"--fast=false", ""},
	} {
		var args []string
		if tc.flag != "" {
			args = []string{tc.flag}
		}
		config, cmd := parseEnvTestConfig(t, args...)
		opener := conversationOpener{config: config, sessionStore: store, cmd: cmd}
		if _, got, err := opener.prepare(ctx, "legacy", nil); err != nil || got.ServiceTier != tc.want {
			t.Fatalf("%q: service tier = %q, %v; want %q", tc.flag, got.ServiceTier, err, tc.want)
		}
	}
}
