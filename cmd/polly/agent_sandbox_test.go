package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

func TestAgentSandboxFlagKeepsParentPolicySeparate(t *testing.T) {
	for _, tc := range []struct {
		name          string
		args          []string
		parentSandbox bool
	}{
		{name: "implicit unsandboxed parent"},
		{name: "explicit unsandboxed parent", args: []string{"--nosandbox"}},
		{name: "sandboxed parent", args: []string{"--sandbox", "workspace+net"}, parentSandbox: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var parsed *Config
			flags, groups := defineFlagsWithGroups()
			cmd := &cli.Command{Flags: flags, MutuallyExclusiveFlags: groups, Action: func(_ context.Context, cmd *cli.Command) error {
				parsed = parseConfig(cmd)
				return validateSandboxFlagCombination(cmd, parsed)
			}}
			args := append([]string{"polly", "--agent-sandbox", "workspace+private-home"}, tc.args...)
			if err := cmd.Run(context.Background(), args); err != nil {
				t.Fatal(err)
			}
			if parsed.AgentSandbox != "workspace+private-home" || parsed.NoSandbox == tc.parentSandbox {
				t.Fatalf("agent policy %q, parent nosandbox %v", parsed.AgentSandbox, parsed.NoSandbox)
			}
			if !tc.parentSandbox && parsed.SandboxPreset != "" {
				t.Fatalf("child policy changed parent preset: %q", parsed.SandboxPreset)
			}
		})
	}
}

func TestAgentSandboxFlagRejectsInvalidPolicy(t *testing.T) {
	for _, spec := range []string{"", "false", "none", "workspace+typo", "git", "workspace+"} {
		t.Run(spec, func(t *testing.T) {
			if err := runConfigValidationCommand("--agent-sandbox=" + spec); err == nil {
				t.Fatalf("accepted invalid agent sandbox %q", spec)
			}
		})
	}
	// Validation must not inspect or mutate the working directory.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Chdir(home)
	if err := runConfigValidationCommand("--agent-sandbox", "default+private-home"); err != nil {
		t.Fatalf("syntax-only child policy validation: %v", err)
	}
}

func TestAgentSandboxDefaultsPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(envVarAgentSandbox, "")
	if err := os.Unsetenv(envVarAgentSandbox); err != nil {
		t.Fatal(err)
	}
	if err := writeUserConfig(filepath.Join(home, userConfigDirName, userConfigFileName), map[string]string{envVarAgentSandbox: "workspace+private-home"}); err != nil {
		t.Fatal(err)
	}
	config, _ := parseEnvTestConfig(t)
	if config.AgentSandbox != "workspace+private-home" || !config.NoSandbox {
		t.Fatalf("file default: child %q parent nosandbox %v", config.AgentSandbox, config.NoSandbox)
	}
	t.Setenv(envVarAgentSandbox, "readonly")
	config, _ = parseEnvTestConfig(t)
	if config.AgentSandbox != "readonly" {
		t.Fatalf("environment did not override file: %q", config.AgentSandbox)
	}
	config, _ = parseEnvTestConfig(t, "--agent-sandbox", "base")
	if config.AgentSandbox != "base" {
		t.Fatalf("flag did not override environment: %q", config.AgentSandbox)
	}
	t.Setenv(envVarAgentSandbox, "bad-preset")
	if err := runConfigValidationCommand(); err == nil || !strings.Contains(err.Error(), "sandbox preset") {
		t.Fatalf("invalid environment policy: %v", err)
	}
}
