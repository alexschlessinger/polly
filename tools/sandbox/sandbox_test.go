package sandbox

import (
	"encoding/json"
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

func TestParseSpec(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		isNil     bool
		net       bool
		denyDNS   bool
		paths     int
		readPaths int
		allowEnv  int
		denyWrite bool
	}{
		{"true", `true`, false, false, false, 0, 0, 0, false},
		{"false", `false`, true, false, false, 0, 0, 0, false},
		{"null", `null`, true, false, false, 0, 0, 0, false},
		{"empty", ``, true, false, false, 0, 0, 0, false},
		{"object defaults", `{}`, false, false, false, 0, 0, 0, false},
		{"allow network", `{"allowNetwork":true}`, false, true, false, 0, 0, 0, false},
		{"writable paths", `{"writablePaths":["/a","/b"]}`, false, false, false, 2, 0, 0, false},
		{"full", `{"allowNetwork":true,"writablePaths":["/x"]}`, false, true, false, 1, 0, 0, false},
		{"readPaths", `{"readPaths":["~/.aws"]}`, false, false, false, 0, 1, 0, false},
		{"allowEnv", `{"allowEnv":["HOME","PATH"]}`, false, false, false, 0, 0, 2, false},
		{"denyWrite", `{"denyWrite":true}`, false, false, false, 0, 0, 0, true},
		{"denyDNS", `{"denyDNS":true}`, false, false, true, 0, 0, 0, false},
		{"denyDNS with network", `{"allowNetwork":true,"denyDNS":true}`, false, true, true, 0, 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := ParseSpec(json.RawMessage(tt.input))
			if tt.isNil {
				if spec != nil {
					t.Fatalf("expected nil, got %+v", spec)
				}
				return
			}
			if spec == nil {
				t.Fatal("expected non-nil spec")
			}
			if spec.AllowNetwork != tt.net {
				t.Fatalf("AllowNetwork = %v, want %v", spec.AllowNetwork, tt.net)
			}
			if spec.DenyDNS != tt.denyDNS {
				t.Fatalf("DenyDNS = %v, want %v", spec.DenyDNS, tt.denyDNS)
			}
			if len(spec.WritablePaths) != tt.paths {
				t.Fatalf("WritablePaths = %v, want %d entries", spec.WritablePaths, tt.paths)
			}
			if len(spec.ReadPaths) != tt.readPaths {
				t.Fatalf("ReadPaths = %v, want %d entries", spec.ReadPaths, tt.readPaths)
			}
			if len(spec.AllowEnv) != tt.allowEnv {
				t.Fatalf("AllowEnv = %v, want %d entries", spec.AllowEnv, tt.allowEnv)
			}
			if spec.DenyWrite != tt.denyWrite {
				t.Fatalf("DenyWrite = %v, want %v", spec.DenyWrite, tt.denyWrite)
			}
		})
	}
}

func TestSpecMergeInto(t *testing.T) {
	base := Config{
		WritablePaths: []string{"/work"},
		AllowNetwork:  false,
		ReadPaths:     []string{"~/.kube"},
		AllowEnv:      []string{"HOME"},
	}
	spec := &Spec{
		AllowNetwork:  true,
		DenyDNS:       true,
		WritablePaths: []string{"/extra"},
		ReadPaths:     []string{"~/.aws"},
		AllowEnv:      []string{"PATH"},
		DenyWrite:     true,
	}
	merged := spec.MergeInto(base)

	if !merged.AllowNetwork {
		t.Fatal("MergeInto should set AllowNetwork to true")
	}
	if !merged.DenyDNS {
		t.Fatal("MergeInto should set DenyDNS to true")
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
		t.Fatal("MergeInto should set DenyWrite to true")
	}
}

func TestSpecMergeIntoDenyDNSOR(t *testing.T) {
	// DenyDNS should OR: if either side sets it, the result is true.
	base := Config{DenyDNS: true}
	spec := &Spec{DenyDNS: false}
	merged := spec.MergeInto(base)
	if !merged.DenyDNS {
		t.Fatal("DenyDNS should be true when base has it set")
	}

	base2 := Config{DenyDNS: false}
	spec2 := &Spec{DenyDNS: true}
	merged2 := spec2.MergeInto(base2)
	if !merged2.DenyDNS {
		t.Fatal("DenyDNS should be true when spec has it set")
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
