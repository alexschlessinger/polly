package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// probeFailSandbox constructs fine but any command run through it exits non-zero,
// simulating a backend (e.g. bwrap) that is present but can't actually start.
type probeFailSandbox struct{}

func (probeFailSandbox) Wrap(cmd *exec.Cmd) error {
	p, err := exec.LookPath("false")
	if err != nil {
		return err
	}
	cmd.Path = p
	cmd.Args = []string{"false"}
	return nil
}

func TestSandboxRegistryOptionsFailsWhenProbeFails(t *testing.T) {
	originalNewSandbox := newSandbox
	newSandbox = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		return probeFailSandbox{}, nil
	}
	t.Cleanup(func() { newSandbox = originalNewSandbox })

	_, err := sandboxRegistryOptions(&Config{})
	if err == nil {
		t.Fatal("sandboxRegistryOptions() error = nil, want failure when the sandbox can't start")
	}
	if !strings.Contains(err.Error(), "POLLYTOOL_NOSANDBOX") {
		t.Fatalf("error = %q, want it to mention the POLLYTOOL_NOSANDBOX escape hatch", err)
	}
}

func TestSandboxRegistryOptionsAppliesPresetAndOverrides(t *testing.T) {
	var captured sandbox.Config
	originalNewSandbox := newSandbox
	newSandbox = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		captured = cfg
		return passthroughSandbox{}, nil
	}
	t.Cleanup(func() { newSandbox = originalNewSandbox })

	work := t.TempDir()
	extraWrite, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	t.Chdir(work)

	opts, err := sandboxRegistryOptions(&Config{
		SandboxPreset: "workspace+net",
		DenyPaths:     []string{"~/.config/secrets"},
		WritePaths:    []string{extraWrite},
	})
	if err != nil {
		t.Fatalf("sandboxRegistryOptions() error = %v", err)
	}
	if len(opts) == 0 {
		t.Fatal("sandboxRegistryOptions() returned no options")
	}

	if !captured.AllowNetwork {
		t.Fatal("workspace+net should allow network")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	realCWD, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	workspaceWritable := false
	for _, writable := range captured.WritablePaths {
		rel, relErr := filepath.Rel(writable, realCWD)
		if relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			workspaceWritable = true
			break
		}
	}
	if !workspaceWritable {
		t.Fatalf("WritablePaths = %v, want an effective grant covering workspace %q", captured.WritablePaths, realCWD)
	}
	realExtraWrite, err := filepath.EvalSymlinks(extraWrite)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(captured.WritablePaths, realExtraWrite) {
		t.Fatalf("WritablePaths = %v, want --writepath entry %q", captured.WritablePaths, realExtraWrite)
	}
	realGitDir, err := filepath.EvalSymlinks(filepath.Join(cwd, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(captured.DenyWritePaths, realGitDir) {
		t.Fatalf("DenyWritePaths = %v, want the .git routing guardrail", captured.DenyWritePaths)
	}
	wantDenied := filepath.Join(extraWrite, ".config", "secrets")
	if !slices.Contains(captured.DenyPaths, wantDenied) {
		t.Fatalf("DenyPaths = %v, want the --denypath entry", captured.DenyPaths)
	}
}

func TestSandboxRegistryOptionsRevalidatesGitPolicyAfterWritePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(home) || filepath.Clean(home) == string(filepath.Separator) {
		t.Skipf("need a bounded absolute home directory, got %q", home)
	}
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	externalConfig := filepath.Join(home, ".polly-sandbox-policy-"+filepath.Base(work), "global.gitconfig")
	if _, err := os.Lstat(filepath.Dir(externalConfig)); err == nil {
		t.Skipf("test target unexpectedly exists: %s", filepath.Dir(externalConfig))
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}

	t.Setenv("GIT_CONFIG_GLOBAL", externalConfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	t.Chdir(work)

	_, err = sandboxRegistryOptions(&Config{
		SandboxPreset: "workspace",
		WritePaths:    []string{home},
	})
	if err == nil || !strings.Contains(err.Error(), "global Git config source") {
		t.Fatalf("sandboxRegistryOptions() error = %v, want post-merge --writepath Git policy rejection", err)
	}
}

func TestSandboxRegistryOptionsRejectsUnknownPreset(t *testing.T) {
	if _, err := sandboxRegistryOptions(&Config{SandboxPreset: "everything"}); err == nil {
		t.Fatal("sandboxRegistryOptions() error = nil, want unknown-preset failure")
	}
}

func TestInitializeSessionFailsWhenSandboxRequestedButUnavailable(t *testing.T) {
	originalNewSandbox := newSandbox
	newSandbox = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		return nil, errors.New("backend missing")
	}
	t.Cleanup(func() {
		newSandbox = originalNewSandbox
	})

	store := sessions.NewSyncMapSessionStore(nil)
	_, session, _, registry, _, _, _, err := initializeSession(&Config{
		NoSkills: true,
	}, store, "", getCommand())
	if err == nil {
		t.Fatal("initializeSession() error = nil, want sandbox startup failure")
	}
	if !strings.Contains(err.Error(), "sandbox requested but unavailable") {
		t.Fatalf("initializeSession() error = %q, want sandbox-unavailable prefix", err)
	}
	if session != nil {
		t.Fatal("initializeSession() returned a non-nil session on sandbox startup failure")
	}
	if registry != nil {
		t.Fatal("initializeSession() returned a non-nil registry on sandbox startup failure")
	}
}

func TestInitializeSessionSucceedsWithoutSandboxWhenBackendUnavailable(t *testing.T) {
	originalNewSandbox := newSandbox
	called := false
	newSandbox = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		called = true
		return nil, errors.New("backend missing")
	}
	t.Cleanup(func() {
		newSandbox = originalNewSandbox
	})

	store := sessions.NewSyncMapSessionStore(nil)
	_, session, agent, registry, _, _, _, err := initializeSession(&Config{
		NoSandbox: true,
		NoSkills:  true,
	}, store, "", getCommand())
	if err != nil {
		t.Fatalf("initializeSession() error = %v", err)
	}
	if called {
		t.Fatal("initializeSession() should not attempt sandbox setup when sandboxing is disabled")
	}
	if session == nil {
		t.Fatal("initializeSession() returned nil session")
	}
	if agent == nil {
		t.Fatal("initializeSession() returned nil agent")
	}
	if registry == nil {
		t.Fatal("initializeSession() returned nil registry")
	}

	t.Cleanup(func() {
		_ = registry.Close()
		session.Close()
	})
}
