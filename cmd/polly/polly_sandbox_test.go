package main

import (
	"errors"
	"os/exec"
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
