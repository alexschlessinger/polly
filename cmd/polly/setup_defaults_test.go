package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
)

// savedDefaults reads the configuration file the setup wrote.
func savedDefaults(t *testing.T, home string) map[string]string {
	t.Helper()
	values, _, err := readUserConfig(filepath.Join(home, userConfigDirName, userConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	return values
}

// flagSetupRunner parses args and builds a runner with the given keys.
func flagSetupRunner(t *testing.T, keys map[string]string, args ...string) *commandRunner {
	t.Helper()
	config, cmd := parseEnvTestConfig(t, args...)
	return &commandRunner{conversationOpener: conversationOpener{config: config, cmd: cmd, llmClient: llm.NewMultiPass(keys)}}
}

func expectDefaults(t *testing.T, got, want map[string]string) {
	t.Helper()
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("%s = %q, want %q in %v", key, got[key], value, got)
		}
	}
}

func TestSetupFlagsComplete(t *testing.T) {
	t.Setenv("POLLYTOOL_MODEL", "openai/from-env")
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"--setup"}, false},
		{[]string{"--setup", "--model", "openai/gpt-5.4", "--effort", "low", "--theme", "default"}, false},
		{[]string{"--setup", "--effort", "low", "--theme", "default", "--nosandbox"}, false},
		{[]string{"--setup", "--model", "openai/gpt-5.4", "--effort", "low", "--theme", "default", "--nosandbox"}, true},
		{[]string{"--setup", "--model", "openai/gpt-5.4", "--effort", "low", "--theme", "default", "--sandbox", "workspace"}, true},
	}
	for _, c := range cases {
		_, cmd := parseEnvTestConfig(t, c.args...)
		if got := setupFlagsComplete(cmd); got != c.want {
			t.Fatalf("%v: complete = %v, want %v", c.args, got, c.want)
		}
	}
}

func TestSetupFromFlagsSavesDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	openai := map[string]string{"openai": "sk-1"}
	runner := flagSetupRunner(t, openai, "--setup", "--model", "openai/gpt-5.4", "--effort", "low", "--theme", "default", "--sandbox", "workspace", "--baseurl", "http://localhost:8080/v1")
	var out strings.Builder
	if err := runner.saveSetupFromFlags(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "defaults saved") {
		t.Fatalf("output: %q", out.String())
	}
	expectDefaults(t, savedDefaults(t, home), map[string]string{
		envVarModel: "openai/gpt-5.4", envVarEffort: "low", envVarTheme: "default", envVarSandbox: "workspace", envVarBaseURL: "http://localhost:8080/v1",
	})
	// No sandbox saves no policy line.
	runner = flagSetupRunner(t, openai, "--setup", "--model", "openai/gpt-5.4", "--effort", "low", "--theme", "default", "--nosandbox")
	if err := runner.saveSetupFromFlags(&strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	got := savedDefaults(t, home)
	if _, ok := got[envVarSandbox]; ok {
		t.Fatalf("no-sandbox default left a sandbox line: %v", got)
	}
	if _, ok := got[envVarNoSandbox]; ok {
		t.Fatalf("no-sandbox default wrote the former spelling: %v", got)
	}
}

func TestSetupFromFlagsRefusesAKeylessProvider(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	runner := flagSetupRunner(t, map[string]string{}, "--setup", "--model", "anthropic/claude-sonnet-4-6", "--effort", "low", "--theme", "default", "--nosandbox")
	err := runner.saveSetupFromFlags(&strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "POLLYTOOL_ANTHROPICKEY") {
		t.Fatalf("err = %v, want the missing-key refusal", err)
	}
	if _, err := os.Stat(filepath.Join(home, userConfigDirName, userConfigFileName)); err == nil {
		t.Fatal("a refused setup wrote the configuration file")
	}
}

func TestTextSetupSavesAnswersAndAppliesThem(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	client := llm.NewMultiPass(map[string]string{"openai": "sk-1"})
	config := &Config{Launch: Settings{Model: "openai/gpt-5.4", ModelHost: "", ThinkingEffort: "high"}, NoSandbox: true}
	runner := &commandRunner{conversationOpener: conversationOpener{config: config, llmClient: client}}
	// provider, model, endpoint kept, key, effort, theme kept, sandbox.
	in := strings.NewReader("anthropic\nclaude-sonnet-4-6\n\nsk-typed\nnope\nlow\n\nworkspace\n")
	var out strings.Builder
	if err := runner.runTextSetup(in, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	text := out.String()
	for _, want := range []string{"Provider (", "[openai]", "Key for anthropic", "Effort (", "defaults saved"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Count(text, "Effort (") != 2 {
		t.Fatalf("a bad effort should ask again:\n%s", text)
	}
	got := savedDefaults(t, home)
	expectDefaults(t, got, map[string]string{envVarModel: "anthropic/claude-sonnet-4-6", envVarEffort: "low", envVarSandbox: "workspace", envVarTheme: "default"})
	if _, ok := got[envVarBaseURL]; ok {
		t.Fatalf("a kept empty endpoint wrote a line: %v", got)
	}
	if config.Launch.Model != "anthropic/claude-sonnet-4-6" || config.Launch.ThinkingEffort != "low" || config.NoSandbox || config.SandboxPreset != "workspace" {
		t.Fatalf("answers not applied to the launch: %+v", config)
	}
	if client.APIKeySource("anthropic") != "session" {
		t.Fatal("typed key not kept for the process")
	}
}

func TestTextSetupEndOfInputRecordsTheSkip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	config := &Config{Launch: Settings{Model: "openai/gpt-5.4"}, NoSandbox: true}
	runner := &commandRunner{conversationOpener: conversationOpener{config: config, llmClient: llm.NewMultiPass(map[string]string{"openai": "sk-1"})}}
	var out strings.Builder
	if err := runner.runTextSetup(strings.NewReader("anthropic\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Setup skipped") {
		t.Fatalf("output: %q", out.String())
	}
	raw, err := os.ReadFile(filepath.Join(home, userConfigDirName, userConfigFileName))
	if err != nil || string(raw) != userConfigHeader {
		t.Fatalf("skip should write a header-only file: %q %v", raw, err)
	}
	if config.Launch.Model != "openai/gpt-5.4" {
		t.Fatalf("a dismissed setup changed the launch: %+v", config)
	}
}
