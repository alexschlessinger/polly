//go:build darwin

package sandbox

import (
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRejectBroadWorkspaceRejectsDarwinMountRoot(t *testing.T) {
	var mountPoint string
	for _, candidate := range []string{t.TempDir(), "/dev"} {
		var stat unix.Statfs_t
		if err := unix.Statfs(candidate, &stat); err != nil {
			continue
		}
		root := filepath.Clean(unix.ByteSliceToString(stat.Mntonname[:]))
		if filepath.IsAbs(root) && filepath.Dir(root) != root {
			mountPoint = root
			break
		}
	}
	if mountPoint == "" {
		t.Skip("no non-filesystem-root Darwin mount is available")
	}

	err := rejectBroadWorkspace(mountPoint)
	if err == nil {
		t.Fatalf("rejectBroadWorkspace(%q) error = nil, want mounted-volume-root rejection", mountPoint)
	}
	for _, want := range []string{"mounted volume root", "bounded project directory", "--sandbox base"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("rejectBroadWorkspace(%q) error = %q, want %q", mountPoint, err, want)
		}
	}
}

func TestRejectBroadWorkspaceAllowsDarwinMountDescendant(t *testing.T) {
	dir := t.TempDir()
	if err := rejectBroadWorkspace(dir); err != nil {
		t.Fatalf("rejectBroadWorkspace(%q) error = %v, want mount descendant accepted", dir, err)
	}
}
