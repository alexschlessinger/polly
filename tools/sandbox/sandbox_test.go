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
		name   string
		input  string
		isNil  bool
		net    bool
		paths  int
	}{
		{"true", `true`, false, false, 0},
		{"false", `false`, true, false, 0},
		{"null", `null`, true, false, 0},
		{"empty", ``, true, false, 0},
		{"object defaults", `{}`, false, false, 0},
		{"allow network", `{"allowNetwork":true}`, false, true, 0},
		{"writable paths", `{"writablePaths":["/a","/b"]}`, false, false, 2},
		{"full", `{"allowNetwork":true,"writablePaths":["/x"]}`, false, true, 1},
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
			if len(spec.WritablePaths) != tt.paths {
				t.Fatalf("WritablePaths = %v, want %d entries", spec.WritablePaths, tt.paths)
			}
		})
	}
}

func TestSpecMergeInto(t *testing.T) {
	base := Config{WritablePaths: []string{"/work"}, AllowNetwork: false}
	spec := &Spec{AllowNetwork: true, WritablePaths: []string{"/extra"}}
	merged := spec.MergeInto(base)

	if !merged.AllowNetwork {
		t.Fatal("MergeInto should set AllowNetwork to true")
	}
	if len(merged.WritablePaths) != 2 {
		t.Fatalf("WritablePaths = %v, want 2 entries", merged.WritablePaths)
	}
	if merged.WritablePaths[0] != "/work" || merged.WritablePaths[1] != "/extra" {
		t.Fatalf("WritablePaths = %v, want [/work /extra]", merged.WritablePaths)
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
