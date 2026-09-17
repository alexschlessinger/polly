package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
)

func TestReadUserConfigParsesLinesAndReportsProblems(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	body := strings.Join([]string{
		"# header",
		"",
		"POLLYTOOL_MODEL=openai/gpt-5.4",
		"export POLLYTOOL_OPENAIKEY='sk-quoted'",
		"POLLYTOOL_THINKING = \"high\"",
		"not a setting",
		"HOME=/elsewhere",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	values, problems, err := readUserConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"POLLYTOOL_MODEL": "openai/gpt-5.4", "POLLYTOOL_THINKING": "high"}
	if len(values) != len(want) {
		t.Fatalf("values = %v, want %v", values, want)
	}
	for k, v := range want {
		if values[k] != v {
			t.Fatalf("%s = %q, want %q", k, values[k], v)
		}
	}
	if len(problems) != 3 || !strings.HasPrefix(problems[0], "line 4: POLLYTOOL_OPENAIKEY ignored") || !strings.HasPrefix(problems[1], "line 6:") || !strings.HasPrefix(problems[2], "line 7:") {
		t.Fatalf("problems = %v", problems)
	}
	if values, problems, err := readUserConfig(filepath.Join(t.TempDir(), "missing")); err != nil || len(values) != 0 || problems != nil {
		t.Fatalf("missing file: %v %v %v", values, problems, err)
	}
}

func TestWriteUserConfigMergesAndProtectsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), userConfigDirName, userConfigFileName)
	if err := writeUserConfig(path, map[string]string{"POLLYTOOL_MODEL": "anthropic/claude-sonnet-4-6", "POLLYTOOL_THINKING": "high"}); err != nil {
		t.Fatal(err)
	}
	// A later save replaces one key, drops another, and keeps the rest.
	if err := writeUserConfig(path, map[string]string{"POLLYTOOL_MODEL": "openai/gpt-5.4", "POLLYTOOL_THINKING": "", "POLLYTOOL_TEMP": "0.5"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := userConfigHeader + "POLLYTOOL_MODEL=openai/gpt-5.4\nPOLLYTOOL_TEMP=0.5\n"
	if string(raw) != want {
		t.Fatalf("file:\n%s\nwant:\n%s", raw, want)
	}
}

func TestFileDefaultsSitBelowEnvironmentAndFlags(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("POLLYTOOL_MODEL", "")
	os.Unsetenv("POLLYTOOL_MODEL")
	t.Setenv("POLLYTOOL_THINKING", "")
	os.Unsetenv("POLLYTOOL_THINKING")
	if err := writeUserConfig(filepath.Join(home, userConfigDirName, userConfigFileName), map[string]string{"POLLYTOOL_MODEL": "openai/from-file", "POLLYTOOL_THINKING": "high"}); err != nil {
		t.Fatal(err)
	}
	config, cmd := parseEnvTestConfig(t)
	if config.Launch.Model != "openai/from-file" || config.Launch.ThinkingEffort != "high" {
		t.Fatalf("file defaults not read: %+v", config.Launch)
	}
	if flagGiven(cmd, "model") || flagGiven(cmd, "thinking") {
		t.Fatal("file defaults counted as given")
	}
	t.Setenv("POLLYTOOL_MODEL", "openai/from-env")
	config, _ = parseEnvTestConfig(t)
	if config.Launch.Model != "openai/from-env" || config.Launch.ThinkingEffort != "high" {
		t.Fatalf("environment does not sit above the file: %+v", config.Launch)
	}
	config, cmd = parseEnvTestConfig(t, "-m", "openai/from-arg")
	if config.Launch.Model != "openai/from-arg" || !flagGiven(cmd, "model") {
		t.Fatalf("argument does not sit above both: %+v", config.Launch)
	}
}

func TestFirstRunPending(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("POLLYTOOL_MODEL", "ollama/gpt-oss")
	if !firstRunPending() {
		t.Fatal("no file means a first run, whatever the environment says")
	}
	if err := writeUserConfig(filepath.Join(home, userConfigDirName, userConfigFileName), nil); err != nil {
		t.Fatal(err)
	}
	if firstRunPending() {
		t.Fatal("a header-only file records a completed or skipped setup")
	}
}

func TestMissingKeyError(t *testing.T) {
	client := llm.NewMultiPass(map[string]string{"openai": "sk-1"})
	cases := []struct {
		model, baseURL string
		wantErr        bool
	}{
		{"openai/gpt-5.4", "", false},
		{"anthropic/claude-sonnet-4-6", "", true},
		{"anthropic/claude-sonnet-4-6", "http://localhost:8080/v1", true},
		{"ollama/gpt-oss", "", false},
		{"deepseek/deepseek-v4-pro", "", true},
		{"deepseek/deepseek-v4-pro", "http://localhost:8080/v1", true},
		{"openai/custom", "http://localhost:8080/v1", false},
	}
	for _, c := range cases {
		err := missingKeyError(client, c.model, c.baseURL)
		if (err != nil) != c.wantErr {
			t.Fatalf("%s at %q: err = %v, want error %v", c.model, c.baseURL, err, c.wantErr)
		}
		if err != nil && !strings.Contains(err.Error(), "POLLYTOOL_") {
			t.Fatalf("error does not name the variable: %v", err)
		}
	}
	client.SetAPIKey("anthropic", "sk-2")
	if err := missingKeyError(client, "anthropic/claude-sonnet-4-6", ""); err != nil {
		t.Fatalf("process override not honored: %v", err)
	}
}

func TestShadowedByEnvironment(t *testing.T) {
	t.Setenv("POLLYTOOL_MODEL", "openai/shell")
	t.Setenv("POLLYTOOL_TEMP", "0.2")
	t.Setenv("POLLYTOOL_THINKING", "")
	os.Unsetenv("POLLYTOOL_THINKING")
	got := shadowedByEnvironment(map[string]string{"POLLYTOOL_MODEL": "openai/saved", "POLLYTOOL_THINKING": "high", "POLLYTOOL_TEMP": ""})
	if strings.Join(got, ",") != "POLLYTOOL_MODEL,POLLYTOOL_TEMP" {
		t.Fatalf("shadowed = %v", got)
	}
}
