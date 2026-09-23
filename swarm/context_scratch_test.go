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

	"github.com/alexschlessinger/pollytool/internal/scratch"
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
	return rebuildRuntime(t, r, nil)
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
			if _, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "look around", ReadOnly: true, Tools: []string{}}); err != nil {
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
			} else if filepath.Dir(c.Scratch) != scratch.Root() || !strings.HasPrefix(filepath.Base(c.Scratch), "live-") {
				t.Fatalf("live scratch = %s, want a reserved live slot in %s", c.Scratch, scratch.Root())
			}
			ec, err := r.contextPolicy(ctx, s, c)
			if err != nil {
				t.Fatal(err)
			}
			if !ec.ReadOnly || ec.Sandbox.DenyWrite || ec.Sandbox.DenyHostTemp || !slices.Equal(ec.Sandbox.WritablePaths, []string{c.Scratch}) || ec.Sandbox.Env["TMPDIR"] != c.Scratch || !slices.Contains(ec.Sandbox.DenyWritePaths, canonicalPath(t, c.Root)) {
				t.Fatalf("member policy = %+v", ec.Sandbox)
			}
			// The runtime directory and the scratch root are each hidden whole,
			// so a slot of the other kind is invisible whether or not it exists
			// yet: a checkout member sees no live scratch, a live member no
			// checkout slot.
			directory := canonicalPath(t, r.config.Directory)
			foreign := filepath.Join(directory, "slot-0007")
			if git {
				foreign = scratch.DirFor(filepath.Join(directory, "live-0007"))
			}
			if err := sandbox.ReadAllowed(ec.Sandbox, filepath.Join(foreign, "notes")); err == nil {
				t.Fatalf("%s readable from a %s context", foreign, map[bool]string{true: "checkout", false: "live"}[git])
			}
			seedReadOnlyScratchCache(t, c.Scratch)
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

// A live member's policy, bound when it starts, already hides the scratch of
// a sibling that starts later: the runtime directory is a private root and
// only the member's own scratch is granted inside it.
func TestContextPolicyHidesSiblingScratch(t *testing.T) {
	r := scratchRuntime(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), false)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "first look", ReadOnly: true, Tools: []string{}}); err != nil {
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
	if _, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "second look", ReadOnly: true, Tools: []string{}}); err != nil {
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
	directory := canonicalPath(t, r.config.Directory)
	for _, c := range s.Contexts {
		ec, err := r.contextPolicy(ctx, s, c)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(ec.Sandbox.DenyPaths, directory) {
			t.Fatalf("runtime directory not hidden: %v", ec.Sandbox.DenyPaths)
		}
		if err := sandbox.WriteAllowed(ec.Sandbox, filepath.Join(c.Scratch, "notes")); err != nil {
			t.Fatalf("member cannot write its own scratch: %v", err)
		}
		for _, other := range s.Contexts {
			if other.ID == c.ID {
				continue
			}
			if err := sandbox.ReadAllowed(ec.Sandbox, filepath.Join(other.Scratch, "notes")); err == nil {
				t.Fatalf("sibling scratch %s readable", other.Scratch)
			}
		}
	}
}

func TestPrepareRemovesOrphanLiveScratch(t *testing.T) {
	r := scratchRuntime(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), false)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "look", ReadOnly: true, Tools: []string{}}); err != nil {
		t.Fatal(err)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	live := onlyContext(t, s).Scratch
	orphan := scratch.DirFor(filepath.Join(canonicalPath(t, r.config.Directory), "live-0009"))
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	seedReadOnlyScratchCache(t, orphan)
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

func TestRelativeRuntimeDirectoryPreservesLiveScratchAndCleansReleasedScratch(t *testing.T) {
	t.Chdir(t.TempDir())
	r := &Runtime{config: Config{Directory: "runtime"}}
	live, err := r.liveScratch(t.TempDir(), "live")
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := r.liveScratch(t.TempDir(), "orphan")
	if err != nil {
		t.Fatal(err)
	}
	r.pruneLiveScratch(map[string]bool{live: true})
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live scratch removed: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan scratch retained: %v", err)
	}
	if err := r.removeContextFiles(context.Background(), &ExecutionContext{Scratch: live}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(live); !os.IsNotExist(err) {
		t.Fatalf("released scratch retained: %v", err)
	}
	outside := t.TempDir()
	if err := r.removeContextFiles(context.Background(), &ExecutionContext{Scratch: outside}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("unowned scratch removed: %v", err)
	}
}

func seedReadOnlyScratchCache(t *testing.T, dir string) {
	t.Helper()
	module := filepath.Join(dir, "gopath", "pkg", "mod", "example@v1")
	if err := os.MkdirAll(module, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, "go.mod"), []byte("module example\n"), 0444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(module, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(module, 0700) })
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
			if _, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "look", ReadOnly: tc.readOnly, Tools: []string{}}); err != nil {
				t.Fatal(err)
			}
			s, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			c := onlyContext(t, s)
			if !strings.Contains(prompt, "Your private scratch directory is "+c.Scratch) || !strings.Contains(prompt, "$TMPDIR") || !strings.Contains(prompt, "Scratch is deleted when your workspace is released") {
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

// A runtime directory inside the observed tree no longer costs the member its
// scratch: scratch lives in the scratch root, outside both. Only an observed
// tree that contains the scratch root itself leaves a member without one, and
// it keeps the all-writes-denied policy rather than failing.
func TestLiveScratchSurvivesRuntimeDirectoryInsideRootAndSkipsAnEnclosingRoot(t *testing.T) {
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
	if _, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "look", ReadOnly: true, Tools: []string{}}); err != nil {
		t.Fatal(err)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := onlyContext(t, s)
	if c.Scratch == "" || sandbox.PathWithin(c.Scratch, c.Root) {
		t.Fatalf("scratch = %q, want one outside the observed tree %s", c.Scratch, c.Root)
	}
	if ec, err := r.contextPolicy(ctx, s, c); err != nil || !ec.ReadOnly || ec.Sandbox.DenyWrite {
		t.Fatalf("member with scratch = %+v, %v", ec.Sandbox, err)
	}
	// An observed tree that encloses the scratch root is the one case left:
	// a scratch there would sit inside the read-only island.
	if dir, err := r.liveScratch(filepath.Dir(scratch.Root()), "id"); err != nil || dir != "" {
		t.Fatalf("liveScratch inside an enclosing root = %q, %v", dir, err)
	}
	c.Scratch = ""
	ec, err := r.contextPolicy(ctx, s, c)
	if err != nil {
		t.Fatal(err)
	}
	if !ec.ReadOnly || !ec.Sandbox.DenyWrite || ec.Sandbox.Env != nil {
		t.Fatalf("member without scratch lost the all-writes-denied policy: %+v", ec.Sandbox)
	}
}
