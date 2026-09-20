package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadableHomePolicy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := Config{DenyHostTemp: true}
	ordinary := filepath.Join(home, "tools", "config")
	if err := ReadAllowed(cfg, ordinary); err != nil {
		t.Fatal(err)
	}
	if WriteAllowed(cfg, ordinary) == nil {
		t.Fatal("ordinary home became writable")
	}
	for _, name := range []string{".ssh/id_ed25519", ".npmrc", ".pollytool/polly.db", ".pollytool/workspaces/repo/sandbox.json", ".pollytool/worktrees/sibling/file"} {
		if ReadAllowed(cfg, filepath.Join(home, name)) == nil {
			t.Fatalf("exposed %s", name)
		}
	}
	strict := cfg.Merge(Config{PrivateHome: true})
	if ReadAllowed(strict, ordinary) == nil {
		t.Fatal("private-home did not hide home")
	}
	if !strict.Merge(Config{}).PrivateHome {
		t.Fatal("overlay dropped private-home")
	}
	strict.ReadPaths = []string{filepath.Dir(ordinary)}
	if err := ReadAllowed(strict, ordinary); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(home, ".pollytool/worktrees/mine")
	cfg.WritablePaths = []string{mine}
	if err := WriteAllowed(cfg, filepath.Join(mine, "file")); err != nil {
		t.Fatal(err)
	}
	if ReadAllowed(cfg, filepath.Join(home, ".pollytool/worktrees/sibling/file")) == nil {
		t.Fatal("member grant exposed sibling")
	}
}

func TestNativeReadableHome(t *testing.T) {
	skipIfWindows(t)
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" {
		t.Skip("opt-in native sandbox")
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	parent, err := os.MkdirTemp(realHome, ".polly-readable-home-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(parent) })
	home := filepath.Join(parent, "home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	for _, name := range []string{"ordinary/config", ".ssh/key", ".pollytool/polly.db", ".pollytool/worktrees/mine/file", ".pollytool/worktrees/sibling/file"} {
		path := filepath.Join(home, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	mine := filepath.Join(home, ".pollytool/worktrees/mine")
	for _, private := range []bool{false, true} {
		cfg := Config{PrivateHome: private, DenyHostTemp: true, WritablePaths: []string{parent, mine}}
		sb, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		script := `cat "$HOME/ordinary/config" >/dev/null`
		if private {
			script = "! " + script
		}
		script += ` && ! cat "$HOME/.ssh/key" && ! cat "$HOME/.pollytool/polly.db" && ! cat "$HOME/.pollytool/worktrees/sibling/file" && echo mine > "$1/file" && cat "$1/file"`
		if !private {
			script += ` && ! sh -c 'echo bad > "$HOME/ordinary/config"'`
		}
		cmd := exec.Command("sh", "-c", script, "sh", mine)
		cmd.Dir = "/"
		if err := wrapCmdForTest(t, sb, cmd); err != nil {
			t.Fatal(err)
		}
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "mine") {
			t.Fatalf("private=%v: %v: %s", private, err, out)
		}
	}
	data, err := os.ReadFile(filepath.Join(home, "ordinary/config"))
	if err != nil || string(data) != "fixture" {
		t.Fatalf("host file changed: %s %v", data, err)
	}
}
