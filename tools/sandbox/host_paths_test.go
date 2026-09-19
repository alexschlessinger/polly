package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestHostExecutionPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix paths")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	cargo := filepath.Join(home, ".cargo", "bin")
	if err := os.MkdirAll(cargo, 0o700); err != nil {
		t.Fatal(err)
	}
	zdot := filepath.Join(home, "zsh")
	t.Setenv("ZDOTDIR", zdot)
	t.Setenv("GIT_CONFIG_GLOBAL", "relative/gitconfig")
	t.Setenv("PATH", cargo+string(os.PathListSeparator)+"/opt/tool/bin")

	paths := HostExecutionPaths()
	for _, want := range []string{
		cargo, filepath.Join(home, ".cargo"), "/opt/tool/bin", "/opt/tool",
		filepath.Join(home, ".zshrc"), filepath.Join(home, ".gitconfig"),
		filepath.Join(home, ".config", "git"), filepath.Join(home, ".config", "systemd"),
		filepath.Join(home, "Library", "LaunchAgents"), zdot,
	} {
		if !slices.Contains(paths, want) {
			t.Errorf("HostExecutionPaths = %v, missing %q", paths, want)
		}
	}
	if slices.Contains(paths, "relative/gitconfig") {
		t.Errorf("HostExecutionPaths listed a relative GIT_CONFIG_GLOBAL: %v", paths)
	}

	config := filepath.Join(home, "xdg")
	t.Setenv("XDG_CONFIG_HOME", config)
	if paths := HostExecutionPaths(); !slices.Contains(paths, filepath.Join(config, "fish")) {
		t.Errorf("HostExecutionPaths = %v, want fish under $XDG_CONFIG_HOME", paths)
	}
}

func TestSensitiveEnvName(t *testing.T) {
	for name, want := range map[string]bool{"NPM_TOKEN": true, "SSH_AUTH_SOCK": true, "POLLYTOOL_MODEL": true, "GOCACHE": false} {
		if got := SensitiveEnvName(name); got != want {
			t.Errorf("SensitiveEnvName(%q) = %v, want %v", name, got, want)
		}
	}
}
