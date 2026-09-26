package main

import (
	"context"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

func TestLoginFlagsRunInsteadOfAConversation(t *testing.T) {
	for _, tc := range []struct {
		args      []string
		name, arg string
		device    bool
	}{
		{[]string{"--login", "codex"}, "login", "codex", false},
		{[]string{"--login", "codex", "--device"}, "login", "codex", true},
		{[]string{"--logout", "codex"}, "logout", "codex", false},
	} {
		cmd := getCommand()
		var got *Config
		var device bool
		cmd.Action = func(_ context.Context, c *cli.Command) error {
			got = parseConfig(c)
			device = c.Bool("device")
			return nil
		}
		if err := cmd.Run(context.Background(), normalizeCommandArgs(append([]string{"polly"}, tc.args...))); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if got == nil || got.Management == nil || got.Management.name != tc.name || got.ManagementArg != tc.arg || device != tc.device {
			t.Fatalf("%v: management = %+v %q, device = %v", tc.args, got.Management, got.ManagementArg, device)
		}
	}
	if err := runConfigValidationCommand("--login", "codex", "--prompt", "hi"); err == nil || !strings.Contains(err.Error(), "--login does not take prompts or files") {
		t.Fatalf("--login with a prompt: %v", err)
	}
	if err := runConfigValidationCommand("--login", "codex", "--logout", "codex"); err == nil {
		t.Fatal("--login and --logout together were accepted")
	}
}
