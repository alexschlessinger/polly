//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRejectBroadWorkspaceRejectsLinuxPrivateRoots(t *testing.T) {
	tempRoots, runRoots := privateLinuxRoots()
	privateRoots := append(append([]string(nil), tempRoots...), runRoots...)
	for _, root := range privateRoots {
		root := root
		t.Run(root, func(t *testing.T) {
			if _, err := os.Stat(root); err != nil {
				t.Skipf("private root %q is unavailable: %v", root, err)
			}
			err := rejectBroadWorkspace(root)
			if err == nil {
				t.Fatalf("rejectBroadWorkspace(%q) error = nil, want private-root rejection", root)
			}
			for _, want := range []string{"private Linux sandbox root", "bounded project directory", "--sandbox base"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("rejectBroadWorkspace(%q) error = %q, want %q", root, err, want)
				}
			}
		})
	}
}

func TestRejectPlatformBroadWorkspaceAllowsLinuxPrivateRootDescendants(t *testing.T) {
	tempRoots, runRoots := privateLinuxRoots()
	privateRoots := append(append([]string(nil), tempRoots...), runRoots...)
	for _, root := range privateRoots {
		dir := filepath.Join(root, ".polly-workspace-private-root-test")
		if err := rejectPlatformBroadWorkspace(dir); err != nil {
			t.Fatalf("rejectPlatformBroadWorkspace(%q) error = %v, want private-root descendant accepted", dir, err)
		}
	}
}

func TestRejectBroadWorkspaceRejectsLinuxMountRoot(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skipf("/proc is unavailable: %v", err)
	}
	err := rejectBroadWorkspace("/proc")
	if err == nil {
		t.Fatal("rejectBroadWorkspace(/proc) error = nil, want mounted-volume-root rejection")
	}
	if !strings.Contains(err.Error(), "mounted volume root") {
		t.Fatalf("rejectBroadWorkspace(/proc) error = %q, want mounted-volume-root rejection", err)
	}
}

func TestLinuxMountPointsDecodesEscapedPaths(t *testing.T) {
	data := []byte("36 25 0:32 / /plain rw,nosuid - tmpfs tmpfs rw\n" +
		`37 25 0:33 / /path\040with\011tab\012line\134slash rw shared:1 - ext4 /dev/sda rw` + "\n")
	mounts, err := linuxMountPoints(data)
	if err != nil {
		t.Fatalf("linuxMountPoints() error = %v", err)
	}
	for _, want := range []string{"/plain", "/path with\ttab\nline\\slash"} {
		if !mounts[want] {
			t.Fatalf("linuxMountPoints() = %v, want decoded mount point %q", mounts, want)
		}
	}
}

func TestLinuxMountPointsRejectsMalformedInput(t *testing.T) {
	for _, data := range []string{
		"",
		"36 25 0:32 / /plain rw tmpfs tmpfs rw\n",
		`36 25 0:32 / /bad\999path rw - tmpfs tmpfs rw`,
	} {
		if _, err := linuxMountPoints([]byte(data)); err == nil {
			t.Fatalf("linuxMountPoints(%q) error = nil, want malformed-input rejection", data)
		}
	}
}
