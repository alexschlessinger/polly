package sandbox

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

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
