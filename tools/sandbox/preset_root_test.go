package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestParsePresetAtRootDoesNotUseWorkingDirectory(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	elsewhere, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(elsewhere)
	cfg, err := ParsePresetAtRoot("workspace", root)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg.WritablePaths, root) || slices.Contains(cfg.WritablePaths, elsewhere) {
		t.Fatalf("workspace not bound to explicit root: %v", cfg.WritablePaths)
	}
	cwd, err := os.Getwd()
	if err != nil || cwd != elsewhere {
		t.Fatalf("parser changed process cwd: %s %v", cwd, err)
	}
	if _, err := ParsePresetAtRoot("workspace", "relative"); err == nil {
		t.Fatal("relative root accepted")
	}
}
