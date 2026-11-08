package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

func TestSwarmWorkspaceCapacity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		env     string
		want    int
		invalid bool
	}{
		{name: "default", want: 512},
		{name: "explicit", args: []string{"--swarm-worktrees=2048"}, want: 2048},
		{name: "zero retains runtime default", args: []string{"--swarm-worktrees=0"}},
		{name: "environment", env: "1600", want: 1600},
		{name: "flag overrides environment", args: []string{"--swarm-worktrees=24"}, env: "1600", want: 24},
		{name: "negative flag", args: []string{"--swarm-worktrees=-1"}, invalid: true},
		{name: "negative environment", env: "-1", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("POLLYTOOL_SWARM_WORKTREES", tc.env)
			var config *Config
			cmd := &cli.Command{Flags: outputConfigFlags(), Action: func(_ context.Context, cmd *cli.Command) error {
				config = parseConfig(cmd)
				return nil
			}}
			err := cmd.Run(context.Background(), append([]string{"polly"}, tc.args...))
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "swarm-worktrees must be non-negative") {
					t.Fatalf("invalid capacity: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if config.SwarmWorktrees != tc.want || config.SwarmConcurrent != 32 || config.SwarmExecutions != 256 {
				t.Fatalf("capacity = %d, concurrent = %d, executions = %d", config.SwarmWorktrees, config.SwarmConcurrent, config.SwarmExecutions)
			}
		})
	}
}

func TestSwarmWorkspaceCapacityFilePrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("POLLYTOOL_SWARM_WORKTREES", "")
	if err := os.Unsetenv("POLLYTOOL_SWARM_WORKTREES"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, userConfigDirName, userConfigFileName)
	if err := writeUserConfig(path, map[string]string{"POLLYTOOL_SWARM_WORKTREES": "1800"}); err != nil {
		t.Fatal(err)
	}
	config, _ := parseEnvTestConfig(t)
	if config.SwarmWorktrees != 1800 {
		t.Fatalf("file capacity = %d", config.SwarmWorktrees)
	}
	t.Setenv("POLLYTOOL_SWARM_WORKTREES", "1900")
	config, _ = parseEnvTestConfig(t)
	if config.SwarmWorktrees != 1900 {
		t.Fatalf("environment capacity = %d", config.SwarmWorktrees)
	}
	config, _ = parseEnvTestConfig(t, "--swarm-worktrees=2000")
	if config.SwarmWorktrees != 2000 {
		t.Fatalf("flag capacity = %d", config.SwarmWorktrees)
	}
	if err := os.Unsetenv("POLLYTOOL_SWARM_WORKTREES"); err != nil {
		t.Fatal(err)
	}
	if err := writeUserConfig(path, map[string]string{"POLLYTOOL_SWARM_WORKTREES": "-1"}); err != nil {
		t.Fatal(err)
	}
	if err := runConfigValidationCommand(); err == nil || !strings.Contains(err.Error(), "swarm-worktrees must be non-negative") {
		t.Fatalf("invalid file capacity: %v", err)
	}
}
