package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadOnlyExposurePreservesDeniesAndIdentity(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	selected := filepath.Join(root, "selected")
	if err := os.Mkdir(selected, 0700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(selected, "secret")
	cfg, err := ExposeReadOnlyPaths(Config{DenyWrite: true, DenyPaths: []string{secret}}, selected)
	if err != nil {
		t.Fatal(err)
	}
	cfg = cfg.Merge(Config{})
	if len(cfg.ReadPaths) != 0 || ReadAllowed(cfg, secret) == nil || WriteAllowed(cfg, filepath.Join(selected, "new")) == nil {
		t.Fatal("visibility expanded read or write authority")
	}
	t.Run("directory identity", func(t *testing.T) {
		skipIfWindows(t)
		if err := os.Rename(selected, selected+"-old"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(selected, 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := PrepareConfig(cfg); err == nil {
			t.Fatal("accepted a replacement for the frozen visible directory")
		}
	})
}

func TestReadOnlyExposureSandbox(t *testing.T) {
	skipIfWindows(t)
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" {
		t.Skip("opt-in process sandbox")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	public, secret := filepath.Join(root, "public"), filepath.Join(root, "secret")
	for path, content := range map[string]string{public: "visible source", secret: "private bytes"} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := ExposeReadOnlyPaths(Config{DenyWrite: true, DenyPaths: []string{secret}}, root)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := New(cfg.Merge(Config{}))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", `cat "$1" && ! cat "$2" && ! touch "$3"`, "sh", public, secret, filepath.Join(root, "new"))
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "visible source") || strings.Contains(string(out), "private bytes") {
		t.Fatalf("read-only visibility: %s %v", out, err)
	}
}

func TestNestedReadDenialsSandbox(t *testing.T) {
	skipIfWindows(t)
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" {
		t.Skip("opt-in process sandbox")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "hidden")
	child := filepath.Join(parent, "child")
	if err := os.MkdirAll(child, 0700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(child, "secret")
	if err := os.WriteFile(secret, []byte("private bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	sb, err := New(Config{WritablePaths: []string{root}, DenyPaths: []string{child, parent}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", `! cat "$1" && printf started`, "sh", secret)
	if err := wrapCmdForTest(t, sb, cmd); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "started") || strings.Contains(string(out), "private bytes") {
		t.Fatalf("nested deny mounts: %s %v", out, err)
	}
}
