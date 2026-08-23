package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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

func TestFreezeAuthorityPathsCanonicalizesDropsAndMinimizes(t *testing.T) {
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
	}
	overlay := Config{
		AllowNetwork:   true,
		DenyDNS:        true,
		WritablePaths:  []string{"/extra"},
		ReadPaths:      []string{"~/.aws"},
		AllowEnv:       []string{"PATH"},
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
	ownedFiles []*os.File
	prepend    bool
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

func (s managedExtraFileSandbox) wrapManaged(cmd *exec.Cmd, _ map[string]string) ([]*os.File, error) {
	if s.prepend {
		extraFiles := append([]*os.File(nil), s.extraFiles...)
		cmd.ExtraFiles = append(extraFiles, cmd.ExtraFiles...)
	} else {
		cmd.ExtraFiles = append(cmd.ExtraFiles, s.extraFiles...)
	}
	return append([]*os.File(nil), s.ownedFiles...), s.err
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

func TestWrapCmdManagedCleanupUsesExplicitOwnership(t *testing.T) {
	callerFile, err := os.CreateTemp(t.TempDir(), "caller-extra-*")
	if err != nil {
		t.Fatal(err)
	}
	defer callerFile.Close()
	ownedFile, err := os.CreateTemp(t.TempDir(), "sandbox-extra-*")
	if err != nil {
		t.Fatal(err)
	}
	borrowedFile, err := os.CreateTemp(t.TempDir(), "borrowed-extra-*")
	if err != nil {
		t.Fatal(err)
	}
	defer borrowedFile.Close()
	laterCallerFile, err := os.CreateTemp(t.TempDir(), "later-caller-extra-*")
	if err != nil {
		t.Fatal(err)
	}
	defer laterCallerFile.Close()
	cmd := exec.Command("true")
	cmd.ExtraFiles = []*os.File{callerFile}
	cleanup, err := WrapCmdManaged(managedExtraFileSandbox{
		extraFiles: []*os.File{ownedFile, borrowedFile},
		ownedFiles: []*os.File{ownedFile},
		prepend:    true,
	}, cmd)
	if err != nil {
		t.Fatal(err)
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, laterCallerFile)

	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if len(cmd.ExtraFiles) != 3 || cmd.ExtraFiles[0] != borrowedFile || cmd.ExtraFiles[1] != callerFile || cmd.ExtraFiles[2] != laterCallerFile {
		t.Fatalf("ExtraFiles = %v, want borrowed and caller-owned files", cmd.ExtraFiles)
	}
	if _, err := callerFile.Stat(); err != nil {
		t.Fatalf("caller-owned descriptor was closed: %v", err)
	}
	if _, err := ownedFile.Stat(); err == nil {
		t.Fatal("sandbox-owned descriptor remains open")
	}
	if _, err := borrowedFile.Stat(); err != nil {
		t.Fatalf("borrowed descriptor was closed: %v", err)
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
		ownedFiles: []*os.File{ownedFile},
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
		ownedFiles: []*os.File{ownedFile, ownedFile},
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

func TestWrapCmdError(t *testing.T) {
	sb := &mockSandbox{err: fmt.Errorf("denied")}
	cmd := exec.Command("echo", "hello")
	if err := WrapCmd(sb, cmd); err == nil {
		t.Fatal("expected error from WrapCmd")
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

// Merging two overlays onto the same base must not alias: the registry reuses
// one baseSandboxCfg for every tool, so if Merge appended into the base's spare
// capacity, one tool's deny path would overwrite another's. The base slice here
// has cap > len to expose the aliasing if it regresses.
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

func TestResolvedExecutablePathUsesAbsoluteBaseForRelativeDir(t *testing.T) {
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
