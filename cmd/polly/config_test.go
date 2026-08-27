package main

import (
	"context"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

func TestValidateModel(t *testing.T) {
	tests := []struct {
		name    string
		model   string
		wantErr string
	}{
		{name: "empty uses default", model: ""},
		{name: "known provider", model: "openai/gpt-5.4"},
		{name: "missing provider prefix", model: "gpt-5.4", wantErr: "model must include provider prefix"},
		{name: "unknown provider", model: "custom/model", wantErr: "unknown provider 'custom'"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateModel(tt.model)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateModel(%q) error = %v", tt.model, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateModel(%q) error = %v, want substring %q", tt.model, err, tt.wantErr)
			}
		})
	}
}

func TestValidateTemperature(t *testing.T) {
	tests := []struct {
		name    string
		temp    float64
		wantErr string
	}{
		{name: "lower bound", temp: 0.0},
		{name: "upper bound", temp: 2.0},
		{name: "below range", temp: -0.1, wantErr: "temperature must be between 0.0 and 2.0"},
		{name: "above range", temp: 2.1, wantErr: "temperature must be between 0.0 and 2.0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTemperature(tt.temp)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateTemperature(%v) error = %v", tt.temp, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateTemperature(%v) error = %v, want substring %q", tt.temp, err, tt.wantErr)
			}
		})
	}
}

func TestConfigFlagsRejectPromptAndFileOnManagementCommands(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "reset rejects prompt",
			args:    []string{"--reset", "ctx", "--prompt", "hello"},
			wantErr: "--reset does not take prompts or files",
		},
		{
			name:    "listskills rejects file",
			args:    []string{"--listskills", "--file", "README.md"},
			wantErr: "--listskills does not take prompts or files",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runConfigValidationCommand(tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("run error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestConfigFlagsRejectPurgeWithOtherFlags(t *testing.T) {
	tests := [][]string{
		{"--purge", "--model", "openai/gpt-5.4"},
		{"--purge", "--confirm"},
		{"--purge", "--meta"},
		{"--purge", "--sandbox", "base"},
		{"--purge", "--nosandbox"},
		{"--purge", "--denypath", "/secrets"},
		{"--purge", "--writepath", "/output"},
		{"--purge", "--allownet"},
	}
	for _, args := range tests {
		err := runConfigValidationCommand(args...)
		if err == nil || !strings.Contains(err.Error(), "--purge must be used alone") {
			t.Errorf("run(%v) error = %v, want purge validation error", args, err)
		}
	}

	for _, args := range [][]string{{"--purge", "--quiet"}, {"--purge", "--debug"}} {
		if err := runConfigValidationCommand(args...); err != nil {
			t.Errorf("run(%v) error = %v, want quiet/debug allowed with purge", args, err)
		}
	}
}

func TestDefineFlagsWithGroupsContextManagementMutex(t *testing.T) {
	_, groups := defineFlagsWithGroups()
	if len(groups) != 1 {
		t.Fatalf("len(groups) = %d, want 1", len(groups))
	}

	got := make([]string, 0, len(groups[0].Flags))
	for _, flagSet := range groups[0].Flags {
		if len(flagSet) != 1 {
			t.Fatalf("len(flagSet) = %d, want 1", len(flagSet))
		}
		got = append(got, flagSet[0].Names()[0])
	}

	want := []string{"reset", "purge", "create", "show", "list", "delete", "add"}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q (full=%v)", i, got[i], want[i], got)
		}
	}
}

func TestParseConfigMeta(t *testing.T) {
	var got bool
	cmd := &cli.Command{
		Flags: func() []cli.Flag { f, _ := defineFlagsWithGroups(); return f }(),
		Action: func(_ context.Context, c *cli.Command) error {
			got = parseConfig(c).Meta
			return nil
		},
	}
	if err := cmd.Run(context.Background(), []string{"polly", "--meta", "-p", "hi"}); err != nil {
		t.Fatalf("run error = %v", err)
	}
	if !got {
		t.Fatal("Meta = false, want true when --meta is set")
	}
}

func TestSandboxPresetFlagDefaultsAndValidation(t *testing.T) {
	var parsed *Config
	flags, groups := defineFlagsWithGroups()
	cmd := &cli.Command{
		Name:                   "polly",
		Flags:                  flags,
		MutuallyExclusiveFlags: groups,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			parsed = parseConfig(cmd)
			return nil
		},
	}

	if err := cmd.Run(context.Background(), []string{"polly"}); err != nil {
		t.Fatalf("run error = %v", err)
	}
	if parsed.SandboxPreset != defaultSandboxPreset {
		t.Fatalf("SandboxPreset = %q, want default %q", parsed.SandboxPreset, defaultSandboxPreset)
	}

	if err := cmd.Run(context.Background(), []string{"polly", "--sandbox", "readonly", "--writepath", "/data", "--allownet"}); err != nil {
		t.Fatalf("run error = %v", err)
	}
	if parsed.SandboxPreset != "readonly" || !parsed.AllowNet || len(parsed.WritePaths) != 1 || parsed.WritePaths[0] != "/data" {
		t.Fatalf("parsed sandbox flags = preset %q, allownet %v, writepaths %v",
			parsed.SandboxPreset, parsed.AllowNet, parsed.WritePaths)
	}

	// A typo'd preset must fail flag validation, not run with another policy.
	if err := runConfigValidationCommand("--sandbox", "workspace+typo"); err == nil || !strings.Contains(err.Error(), "unknown sandbox preset") {
		t.Fatalf("run error = %v, want unknown-preset validation error", err)
	}
}

func TestSandboxPresetFlagValidationDoesNotInspectWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows os.UserHomeDir
	t.Setenv("POLLYTOOL_SANDBOX", defaultSandboxPreset)
	t.Chdir(home)

	// This command never constructs a sandbox. Supplying the documented default
	// through the environment must not turn flag validation into a workspace
	// scan (which rejects a home-directory cwd).
	if err := runConfigValidationCommand("--list"); err != nil {
		t.Fatalf("run error = %v, want syntax-only preset validation", err)
	}

	// Subcommands inherit root flag parsing but do not use the chat sandbox.
	// Reaching embed's own input validation proves the preset validator did not
	// inspect (and reject) the home-directory workspace first.
	err := getCommand().Run(context.Background(), []string{"polly", "embed"})
	if err == nil || !strings.Contains(err.Error(), "no input provided") {
		t.Fatalf("polly embed error = %v, want embed input validation after syntax-only root parsing", err)
	}
}

func TestSandboxFlagsConflictWithEffectiveNoSandbox(t *testing.T) {
	for _, args := range [][]string{
		{"--sandbox", "readonly"},
		{"--denypath", "/secrets"},
		{"--writepath", "/output"},
		{"--allownet"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			t.Setenv("POLLYTOOL_NOSANDBOX", "true")
			err := runConfigValidationCommand(args...)
			if err == nil || !strings.Contains(err.Error(), "--nosandbox cannot be enabled with") {
				t.Errorf("run(%v) error = %v, want nosandbox conflict", args, err)
			}
		})
	}
}

func TestSandboxFlagsFromEnvConflictWithCLINoSandbox(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   string
		value string
	}{
		{name: "sandbox", env: "POLLYTOOL_SANDBOX", value: "readonly"},
		{name: "denypath", env: "POLLYTOOL_DENYPATHS", value: "/secrets"},
		{name: "writepath", env: "POLLYTOOL_WRITEPATHS", value: "/output"},
		{name: "allownet", env: "POLLYTOOL_ALLOWNET", value: "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)
			err := runConfigValidationCommand("--nosandbox")
			if err == nil || !strings.Contains(err.Error(), "--"+tc.name) {
				t.Fatalf("run error = %v, want env-sourced --%s conflict", err, tc.name)
			}
		})
	}
}

func TestNoSandboxDoesNotConflictWithDefaultPreset(t *testing.T) {
	if err := runConfigValidationCommand("--nosandbox"); err != nil {
		t.Fatalf("run error = %v, default --sandbox value must not count as explicitly set", err)
	}
}

func TestSandboxFlagsAllowExplicitNoSandboxFalse(t *testing.T) {
	t.Setenv("POLLYTOOL_NOSANDBOX", "true")
	if err := runConfigValidationCommand("--nosandbox=false", "--sandbox", "readonly", "--denypath", "/secrets"); err != nil {
		t.Fatalf("run error = %v, want explicit --nosandbox=false to restore sandboxing", err)
	}
}

func TestManagementCommandIgnoresAmbientNoSandboxPolicyConflict(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows os.UserHomeDir
	t.Setenv("POLLYTOOL_NOSANDBOX", "true")
	t.Setenv("POLLYTOOL_SANDBOX", "readonly")

	if err := getCommand().Run(context.Background(), []string{"polly", "--list"}); err != nil {
		t.Fatalf("polly --list error = %v, management command must not validate unused sandbox policy", err)
	}
}

func runConfigValidationCommand(args ...string) error {
	flags, groups := defineFlagsWithGroups()
	cmd := &cli.Command{
		Name:                   "polly",
		Flags:                  flags,
		MutuallyExclusiveFlags: groups,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return validateSandboxFlagCombination(cmd, parseConfig(cmd))
		},
	}

	return cmd.Run(context.Background(), append([]string{"polly"}, args...))
}

func TestEffectiveDefaultSystemPrompt(t *testing.T) {
	// Managed REPL with the untouched default: markdown-friendly swap.
	if got := effectiveDefaultSystemPrompt(defaultSystemPrompt, false, true); got != defaultREPLSystemPrompt {
		t.Fatalf("managed REPL default = %q, want the markdown-friendly prompt", got)
	}
	// Explicit -s always wins, even if it matches the default text.
	if got := effectiveDefaultSystemPrompt(defaultSystemPrompt, true, true); got != defaultSystemPrompt {
		t.Fatalf("explicit -s was overridden: %q", got)
	}
	// The fallback line REPL renders no markdown: keep the plain default.
	if got := effectiveDefaultSystemPrompt(defaultSystemPrompt, false, false); got != defaultSystemPrompt {
		t.Fatalf("fallback REPL default = %q, want the plain prompt", got)
	}
	// A custom prompt from env/flag passes through untouched.
	if got := effectiveDefaultSystemPrompt("be a pirate", false, true); got != "be a pirate" {
		t.Fatalf("custom prompt mangled: %q", got)
	}
}
