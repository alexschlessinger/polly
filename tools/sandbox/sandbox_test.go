package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// stubSandbox lets a test control how the probe command is wrapped.
type stubSandbox struct{ rewrite func(*exec.Cmd) }

func (s stubSandbox) Wrap(cmd *exec.Cmd) error {
	if s.rewrite != nil {
		s.rewrite(cmd)
	}
	return nil
}

func TestProbeNilSandbox(t *testing.T) {
	if err := Probe(nil); err != nil {
		t.Fatalf("Probe(nil) = %v, want nil (no sandbox = nothing to probe)", err)
	}
}

func TestProbePassesWhenCommandSucceeds(t *testing.T) {
	// stub leaves the probe command (true) intact -> it should exit 0.
	if err := Probe(stubSandbox{}); err != nil {
		t.Fatalf("Probe() = %v, want nil when the sandboxed command succeeds", err)
	}
}

func TestNormalizeConfigPathsMakesRelativePolicyPathsAbsolute(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	original := Config{
		WritablePaths: []string{"build/output"},
		ReadPaths:     []string{"secrets/readable"},
		DenyPaths:     []string{".env"},
	}

	got, err := normalizeConfigPaths(original)
	if err != nil {
		t.Fatalf("normalizeConfigPaths() error = %v", err)
	}
	if want := filepath.Join(cwd, "build/output"); got.WritablePaths[0] != want {
		t.Fatalf("WritablePaths[0] = %q, want %q", got.WritablePaths[0], want)
	}
	if want := filepath.Join(cwd, "secrets/readable"); got.ReadPaths[0] != want {
		t.Fatalf("ReadPaths[0] = %q, want %q", got.ReadPaths[0], want)
	}
	if want := filepath.Join(cwd, ".env"); got.DenyPaths[0] != want {
		t.Fatalf("DenyPaths[0] = %q, want %q", got.DenyPaths[0], want)
	}
	if original.DenyPaths[0] != ".env" {
		t.Fatalf("normalizeConfigPaths mutated its input: %+v", original)
	}
}

func TestNormalizeConfigPathsRejectsEmptyPath(t *testing.T) {
	if _, err := normalizeConfigPaths(Config{DenyPaths: []string{""}}); err == nil {
		t.Fatal("normalizeConfigPaths() accepted an empty deny path")
	}
}

func TestNormalizeConfigPathsCopiesPassEnv(t *testing.T) {
	pass := []string{"SSH_AUTH_SOCK"}
	prepared, err := PrepareConfig(Config{PassEnv: pass})
	if err != nil {
		t.Fatalf("PrepareConfig() error = %v", err)
	}
	pass[0] = "MUTATED"
	if len(prepared.PassEnv) != 1 || prepared.PassEnv[0] != "SSH_AUTH_SOCK" {
		t.Fatalf("prepared PassEnv aliases caller slice: %v", prepared.PassEnv)
	}
}

func TestNormalizeConfigPathsCopiesAllowEnv(t *testing.T) {
	allow := []string{"SAFE_NAME"}
	prepared, err := PrepareConfig(Config{AllowEnv: allow})
	if err != nil {
		t.Fatal(err)
	}
	allow[0] = "AWS_SECRET_ACCESS_KEY"
	if len(prepared.AllowEnv) != 1 || prepared.AllowEnv[0] != "SAFE_NAME" {
		t.Fatalf("prepared AllowEnv aliases caller slice: %v", prepared.AllowEnv)
	}
}

func TestResolvedExecutablePathUsesAbsoluteBaseForRelativeDir(t *testing.T) {
	skipIfWindows(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tool := filepath.Join(dir, "tool")
	if err := os.WriteFile(tool, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	relativeDir, err := filepath.Rel(cwd, dir)
	if err != nil {
		t.Fatal(err)
	}
	cmd := &exec.Cmd{Path: "./tool", Dir: relativeDir}
	got, err := resolvedExecutablePath(cmd)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(tool)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("resolvedExecutablePath() = %q, want %q", got, want)
	}
}

func TestFreezeAuthorityPathsCanonicalizesDropsAndMinimizes(t *testing.T) {
	skipIfWindows(t)
	root := t.TempDir()
	writable := filepath.Join(root, "writable")
	nestedWritable := filepath.Join(writable, "nested")
	readable := filepath.Join(root, "readable")
	nestedReadable := filepath.Join(readable, "nested")
	symlinkTarget := filepath.Join(root, "symlink-target")
	for _, path := range []string{nestedWritable, nestedReadable, symlinkTarget} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	writableLink := filepath.Join(root, "writable-link")
	readLink := filepath.Join(root, "read-link")
	if err := os.Symlink(nestedWritable, writableLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(symlinkTarget, readLink); err != nil {
		t.Fatal(err)
	}

	cfg, err := freezeAuthorityPaths(Config{
		WritablePaths: []string{writable, nestedWritable, writableLink, filepath.Join(root, "missing-write")},
		ReadPaths:     []string{readable, nestedReadable, readLink, filepath.Join(root, "missing-read")},
	})
	if err != nil {
		t.Fatal(err)
	}
	writableReal, _ := filepath.EvalSymlinks(writable)
	readableReal, _ := filepath.EvalSymlinks(readable)
	symlinkTargetReal, _ := filepath.EvalSymlinks(symlinkTarget)
	if len(cfg.WritablePaths) != 1 || cfg.WritablePaths[0] != writableReal {
		t.Fatalf("frozen writablePaths = %v, want only canonical parent %q", cfg.WritablePaths, writableReal)
	}
	wantReads := map[string]bool{readableReal: true, symlinkTargetReal: true}
	if len(cfg.ReadPaths) != len(wantReads) {
		t.Fatalf("frozen readPaths = %v, want %v", cfg.ReadPaths, wantReads)
	}
	for _, path := range cfg.ReadPaths {
		if !wantReads[path] {
			t.Fatalf("unexpected frozen readPath %q in %v", path, cfg.ReadPaths)
		}
	}

	// Linux private roots are not host-backed when named exactly. Their
	// descendants therefore remain effective explicit grants.
	nonCovering, err := freezeAuthorityPaths(Config{WritablePaths: []string{writable, nestedWritable}}, writableReal)
	if err != nil {
		t.Fatal(err)
	}
	if len(nonCovering.WritablePaths) != 2 {
		t.Fatalf("non-covering root removed nested grant: %v", nonCovering.WritablePaths)
	}
}

func TestPrepareConfigPreservesAndValidatesReadPathAlias(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	for _, path := range []string{first, second} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(root, "read-alias")
	if err := os.Symlink(first, alias); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareConfig(Config{ReadPaths: []string{alias}})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.ReadPaths) != 1 || prepared.ReadPaths[0] != first {
		t.Fatalf("prepared ReadPaths = %v, want canonical target %q", prepared.ReadPaths, first)
	}
	if len(prepared.readPathAliases) != 1 || prepared.readPathAliases[0].path != alias || prepared.readPathAliases[0].target != first {
		t.Fatalf("prepared read alias metadata = %+v", prepared.readPathAliases)
	}
	foundLeaf := false
	for _, symlink := range prepared.readPathAliases[0].symlinks {
		if symlink.path == alias {
			foundLeaf = true
		}
	}
	if !foundLeaf {
		t.Fatalf("prepared alias route omits lexical symlink %q: %+v", alias, prepared.readPathAliases[0].symlinks)
	}
	if _, err := PrepareConfig(prepared); err != nil {
		t.Fatalf("repeated PrepareConfig rejected unchanged alias: %v", err)
	}
	broader, err := PrepareConfig(prepared.Merge(Config{ReadPaths: []string{root}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(broader.readPathAliases) != 1 {
		t.Fatalf("broader merged read grant dropped covered alias metadata: %+v", broader.readPathAliases)
	}
	deepCopy := prepared.Merge(Config{})
	deepCopy.readPathAliases[0].path = second
	deepCopy.readPathAliases[0].symlinks[0].path = second
	if prepared.readPathAliases[0].path != alias || prepared.readPathAliases[0].symlinks[0].path == second {
		t.Fatal("Config.Merge aliased private read-path metadata")
	}
	disjoint := prepared.Merge(Config{})
	disjoint.ReadPaths = []string{second}
	disjoint, err = PrepareConfig(disjoint)
	if err != nil {
		t.Fatalf("PrepareConfig rejected a disjoint replacement read grant: %v", err)
	}
	if len(disjoint.readPathAliases) != 0 {
		t.Fatalf("disjoint ReadPaths retained private alias authority: %+v", disjoint.readPathAliases)
	}
	cleared := prepared
	cleared.ReadPaths = nil
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareConfig(prepared); err == nil {
		t.Fatal("PrepareConfig accepted a replaced and retargeted read-path alias")
	}
	cleared, err = PrepareConfig(cleared)
	if err != nil {
		t.Fatalf("PrepareConfig validated an alias after its public grant was removed: %v", err)
	}
	if len(cleared.readPathAliases) != 0 {
		t.Fatalf("cleared ReadPaths retained private alias authority: %+v", cleared.readPathAliases)
	}
}

func TestPrepareConfigRejectsReplacementAcrossMergeAndLaterConstruction(t *testing.T) {
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	granted := filepath.Join(root, "granted")
	external := filepath.Join(root, "external")
	for _, path := range []string{granted, external} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}

	prepared, err := PrepareConfig(Config{WritablePaths: []string{granted}})
	if err != nil {
		t.Fatalf("PrepareConfig() error = %v", err)
	}
	if len(prepared.authorityPaths) != 1 || prepared.authorityPaths[0].path != granted {
		t.Fatalf("prepared authority identities = %+v, want %q", prepared.authorityPaths, granted)
	}
	merged, err := PrepareConfig(prepared.Merge(Config{
		AllowNetwork:  true,
		WritablePaths: []string{root},
	}))
	if err != nil {
		t.Fatalf("PrepareConfig(merged parent) error = %v", err)
	}
	if len(merged.WritablePaths) == 0 || merged.WritablePaths[len(merged.WritablePaths)-1] != root {
		t.Fatalf("merged writable paths = %v, want broader parent %q", merged.WritablePaths, root)
	}
	if len(merged.authorityPaths) != 2 {
		t.Fatalf("broader merge dropped prepared child identity: %+v", merged.authorityPaths)
	}

	original := filepath.Join(root, "granted-original")
	if err := os.Rename(granted, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, granted); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareConfig(merged); err == nil {
		t.Fatal("PrepareConfig() accepted a prepared authority path rerouted before later construction")
	}
}

func TestNormalizeAndMergeCopyPreparedAuthorityIdentities(t *testing.T) {
	path := t.TempDir()
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareConfig(Config{ReadPaths: []string{path}})
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := normalizeConfigPaths(prepared)
	if err != nil {
		t.Fatal(err)
	}
	merged := prepared.Merge(Config{})

	normalized.authorityPaths[0].path = "/mutated-normalized"
	merged.authorityPaths[0].path = "/mutated-merged"
	if got := prepared.authorityPaths[0].path; got != path {
		t.Fatalf("prepared authority metadata was aliased: %q", got)
	}
}

func TestProbeFailsWhenCommandFails(t *testing.T) {
	// Rewrite the probe command to one that exits non-zero, simulating a
	// sandbox backend that is present but can't actually start a command.
	sb := stubSandbox{rewrite: func(cmd *exec.Cmd) {
		p, err := exec.LookPath("false")
		if err != nil {
			t.Skipf("no 'false' binary: %v", err)
		}
		cmd.Path = p
		cmd.Args = []string{"false"}
	}}
	if err := Probe(sb); err == nil {
		t.Fatal("Probe() = nil, want error when the sandboxed command fails to run")
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg := Config{}
	if cfg.AllowNetwork {
		t.Fatal("AllowNetwork should default to false")
	}
	if len(cfg.WritablePaths) != 0 {
		t.Fatalf("WritablePaths should default to empty, got %v", cfg.WritablePaths)
	}
}

func TestDeniedPathsNotEmpty(t *testing.T) {
	if len(DeniedPaths) == 0 {
		t.Fatal("DeniedPaths should not be empty")
	}
	var hasDir, hasFile bool
	for _, p := range DeniedPaths {
		if !strings.HasPrefix(p.Path, "~/") {
			t.Fatalf("DeniedPaths entry %q should start with ~/", p.Path)
		}
		switch p.Kind {
		case DeniedPathDir:
			hasDir = true
		case DeniedPathFile:
			hasFile = true
		default:
			t.Fatalf("DeniedPaths entry %q has unknown kind %q", p.Path, p.Kind)
		}
	}
	if !hasDir {
		t.Fatal("DeniedPaths should include at least one directory")
	}
	if !hasFile {
		t.Fatal("DeniedPaths should include at least one file")
	}
}

func TestParseConfig(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		isNil     bool
		wantErr   bool
		net       bool
		denyDNS   bool
		paths     int
		readPaths int
		allowEnv  int
		denyWrite bool
	}{
		{"true", `true`, false, false, false, false, 0, 0, 0, false},
		{"false", `false`, true, false, false, false, 0, 0, 0, false},
		{"null", `null`, true, false, false, false, 0, 0, 0, false},
		{"empty", ``, true, false, false, false, 0, 0, 0, false},
		{"object defaults", `{}`, false, false, false, false, 0, 0, 0, false},
		{"allow network", `{"allowNetwork":true}`, false, false, true, false, 0, 0, 0, false},
		{"writable paths", `{"writablePaths":["/a","/b"]}`, false, false, false, false, 2, 0, 0, false},
		{"full", `{"allowNetwork":true,"writablePaths":["/x"]}`, false, false, true, false, 1, 0, 0, false},
		{"readPaths", `{"readPaths":["~/.aws"]}`, false, false, false, false, 0, 1, 0, false},
		{"allowEnv", `{"allowEnv":["HOME","PATH"]}`, false, false, false, false, 0, 0, 2, false},
		{"denyWrite", `{"denyWrite":true}`, false, false, false, false, 0, 0, 0, true},
		{"denyDNS", `{"denyDNS":true}`, false, false, false, true, 0, 0, 0, false},
		{"denyDNS with network", `{"allowNetwork":true,"denyDNS":true}`, false, false, true, true, 0, 0, 0, false},
		{"invalid string", `"yes"`, false, true, false, false, 0, 0, 0, false},
		{"invalid number", `123`, false, true, false, false, 0, 0, 0, false},
		{"invalid array", `["network"]`, false, true, false, false, 0, 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := ParseConfig(json.RawMessage(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for input %s, got config %+v", tt.input, cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.isNil {
				if cfg != nil {
					t.Fatalf("expected nil, got %+v", cfg)
				}
				return
			}
			if cfg == nil {
				t.Fatal("expected non-nil config")
			}
			if cfg.AllowNetwork != tt.net {
				t.Fatalf("AllowNetwork = %v, want %v", cfg.AllowNetwork, tt.net)
			}
			if cfg.DenyDNS != tt.denyDNS {
				t.Fatalf("DenyDNS = %v, want %v", cfg.DenyDNS, tt.denyDNS)
			}
			if len(cfg.WritablePaths) != tt.paths {
				t.Fatalf("WritablePaths = %v, want %d entries", cfg.WritablePaths, tt.paths)
			}
			if len(cfg.ReadPaths) != tt.readPaths {
				t.Fatalf("ReadPaths = %v, want %d entries", cfg.ReadPaths, tt.readPaths)
			}
			if len(cfg.AllowEnv) != tt.allowEnv {
				t.Fatalf("AllowEnv = %v, want %d entries", cfg.AllowEnv, tt.allowEnv)
			}
			if cfg.DenyWrite != tt.denyWrite {
				t.Fatalf("DenyWrite = %v, want %v", cfg.DenyWrite, tt.denyWrite)
			}
		})
	}
}

func TestMerge(t *testing.T) {
	base := Config{
		WritablePaths: []string{"/work"},
		AllowNetwork:  false,
		ReadPaths:     []string{"~/.kube"},
		AllowEnv:      []string{"HOME"},
		PassEnv:       []string{"SSH_AUTH_SOCK"},
	}
	overlay := Config{
		AllowNetwork:   true,
		DenyDNS:        true,
		WritablePaths:  []string{"/extra"},
		ReadPaths:      []string{"~/.aws"},
		AllowEnv:       []string{"PATH"},
		PassEnv:        []string{"GPG_AGENT_INFO"},
		DenyWrite:      true,
		DenyWritePaths: []string{"/work/.git/hooks"},
	}
	merged := base.Merge(overlay)

	if !merged.AllowNetwork {
		t.Fatal("Merge should set AllowNetwork to true")
	}
	if !merged.DenyDNS {
		t.Fatal("Merge should set DenyDNS to true")
	}
	if len(merged.WritablePaths) != 2 {
		t.Fatalf("WritablePaths = %v, want 2 entries", merged.WritablePaths)
	}
	if merged.WritablePaths[0] != "/work" || merged.WritablePaths[1] != "/extra" {
		t.Fatalf("WritablePaths = %v, want [/work /extra]", merged.WritablePaths)
	}
	if len(merged.ReadPaths) != 2 || merged.ReadPaths[0] != "~/.kube" || merged.ReadPaths[1] != "~/.aws" {
		t.Fatalf("ReadPaths = %v, want [~/.kube ~/.aws]", merged.ReadPaths)
	}
	if len(merged.AllowEnv) != 2 || merged.AllowEnv[0] != "HOME" || merged.AllowEnv[1] != "PATH" {
		t.Fatalf("AllowEnv = %v, want [HOME PATH]", merged.AllowEnv)
	}
	if len(merged.PassEnv) != 2 || merged.PassEnv[0] != "SSH_AUTH_SOCK" || merged.PassEnv[1] != "GPG_AGENT_INFO" {
		t.Fatalf("PassEnv = %v, want [SSH_AUTH_SOCK GPG_AGENT_INFO]", merged.PassEnv)
	}
	if !merged.DenyWrite {
		t.Fatal("Merge should set DenyWrite to true")
	}
	if len(merged.DenyWritePaths) != 1 || merged.DenyWritePaths[0] != "/work/.git/hooks" {
		t.Fatalf("DenyWritePaths = %v, want [/work/.git/hooks]", merged.DenyWritePaths)
	}
}

func TestMergeDenyDNSOR(t *testing.T) {
	// DenyDNS should OR: if either side sets it, the result is true.
	base := Config{DenyDNS: true}
	overlay := Config{DenyDNS: false}
	merged := base.Merge(overlay)
	if !merged.DenyDNS {
		t.Fatal("DenyDNS should be true when base has it set")
	}

	base2 := Config{DenyDNS: false}
	overlay2 := Config{DenyDNS: true}
	merged2 := base2.Merge(overlay2)
	if !merged2.DenyDNS {
		t.Fatal("DenyDNS should be true when overlay has it set")
	}
}

type mockSandbox struct {
	called bool
	err    error
}

type extraFileSandbox struct {
	file    *os.File
	prepend bool
	err     error
}

type managedExtraFileSandbox struct {
	extraFiles []*os.File
	err        error
}

func wrapCmdForTest(t *testing.T, sb Sandbox, cmd *exec.Cmd) error {
	t.Helper()
	cleanup, err := WrapCmdManaged(sb, cmd)
	if err == nil {
		t.Cleanup(func() { _ = cleanup() })
	}
	return err
}

func (s extraFileSandbox) Wrap(cmd *exec.Cmd) error {
	if s.prepend {
		cmd.ExtraFiles = append([]*os.File{s.file}, cmd.ExtraFiles...)
	} else {
		cmd.ExtraFiles = append(cmd.ExtraFiles, s.file)
	}
	return s.err
}

func (s managedExtraFileSandbox) Wrap(cmd *exec.Cmd) error {
	return ErrManagedWrapRequired
}

func (s managedExtraFileSandbox) wrapManaged(cmd *exec.Cmd, _ map[string]string) error {
	cmd.ExtraFiles = append(cmd.ExtraFiles, s.extraFiles...)
	return s.err
}

func (m *mockSandbox) Wrap(cmd *exec.Cmd) error {
	m.called = true
	return m.err
}

func TestWrapCmdNilSandbox(t *testing.T) {
	cmd := exec.Command("echo", "hello")
	if err := WrapCmd(nil, cmd); err != nil {
		t.Fatalf("WrapCmd(nil) should return nil, got %v", err)
	}
}

func TestWrapCmdApplied(t *testing.T) {
	sb := &mockSandbox{}
	cmd := exec.Command("echo", "hello")
	if err := WrapCmd(sb, cmd); err != nil {
		t.Fatalf("WrapCmd returned unexpected error: %v", err)
	}
	if !sb.called {
		t.Fatal("expected Wrap to be called")
	}
}

func TestWrapCmdError(t *testing.T) {
	sb := &mockSandbox{err: fmt.Errorf("denied")}
	cmd := exec.Command("echo", "hello")
	if err := WrapCmd(sb, cmd); err == nil {
		t.Fatal("expected error from WrapCmd")
	}
}

func TestWrapCmdWithEnvRejectsSandboxWithoutExplicitEnvSupport(t *testing.T) {
	sb := &mockSandbox{}
	cmd := exec.Command("echo", "hello")
	err := WrapCmdWithEnv(sb, cmd, map[string]string{"LD_PRELOAD": "/tmp/inject.so"})
	if err == nil || !strings.Contains(err.Error(), "cannot safely pass explicit target environment") {
		t.Fatalf("WrapCmdWithEnv error = %v, want fail-closed unsupported-sandbox error", err)
	}
	if sb.called {
		t.Fatal("unsupported sandbox was called after explicit environment validation failed")
	}
}

func TestWrapCmdWithEnvNilSandboxOverridesInheritedValue(t *testing.T) {
	cmd := exec.Command("echo", "hello")
	cmd.Env = []string{"TOKEN=ambient", "SAFE=kept"}
	if err := WrapCmdWithEnv(nil, cmd, map[string]string{"TOKEN": "explicit"}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Env, " ")
	if joined != "SAFE=kept TOKEN=explicit" {
		t.Fatalf("merged Env = %q, want %q", joined, "SAFE=kept TOKEN=explicit")
	}
}

func TestWrapCmdWithEnvRejectsInvalidExplicitEntries(t *testing.T) {
	tests := []map[string]string{
		{"": "value"},
		{"BAD=NAME": "value"},
		{"BAD\x00NAME": "value"},
		{"NAME": "bad\x00value"},
	}
	for _, explicitEnv := range tests {
		cmd := exec.Command("true")
		if err := WrapCmdWithEnv(nil, cmd, explicitEnv); err == nil {
			t.Fatalf("WrapCmdWithEnv accepted invalid environment %q", explicitEnv)
		}
		if cmd.Env != nil {
			t.Fatalf("invalid environment mutated command Env: %v", cmd.Env)
		}
	}
}

func TestWrapCmdManagedLeavesLegacySandboxExtraFilesOwnedBySandbox(t *testing.T) {
	borrowedFile, err := os.CreateTemp(t.TempDir(), "legacy-extra-*")
	if err != nil {
		t.Fatal(err)
	}
	defer borrowedFile.Close()
	sb := extraFileSandbox{file: borrowedFile}

	for i := 0; i < 2; i++ {
		cmd := exec.Command("true")
		cleanup, err := WrapCmdManaged(sb, cmd)
		if err != nil {
			t.Fatal(err)
		}
		if err := cleanup(); err != nil {
			t.Fatal(err)
		}
		if len(cmd.ExtraFiles) != 1 || cmd.ExtraFiles[0] != borrowedFile {
			t.Fatalf("ExtraFiles = %v, want legacy sandbox file", cmd.ExtraFiles)
		}
		if _, err := borrowedFile.Stat(); err != nil {
			t.Fatalf("legacy sandbox descriptor was closed after wrap %d: %v", i+1, err)
		}
	}
}

func TestWrapCmdManagedLeavesLegacySandboxExtraFilesOwnedOnError(t *testing.T) {
	borrowedFile, err := os.CreateTemp(t.TempDir(), "legacy-error-extra-*")
	if err != nil {
		t.Fatal(err)
	}
	defer borrowedFile.Close()
	cmd := exec.Command("true")
	cleanup, err := WrapCmdManaged(extraFileSandbox{file: borrowedFile, err: fmt.Errorf("wrap failed")}, cmd)
	if err == nil {
		t.Fatal("WrapCmdManaged returned nil error")
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := borrowedFile.Stat(); err != nil {
		t.Fatalf("legacy sandbox descriptor was closed on Wrap error: %v", err)
	}
}

func TestWrapCmdManagedLeavesLegacySandboxExtraFileOrderUntouched(t *testing.T) {
	callerFile, err := os.CreateTemp(t.TempDir(), "legacy-caller-extra-*")
	if err != nil {
		t.Fatal(err)
	}
	defer callerFile.Close()
	borrowedFile, err := os.CreateTemp(t.TempDir(), "legacy-prepended-extra-*")
	if err != nil {
		t.Fatal(err)
	}
	defer borrowedFile.Close()
	cmd := exec.Command("true")
	cmd.ExtraFiles = []*os.File{callerFile}
	cleanup, err := WrapCmdManaged(extraFileSandbox{file: borrowedFile, prepend: true}, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if len(cmd.ExtraFiles) != 2 || cmd.ExtraFiles[0] != borrowedFile || cmd.ExtraFiles[1] != callerFile {
		t.Fatalf("ExtraFiles = %v, want legacy sandbox order", cmd.ExtraFiles)
	}
	if _, err := callerFile.Stat(); err != nil {
		t.Fatalf("caller-owned descriptor was closed: %v", err)
	}
	if _, err := borrowedFile.Stat(); err != nil {
		t.Fatalf("legacy sandbox descriptor was closed: %v", err)
	}
}

func TestWrapCmdManagedCleanupPreservesCallerExtraFiles(t *testing.T) {
	callerFile, err := os.CreateTemp(t.TempDir(), "caller-extra-*")
	if err != nil {
		t.Fatal(err)
	}
	defer callerFile.Close()
	ownedFile, err := os.CreateTemp(t.TempDir(), "sandbox-extra-*")
	if err != nil {
		t.Fatal(err)
	}
	laterCallerFile, err := os.CreateTemp(t.TempDir(), "later-caller-extra-*")
	if err != nil {
		t.Fatal(err)
	}
	defer laterCallerFile.Close()
	cmd := exec.Command("true")
	cmd.ExtraFiles = []*os.File{callerFile}
	cleanup, err := WrapCmdManaged(managedExtraFileSandbox{extraFiles: []*os.File{ownedFile}}, cmd)
	if err != nil {
		t.Fatal(err)
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, laterCallerFile)

	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if len(cmd.ExtraFiles) != 2 || cmd.ExtraFiles[0] != callerFile || cmd.ExtraFiles[1] != laterCallerFile {
		t.Fatalf("ExtraFiles = %v, want both caller-owned files", cmd.ExtraFiles)
	}
	if _, err := callerFile.Stat(); err != nil {
		t.Fatalf("caller-owned descriptor was closed: %v", err)
	}
	if _, err := ownedFile.Stat(); err == nil {
		t.Fatal("sandbox-owned descriptor remains open")
	}
	if _, err := laterCallerFile.Stat(); err != nil {
		t.Fatalf("later caller-owned descriptor was closed: %v", err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("second cleanup call = %v", err)
	}
}

func TestWrapCmdManagedClosesAppendedFilesOnWrapError(t *testing.T) {
	ownedFile, err := os.CreateTemp(t.TempDir(), "sandbox-error-extra-*")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("true")
	if _, err := WrapCmdManaged(managedExtraFileSandbox{
		extraFiles: []*os.File{ownedFile},
		err:        fmt.Errorf("wrap failed"),
	}, cmd); err == nil {
		t.Fatal("WrapCmdManaged returned nil error")
	}
	if _, err := ownedFile.Stat(); err == nil {
		t.Fatal("sandbox-owned descriptor remains open after Wrap error")
	}
}

func TestWrapCmdManagedClosesDuplicateOwnedFileOnce(t *testing.T) {
	ownedFile, err := os.CreateTemp(t.TempDir(), "sandbox-duplicate-extra-*")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("true")
	cleanup, err := WrapCmdManaged(managedExtraFileSandbox{
		extraFiles: []*os.File{ownedFile, ownedFile},
	}, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup duplicate descriptor: %v", err)
	}
	if len(cmd.ExtraFiles) != 0 {
		t.Fatalf("ExtraFiles = %v, want empty", cmd.ExtraFiles)
	}
	if _, err := ownedFile.Stat(); err == nil {
		t.Fatal("sandbox-owned descriptor remains open")
	}
	if err := cleanup(); err != nil {
		t.Fatalf("second cleanup call = %v", err)
	}
}
func TestPrepareConfigDenyWriteDropsObsoleteWritableIdentity(t *testing.T) {
	root := t.TempDir()
	writable := filepath.Join(root, "writable")
	if err := os.Mkdir(writable, 0o700); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareConfig(Config{WritablePaths: []string{writable}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(writable); err != nil {
		t.Fatal(err)
	}

	readOnly, err := PrepareConfig(prepared.Merge(Config{DenyWrite: true}))
	if err != nil {
		t.Fatalf("DenyWrite retained obsolete writable identity: %v", err)
	}
	if len(readOnly.WritablePaths) != 0 || len(readOnly.authorityPaths) != 0 {
		t.Fatalf("DenyWrite config retained writable authority: paths=%v identities=%v", readOnly.WritablePaths, readOnly.authorityPaths)
	}
}

func TestPrepareConfigDenyWriteRetainsCurrentReadIdentity(t *testing.T) {
	skipIfWindows(t)
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareConfig(Config{WritablePaths: []string{shared}})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err = PrepareConfig(prepared.Merge(Config{ReadPaths: []string{shared}}))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(shared, shared+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := PrepareConfig(prepared.Merge(Config{DenyWrite: true})); err == nil {
		t.Fatal("DenyWrite dropped identity still required by ReadPaths")
	}
}

func TestPrepareConfigDenyWriteCanonicalizesRestoredReadAliasBeforeIdentityFiltering(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	target2 := filepath.Join(root, "target2")
	alias := filepath.Join(root, "alias")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target2, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareConfig(Config{ReadPaths: []string{alias}})
	if err != nil {
		t.Fatal(err)
	}

	unchanged := prepared.Merge(Config{})
	unchanged.ReadPaths = []string{alias}
	unchanged.DenyWrite = true
	unchanged, err = PrepareConfig(unchanged)
	if err != nil {
		t.Fatalf("unchanged prepared alias was rejected under DenyWrite: %v", err)
	}
	canonicalTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(unchanged.ReadPaths) != 1 || unchanged.ReadPaths[0] != canonicalTarget {
		t.Fatalf("canonical ReadPaths = %v, want [%s]", unchanged.ReadPaths, canonicalTarget)
	}

	if err := os.Rename(target, target+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	replaced := prepared.Merge(Config{})
	replaced.ReadPaths = []string{alias}
	replaced.DenyWrite = true
	if _, err := PrepareConfig(replaced); err == nil {
		t.Fatal("DenyWrite accepted replaced canonical target through restored ReadPaths alias")
	}

	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(target+"-old", target); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target2, alias); err != nil {
		t.Fatal(err)
	}
	retargeted := prepared.Merge(Config{})
	retargeted.ReadPaths = []string{alias}
	retargeted.DenyWrite = true
	if _, err := PrepareConfig(retargeted); err == nil {
		t.Fatal("DenyWrite accepted retargeted restored ReadPaths alias")
	}
}

func TestPrepareConfigNarrowedReadAliasDropsBroaderAliasMetadata(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	alias := filepath.Join(root, "alias")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	allowedTarget := filepath.Join(target, "allowed.txt")
	if err := os.WriteFile(allowedTarget, []byte("allowed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "other.txt"), []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareConfig(Config{ReadPaths: []string{alias}})
	if err != nil {
		t.Fatal(err)
	}

	narrowed := prepared.Merge(Config{})
	allowedAlias := filepath.Join(alias, "allowed.txt")
	narrowed.ReadPaths = []string{allowedAlias}
	narrowed.DenyWrite = true
	narrowed, err = PrepareConfig(narrowed)
	if err != nil {
		t.Fatal(err)
	}
	if len(narrowed.readPathAliases) != 1 {
		t.Fatalf("narrowed alias metadata = %v, want one child alias", narrowed.readPathAliases)
	}
	got := narrowed.readPathAliases[0]
	if got.path != allowedAlias {
		t.Fatalf("narrowed alias path = %q, want %q", got.path, allowedAlias)
	}
	canonicalTarget, err := filepath.EvalSymlinks(allowedTarget)
	if err != nil {
		t.Fatal(err)
	}
	if got.target != canonicalTarget {
		t.Fatalf("narrowed alias target = %q, want %q", got.target, canonicalTarget)
	}
	foundParentRoute := false
	for _, symlink := range got.symlinks {
		if symlink.path == alias {
			foundParentRoute = true
			break
		}
	}
	if !foundParentRoute {
		t.Fatalf("narrowed alias route omitted prepared parent %q: %+v", alias, got.symlinks)
	}
}

func TestPrepareConfigNarrowedReadAliasRejectsParentRetarget(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	target2 := filepath.Join(root, "target2")
	alias := filepath.Join(root, "alias")
	for _, path := range []string{target, target2} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "allowed.txt"), []byte("allowed"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareConfig(Config{ReadPaths: []string{alias}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target2, alias); err != nil {
		t.Fatal(err)
	}

	narrowed := prepared.Merge(Config{})
	narrowed.ReadPaths = []string{filepath.Join(alias, "allowed.txt")}
	narrowed.DenyWrite = true
	if _, err := PrepareConfig(narrowed); err == nil {
		t.Fatal("PrepareConfig accepted a narrowed child through a retargeted prepared parent alias")
	}
}

func TestPrepareConfigNarrowedReadAliasRejectsCaseEquivalentParentRetarget(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	target2 := filepath.Join(root, "target2")
	alias := filepath.Join(root, "read-alias")
	caseEquivalentAlias := filepath.Join(root, "READ-ALIAS")
	for _, path := range []string{target, target2} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "allowed.txt"), []byte("allowed"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Lstat(alias)
	if err != nil {
		t.Fatal(err)
	}
	caseEquivalentInfo, err := os.Lstat(caseEquivalentAlias)
	if err != nil || !os.SameFile(aliasInfo, caseEquivalentInfo) {
		t.Skip("test filesystem is case-sensitive")
	}
	prepared, err := PrepareConfig(Config{ReadPaths: []string{alias}})
	if err != nil {
		t.Fatal(err)
	}

	unchanged := prepared.Merge(Config{})
	unchanged.ReadPaths = []string{filepath.Join(caseEquivalentAlias, "allowed.txt")}
	unchanged.DenyWrite = true
	unchanged, err = PrepareConfig(unchanged)
	if err != nil {
		t.Fatalf("PrepareConfig rejected an unchanged case-equivalent child route: %v", err)
	}
	if len(unchanged.readPathAliases) != 1 || unchanged.readPathAliases[0].path != filepath.Join(caseEquivalentAlias, "allowed.txt") {
		t.Fatalf("case-equivalent child alias metadata = %+v", unchanged.readPathAliases)
	}

	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target2, alias); err != nil {
		t.Fatal(err)
	}
	retargeted := prepared.Merge(Config{})
	retargeted.ReadPaths = []string{filepath.Join(caseEquivalentAlias, "allowed.txt")}
	retargeted.DenyWrite = true
	if _, err := PrepareConfig(retargeted); err == nil {
		t.Fatal("PrepareConfig accepted a case-equivalent child through a retargeted prepared parent alias")
	}
}

func TestPrepareConfigNarrowedReadAliasDoesNotConflateDistinctAlias(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	firstAlias := filepath.Join(root, "first-alias")
	secondAlias := filepath.Join(root, "second-alias")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "allowed.txt"), []byte("allowed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, firstAlias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, secondAlias); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareConfig(Config{ReadPaths: []string{firstAlias}})
	if err != nil {
		t.Fatal(err)
	}

	narrowed := prepared.Merge(Config{})
	allowedSecondAlias := filepath.Join(secondAlias, "allowed.txt")
	narrowed.ReadPaths = []string{allowedSecondAlias}
	narrowed.DenyWrite = true
	narrowed, err = PrepareConfig(narrowed)
	if err != nil {
		t.Fatalf("PrepareConfig conflated distinct aliases to the same target: %v", err)
	}
	if len(narrowed.readPathAliases) != 1 || narrowed.readPathAliases[0].path != allowedSecondAlias {
		t.Fatalf("distinct child alias metadata = %+v, want only %q", narrowed.readPathAliases, allowedSecondAlias)
	}
}

func TestPrepareConfigNarrowedReadAliasRejectsParentTargetReplacement(t *testing.T) {
	skipIfWindows(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	alias := filepath.Join(root, "alias")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "allowed.txt"), []byte("allowed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareConfig(Config{ReadPaths: []string{alias}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(target, target+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "allowed.txt"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	narrowed := prepared.Merge(Config{})
	narrowed.ReadPaths = []string{filepath.Join(alias, "allowed.txt")}
	narrowed.DenyWrite = true
	if _, err := PrepareConfig(narrowed); err == nil {
		t.Fatal("PrepareConfig accepted a narrowed child after its prepared target directory was replaced")
	}
}

func TestFilterEnvStripsSensitiveByDefault(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"HOME=/home/user",
		"EDITOR=vi",
		"POLLYTOOL_ANTHROPICKEY=sk-1",
		"SSH_AUTH_SOCK=/run/agent.sock",
		"GPG_AGENT_INFO=/run/gpg",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus",
		"DOCKER_HOST=unix:///var/run/docker.sock",
		"XDG_RUNTIME_DIR=/run/user/1000",
		"AWS_SECRET_ACCESS_KEY=aws-secret",
		"AWS_REGION=us-east-1",
		"GITHUB_TOKEN=ghp",
		"OPENAI_API_KEY=sk-2",
		"DB_PASSWORD=hunter2",
		"GOOGLE_APPLICATION_CREDENTIALS=/creds.json",
		"PASSWORD=bare-password",
		"TOKEN=bare-token",
		"API_KEY=bare-api-key",
		"APIKEY=bare-api-key",
		"SECRET=bare-secret",
		"SECRET_KEY=bare-secret-key",
		"ACCESS_KEY=bare-access-key",
		"PASSPHRASE=bare-passphrase",
		"CREDENTIALS=/creds.json",
		"PRIVATE_KEY=private-key",
		"PGPASSWORD=postgres-password",
		"PGPASSFILE=/home/user/.pgpass",
		"MYSQL_PWD=mysql-password",
		"REDISCLI_AUTH=redis-password",
		"DATABASE_URL=postgres://user:password@db.example/app",
	}
	got, stripped := filterEnv(env, nil, nil)

	want := map[string]bool{"PATH=/usr/bin": true, "HOME=/home/user": true, "EDITOR=vi": true}
	if len(got) != len(want) {
		t.Fatalf("filterEnv = %v, want only %v", got, want)
	}
	for _, e := range got {
		if !want[e] {
			t.Fatalf("sensitive var %q survived default filtering: %v", e, got)
		}
	}

	// Stripped reports names only — a value leaking into it would end up in
	// debug logs.
	if len(stripped) != len(env)-len(want) {
		t.Fatalf("stripped = %v, want %d names", stripped, len(env)-len(want))
	}
	for _, name := range stripped {
		if strings.ContainsAny(name, "=/") {
			t.Fatalf("stripped entry %q looks like more than a var name", name)
		}
	}
}

func TestFilterEnvAllowEnvOverridesSensitivity(t *testing.T) {
	// An explicit allowlist wins, even for vars the heuristics call sensitive.
	env := []string{"GITHUB_TOKEN=ghp", "PGPASSWORD=postgres-password", "PATH=/usr/bin", "HOME=/home/user"}
	got, stripped := filterEnv(env, []string{"PGPASSWORD"}, nil)
	if len(got) != 1 || got[0] != "PGPASSWORD=postgres-password" {
		t.Fatalf("filterEnv with allowEnv = %v, want only the explicitly allowed PGPASSWORD", got)
	}
	if len(stripped) != 3 {
		t.Fatalf("stripped = %v, want the three non-allowlisted names", stripped)
	}
}

func TestFilterEnvPassEnvExemptsSensitiveNames(t *testing.T) {
	env := []string{"SSH_AUTH_SOCK=/tmp/agent.sock", "GITHUB_TOKEN=ghp", "PATH=/usr/bin"}
	got, _ := filterEnv(env, nil, []string{"SSH_AUTH_SOCK"})
	if !slices.Contains(got, "SSH_AUTH_SOCK=/tmp/agent.sock") {
		t.Fatalf("filterEnv = %v, want passEnv to exempt SSH_AUTH_SOCK from stripping", got)
	}
	if slices.Contains(got, "GITHUB_TOKEN=ghp") {
		t.Fatalf("filterEnv = %v, other sensitive names must still be stripped", got)
	}
	if !slices.Contains(got, "PATH=/usr/bin") {
		t.Fatalf("filterEnv = %v, passEnv must remain additive over the default filtering", got)
	}
}

func TestFilterEnvPassEnvIgnoredUnderAllowEnv(t *testing.T) {
	env := []string{"SSH_AUTH_SOCK=/tmp/agent.sock", "PATH=/usr/bin"}
	got, _ := filterEnv(env, []string{"PATH"}, []string{"SSH_AUTH_SOCK"})
	if len(got) != 1 || got[0] != "PATH=/usr/bin" {
		t.Fatalf("filterEnv = %v, want the strict allowlist to ignore passEnv", got)
	}
}

func TestFilterEnvPassEnvDoesNotInjectMissingNames(t *testing.T) {
	got, _ := filterEnv([]string{"PATH=/usr/bin"}, nil, []string{"SSH_AUTH_SOCK"})
	if len(got) != 1 || got[0] != "PATH=/usr/bin" {
		t.Fatalf("filterEnv = %v, passEnv must only exempt, never inject", got)
	}
}

// filterEnv must never return a nil slice: callers assign it to cmd.Env, and
// os/exec treats a nil Env as "inherit the full parent environment" — which
// would defeat the filtering entirely. The dangerous case is an allowlist whose
// names are all absent from the environment.
func TestFilterEnvNeverReturnsNil(t *testing.T) {
	cases := []struct {
		name     string
		env      []string
		allowEnv []string
	}{
		{"allowlist matches nothing", []string{"PATH=/usr/bin", "MY_API_KEY=sk"}, []string{"GITHUB_TOKEN"}},
		{"empty env with allowlist", nil, []string{"GITHUB_TOKEN"}},
		{"all vars sensitive, no allowlist", []string{"AWS_SECRET=x", "FOO_TOKEN=y"}, nil},
		{"empty env, no allowlist", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			filtered, _ := filterEnv(tc.env, tc.allowEnv, nil)
			if filtered == nil {
				t.Fatalf("filterEnv returned a nil slice; cmd.Env=nil makes exec inherit the full parent environment")
			}
			if len(filtered) != 0 {
				t.Fatalf("expected an empty (but non-nil) env, got %v", filtered)
			}
		})
	}
}

// exerciseWorkspaceGitLeafSandbox is the shared body of the Darwin and Linux
// end-to-end tests: under workspace+git a real sandboxed `git commit`
// succeeds, while the host-code-exec surface (config, hooks, routing) stays
// unwritable and the host-visible bytes stay unchanged. The platform test has
// already skipped when its backend is unavailable.
func exerciseWorkspaceGitLeafSandbox(t *testing.T) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	isolateGitConfig(t)
	work := t.TempDir()
	hostGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command(gitPath, append([]string{"-C", work}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("host git %v: %v (%s)", args, err, out)
		}
	}
	hostGit("init", "--quiet")
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hostGit("add", "file.txt")
	t.Chdir(work)

	cfg, err := ParsePreset("workspace+git")
	if err != nil {
		t.Fatalf("ParsePreset(workspace+git) error = %v", err)
	}
	sb, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	configPath := filepath.Join(work, ".git", "config")
	originalConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	commit := exec.Command(gitPath, "-C", work,
		"-c", "user.name=polly", "-c", "user.email=polly@example.invalid",
		"-c", "commit.gpgsign=false",
		"commit", "--quiet", "-m", "sandboxed commit")
	if err := wrapCmdForTest(t, sb, commit); err != nil {
		t.Fatalf("Wrap() commit error = %v", err)
	}
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("sandboxed git commit failed: %v (%s)", err, out)
	}
	verify := exec.Command(gitPath, "-C", work, "rev-parse", "--verify", "HEAD")
	if out, err := verify.CombinedOutput(); err != nil {
		t.Fatalf("sandboxed commit not visible on the host: %v (%s)", err, out)
	}

	blocked := []struct {
		name string
		cmd  *exec.Cmd
	}{
		{
			name: "write config through git",
			cmd:  exec.Command(gitPath, "-C", work, "config", "core.fsmonitor", "/tmp/evil"),
		},
		{
			name: "append to config",
			cmd:  exec.Command("bash", "-c", `echo '[core]' >> "$1"`, "bash", configPath),
		},
		{
			name: "plant hook",
			cmd:  exec.Command("bash", "-c", `: > "$1"`, "bash", filepath.Join(work, ".git", "hooks", "pre-commit")),
		},
		{
			name: "relocate routing directory",
			cmd:  exec.Command("mv", filepath.Join(work, ".git"), filepath.Join(work, ".git-old")),
		},
	}
	for _, tt := range blocked {
		t.Run(tt.name, func(t *testing.T) {
			if err := wrapCmdForTest(t, sb, tt.cmd); err != nil {
				t.Fatalf("Wrap() error = %v", err)
			}
			if out, err := tt.cmd.CombinedOutput(); err == nil {
				t.Fatalf("guarded git mutation succeeded, output: %s", out)
			}
		})
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(originalConfig) {
		t.Fatalf(".git/config bytes changed under the sandbox:\n%s", after)
	}
	if _, err := os.Stat(filepath.Join(work, ".git", "hooks", "pre-commit")); !os.IsNotExist(err) {
		t.Fatalf("hook was planted despite the pin: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, ".git")); err != nil {
		t.Fatalf("routing directory moved or removed: %v", err)
	}
}

// exerciseWorkspaceGitWorktreeCommitSandbox verifies a linked worktree stays
// commit-capable in leaf mode while its per-worktree pointers stay pinned.
func exerciseWorkspaceGitWorktreeCommitSandbox(t *testing.T) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	isolateGitConfig(t)
	root := t.TempDir()
	mainDir := filepath.Join(root, "main")
	if err := os.MkdirAll(mainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hostGit := func(dir string, args ...string) {
		t.Helper()
		full := append([]string{"-C", dir,
			"-c", "user.name=polly", "-c", "user.email=polly@example.invalid",
			"-c", "commit.gpgsign=false"}, args...)
		cmd := exec.Command(gitPath, full...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("host git %v: %v (%s)", args, err, out)
		}
	}
	hostGit(mainDir, "init", "--quiet")
	if err := os.WriteFile(filepath.Join(mainDir, "file.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hostGit(mainDir, "add", "file.txt")
	hostGit(mainDir, "commit", "--quiet", "-m", "initial")
	worktree := filepath.Join(root, "wt")
	hostGit(mainDir, "worktree", "add", "--quiet", worktree)
	t.Chdir(root)

	cfg, err := ParsePreset("workspace+git")
	if err != nil {
		t.Fatalf("ParsePreset(workspace+git) error = %v", err)
	}
	sb, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := os.WriteFile(filepath.Join(worktree, "new.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	add := exec.Command(gitPath, "-C", worktree, "add", "new.txt")
	if err := wrapCmdForTest(t, sb, add); err != nil {
		t.Fatalf("Wrap() add error = %v", err)
	}
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("sandboxed git add in worktree failed: %v (%s)", err, out)
	}
	commit := exec.Command(gitPath, "-C", worktree,
		"-c", "user.name=polly", "-c", "user.email=polly@example.invalid",
		"-c", "commit.gpgsign=false",
		"commit", "--quiet", "-m", "worktree commit")
	if err := wrapCmdForTest(t, sb, commit); err != nil {
		t.Fatalf("Wrap() commit error = %v", err)
	}
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("sandboxed git commit in worktree failed: %v (%s)", err, out)
	}

	pointer := filepath.Join(mainDir, ".git", "worktrees", "wt", "gitdir")
	original, err := os.ReadFile(pointer)
	if err != nil {
		t.Fatal(err)
	}
	retarget := exec.Command("bash", "-c", `echo /elsewhere > "$1"`, "bash", pointer)
	if err := wrapCmdForTest(t, sb, retarget); err != nil {
		t.Fatalf("Wrap() retarget error = %v", err)
	}
	if out, err := retarget.CombinedOutput(); err == nil {
		t.Fatalf("worktree gitdir pointer retarget succeeded, output: %s", out)
	}
	if after, err := os.ReadFile(pointer); err != nil || string(after) != string(original) {
		t.Fatalf("worktree gitdir pointer changed: %v", err)
	}
}

// exerciseSSHAgentGrantSandbox proves the agent path end to end: a real
// ssh-agent on the host, PassEnv delivering SSH_AUTH_SOCK, and the socket
// grant making it connectable — ssh-add exits 0 or 1 (agent reached), never
// 2 (agent unreachable).
func exerciseSSHAgentGrantSandbox(t *testing.T) {
	t.Helper()
	agentPath, err := exec.LookPath("ssh-agent")
	if err != nil {
		t.Skipf("ssh-agent unavailable: %v", err)
	}
	sshAddPath, err := exec.LookPath("ssh-add")
	if err != nil {
		t.Skipf("ssh-add unavailable: %v", err)
	}
	dir, err := os.MkdirTemp("", "pagent")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "agent.sock")
	agent := exec.Command(agentPath, "-D", "-a", sock)
	if err := agent.Start(); err != nil {
		t.Skipf("start ssh-agent: %v", err)
	}
	t.Cleanup(func() {
		_ = agent.Process.Kill()
		_, _ = agent.Process.Wait()
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		if info, err := os.Lstat(sock); err == nil && info.Mode()&os.ModeSocket != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Skip("ssh-agent socket never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}

	sb, err := New(Config{
		PassEnv:          []string{"SSH_AUTH_SOCK"},
		AllowUnixSockets: []string{sock},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sshAddPath, "-l")
	cmd.Env = append(os.Environ(), "SSH_AUTH_SOCK="+sock)
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return // agent reached, no identities loaded — the grant works
		}
		t.Fatalf("sandboxed ssh-add could not reach the granted agent: %v (%s)", err, out)
	}
}

// listenUnixSocket binds a short-lived Unix socket in its own short-named
// temp directory: sockaddr_un paths are limited to ~104 bytes on Darwin and
// t.TempDir paths (which embed the test name) can exceed that.
func listenUnixSocket(t *testing.T) (dir, path string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "psock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path = filepath.Join(dir, "a.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("bind test Unix socket at %q: %v", path, err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return dir, path
}

func TestPrepareConfigFreezesAllowUnixSockets(t *testing.T) {
	dir, sock := listenUnixSocket(t)
	alias := filepath.Join(dir, "alias.sock")
	if err := os.Symlink(sock, alias); err != nil {
		t.Fatal(err)
	}

	prepared, err := PrepareConfig(Config{AllowUnixSockets: []string{
		alias,
		sock,
		filepath.Join(dir, "missing.sock"),
	}})
	if err != nil {
		t.Fatalf("PrepareConfig() error = %v", err)
	}
	realSock, err := filepath.EvalSymlinks(sock)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.AllowUnixSockets) != 1 || prepared.AllowUnixSockets[0] != realSock {
		t.Fatalf("AllowUnixSockets = %v, want the deduped canonical socket %q with the missing entry dropped", prepared.AllowUnixSockets, realSock)
	}
}

func TestEffectiveUnixSocketGrants(t *testing.T) {
	dir, sock := listenUnixSocket(t)
	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	grants := effectiveUnixSocketGrants(Config{AllowUnixSockets: []string{
		sock,
		file,
		filepath.Join(dir, "missing.sock"),
	}}, nil)
	if len(grants) != 1 || grants[0].path != sock {
		t.Fatalf("grants = %+v, want only the live socket %q", grants, sock)
	}

	denied := []DeniedPath{{Path: dir, Kind: DeniedPathDir}}
	if grants := effectiveUnixSocketGrants(Config{AllowUnixSockets: []string{sock}}, denied); len(grants) != 0 {
		t.Fatalf("grants = %+v, want a socket under a denied path dropped", grants)
	}
	exempt := Config{AllowUnixSockets: []string{sock}, ReadPaths: []string{dir}}
	if grants := effectiveUnixSocketGrants(exempt, denied); len(grants) != 1 {
		t.Fatalf("grants = %+v, want the denied-path drop lifted by a covering ReadPaths exemption", grants)
	}
}

func TestParseConfigPassEnvAndAllowUnixSockets(t *testing.T) {
	cfg, err := ParseConfig(json.RawMessage(`{"passEnv":["SSH_AUTH_SOCK"],"allowUnixSockets":["/tmp/agent.sock"]}`))
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	if cfg == nil || len(cfg.PassEnv) != 1 || cfg.PassEnv[0] != "SSH_AUTH_SOCK" {
		t.Fatalf("PassEnv = %+v, want [SSH_AUTH_SOCK]", cfg)
	}
	if len(cfg.AllowUnixSockets) != 1 || cfg.AllowUnixSockets[0] != "/tmp/agent.sock" {
		t.Fatalf("AllowUnixSockets = %+v, want [/tmp/agent.sock]", cfg)
	}
}

func TestCommandSummary(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{nil, ""},
		{[]string{"bash"}, "bash"},
		{[]string{"bash", "-c"}, "bash -c"},
		{[]string{"bash", "-c", "echo secret-payload"}, "bash -c"},
	}
	for _, tt := range tests {
		if got := commandSummary(tt.args); got != tt.want {
			t.Fatalf("commandSummary(%v) = %q, want %q", tt.args, got, tt.want)
		}
	}
}

func TestAllDeniedPathsIncludesUserDenyPaths(t *testing.T) {
	dir := t.TempDir()
	extraDir := filepath.Join(dir, "secrets")
	if err := os.MkdirAll(extraDir, 0700); err != nil {
		t.Fatal(err)
	}
	extraFile := filepath.Join(dir, "token.txt")
	if err := os.WriteFile(extraFile, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "nope")

	got := allDeniedPaths(Config{DenyPaths: []string{extraDir, extraFile, missing}})

	kinds := make(map[string]DeniedPathKind, len(got))
	for _, p := range got {
		kinds[p.Path] = p.Kind
	}
	if kinds[extraDir] != DeniedPathDir {
		t.Fatalf("existing directory %q kind = %q, want %q", extraDir, kinds[extraDir], DeniedPathDir)
	}
	if kinds[extraFile] != DeniedPathFile {
		t.Fatalf("existing file %q kind = %q, want %q", extraFile, kinds[extraFile], DeniedPathFile)
	}
	if kinds[missing] != DeniedPathFile {
		t.Fatalf("missing path %q kind = %q, want %q (platforms drop or harmlessly deny it)", missing, kinds[missing], DeniedPathFile)
	}
	if len(got) != len(ExpandHome(DeniedPaths))+3 {
		t.Fatalf("allDeniedPaths returned %d entries, want built-ins plus 3", len(got))
	}
}

func TestParseConfigDenyPaths(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"denyPaths":["~/secrets","/var/private"]}`))
	if err != nil {
		t.Fatalf("ParseConfig error = %v", err)
	}
	if len(cfg.DenyPaths) != 2 {
		t.Fatalf("DenyPaths = %v, want 2 entries", cfg.DenyPaths)
	}
}

func TestMergeDenyPaths(t *testing.T) {
	merged := Config{DenyPaths: []string{"/a"}}.Merge(Config{DenyPaths: []string{"/b"}})
	if len(merged.DenyPaths) != 2 || merged.DenyPaths[0] != "/a" || merged.DenyPaths[1] != "/b" {
		t.Fatalf("DenyPaths = %v, want [/a /b]", merged.DenyPaths)
	}
}

// Merging two overlays onto the same base must not alias: the registry reuses
// one baseSandboxCfg for every tool, so if Merge appended into the base's spare
// capacity, one tool's deny path would overwrite another's. The base slice here
// has cap > len to expose the aliasing if it regresses.
func TestMergeDoesNotAliasBase(t *testing.T) {
	base := Config{DenyPaths: make([]string, 1, 8)}
	base.DenyPaths[0] = "/base"

	mergedA := base.Merge(Config{DenyPaths: []string{"/toolA"}})
	mergedB := base.Merge(Config{DenyPaths: []string{"/toolB"}})

	if got := mergedA.DenyPaths; len(got) != 2 || got[1] != "/toolA" {
		t.Fatalf("mergedA.DenyPaths = %v, want [/base /toolA] — tool B's merge contaminated tool A", got)
	}
	if got := mergedB.DenyPaths; len(got) != 2 || got[1] != "/toolB" {
		t.Fatalf("mergedB.DenyPaths = %v, want [/base /toolB]", got)
	}
	// The base itself must be untouched.
	if len(base.DenyPaths) != 1 || base.DenyPaths[0] != "/base" {
		t.Fatalf("base mutated by Merge: %v", base.DenyPaths)
	}
}

func TestMergeCarriesAndCopiesGitPoliciesFromBothOperands(t *testing.T) {
	base := Config{gitPolicies: []gitWorkspacePolicy{{
		workspace: "/base",
		repositories: []gitRepositoryContext{{
			workTree: "/base",
			gitDir:   "/base/.git",
		}},
		protected: []string{"/base/.git"},
	}}}
	overlay := Config{gitPolicies: []gitWorkspacePolicy{{
		workspace: "/overlay",
		repositories: []gitRepositoryContext{{
			workTree: "/overlay",
			gitDir:   "/overlay/.git",
		}},
		protected: []string{"/overlay/.git"},
	}}}

	merged := base.Merge(overlay)
	if len(merged.gitPolicies) != 2 {
		t.Fatalf("merged Git policies = %d, want both operands", len(merged.gitPolicies))
	}
	if merged.gitPolicies[0].workspace != "/base" || merged.gitPolicies[1].workspace != "/overlay" {
		t.Fatalf("merged Git workspaces = %q, %q", merged.gitPolicies[0].workspace, merged.gitPolicies[1].workspace)
	}

	base.gitPolicies[0].repositories[0].gitDir = "/mutated-base"
	overlay.gitPolicies[0].protected[0] = "/mutated-overlay"
	if got := merged.gitPolicies[0].repositories[0].gitDir; got != "/base/.git" {
		t.Fatalf("base repository slice aliases merged policy: %q", got)
	}
	if got := merged.gitPolicies[1].protected[0]; got != "/overlay/.git" {
		t.Fatalf("overlay protected slice aliases merged policy: %q", got)
	}

	merged.gitPolicies[0].protected[0] = "/mutated-merged"
	if got := base.gitPolicies[0].protected[0]; got != "/base/.git" {
		t.Fatalf("merged policy aliases base protected slice: %q", got)
	}
}

func TestNormalizeConfigPathsRetainsAndCopiesGitPolicies(t *testing.T) {
	original := Config{gitPolicies: []gitWorkspacePolicy{{
		workspace: "/workspace",
		repositories: []gitRepositoryContext{{
			workTree: "/workspace",
			gitDir:   "/workspace/.git",
		}},
		protected: []string{"/workspace/.git"},
	}}}

	normalized, err := normalizeConfigPaths(original)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized.gitPolicies) != 1 || normalized.gitPolicies[0].workspace != "/workspace" {
		t.Fatalf("normalized Git policies = %+v", normalized.gitPolicies)
	}
	normalized.gitPolicies[0].repositories[0].gitDir = "/mutated"
	normalized.gitPolicies[0].protected[0] = "/mutated"
	if got := original.gitPolicies[0].repositories[0].gitDir; got != "/workspace/.git" {
		t.Fatalf("normalization aliases repository policy: %q", got)
	}
	if got := original.gitPolicies[0].protected[0]; got != "/workspace/.git" {
		t.Fatalf("normalization aliases protected policy: %q", got)
	}
}

func TestExpandHome(t *testing.T) {
	paths := ExpandHome([]DeniedPath{
		{Path: "~/.ssh", Kind: DeniedPathDir},
		{Path: "~/.aws", Kind: DeniedPathDir},
	})
	if len(paths) != 2 {
		t.Fatalf("ExpandHome returned %d paths, want 2", len(paths))
	}
	for _, p := range paths {
		if strings.HasPrefix(p.Path, "~/") {
			t.Fatalf("ExpandHome did not expand %q", p.Path)
		}
		if !strings.Contains(p.Path, ".ssh") && !strings.Contains(p.Path, ".aws") {
			t.Fatalf("unexpected expanded path: %q", p.Path)
		}
		if p.Kind != DeniedPathDir {
			t.Fatalf("expected kind %q, got %q", DeniedPathDir, p.Kind)
		}
	}
}
