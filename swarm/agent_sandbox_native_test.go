package swarm

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/subagent"
)

// Non-Git research uses live files, so policy setup must not create Git guard
// files in a nested repository before the child's read-only policy takes effect.
func TestAgentSandboxReadOnlyLiveSourceDoesNotMaterializeGitGuards(t *testing.T) {
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" {
		t.Skip("opt-in process sandbox")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("sandbox platform")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, tc := range []struct {
		name, floor, request string
	}{
		{name: "readonly floor", floor: "readonly", request: "workspace+git"},
		{name: "base floor", floor: "base", request: "workspace+git"},
		{name: "later readonly", floor: "workspace+git", request: "readonly"},
		{name: "read-only role", request: "workspace+git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := rebuildRuntime(t, runtimeTest(t, doneModel(), 1, 1), func(c *Config) {
				c.AgentSandbox = tc.floor
			})
			suspendAutoRelease(t, r)
			nested := filepath.Join(r.config.Root, "nested")
			if err := os.Mkdir(nested, 0o700); err != nil {
				t.Fatal(err)
			}
			refreshGit(t, nested, "init", "-q")
			hooks := filepath.Join(nested, ".git", "hooks")
			if err := os.RemoveAll(hooks); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			result, err := r.Spawn(ctx, subagent.Request{Label: "Read-only researcher", Task: "Inspect the live source", ReadOnly: true, Sandbox: tc.request})
			if err != nil {
				t.Fatal(err)
			}
			s, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			member := s.Members[result.Session]
			if member == nil || s.Contexts[member.Context] == nil {
				t.Fatal("missing member context")
			}
			if c := s.Contexts[member.Context]; c.Checkout != nil || !c.ReadOnly {
				t.Fatalf("expected live read-only context: %+v", c)
			}
			if _, err := os.Stat(hooks); !os.IsNotExist(err) {
				t.Fatalf("child policy setup created or changed the missing Git hooks directory: %v", err)
			}
		})
	}
}
