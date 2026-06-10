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
		AllowNetwork:  true,
		DenyDNS:       true,
		WritablePaths: []string{"/extra"},
		ReadPaths:     []string{"~/.aws"},
		AllowEnv:      []string{"PATH"},
		DenyWrite:     true,
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

func TestFilterEnvStripsSensitiveByDefault(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"HOME=/home/user",
		"EDITOR=vi",
		"POLLYTOOL_ANTHROPICKEY=sk-1",
		"SSH_AUTH_SOCK=/run/agent.sock",
		"GPG_AGENT_INFO=/run/gpg",
		"AWS_SECRET_ACCESS_KEY=aws-secret",
		"AWS_REGION=us-east-1",
		"GITHUB_TOKEN=ghp",
		"OPENAI_API_KEY=sk-2",
		"DB_PASSWORD=hunter2",
		"GOOGLE_APPLICATION_CREDENTIALS=/creds.json",
	}
	got, stripped := filterEnv(env, nil)

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
	env := []string{"GITHUB_TOKEN=ghp", "PATH=/usr/bin", "HOME=/home/user"}
	got, stripped := filterEnv(env, []string{"GITHUB_TOKEN"})
	if len(got) != 1 || got[0] != "GITHUB_TOKEN=ghp" {
		t.Fatalf("filterEnv with allowEnv = %v, want only the explicitly allowed GITHUB_TOKEN", got)
	}
	if len(stripped) != 2 {
		t.Fatalf("stripped = %v, want the two non-allowlisted names", stripped)
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
			filtered, _ := filterEnv(tc.env, tc.allowEnv)
			if filtered == nil {
				t.Fatalf("filterEnv returned a nil slice; cmd.Env=nil makes exec inherit the full parent environment")
			}
			if len(filtered) != 0 {
				t.Fatalf("expected an empty (but non-nil) env, got %v", filtered)
			}
		})
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
