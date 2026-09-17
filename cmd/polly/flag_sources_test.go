package main

import (
	"context"
	"os"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/urfave/cli/v3"
)

// parseEnvTestConfig parses args with the environment left as it is, unlike
// parseStorageTestConfig, which clears the POLLYTOOL_* variables first.
func parseEnvTestConfig(t *testing.T, args ...string) (*Config, *cli.Command) {
	t.Helper()
	var config *Config
	var parsed *cli.Command
	cmd := getCommand()
	cmd.Action = func(_ context.Context, cmd *cli.Command) error {
		config, parsed = parseConfig(cmd), cmd
		return nil
	}
	if err := cmd.Run(context.Background(), append([]string{"polly"}, args...)); err != nil {
		t.Fatal(err)
	}
	return config, parsed
}

// parseWith runs the root command on args and reports flagGiven and IsSet
// for name as seen from the action.
func parseWith(t *testing.T, name string, args ...string) (given, isSet bool, value string) {
	t.Helper()
	cmd := getCommand()
	cmd.Action = func(ctx context.Context, cmd *cli.Command) error {
		given, isSet, value = flagGiven(cmd, name), cmd.IsSet(name), cmd.String(name)
		return nil
	}
	if err := cmd.Run(context.Background(), append([]string{"polly"}, args...)); err != nil {
		t.Fatal(err)
	}
	return given, isSet, value
}

func TestFlagGivenSeparatesArgumentsFromEnvironmentDefaults(t *testing.T) {
	t.Setenv("POLLYTOOL_MODEL", "")
	os.Unsetenv("POLLYTOOL_MODEL")
	if given, isSet, _ := parseWith(t, "model"); given || isSet {
		t.Fatal("built-in default counted as set")
	}
	t.Setenv("POLLYTOOL_MODEL", "openai/from-env")
	given, isSet, value := parseWith(t, "model")
	if given || !isSet || value != "openai/from-env" {
		t.Fatalf("environment default: given=%v isSet=%v value=%q", given, isSet, value)
	}
	given, _, value = parseWith(t, "model", "-m", "openai/from-arg")
	if !given || value != "openai/from-arg" {
		t.Fatalf("argument over environment: given=%v value=%q", given, value)
	}
	// A flag with no environment source behaves as before.
	if given, _, _ := parseWith(t, "confirm", "--confirm"); !given {
		t.Fatal("argument-only flag not reported as given")
	}
}

func TestEnvironmentDefaultsDoNotOverrideStoredSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("POLLYTOOL_MODEL", "openai/from-env")
	t.Setenv("POLLYTOOL_THINKING", "high")
	config, cmd := parseEnvTestConfig(t)
	if config.Launch.Model != "openai/from-env" || config.Launch.ThinkingEffort != "high" {
		t.Fatalf("launch settings ignore the environment: %+v", config.Launch)
	}
	store, err := setupSessionStore(config, "stored", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	session := testAcquireSession(t, store, "stored")
	if err := updateMetadata(context.Background(), session, func(md *sessions.Metadata) {
		md.Model, md.ThinkingEffort = "anthropic/stored", "off"
	}); err != nil {
		t.Fatal(err)
	}
	opener := &conversationOpener{config: config, sessionStore: store, cmd: cmd}
	_, settings, err := opener.prepare(context.Background(), "stored", notifyStderr)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Model != "anthropic/stored" || settings.ThinkingEffort != "off" {
		t.Fatalf("environment default overrode the stored session: %+v", settings)
	}
	md := &sessions.Metadata{Model: "anthropic/stored", ThinkingEffort: "off"}
	applyFlagSettings(md, &settings, cmd)
	if md.Model != "anthropic/stored" || md.ThinkingEffort != "off" {
		t.Fatalf("environment default reached metadata: %+v", md)
	}

	// The same values on the command line still override and persist.
	config, cmd = parseEnvTestConfig(t, "--model", "openai/from-arg")
	opener = &conversationOpener{config: config, sessionStore: store, cmd: cmd}
	_, settings, err = opener.prepare(context.Background(), "stored", notifyStderr)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Model != "openai/from-arg" || settings.ThinkingEffort != "off" {
		t.Fatalf("argument did not override only its own setting: %+v", settings)
	}
}
