package sandbox

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeExtraReadHome points HOME at a controllable directory so the home and
// credential rejections can be exercised without touching the real one.
func fakeExtraReadHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "fake-home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatalf("mkdir fake home: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func TestValidateExtraReadDirRejectsInvalidPaths(t *testing.T) {
	temp := t.TempDir()
	workspace := filepath.Join(temp, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	interior := filepath.Join(workspace, "inside")
	if err := os.Mkdir(interior, 0o700); err != nil {
		t.Fatalf("mkdir interior: %v", err)
	}
	file := filepath.Join(temp, "notes.txt")
	if err := os.WriteFile(file, []byte("text"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	scratch := filepath.Join(temp, "scratch")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatalf("mkdir scratch: %v", err)
	}
	missingSymlink := filepath.Join(temp, "missing-link")
	if err := os.Symlink(filepath.Join(temp, "nowhere"), missingSymlink); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	fileSymlink := filepath.Join(temp, "file-link")
	if err := os.Symlink(file, fileSymlink); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	tests := []struct {
		name string
		path string
		want string
	}{
		{"empty", "", "is empty"},
		{"nonexistent", filepath.Join(temp, "missing"), "does not exist"},
		{"symlink to nonexistent", missingSymlink, "does not exist"},
		{"file", file, "is not a directory"},
		{"symlink to file", fileSymlink, "is not a directory"},
		{"filesystem root", "/", "is the filesystem root"},
		{"workspace interior", interior, "is inside the workspace"},
		{"workspace itself", workspace, "is inside the workspace"},
		{"temp root", os.TempDir(), "is inside the OS temp directory"},
		{"tmp", "/tmp", "is inside the OS temp directory"},
		{"inside temp", scratch, "is inside the OS temp directory"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ValidateExtraReadDir(workspace, test.path)
			if err == nil {
				t.Fatalf("ValidateExtraReadDir(%q) = %q, want error containing %q", test.path, got, test.want)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateExtraReadDir(%q) error = %q, want it to contain %q", test.path, err, test.want)
			}
		})
	}
}

func TestValidateExtraReadDirRejectsHomeAndCredentialPaths(t *testing.T) {
	home := fakeExtraReadHome(t)
	for _, name := range []string{".ssh", ".gnupg", ".aws/sso"} {
		if err := os.MkdirAll(filepath.Join(home, name), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}
	homeSymlink := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(home, homeSymlink); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	tests := []struct {
		name string
		path string
		want string
	}{
		{"home", home, "is the home directory"},
		{"ancestor of home", filepath.Dir(home), "is the home directory"},
		{"symlink to home", homeSymlink, "is the home directory"},
		{"credential directory", filepath.Join(home, ".ssh"), "contains the masked credential path"},
		{"credential spelling", "~/.gnupg", "contains the masked credential path"},
		{"inside a credential directory", "~/.aws/sso", "is inside the masked credential path"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ValidateExtraReadDir(t.TempDir(), test.path)
			if err == nil {
				t.Fatalf("ValidateExtraReadDir(%q) = %q, want error containing %q", test.path, got, test.want)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateExtraReadDir(%q) error = %q, want it to contain %q", test.path, err, test.want)
			}
		})
	}
}

func TestValidateExtraReadDirAcceptsAncestorAndSibling(t *testing.T) {
	workspace := "/opt/polly-extra-read-dir-workspace"
	for _, path := range []string{"/opt", "/usr/local"} {
		got, err := ValidateExtraReadDir(workspace, path)
		if err != nil {
			t.Fatalf("ValidateExtraReadDir(%q): %v", path, err)
		}
		want, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatalf("EvalSymlinks(%q): %v", path, err)
		}
		if got != want {
			t.Fatalf("ValidateExtraReadDir(%q) = %q, want canonical %q", path, got, want)
		}
	}
}

func TestValidateExtraReadDirResolvesRelativePaths(t *testing.T) {
	workspace := t.TempDir()
	t.Chdir(workspace)
	sub := filepath.Join(workspace, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	for _, path := range []string{"sub", "."} {
		if got, err := ValidateExtraReadDir(workspace, path); err == nil || !strings.Contains(err.Error(), "is inside the workspace") {
			t.Fatalf("ValidateExtraReadDir(%q) = (%q, %v), want workspace-interior rejection", path, got, err)
		}
	}
}

func TestValidateExtraReadDirResolvesSymlinksToRealPaths(t *testing.T) {
	temp := t.TempDir()
	target := filepath.Join(temp, "real")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	link := filepath.Join(temp, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	// The target sits inside the OS temp directory, so the strict
	// validator rejects it — but only after resolving the link, and the
	// rejection carries the canonical real path.
	_, err = ValidateExtraReadDir("/opt/polly-extra-read-dir-workspace", link)
	if err == nil {
		t.Fatalf("ValidateExtraReadDir(%q) accepted a temp-family path", link)
	}
	if !strings.Contains(err.Error(), realTarget) {
		t.Fatalf("ValidateExtraReadDir(%q) error = %q, want it to carry the canonical path %q", link, err, realTarget)
	}
}

func TestCanonicalizeExtraReadDirAcceptsMissingPaths(t *testing.T) {
	temp := t.TempDir()
	deleted := filepath.Join(temp, "deleted")
	if err := os.Mkdir(deleted, 0o700); err != nil {
		t.Fatalf("mkdir deleted: %v", err)
	}
	if err := os.Remove(deleted); err != nil {
		t.Fatalf("remove deleted: %v", err)
	}
	resolvedTemp, err := filepath.EvalSymlinks(temp)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	for path, want := range map[string]string{
		deleted:                       filepath.Join(resolvedTemp, "deleted"),
		filepath.Join(temp, "a", "b"): filepath.Join(resolvedTemp, "a", "b"),
	} {
		got, err := CanonicalizeExtraReadDir(path)
		if err != nil {
			t.Fatalf("CanonicalizeExtraReadDir(%q): %v", path, err)
		}
		if got != want {
			t.Fatalf("CanonicalizeExtraReadDir(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestCanonicalizeExtraReadDirStillRejectsUnsafePaths(t *testing.T) {
	home := fakeExtraReadHome(t)
	tests := []struct {
		name string
		path string
		want string
	}{
		{"filesystem root", "/", "is the filesystem root"},
		{"home", home, "is the home directory"},
		{"ancestor of home", filepath.Dir(home), "is the home directory"},
		{"credential spelling", "~/.ssh", "contains the masked credential path"},
		{"inside a credential directory", "~/.aws/sso/cache", "is inside the masked credential path"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := CanonicalizeExtraReadDir(test.path)
			if err == nil {
				t.Fatalf("CanonicalizeExtraReadDir(%q) = %q, want error containing %q", test.path, got, test.want)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("CanonicalizeExtraReadDir(%q) error = %q, want it to contain %q", test.path, err, test.want)
			}
		})
	}
}

func TestCanonicalizeExtraReadDirResolvesSymlinksToRealPaths(t *testing.T) {
	temp := t.TempDir()
	target := filepath.Join(temp, "real")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	link := filepath.Join(temp, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got, err := CanonicalizeExtraReadDir(link)
	if err != nil {
		t.Fatalf("CanonicalizeExtraReadDir(%q): %v", link, err)
	}
	if want, err := filepath.EvalSymlinks(target); err != nil || got != want {
		t.Fatalf("CanonicalizeExtraReadDir(%q) = %q, want canonical %q", link, got, want)
	}
}

func TestMergeExtraReadDirs(t *testing.T) {
	if got := MergeExtraReadDirs(nil, nil); got != nil {
		t.Fatalf("MergeExtraReadDirs(nil, nil) = %#v, want nil", got)
	}
	if got := MergeExtraReadDirs([]string{"/repos/alpha"}, nil); !reflect.DeepEqual(got, []string{"/repos/alpha"}) {
		t.Fatalf("MergeExtraReadDirs with no additions = %#v", got)
	}
	if got := MergeExtraReadDirs(nil, []string{"/repos/alpha", "/repos/beta"}); !reflect.DeepEqual(got, []string{"/repos/alpha", "/repos/beta"}) {
		t.Fatalf("merge into empty = %#v", got)
	}
	if got := MergeExtraReadDirs([]string{"/repos/alpha"}, []string{"/repos/alpha"}); !reflect.DeepEqual(got, []string{"/repos/alpha"}) {
		t.Fatalf("exact duplicate kept: %#v", got)
	}
	if got := MergeExtraReadDirs([]string{"/repos/alpha"}, []string{"/repos/alpha/nested"}); !reflect.DeepEqual(got, []string{"/repos/alpha"}) {
		t.Fatalf("subsumed descendant kept: %#v", got)
	}
	if got := MergeExtraReadDirs([]string{"/repos/alpha/nested"}, []string{"/repos/alpha"}); !reflect.DeepEqual(got, []string{"/repos/alpha/nested", "/repos/alpha"}) {
		t.Fatalf("new ancestor dropped existing descendants: %#v", got)
	}
	if got := MergeExtraReadDirs([]string{"/repos/beta", "/repos/alpha"}, []string{"/repos/alpha/x", "/repos/gamma"}); !reflect.DeepEqual(got, []string{"/repos/beta", "/repos/alpha", "/repos/gamma"}) {
		t.Fatalf("order-stable merge = %#v", got)
	}
	merged := MergeExtraReadDirs([]string{"/repos/alpha", "/repos/beta"}, []string{"/repos/beta/gamma"})
	if again := MergeExtraReadDirs(merged, []string{"/repos/beta/gamma", "/repos/beta/gamma/delta"}); !reflect.DeepEqual(again, merged) {
		t.Fatalf("repeated adds do not converge: %#v then %#v", merged, again)
	}
	if got := MergeExtraReadDirs([]string{"/repos/alpha", ""}, []string{"/repos/beta"}); !reflect.DeepEqual(got, []string{"/repos/alpha", "/repos/beta"}) {
		t.Fatalf("empty entry kept: %#v", got)
	}
}
