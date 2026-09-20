package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/envstorage"
)

func TestManagedStoragePrivateOutsideHome(t *testing.T) {
	skipIfWindows(t)
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" {
		t.Skip("opt-in native sandbox")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", base)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "cache"))
	root, err := envstorage.DataRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := envstorage.EnsurePrivateRoots(); err != nil {
		t.Fatal(err)
	}
	mine, other := filepath.Join(root, "mine"), filepath.Join(root, "other")
	for _, path := range []string{mine, other} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(other, "secret"), []byte("sibling"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{WritablePaths: []string{mine}}
	if err := ReadAllowed(cfg, filepath.Join(other, "secret")); err == nil {
		t.Fatal("file tools exposed sibling storage")
	}
	sb, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", `printf mine > "$1/file" && cat "$1/file" && ! cat "$2/secret"`, "sh", mine, other)
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "mine") || strings.Contains(string(out), "sibling") {
		t.Fatalf("storage isolation: %s %v", out, err)
	}
}
