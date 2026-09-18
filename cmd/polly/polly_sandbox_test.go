package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
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
	skipIfWindows(t)
	var captured sandbox.Config
	originalNewSandbox := newSandbox
	newSandbox = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		captured = cfg
		return passthroughSandbox{}, nil
	}
	t.Cleanup(func() { newSandbox = originalNewSandbox })

	work := t.TempDir()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	extraWrite, err := os.MkdirTemp(home, ".polly-writepath-")
	if err != nil {
		t.Skipf("cannot create a grant under the home directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(extraWrite) })
	if err := os.MkdirAll(filepath.Join(work, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	t.Chdir(work)

	extraRead := t.TempDir()
	opts, err := sandboxRegistryOptions(&Config{
		SandboxPreset: "workspace+net",
		DenyPaths:     []string{"~/.config/secrets"},
		WritePaths:    []string{extraWrite},
		ReadPaths:     []string{extraRead},
	})
	if err != nil {
		t.Fatalf("sandboxRegistryOptions() error = %v", err)
	}
	if realExtraRead, err := filepath.EvalSymlinks(extraRead); err != nil || !slices.Contains(captured.ReadPaths, realExtraRead) {
		t.Fatalf("ReadPaths = %v, want --readpath entry %q (%v)", captured.ReadPaths, extraRead, err)
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
	wantDenied := filepath.Join(home, ".config", "secrets")
	if !slices.Contains(captured.DenyPaths, wantDenied) {
		t.Fatalf("DenyPaths = %v, want the --denypath entry", captured.DenyPaths)
	}
}

func TestSandboxRegistryOptionsDefaultPresetUsesGitLeafMode(t *testing.T) {
	skipIfWindows(t)
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
	if err := os.WriteFile(filepath.Join(work, ".git", "config"), []byte("[core]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	t.Chdir(work)

	if _, err := sandboxRegistryOptions(&Config{SandboxPreset: defaultSandboxPreset}); err != nil {
		t.Fatalf("sandboxRegistryOptions() error = %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	realGitDir, err := filepath.EvalSymlinks(filepath.Join(cwd, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(captured.DenyWritePaths, realGitDir) {
		t.Fatalf("DenyWritePaths = %v, default preset must not pin the whole gitdir", captured.DenyWritePaths)
	}
	for _, want := range []string{
		filepath.Join(realGitDir, "config"),
		filepath.Join(realGitDir, "hooks"),
	} {
		if !slices.Contains(captured.DenyWritePaths, want) {
			t.Fatalf("DenyWritePaths = %v, want git leaf %q under the default preset", captured.DenyWritePaths, want)
		}
	}
}

func TestSandboxRegistryOptionsRevalidatesGitPolicyAfterWritePath(t *testing.T) {
	skipIfWindows(t)
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

	if err := os.MkdirAll(filepath.Dir(externalConfig), 0o700); err != nil {
		t.Skipf("cannot create a test target under the home directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(externalConfig)) })
	t.Setenv("GIT_CONFIG_GLOBAL", externalConfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	t.Chdir(work)

	// The home directory itself is never a grant; the external location under
	// it is what --writepath makes host-writable.
	_, err = sandboxRegistryOptions(&Config{
		SandboxPreset: "workspace",
		WritePaths:    []string{filepath.Dir(externalConfig)},
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
	skipIfWindows(t)
	originalNewSandbox := newSandbox
	newSandbox = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		return passthroughSandbox{}, nil
	}
	t.Cleanup(func() { newSandbox = originalNewSandbox })

	warnings := newBroadWritablePathWarner()
	opts, _, err := sandboxRegistryOptionsWithWarnings(&Config{
		SandboxPreset: "base",
		WritePaths:    []string{string(filepath.Separator)},
	}, warnings, nil, nil)
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

func TestQuietSilencesBroadWritablePathWarnings(t *testing.T) {
	skipIfWindows(t)
	originalNewSandbox := newSandbox
	newSandbox = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		return passthroughSandbox{}, nil
	}
	t.Cleanup(func() { newSandbox = originalNewSandbox })

	warnings := newBroadWritablePathWarner()
	opts, _, err := sandboxRegistryOptionsWithWarnings(&Config{
		SandboxPreset: "base",
		WritePaths:    []string{string(filepath.Separator)},
		Quiet:         true,
	}, warnings, nil, nil)
	if err != nil {
		t.Fatalf("sandboxRegistryOptions() error = %v", err)
	}
	registry := tools.NewToolRegistry(nil, opts...)
	t.Cleanup(func() { _ = registry.Close() })
	if _, err := registry.NewSandbox(&sandbox.Config{WritablePaths: []string{string(filepath.Separator)}}); err != nil {
		t.Fatalf("NewSandbox() error = %v", err)
	}
	if got := warnings.Drain(); len(got) != 0 {
		t.Fatalf("--quiet still produced warnings: %q", got)
	}
}

func TestSandboxRegistryOptionsWarnsBroadPerToolOverlay(t *testing.T) {
	skipIfWindows(t)
	originalNewSandbox := newSandbox
	newSandbox = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		return passthroughSandbox{}, nil
	}
	t.Cleanup(func() { newSandbox = originalNewSandbox })

	warnings := newBroadWritablePathWarner()
	opts, _, err := sandboxRegistryOptionsWithWarnings(&Config{SandboxPreset: "base"}, warnings, nil, nil)
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
		opts, _, err := sandboxRegistryOptionsWithWarnings(&Config{SandboxPreset: "base"}, warnings, nil, nil)
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
	_, _, err := sandboxRegistryOptionsWithWarnings(&Config{
		SandboxPreset: "base",
		WritePaths:    []string{string(filepath.Separator)},
	}, warnings, nil, nil)
	if err == nil {
		t.Fatal("sandboxRegistryOptionsWithWarnings() error = nil, want factory failure")
	}
	if got := warnings.Drain(); len(got) != 0 {
		t.Fatalf("failed factory warnings = %q, want none", got)
	}
}

func TestBroadWritablePathWarnerConcurrentWarnAndDrain(t *testing.T) {
	skipIfWindows(t)
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

	store := testOpenMemoryStore(t, nil)
	state, err := (&conversationOpener{config: &Config{
		NoSkills: true,
	}, sessionStore: store, cmd: getCommand()}).openNew(context.Background(), "", false)
	if err == nil {
		t.Fatal("openNew() error = nil, want sandbox startup failure")
	}
	if !strings.Contains(err.Error(), "sandbox requested but unavailable") {
		t.Fatalf("openNew() error = %q, want sandbox-unavailable prefix", err)
	}
	if state != nil {
		t.Fatal("openNew() returned state on sandbox startup failure")
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

	store := testOpenMemoryStore(t, nil)
	state, err := (&conversationOpener{config: &Config{
		NoSandbox: true,
		NoSkills:  true,
	}, sessionStore: store, cmd: getCommand()}).openNew(context.Background(), "", false)
	if err != nil {
		t.Fatalf("openNew() error = %v", err)
	}
	if called {
		t.Fatal("openNew() should not attempt sandbox setup when sandboxing is disabled")
	}
	if state.session == nil {
		t.Fatal("openNew() returned nil session")
	}
	if state.agent == nil {
		t.Fatal("openNew() returned nil agent")
	}
	if state.toolRegistry == nil {
		t.Fatal("openNew() returned nil registry")
	}

	t.Cleanup(func() { _ = state.Close() })
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

	store := testOpenMemoryStore(t, nil)
	state, err := (&conversationOpener{config: &Config{
		NoSandbox: true,
		NoSkills:  true,
		Tools:     []string{"bash"},
	}, sessionStore: store, cmd: getCommand()}).openNew(context.Background(), "", false)
	if !errors.Is(err, wantErr) {
		t.Fatalf("openNew() error = %v, want %v", err, wantErr)
	}
	if state != nil {
		t.Fatalf("openNew() returned state %v after failure", state)
	}
	if captured == nil {
		t.Fatal("skill runtime constructor was not called")
	}
	if got := len(captured.All()); got != 0 {
		t.Fatalf("registry retained %d tools after initialization failure", got)
	}
}

// The probe's spawn no longer holds an open back: the options come back at
// once, and a backend that cannot start fails the first turn before any
// request or persistence.
func TestSandboxProbeFailureLandsOnTheFirstTurn(t *testing.T) {
	originalNewSandbox := newSandbox
	newSandbox = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		return probeFailSandbox{}, nil
	}
	t.Cleanup(func() { newSandbox = originalNewSandbox })
	t.Chdir(t.TempDir())

	opts, probe, err := sandboxRegistryOptionsWithWarnings(&Config{}, newBroadWritablePathWarner(), nil, nil)
	if err != nil || probe == nil {
		t.Fatalf("sandboxRegistryOptionsWithWarnings() = %v, %v; want options and a pending probe", probe, err)
	}
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "probe-turn")
	registry := tools.NewToolRegistry(nil, opts...)
	t.Cleanup(func() { _ = registry.Close() })
	artifactStore := session.ArtifactStore()
	model := &captureCompletionLLM{response: messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done", StopReason: messages.StopReasonEndTurn}}
	state := &conversationState{
		session: session, artifactStore: artifactStore, toolRegistry: registry, sandboxProbe: probe,
		agent: llm.NewAgent(model, registry, llm.AgentConfig{ArtifactStore: artifactStore}),
	}
	state.settings = Settings{Model: "test/model", MaxTokens: 128}
	config := &Config{}
	var stdout, stderr bytes.Buffer
	ui := newLineTurnUI(config, nil)
	ui.writer, ui.errWriter = &stdout, &stderr

	code, err := executeTurnWithUserMessage(context.Background(), config, state, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "hello"}, nil, nil, ui, false)
	if err == nil || code != 1 || !strings.Contains(err.Error(), "POLLYTOOL_NOSANDBOX") {
		t.Fatalf("turn = code %d, err %v; want the probe failure with its escape hatch", code, err)
	}
	if model.request != nil {
		t.Fatal("the model was called despite the failed probe")
	}
	if history := testSessionHistory(t, session); len(history) != 0 {
		t.Fatalf("user message persisted despite the failed probe: %#v", history)
	}
}

func TestOpenConversationStateDefersSandboxProbeFailureToTurns(t *testing.T) {
	originalNewSandbox := newSandbox
	newSandbox = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		return probeFailSandbox{}, nil
	}
	t.Cleanup(func() { newSandbox = originalNewSandbox })

	store := testOpenMemoryStore(t, nil)
	state, err := (&conversationOpener{config: &Config{NoSkills: true, SandboxPreset: "base"}, sessionStore: store}).open(context.Background(), "probe-open", Settings{}, false)
	if err != nil {
		t.Fatalf("open() error = %v, want the open to succeed with the probe pending", err)
	}
	t.Cleanup(func() { _ = state.Close() })
	if err := state.sandboxProbe.wait(context.Background()); err == nil || !strings.Contains(err.Error(), "POLLYTOOL_NOSANDBOX") {
		t.Fatalf("probe error = %v, want the startup failure with its escape hatch", err)
	}
}

// writeSchemaTool writes an executable shell tool whose --schema answer is
// valid, so loading it fails only when the sandbox cannot start it.
func writeSchemaTool(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "probe-tool.sh")
	script := `#!/bin/sh
if [ "$1" = "--schema" ]; then
  printf '%s\n' '{"title":"probe_tool","description":"probe","type":"object","properties":{}}'
  exit 0
fi
exit 1
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertSandboxStartFailure(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "POLLYTOOL_NOSANDBOX") {
		t.Fatalf("error = %v, want the sandbox startup failure with its escape hatch", err)
	}
}

// A tool that spawns while loading runs under the backend the probe checks,
// so on a backend that cannot start the load fails first. The open must still
// report the sandbox failure with its escape hatch rather than the raw load
// error, on both the --tools path and the persisted ActiveTools path.
func TestOpenConversationStateReportsSandboxFailureOverToolLoadFailure(t *testing.T) {
	skipIfWindows(t)
	originalNewSandbox := newSandbox
	newSandbox = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		return probeFailSandbox{}, nil
	}
	t.Cleanup(func() { newSandbox = originalNewSandbox })
	script := writeSchemaTool(t)

	t.Run("command-line tools", func(t *testing.T) {
		store := testOpenMemoryStore(t, nil)
		_, err := (&conversationOpener{config: &Config{NoSkills: true, SandboxPreset: "base", Tools: []string{script}}, sessionStore: store}).open(context.Background(), "probe-tools", Settings{}, false)
		assertSandboxStartFailure(t, err)
	})
	t.Run("persisted tools", func(t *testing.T) {
		store := testOpenMemoryStore(t, &sessions.Metadata{ActiveTools: []tools.ToolLoaderInfo{{Name: "probe_tool", Type: "shell", Source: script}}})
		_, err := (&conversationOpener{config: &Config{NoSkills: true, SandboxPreset: "base"}, sessionStore: store}).open(context.Background(), "probe-resume", Settings{}, false)
		assertSandboxStartFailure(t, err)
	})
}

func captureSandboxConfig(t *testing.T) *sandbox.Config {
	t.Helper()
	captured := &sandbox.Config{}
	originalNewSandbox := newSandbox
	newSandbox = func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		*captured = cfg
		return passthroughSandbox{}, nil
	}
	t.Cleanup(func() { newSandbox = originalNewSandbox })
	return captured
}

func TestSandboxRegistryOptionsExposesCwdWhenNotWritable(t *testing.T) {
	skipIfWindows(t)
	captured := captureSandboxConfig(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	project := filepath.Join(home, "proj")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	for _, preset := range []string{"base", "readonly"} {
		if _, err := sandboxRegistryOptions(&Config{SandboxPreset: preset}); err != nil {
			t.Fatalf("sandboxRegistryOptions(%s) error = %v", preset, err)
		}
		if err := sandbox.ReadAllowed(*captured, filepath.Join(project, "notes.txt")); err != nil {
			t.Fatalf("%s: working directory not readable: %v", preset, err)
		}
	}
}

func TestSandboxRegistryOptionsSkipsCwdExposureForHome(t *testing.T) {
	skipIfWindows(t)
	captureSandboxConfig(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Chdir(home)
	warnings := newBroadWritablePathWarner()
	_, probe, err := sandboxRegistryOptionsWithWarnings(&Config{SandboxPreset: "base"}, warnings, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	drained := warnings.Drain()
	if len(drained) != 1 || !strings.Contains(drained[0], "home directory") {
		t.Fatalf("warnings = %q, want one home-directory notice", drained)
	}
}

func TestHomeReadGrantsIncludeSkillsAndAttachmentCache(t *testing.T) {
	skipIfWindows(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	skillRoot := filepath.Join(home, ".pollytool", "skills", "demo")
	cache := filepath.Join(home, ".pollytool", "cache", "skills")
	attachments := filepath.Join(home, ".cache", "pollytool", "attachments")
	for _, dir := range []string{skillRoot, cache, attachments} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := homeReadGrants(&Config{ReadPaths: []string{"~/docs"}}, []string{skillRoot, "/nonexistent/skill"})
	want := []string{skillRoot, filepath.Join(home, ".pollytool", "skills"), cache, attachments, "~/docs"}
	if !slices.Equal(got, want) {
		t.Fatalf("homeReadGrants() = %v, want %v", got, want)
	}
}

func TestSandboxPostureSettingStringMentionsReadGrants(t *testing.T) {
	posture := sandboxPosture{state: sandboxPostureActive, preset: "base", readGrants: 3}
	if got := posture.settingString(); !strings.Contains(got, "home: private, 3 read grants") {
		t.Fatalf("settingString() = %q, want the private home and the read grant count", got)
	}
}

func TestSandboxRegistryOptionsNeverExposesDeniedCwd(t *testing.T) {
	skipIfWindows(t)
	captured := captureSandboxConfig(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	project := filepath.Join(home, "secrets", "proj")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	for _, denied := range []string{project, filepath.Dir(project)} {
		warnings := newBroadWritablePathWarner()
		_, probe, err := sandboxRegistryOptionsWithWarnings(&Config{SandboxPreset: "base", DenyPaths: []string{denied}}, warnings, nil, nil)
		if err != nil {
			t.Fatalf("sandboxRegistryOptions(--denypath %s) error = %v", denied, err)
		}
		if err := probe.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := sandbox.ReadAllowed(*captured, filepath.Join(project, "notes.txt")); err == nil {
			t.Fatalf("--denypath %s: working directory exposure overrode the denial", denied)
		}
		if drained := warnings.Drain(); len(drained) != 1 || !strings.Contains(drained[0], "inside a denied path") {
			t.Fatalf("--denypath %s: warnings = %v, want one denied working directory warning", denied, drained)
		}
	}
}
