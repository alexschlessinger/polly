package swarm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// scratchRuntime builds a test runtime whose root is a Git repository when
// git is set, so members receive isolated checkouts, or a plain directory,
// so read-only members observe the live tree.
func scratchRuntime(t *testing.T, model llm.LLM, git bool) *Runtime {
	t.Helper()
	if git {
		skipIfWindows(t)
	}
	r := runtimeTest(t, model, 1, 4)
	if !git {
		return r
	}
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-qm", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = r.config.Root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
	}
	config := r.config
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	fresh, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fresh.Close() })
	return fresh
}

func canonicalPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func onlyContext(t *testing.T, s *State) *ExecutionContext {
	t.Helper()
	if len(s.Contexts) != 1 {
		t.Fatalf("contexts = %d, want 1", len(s.Contexts))
	}
	for _, c := range s.Contexts {
		return c
	}
	return nil
}

func TestContextScratchLifecycle(t *testing.T) {
	for _, git := range []bool{true, false} {
		t.Run(map[bool]string{true: "checkout", false: "live"}[git], func(t *testing.T) {
			r := scratchRuntime(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), git)
			// This test owns manual cleanup; automatic release has its own coverage.
			suspendAutoRelease(t, r)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, err := r.Agent(ctx, "", AgentRequest{Task: "look around", ReadOnly: true, Tools: []string{}}); err != nil {
				t.Fatal(err)
			}
			s, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			c := onlyContext(t, s)
			if c.Scratch == "" {
				t.Fatalf("context without scratch: %+v", c)
			}
			if info, err := os.Stat(c.Scratch); err != nil || !info.IsDir() {
				t.Fatalf("scratch missing: %v", err)
			}
			if git {
				if c.Checkout == nil || c.Scratch != c.Checkout.ScratchDir() {
					t.Fatalf("checkout scratch = %s, checkout %+v", c.Scratch, c.Checkout)
				}
			} else if filepath.Dir(c.Scratch) != filepath.Join(canonicalPath(t, r.config.Directory), "scratch") || !strings.HasPrefix(filepath.Base(c.Scratch), "live-") {
				t.Fatalf("live scratch = %s, want a reserved live slot under %s/scratch", c.Scratch, r.config.Directory)
			}
			ec, err := r.contextPolicy(ctx, s, c)
			if err != nil {
				t.Fatal(err)
			}
			if !ec.ReadOnly || ec.Sandbox.DenyWrite || ec.Sandbox.DenyHostTemp || !slices.Equal(ec.Sandbox.WritablePaths, []string{c.Scratch}) || ec.Sandbox.Env["TMPDIR"] != c.Scratch || !slices.Contains(ec.Sandbox.DenyWritePaths, canonicalPath(t, c.Root)) {
				t.Fatalf("member policy = %+v", ec.Sandbox)
			}
			// Slots of the other kind are denied by name before they exist.
			directory := canonicalPath(t, r.config.Directory)
			foreign := filepath.Join(directory, "slot-0007")
			if git {
				foreign = filepath.Join(directory, "scratch")
			}
			if err := sandbox.ReadAllowed(ec.Sandbox, filepath.Join(foreign, "notes")); err == nil {
				t.Fatalf("%s readable from a %s context", foreign, map[bool]string{true: "checkout", false: "live"}[git])
			}
			if err := r.Cleanup(ctx, c.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(c.Scratch); !os.IsNotExist(err) {
				t.Fatalf("cleanup kept the scratch: %v", err)
			}
			if s, err = r.State(ctx); err != nil || s.Contexts[c.ID] != nil {
				t.Fatalf("context retained after cleanup: %v", err)
			}
		})
	}
}

// A live member's policy, bound when it starts, already denies the scratch
// of a sibling that starts later: scratch slots are reserved by name.
func TestContextPolicyDeniesSiblingScratch(t *testing.T) {
	r := scratchRuntime(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), false)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := r.Agent(ctx, "", AgentRequest{Task: "first look", ReadOnly: true, Tools: []string{}}); err != nil {
		t.Fatal(err)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first := onlyContext(t, s)
	early, err := r.contextPolicy(ctx, s, first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Agent(ctx, "", AgentRequest{Task: "second look", ReadOnly: true, Tools: []string{}}); err != nil {
		t.Fatal(err)
	}
	if s, err = r.State(ctx); err != nil {
		t.Fatal(err)
	}
	if len(s.Contexts) != 2 {
		t.Fatalf("contexts = %d, want 2", len(s.Contexts))
	}
	for _, c := range s.Contexts {
		if c.ID == first.ID {
			continue
		}
		if err := sandbox.ReadAllowed(early.Sandbox, filepath.Join(c.Scratch, "notes")); err == nil {
			t.Fatalf("policy bound before %s started still reads its scratch %s", c.ID, c.Scratch)
		}
		if err := sandbox.WriteAllowed(early.Sandbox, filepath.Join(c.Scratch, "notes")); err == nil {
			t.Fatalf("policy bound before %s started still writes its scratch %s", c.ID, c.Scratch)
		}
	}
	if err := sandbox.WriteAllowed(early.Sandbox, filepath.Join(first.Scratch, "notes")); err != nil {
		t.Fatalf("member cannot write its own scratch: %v", err)
	}
	for _, c := range s.Contexts {
		ec, err := r.contextPolicy(ctx, s, c)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(ec.Sandbox.DenyPaths, c.Scratch) {
			t.Fatalf("member denied its own scratch: %v", ec.Sandbox.DenyPaths)
		}
		for _, other := range s.Contexts {
			if other.ID != c.ID && !slices.Contains(ec.Sandbox.DenyPaths, other.Scratch) {
				t.Fatalf("sibling scratch %s readable: %v", other.Scratch, ec.Sandbox.DenyPaths)
			}
		}
	}
}

func TestPrepareRemovesOrphanLiveScratch(t *testing.T) {
	r := scratchRuntime(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), false)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := r.Agent(ctx, "", AgentRequest{Task: "look", ReadOnly: true, Tools: []string{}}); err != nil {
		t.Fatal(err)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	live := onlyContext(t, s).Scratch
	orphan := filepath.Join(r.config.Directory, "scratch", "live-0009")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	config := r.config
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if _, err := restored.CreateTask(ctx, "trigger preparation", "none", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan scratch survived preparation: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live scratch removed: %v", err)
	}
}

func TestMemberPromptDescribesScratch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		git      bool
		readOnly bool
	}{{"read-only live", false, true}, {"read-only checkout", true, true}, {"editing checkout", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			var prompt string
			r := scratchRuntime(t, modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				for _, message := range req.Messages {
					if message.Role == messages.MessageRoleSystem {
						prompt += message.Content
					}
				}
				return answer("done")
			}), tc.git)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, err := r.Agent(ctx, "", AgentRequest{Task: "look", ReadOnly: tc.readOnly, Tools: []string{}}); err != nil {
				t.Fatal(err)
			}
			s, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			c := onlyContext(t, s)
			if !strings.Contains(prompt, "Your private scratch directory is "+c.Scratch) || !strings.Contains(prompt, "$TMPDIR") {
				t.Fatalf("prompt lacks the scratch: %s", prompt)
			}
			if strings.Contains(prompt, "including scratch files and temporary directories") {
				t.Fatalf("prompt still denies scratch: %s", prompt)
			}
			if readOnly := strings.Contains(prompt, "This context is read-only: files in "+c.Root); readOnly != tc.readOnly {
				t.Fatalf("read-only sentence present = %v, want %v: %s", readOnly, tc.readOnly, prompt)
			}
		})
	}
}

// A runtime directory inside the observed tree cannot host a writable
// scratch, so the member keeps the all-writes-denied policy instead of failing.
func TestLiveScratchSkippedWhenRuntimeDirectoryInsideRoot(t *testing.T) {
	seed := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), 1, 1)
	config := seed.config
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	config.Directory = filepath.Join(config.Root, ".pollytool", "worktrees")
	r, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := r.Agent(ctx, "", AgentRequest{Task: "look", ReadOnly: true, Tools: []string{}}); err != nil {
		t.Fatal(err)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := onlyContext(t, s)
	if c.Scratch != "" {
		t.Fatalf("scratch inside the observed tree: %s", c.Scratch)
	}
	ec, err := r.contextPolicy(ctx, s, c)
	if err != nil {
		t.Fatal(err)
	}
	if !ec.ReadOnly || !ec.Sandbox.DenyWrite || ec.Sandbox.Env != nil {
		t.Fatalf("member without scratch lost the all-writes-denied policy: %+v", ec.Sandbox)
	}
}
