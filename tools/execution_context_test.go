package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"maps"
	"slices"
	"strings"
)

func TestBoundNativeFilesAndReadOnlyPolicy(t *testing.T) {
	parent, child := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(parent, "file.txt"), []byte("parent"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "file.txt"), []byte("child"), 0600); err != nil {
		t.Fatal(err)
	}
	registry := NewToolRegistry(nil, WithUnsafeNoSandbox())
	defer registry.Close()
	for _, name := range []string{"read_file", "write_file"} {
		if _, err := registry.LoadToolAuto(name); err != nil {
			t.Fatal(err)
		}
	}
	ec, err := registry.ExecutionPolicy(child, ExecutionGrant{ReadOnly: true, DeniedReads: []string{parent}})
	if err != nil {
		t.Fatal(err)
	}
	bound, _, err := registry.BindExecutionContext(ec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	reader, _ := bound.Get("read_file")
	if out, err := reader.Execute(context.Background(), map[string]any{"path": "file.txt"}); err != nil || out == "parent" {
		t.Fatalf("read bound file: %s %v", out, err)
	}
	if _, err := reader.Execute(context.Background(), map[string]any{"path": filepath.Join(parent, "file.txt")}); err == nil {
		t.Fatal("read parent despite denial")
	}
	writer, _ := bound.Get("write_file")
	if _, err := writer.Execute(context.Background(), map[string]any{"path": "file.txt", "content": "bad"}); err == nil {
		t.Fatal("read-only native policy bypassed without process sandbox")
	}
}

type failBeforeTarget struct{}

func (failBeforeTarget) Wrap(cmd *exec.Cmd) error {
	cmd.Path = "/bin/sh"
	cmd.Args = []string{"sh", "-c", "exit 7"}
	return nil
}
func TestBashDistinguishesSandboxSetupFromCommandExit(t *testing.T) {
	skipIfWindows(t)
	bash := NewUnsafeBashTool(t.TempDir()).WithSandbox(failBeforeTarget{})
	_, err := bash.ExecuteOutput(context.Background(), map[string]any{"command": "exit 9"})
	var command *CommandError
	if err == nil || errors.As(err, &command) {
		t.Fatalf("setup became command exit: %v", err)
	}
	_, err = NewUnsafeBashTool(t.TempDir()).ExecuteOutput(context.Background(), map[string]any{"command": "exit 9"})
	if !errors.As(err, &command) || command.ExitCode != 9 {
		t.Fatalf("ordinary exit: %v", err)
	}
}

func TestExecutionPolicyRetainsDNSBlockAndMCPOverlaysKeepOnlyRestrictions(t *testing.T) {
	registry := NewToolRegistry(nil, WithSandboxFactory(func(cfg sandbox.Config) (sandbox.Sandbox, error) { return &mockSandbox{}, nil }, sandbox.Config{AllowNetwork: true, DenyDNS: true}))
	defer registry.Close()
	ec, err := registry.ExecutionPolicy(t.TempDir(), ExecutionGrant{})
	if err != nil {
		t.Fatal(err)
	}
	if !ec.Sandbox.AllowNetwork || !ec.Sandbox.DenyDNS {
		t.Fatalf("member policy widened the parent's network policy: %+v", ec.Sandbox)
	}
	home := t.TempDir()
	config, err := json.Marshal(sandbox.Config{
		AllowNetwork: true, WritablePaths: []string{home},
		DenyWrite: true, DenyDNS: true, DenyPaths: []string{filepath.Join(home, ".ssh")},
	})
	if err != nil {
		t.Fatal(err)
	}
	overlay := restrictiveSandboxOverlay(&MCPConfig{Sandbox: config})
	kept, err := sandbox.ParseConfig(overlay)
	if err != nil || kept == nil {
		t.Fatalf("overlay: %v %v", kept, err)
	}
	if kept.AllowNetwork || len(kept.WritablePaths) != 0 {
		t.Fatalf("server grants survived context binding: %+v", kept)
	}
	if !kept.DenyWrite || !kept.DenyDNS || len(kept.DenyPaths) != 1 {
		t.Fatalf("server restrictions dropped: %+v", kept)
	}
	if restrictiveSandboxOverlay(&MCPConfig{Sandbox: json.RawMessage(`false`)}) != nil || restrictiveSandboxOverlay(&MCPConfig{}) != nil {
		t.Fatal("opt-out or absent overlays must bind with the member policy alone")
	}
}

func TestBoundShellKeepsRestrictionsWithoutToolGrants(t *testing.T) {
	root, extra := t.TempDir(), t.TempDir()
	secret := filepath.Join(root, "secret")
	writeBlocked := filepath.Join(root, "protected")
	overlay := sandbox.Config{DenyPaths: []string{secret}, DenyWritePaths: []string{writeBlocked}, DenyWrite: true, DenyDNS: true, AllowNetwork: true, WritablePaths: []string{extra}}
	tool := &ShellTool{Command: "/bin/sh", schema: schema.ToolSchemaFromString(`{"title":"restricted","type":"object","properties":{}}`), sandboxCfg: &overlay}
	registry := NewToolRegistry([]Tool{tool}, WithSandboxFactory(func(sandbox.Config) (sandbox.Sandbox, error) { return &mockSandbox{}, nil }, sandbox.DefaultConfig()))
	defer registry.Close()
	ec, err := registry.ExecutionPolicy(root, ExecutionGrant{})
	if err != nil {
		t.Fatal(err)
	}
	bound, omitted, err := registry.BindExecutionContext(ec, []string{"restricted"})
	if err != nil {
		t.Fatalf("bind: %v; omitted %v", err, omitted)
	}
	defer bound.Close()
	got, _ := bound.Get("restricted")
	config := unwrapTool(got).(*ShellTool).SandboxDetails().Config
	if config == nil || !config.DenyWrite || !config.DenyDNS || config.AllowNetwork {
		t.Fatalf("bound shell policy: %+v", config)
	}
	if sandbox.ReadAllowed(*config, secret) == nil {
		t.Fatal("bound shell lost its read denial")
	}
	if len(config.DenyWritePaths) != 1 || config.DenyWritePaths[0] != writeBlocked {
		t.Fatalf("bound shell lost write island: %v", config.DenyWritePaths)
	}
	for _, path := range config.WritablePaths {
		if path == extra {
			t.Fatal("bound shell retained a tool-local write grant")
		}
	}
}

func TestExecutionPolicyReadOnlyScratch(t *testing.T) {
	canonical := func(path string) string {
		t.Helper()
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}
	registry := NewToolRegistry(nil, WithUnsafeNoSandbox())
	defer registry.Close()
	if _, err := registry.LoadToolAuto("write_file"); err != nil {
		t.Fatal(err)
	}
	root, scratch := canonical(t.TempDir()), canonical(t.TempDir())
	ec, err := registry.ExecutionPolicy(root, ExecutionGrant{ReadOnly: true, Scratch: scratch})
	if err != nil {
		t.Fatal(err)
	}
	if !ec.ReadOnly || ec.Scratch != scratch || ec.Sandbox.DenyWrite || ec.Sandbox.DenyHostTemp || !slices.Equal(ec.Sandbox.WritablePaths, []string{scratch}) || !slices.Contains(ec.Sandbox.DenyWritePaths, root) {
		t.Fatalf("read-only scratch policy = %+v", ec)
	}
	wantEnv := map[string]string{"TMPDIR": scratch, "TMP": scratch, "TEMP": scratch}
	if !maps.Equal(ec.Sandbox.Env, wantEnv) {
		t.Fatalf("scratch env = %v, want %v", ec.Sandbox.Env, wantEnv)
	}
	if err := sandbox.WriteAllowed(ec.Sandbox, filepath.Join(root, "f")); err == nil {
		t.Fatal("read-only root writable in-process")
	}
	if err := sandbox.WriteAllowed(ec.Sandbox, filepath.Join(scratch, "f")); err != nil {
		t.Fatalf("scratch write refused: %v", err)
	}
	// Host temp stays writable, as for every context: bash 3.2 here-documents
	// need a system temp directory or they land in the read-only checkout.
	if err := sandbox.WriteAllowed(ec.Sandbox, filepath.Join(os.TempDir(), "polly-scratch-probe")); err != nil {
		t.Fatalf("host temp refused for a read-only context: %v", err)
	}
	bound, _, err := registry.BindExecutionContext(ec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	writer, _ := bound.Get("write_file")
	if _, err := writer.Execute(context.Background(), map[string]any{"path": "file.txt", "content": "bad"}); err == nil {
		t.Fatal("bound write into the read-only root succeeded")
	}
	if _, err := writer.Execute(context.Background(), map[string]any{"path": filepath.Join(scratch, "note.txt"), "content": "ok"}); err != nil {
		t.Fatalf("bound write into the scratch: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(scratch, "note.txt")); err != nil || string(data) != "ok" {
		t.Fatalf("scratch file = %q %v", data, err)
	}
	if _, err := registry.ExecutionPolicy(root, ExecutionGrant{ReadOnly: true, Scratch: filepath.Join(scratch, "missing")}); err == nil || !strings.Contains(err.Error(), "must be an existing directory") {
		t.Fatalf("missing scratch accepted: %v", err)
	}
	nested := filepath.Join(root, "scratch")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ExecutionPolicy(root, ExecutionGrant{ReadOnly: true, Scratch: nested}); err == nil || !strings.Contains(err.Error(), "outside the execution root") {
		t.Fatalf("nested scratch accepted: %v", err)
	}
	editing, err := registry.ExecutionPolicy(root, ExecutionGrant{Scratch: scratch})
	if err != nil || editing.ReadOnly || editing.Sandbox.DenyHostTemp || editing.Sandbox.DenyWrite || !slices.Equal(editing.Sandbox.WritablePaths, []string{root, scratch}) || !maps.Equal(editing.Sandbox.Env, wantEnv) {
		t.Fatalf("editing scratch policy = %+v %v", editing, err)
	}
}

// Executable --schema discovery keeps the operator's host-temp denial along
// with the other deny rules it retains; the default policy's explicit temp
// grant does not bring it back.
func TestSchemaSandboxKeepsDenyHostTemp(t *testing.T) {
	var configs []sandbox.Config
	factory := func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		configs = append(configs, cfg)
		return &mockSandbox{}, nil
	}
	registry := NewToolRegistry(nil, WithSandboxFactory(factory, sandbox.Config{DenyHostTemp: true}))
	defer registry.Close()
	if _, err := registry.newSchemaSandbox(""); err != nil {
		t.Fatal(err)
	}
	cfg := configs[len(configs)-1]
	if !cfg.DenyHostTemp {
		t.Fatalf("schema discovery dropped denyHostTemp: %+v", cfg)
	}
	if err := sandbox.WriteAllowed(cfg, filepath.Join(os.TempDir(), "polly-schema-probe")); err == nil {
		t.Fatal("host temp stayed writable during schema discovery")
	}
}

// An operator's readonly preset keeps denying every write, scratch included.
func TestDenyWritePresetStillDeniesScratch(t *testing.T) {
	registry := NewToolRegistry(nil, WithSandboxFactory(mockSandboxFactory(&mockSandbox{}), sandbox.Config{DenyWrite: true}))
	defer registry.Close()
	scratch := t.TempDir()
	for _, readOnly := range []bool{true, false} {
		ec, err := registry.ExecutionPolicy(t.TempDir(), ExecutionGrant{ReadOnly: readOnly, Scratch: scratch})
		if err != nil {
			t.Fatal(err)
		}
		if !ec.Sandbox.DenyWrite || !ec.ReadOnly || ec.Sandbox.Env != nil {
			t.Fatalf("readOnly=%v: operator denyWrite weakened: %+v", readOnly, ec.Sandbox)
		}
		if err := sandbox.WriteAllowed(ec.Sandbox, filepath.Join(scratch, "f")); err == nil || !strings.Contains(err.Error(), "denies all file writes") {
			t.Fatalf("readOnly=%v: scratch writable under denyWrite: %v", readOnly, err)
		}
	}
}

// An explicit credential grant in the parent (the ssh preset's ~/.ssh/config)
// reaches a member like any other read grant.
func TestExecutionPolicyInheritsCredentialReadGrants(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sshConfig := filepath.Join(home, ".ssh", "config")
	toolchain := filepath.Join(home, "toolchain")
	if err := os.MkdirAll(filepath.Dir(sshConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sshConfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(toolchain, 0o700); err != nil {
		t.Fatal(err)
	}
	base := sandbox.DefaultConfig()
	base.ReadPaths = []string{sshConfig, toolchain}
	registry := NewToolRegistry(nil, WithSandboxFactory(sandbox.New, base))
	defer registry.Close()
	root := t.TempDir()
	ec, err := registry.ExecutionPolicy(root, ExecutionGrant{})
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, path := range []string{sshConfig, toolchain} {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, resolved)
	}
	if !slices.Equal(ec.Sandbox.ReadPaths, want) {
		t.Fatalf("member ReadPaths = %v, want the credential and toolchain grants %v", ec.Sandbox.ReadPaths, want)
	}
	if err := sandbox.ReadAllowed(ec.Sandbox, want[0]); err != nil {
		t.Fatalf("member cannot read the inherited credential grant: %v", err)
	}
}

// The parent's Unix-socket grants (the ssh preset's agent) reach a member,
// except a socket inside a path the member may not read.
func TestExecutionPolicyInheritsSocketGrants(t *testing.T) {
	agent := filepath.Join(t.TempDir(), "agent.sock")
	parent := t.TempDir()
	hidden := filepath.Join(parent, "server.sock")
	// Preparation drops a grant that does not exist and freezes the rest to
	// their real paths; the socket type is checked only when a command runs.
	for _, path := range []string{agent, hidden} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	agent, err := filepath.EvalSymlinks(agent)
	if err != nil {
		t.Fatal(err)
	}
	base := sandbox.DefaultConfig()
	base.AllowUnixSockets = []string{agent, hidden}
	registry := NewToolRegistry(nil, WithSandboxFactory(func(sandbox.Config) (sandbox.Sandbox, error) { return &mockSandbox{}, nil }, base))
	defer registry.Close()
	ec, err := registry.ExecutionPolicy(t.TempDir(), ExecutionGrant{DeniedReads: []string{parent}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ec.Sandbox.AllowUnixSockets, []string{agent}) {
		t.Fatalf("member AllowUnixSockets = %v, want only the agent socket %q", ec.Sandbox.AllowUnixSockets, agent)
	}
}

func TestExecutionPolicyDropsInheritedGrantsUnderDeniedReads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	notes := filepath.Join(home, "notes")
	work := filepath.Join(notes, "work")
	toolchain := filepath.Join(home, "toolchain")
	for _, dir := range []string{work, toolchain} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	base := sandbox.DefaultConfig()
	base.ReadPaths = []string{work, toolchain}
	registry := NewToolRegistry(nil, WithSandboxFactory(sandbox.New, base))
	defer registry.Close()
	ec, err := registry.ExecutionPolicy(t.TempDir(), ExecutionGrant{DeniedReads: []string{notes}})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(toolchain)
	if err != nil {
		t.Fatal(err)
	}
	if len(ec.Sandbox.ReadPaths) != 1 || ec.Sandbox.ReadPaths[0] != resolved {
		t.Fatalf("member ReadPaths = %v, want only the toolchain grant %q", ec.Sandbox.ReadPaths, resolved)
	}
	if err := sandbox.ReadAllowed(ec.Sandbox, filepath.Join(work, "secret.txt")); err == nil {
		t.Fatal("inherited grant under a member's denied read stayed readable")
	}
}

// A shell tool that lives inside a denied path never loads and is omitted
// when a context binds it: exposing its executable would otherwise override
// the mask.
func TestShellToolInsideDeniedPathIsRefused(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	denied := filepath.Join(dir, "denied")
	if err := os.Mkdir(denied, 0o700); err != nil {
		t.Fatal(err)
	}
	script := createTestScript(t, denied)
	base := sandbox.DefaultConfig()
	base.DenyPaths = []string{denied}
	factory := func(sandbox.Config) (sandbox.Sandbox, error) { return &mockSandbox{}, nil }
	registry := NewToolRegistry(nil, WithSandboxFactory(factory, base))
	defer registry.Close()
	if _, err := registry.LoadShellToolWithNamespace(script, "fixture"); err == nil || !strings.Contains(err.Error(), "blocked from reads") {
		t.Fatalf("loading a shell tool inside a denied path = %v, want a mask refusal", err)
	}
	tool := &ShellTool{Command: script, schema: schema.ToolSchemaFromString(`{"title":"denied","type":"object","properties":{}}`)}
	parent := NewToolRegistry([]Tool{tool}, WithSandboxFactory(factory, base))
	defer parent.Close()
	ec, err := parent.ExecutionPolicy(dir, ExecutionGrant{})
	if err != nil {
		t.Fatal(err)
	}
	bound, omitted, err := parent.BindExecutionContext(ec, nil)
	if err != nil {
		t.Fatalf("bind: %v omitted %v", err, omitted)
	}
	defer bound.Close()
	if _, ok := bound.Get("denied"); ok || !slices.Contains(omitted, "denied") {
		t.Fatalf("bound a shell tool inside a denied path: omitted=%v", omitted)
	}
	if _, _, err := parent.BindExecutionContext(ec, []string{"denied"}); err == nil {
		t.Fatal("a required shell tool inside a denied path must fail the bind")
	}
}
