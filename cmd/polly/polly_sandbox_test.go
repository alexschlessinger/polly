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
	if err := os.MkdirAll(filepath.Join(work, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)

	opts, err := sandboxRegistryOptions(&Config{
		SandboxPreset: "workspace+net",
		DenyPaths:     []string{"~/.config/secrets"},
		WritePaths:    []string{"/data"},
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
	if !slices.Contains(captured.WritablePaths, cwd) {
		t.Fatalf("WritablePaths = %v, want cwd %q from the workspace preset", captured.WritablePaths, cwd)
	}
	if !slices.Contains(captured.WritablePaths, "/data") {
		t.Fatalf("WritablePaths = %v, want --writepath entry /data", captured.WritablePaths)
	}
	if !slices.Contains(captured.DenyWritePaths, filepath.Join(cwd, ".git", "hooks")) {
		t.Fatalf("DenyWritePaths = %v, want the .git/hooks guardrail", captured.DenyWritePaths)
	}
	if !slices.Contains(captured.DenyPaths, "~/.config/secrets") {
		t.Fatalf("DenyPaths = %v, want the --denypath entry", captured.DenyPaths)
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
