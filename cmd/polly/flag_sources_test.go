package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// openExtraReadDirSession stores ExtraReadDirs on a fresh session and opens it
// the way a run does: through a conversationOpener with the parsed flags, so
// the --add-dir merge and validation in open run against a real record.
func openExtraReadDirSession(t *testing.T, args []string, stored []string, name string, prepareHome ...func(home string)) (sessions.SessionStore, error) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, setup := range prepareHome {
		setup(home)
	}
	config, cmd := parseEnvTestConfig(t, args...)
	store, err := setupSessionStore(config, name, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if stored != nil {
		// Stage the persisted list, then release the session so open can
		// acquire it the way a real run does.
		session, err := store.Acquire(context.Background(), name, sessions.AcquireOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := updateMetadata(context.Background(), session, func(md *sessions.Metadata) {
			md.ExtraReadDirs = stored
		}); err != nil {
			_ = session.Close()
			t.Fatal(err)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	}
	opener := &conversationOpener{config: config, sessionStore: store, cmd: cmd}
	_, err = opener.open(context.Background(), name, Settings{}, false)
	return store, err
}

func TestResumeMergesAddDirIntoStoredExtraReadDirs(t *testing.T) {
	store, err := openExtraReadDirSession(t, []string{"--nosandbox", "--add-dir", "/opt"},
		[]string{"/usr/local", "/polly-add-dir-recreate-me"}, "stored")
	if err != nil {
		t.Fatal(err)
	}
	// The flagged dir joins the stored list instead of replacing it, a
	// stored dir that no longer exists stays on the record, and the
	// merged list is persisted by the open.
	md, err := store.GetMetadata(context.Background(), "stored")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/usr/local", "/polly-add-dir-recreate-me", "/opt"}
	if !slices.Equal(md.ExtraReadDirs, want) {
		t.Fatalf("ExtraReadDirs = %v, want the stored list plus the flagged dir %v", md.ExtraReadDirs, want)
	}

	// A repeated open with the same flag dedupes instead of growing the list.
	store, err = openExtraReadDirSession(t, []string{"--nosandbox", "--add-dir", "/opt"},
		[]string{"/usr/local"}, "stored2")
	if err != nil {
		t.Fatal(err)
	}
	md, err = store.GetMetadata(context.Background(), "stored2")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(md.ExtraReadDirs, []string{"/usr/local", "/opt"}) {
		t.Fatalf("ExtraReadDirs = %v, want each directory once", md.ExtraReadDirs)
	}
}

func TestOpenRejectsInvalidAddDirEntries(t *testing.T) {
	// Each subtest's HOME is its own t.TempDir, so on Linux /tmp itself is
	// an ancestor of HOME and fails the home check first; this sibling temp
	// directory is inside the OS temp directory without containing HOME.
	temp := t.TempDir()
	file := filepath.Join(temp, "file.txt")
	if err := os.WriteFile(file, []byte("text"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		path    string
		want    string
		prepare func(home string)
	}{
		{"nonexistent", "/polly-no-such-add-dir", "does not exist", nil},
		{"filesystem root", "/", "is the filesystem root", nil},
		{"home and ancestor", "~", "is the home directory or an ancestor of it", nil},
		{"temp directory", temp, "is inside the OS temp directory", nil},
		{"workspace interior", ".", "is inside the workspace", nil},
		{"not a directory", file, "is not a directory", nil},
		{"credential directory", "~/.ssh", "contains the masked credential path", func(home string) {
			_ = os.Mkdir(filepath.Join(home, ".ssh"), 0o700)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// --nosandbox still validates: the open must fail before the
			// session runs, and the entry never reaches the record. The
			// validator's message names the canonical path itself.
			var prepare []func(home string)
			if tt.prepare != nil {
				prepare = append(prepare, tt.prepare)
			}
			_, err := openExtraReadDirSession(t, []string{"--nosandbox", "--add-dir", tt.path}, nil, "stored", prepare...)
			if err == nil || !strings.Contains(err.Error(), "--add-dir") || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("open error = %v, want an --add-dir rejection containing %q", err, tt.want)
			}
		})
	}
}
