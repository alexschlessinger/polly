package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
)

func readTestUserConfig(t *testing.T, home string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, userConfigDirName, userConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
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
	config, cmd := parseEnvTestConfig(t, "--setup", "--model", "openai/gpt-5.4", "--effort", "low", "--theme", "default", "--sandbox", "workspace", "--baseurl", "http://localhost:8080/v1")
	runner := &commandRunner{conversationOpener: conversationOpener{config: config, cmd: cmd, llmClient: llm.NewMultiPass(map[string]string{"openai": "sk-1"})}}
	var out strings.Builder
	if err := runner.saveSetupFromFlags(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "defaults saved") {
		t.Fatalf("output: %q", out.String())
	}
	got := readTestUserConfig(t, home)
	for _, want := range []string{"POLLYTOOL_MODEL=openai/gpt-5.4\n", "POLLYTOOL_EFFORT=low\n", "POLLYTOOL_THEME=default\n", "POLLYTOOL_SANDBOX=workspace\n", "POLLYTOOL_BASEURL=http://localhost:8080/v1\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	// No sandbox saves no policy line.
	config, cmd = parseEnvTestConfig(t, "--setup", "--model", "openai/gpt-5.4", "--effort", "low", "--theme", "default", "--nosandbox")
	runner = &commandRunner{conversationOpener: conversationOpener{config: config, cmd: cmd, llmClient: llm.NewMultiPass(map[string]string{"openai": "sk-1"})}}
	if err := runner.saveSetupFromFlags(&out); err != nil {
		t.Fatal(err)
	}
	if got := readTestUserConfig(t, home); strings.Contains(got, "POLLYTOOL_SANDBOX") || strings.Contains(got, "NOSANDBOX") {
		t.Fatalf("no-sandbox default left a sandbox line:\n%s", got)
	}
}

func TestSetupFromFlagsRefusesAKeylessProvider(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	config, cmd := parseEnvTestConfig(t, "--setup", "--model", "anthropic/claude-sonnet-4-6", "--effort", "low", "--theme", "default", "--nosandbox")
	runner := &commandRunner{conversationOpener: conversationOpener{config: config, cmd: cmd, llmClient: llm.NewMultiPass(map[string]string{})}}
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
	got := readTestUserConfig(t, home)
	for _, want := range []string{"POLLYTOOL_MODEL=anthropic/claude-sonnet-4-6\n", "POLLYTOOL_EFFORT=low\n", "POLLYTOOL_SANDBOX=workspace\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "POLLYTOOL_BASEURL") {
		t.Fatalf("a kept empty endpoint wrote a line:\n%s", got)
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
	if got := readTestUserConfig(t, home); got != userConfigHeader {
		t.Fatalf("skip should write a header-only file: %q", got)
	}
	if config.Launch.Model != "openai/gpt-5.4" {
		t.Fatalf("a dismissed setup changed the launch: %+v", config)
	}
}
