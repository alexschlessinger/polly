package swarm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// Two concurrent editing members cannot see each other's tree or scratch,
// the runtime directory's other slots, or the parent checkout, while their
// own tree, scratch and the shared Git object store keep working.
func TestCheckoutMembersCannotReadEachOther(t *testing.T) {
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" {
		t.Skip("opt-in process sandbox")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("sandbox platform")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var once sync.Once
	model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return answer("done")
	})
	r := runtimeTest(t, model, 2, 2)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	root := r.config.Root
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(root, "dirty.txt"), []byte("parent secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "dirty.txt")
	git("-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-qm", "base")
	t.Chdir(root)
	cfg, err := sandbox.ParsePreset("workspace+git")
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewToolRegistry(nil, tools.WithNativeTools(), tools.WithSandboxFactory(sandbox.New, cfg))
	defer registry.Close()
	for _, name := range []string{"read_file", "bash"} {
		if _, err := registry.LoadToolAuto(name); err != nil {
			t.Fatal(err)
		}
	}
	useRegistry(&r.config, registry)
	r.config.MaxWorktrees = 8
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var children []subagent.Result
	for n := 0; n < 2; n++ {
		child, err := r.Spawn(ctx, subagent.Request{Task: "edit", Label: fmt.Sprintf("editor %d", n), Background: true})
		if err != nil {
			t.Fatalf("spawn %d: %v", n, err)
		}
		children = append(children, child)
	}
	for range children {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("two members did not start concurrently")
		}
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var contexts []*ExecutionContext
	for _, c := range s.Contexts {
		if c.Checkout == nil {
			t.Fatalf("member %s has no checkout", c.ID)
		}
		contexts = append(contexts, c)
	}
	if len(contexts) != 2 {
		t.Fatalf("contexts = %d, want 2", len(contexts))
	}
	runtimeDir, err := r.runtimeDirectory()
	if err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(root, ".git")
	quote := func(path string) string { return "'" + strings.ReplaceAll(path, "'", "'\"'\"'") + "'" }
	for _, member := range contexts {
		var other *ExecutionContext
		for _, c := range contexts {
			if c.ID != member.ID {
				other = c
			}
		}
		ec, err := r.contextPolicy(ctx, s, member)
		if err != nil {
			t.Fatal(err)
		}
		bound, _, err := registry.BindExecutionContext(ec, []string{"bash", "read_file"})
		if err != nil {
			t.Fatal(err)
		}
		bash, _ := bound.Get("bash")
		read, _ := bound.Get("read_file")
		for _, command := range []string{
			"cat " + quote(filepath.Join(other.Root, "dirty.txt")),
			"ls " + quote(other.Scratch),
			"cat " + quote(filepath.Join(root, "dirty.txt")),
			"touch " + quote(filepath.Join(gitDir, "objects", "forbidden")),
		} {
			if out, err := bash.Execute(ctx, map[string]any{"command": command}); err == nil {
				t.Errorf("member %s could run %s: %s", member.ID, command, out)
			}
		}
		for _, command := range []string{
			"ls " + quote(member.Root),
			`printf x > "$TMPDIR/probe" && cat "$TMPDIR/probe"`,
			"git rev-parse --git-common-dir && git cat-file -t HEAD && test -d " + quote(filepath.Join(gitDir, "objects")),
		} {
			if out, err := bash.Execute(ctx, map[string]any{"command": command}); err != nil {
				t.Errorf("member %s could not run %s: %s %v", member.ID, command, out, err)
			}
		}
		if out, err := bash.Execute(ctx, map[string]any{"command": "ls " + quote(runtimeDir)}); err == nil && strings.Contains(out, filepath.Base(filepath.Dir(other.Root))) {
			t.Errorf("member %s listed the sibling slot: %s", member.ID, out)
		}
		if _, err := read.Execute(ctx, map[string]any{"path": filepath.Join(other.Root, "dirty.txt")}); err == nil {
			t.Errorf("member %s read the sibling tree in-process", member.ID)
		}
		if out, err := read.Execute(ctx, map[string]any{"path": filepath.Join(member.Root, "dirty.txt")}); err != nil || !strings.Contains(out, "parent secret") {
			t.Errorf("member %s could not read its own tree: %q %v", member.ID, out, err)
		}
		bound.Close()
	}
	once.Do(func() { close(release) })
	for _, child := range children {
		select {
		case <-child.Done:
		case <-ctx.Done():
			t.Fatal("members did not finish")
		}
	}
}
