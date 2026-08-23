package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools"
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

func TestSandboxRegistryOptionsWarnsBroadBaseOnce(t *testing.T) {
	originalNewSandbox := newSandbox
	newSandbox = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		return passthroughSandbox{}, nil
	}
	t.Cleanup(func() { newSandbox = originalNewSandbox })

	warnings := newBroadWritablePathWarner()
	opts, err := sandboxRegistryOptionsWithWarnings(&Config{
		SandboxPreset: "base",
		WritePaths:    []string{string(filepath.Separator)},
	}, warnings)
	if err != nil {
		t.Fatalf("sandboxRegistryOptions() error = %v", err)
	}
	registry := tools.NewToolRegistry(nil, opts...)
	t.Cleanup(func() { _ = registry.Close() })
	if _, err := registry.NewSandbox(nil); err != nil {
		t.Fatalf("NewSandbox() error = %v", err)
	}

	got := strings.Join(warnings.Drain(), "\n")
	if strings.Count(got, "sandbox writable path") != 1 {
		t.Fatalf("warning output = %q, want exactly one broad-path warning", got)
	}
	if !strings.Contains(got, "filesystem root") || !strings.Contains(got, "remove or narrow the originating --writepath/POLLYTOOL_WRITEPATHS or tool writablePaths setting") {
		t.Fatalf("warning output = %q, want scope and actionable remediation", got)
	}
}

func TestSandboxRegistryOptionsWarnsBroadPerToolOverlay(t *testing.T) {
	originalNewSandbox := newSandbox
	newSandbox = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		return passthroughSandbox{}, nil
	}
	t.Cleanup(func() { newSandbox = originalNewSandbox })

	warnings := newBroadWritablePathWarner()
	opts, err := sandboxRegistryOptionsWithWarnings(&Config{SandboxPreset: "base"}, warnings)
	if err != nil {
		t.Fatalf("sandboxRegistryOptions() error = %v", err)
	}
	registry := tools.NewToolRegistry(nil, opts...)
	t.Cleanup(func() { _ = registry.Close() })
	if got := warnings.Drain(); len(got) != 0 {
		t.Fatalf("base startup warnings = %q, want none", got)
	}

	root := string(filepath.Separator)
	if _, err := registry.NewSandbox(&sandbox.Config{WritablePaths: []string{root}}); err != nil {
		t.Fatalf("NewSandbox(per-tool overlay) error = %v", err)
	}
	select {
	case <-warnings.Notify():
	default:
		t.Fatal("per-tool warning did not signal Notify")
	}
	got := strings.Join(warnings.Drain(), "\n")
	if !strings.Contains(got, "sandbox writable path") || !strings.Contains(got, "filesystem root") {
		t.Fatalf("per-tool warning output = %q, want broad-path warning", got)
	}
	if _, err := registry.NewSandbox(&sandbox.Config{WritablePaths: []string{root}}); err != nil {
		t.Fatalf("NewSandbox(repeated overlay) error = %v", err)
	}
	select {
	case <-warnings.Notify():
		t.Fatal("deduplicated warning unexpectedly signaled Notify again")
	default:
	}
	if got := warnings.Drain(); len(got) != 0 {
		t.Fatalf("deduplicated warning drain = %q, want empty", got)
	}
}

func TestSandboxRegistryOptionsDoesNotWarnForIneffectiveBroadWrite(t *testing.T) {
	originalNewSandbox := newSandbox
	newSandbox = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		return passthroughSandbox{}, nil
	}
	t.Cleanup(func() { newSandbox = originalNewSandbox })

	root := string(filepath.Separator)
	for _, overlay := range []sandbox.Config{
		{WritablePaths: []string{root}, DenyWrite: true},
		{WritablePaths: []string{root}, DenyWritePaths: []string{root}},
	} {
		warnings := newBroadWritablePathWarner()
		opts, err := sandboxRegistryOptionsWithWarnings(&Config{SandboxPreset: "base"}, warnings)
		if err != nil {
			t.Fatalf("sandboxRegistryOptions() error = %v", err)
		}
		registry := tools.NewToolRegistry(nil, opts...)
		if _, err := registry.NewSandbox(&overlay); err != nil {
			_ = registry.Close()
			t.Fatalf("NewSandbox(ineffective overlay) error = %v", err)
		}
		_ = registry.Close()
		if got := warnings.Drain(); len(got) != 0 {
			t.Fatalf("ineffective broad write warning = %q, want none", got)
		}
	}
}

func TestSandboxRegistryOptionsDoesNotWarnWhenFactoryFails(t *testing.T) {
	originalNewSandbox := newSandbox
	newSandbox = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		return nil, errors.New("backend unavailable")
	}
	t.Cleanup(func() { newSandbox = originalNewSandbox })

	warnings := newBroadWritablePathWarner()
	_, err := sandboxRegistryOptionsWithWarnings(&Config{
		SandboxPreset: "base",
		WritePaths:    []string{string(filepath.Separator)},
	}, warnings)
	if err == nil {
		t.Fatal("sandboxRegistryOptionsWithWarnings() error = nil, want factory failure")
	}
	if got := warnings.Drain(); len(got) != 0 {
		t.Fatalf("failed factory warnings = %q, want none", got)
	}
}

func TestBroadWritablePathWarnerConcurrentWarnAndDrain(t *testing.T) {
	warnings := newBroadWritablePathWarner()
	cfg := sandbox.Config{WritablePaths: []string{string(filepath.Separator)}}
	var wg sync.WaitGroup
	var collectedMu sync.Mutex
	var collected []string
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				warnings.Warn(cfg)
				return
			}
			got := warnings.Drain()
			collectedMu.Lock()
			collected = append(collected, got...)
			collectedMu.Unlock()
		}(i)
	}
	wg.Wait()
	collected = append(collected, warnings.Drain()...)
	if len(collected) != 1 {
		t.Fatalf("concurrent warning count = %d (%q), want exactly one", len(collected), collected)
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
	}, store, "", getCommand(), nil)
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
	}, store, "", getCommand(), nil)
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

func TestInitializeSessionClosesRegistryWhenSkillRuntimeFails(t *testing.T) {
	wantErr := errors.New("skill runtime failed")
	originalNewSkillRuntime := newSkillRuntimeImpl
	var captured *tools.ToolRegistry
	newSkillRuntimeImpl = func(_ *skills.Catalog, registry *tools.ToolRegistry) (*tools.SkillRuntime, error) {
		captured = registry
		if _, ok := registry.Get("bash"); !ok {
			t.Fatal("test setup did not load bash before skill runtime construction")
		}
		return nil, wantErr
	}
	t.Cleanup(func() { newSkillRuntimeImpl = originalNewSkillRuntime })

	store := sessions.NewSyncMapSessionStore(nil)
	_, session, _, registry, _, _, _, err := initializeSession(&Config{
		NoSandbox: true,
		NoSkills:  true,
		Tools:     []string{"bash"},
	}, store, "", getCommand(), nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("initializeSession() error = %v, want %v", err, wantErr)
	}
	if session != nil || registry != nil {
		t.Fatalf("initializeSession() returned session=%v registry=%v after failure", session, registry)
	}
	if captured == nil {
		t.Fatal("skill runtime constructor was not called")
	}
	if got := len(captured.All()); got != 0 {
		t.Fatalf("registry retained %d tools after initialization failure", got)
	}
}
