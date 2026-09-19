package sandbox

import (
	"os"
	"path/filepath"
)

// HostExecutionPaths lists the places the host runs code from on its own,
// outside any sandbox, whether or not they exist yet: every absolute PATH
// entry and the install prefixes behind it (see pathEntryPrefixes), the
// shell startup files, the per-user service directories (launchd agents,
// systemd user units and environment, XDG autostart), and the user's Git
// configuration. A write grant at, inside or containing one of them lets a
// sandboxed command change what the host later runs. The list covers the
// common places rather than all of them: an editor's plugin directory, for
// one, is not on it.
func HostExecutionPaths() []string {
	home := resolvedHomeDir()
	if home == "" {
		return nil
	}
	paths := pathEntryPrefixes(home)
	for _, name := range homeStartupPaths {
		paths = append(paths, filepath.Join(home, name))
	}
	config := filepath.Join(home, ".config")
	if value := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(value) {
		config = filepath.Clean(value)
	}
	for _, name := range configStartupPaths {
		paths = append(paths, filepath.Join(config, name))
	}
	for _, name := range []string{"ZDOTDIR", "GIT_CONFIG_GLOBAL"} {
		if value := os.Getenv(name); filepath.IsAbs(value) {
			paths = append(paths, filepath.Clean(value))
		}
	}
	return paths
}

// homeStartupPaths are the shell startup files, service directories and Git
// configuration HostExecutionPaths lists under the home directory.
var homeStartupPaths = []string{
	".profile", ".bash_profile", ".bash_login", ".bashrc", ".bash_logout",
	".zshenv", ".zprofile", ".zshrc", ".zlogin", ".zlogout",
	".cshrc", ".tcshrc", ".login", ".kshrc", ".mkshrc",
	".gitconfig",
	"Library/LaunchAgents",
	".local/share/systemd",
}

// configStartupPaths are the ones it lists under the XDG configuration
// directory, ~/.config unless $XDG_CONFIG_HOME moves it.
var configStartupPaths = []string{
	"fish", "nushell", "git", "systemd", "autostart", "environment.d",
}

// SensitiveEnvName reports whether the sandbox strips a variable of this name
// from the environment by default: a credential-shaped name, an agent socket
// or a host runtime handle. passEnv exempts such a name.
func SensitiveEnvName(name string) bool {
	return isSensitiveEnv(name)
}
