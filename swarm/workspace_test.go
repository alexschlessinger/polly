package swarm

import (
	"bytes"
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

func TestFourAgentsFromLinkedWorkspace(t *testing.T) {
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" {
		t.Skip("opt-in process sandbox")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("sandbox platform")
	}
	for _, preset := range []string{"workspace", "workspace+git"} {
		for _, readOnly := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/readOnly=%t", preset, readOnly), func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
				t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
				entered := make(chan struct{}, 4)
				release := make(chan struct{})
				var once sync.Once
				model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
					if req.Messages[len(req.Messages)-1].Role != messages.MessageRoleTool {
						entered <- struct{}{}
						select {
						case <-release:
						case <-ctx.Done():
							return answer("interrupted")
						}
						calls := []messages.ChatMessageToolCall{{ID: "read", Name: "read_file", Arguments: `{"path":"dirty.txt"}`}, {ID: "git", Name: "bash", Arguments: `{"command":"git status --porcelain && git rev-parse --is-inside-work-tree"}`}}
						if !readOnly {
							calls = append(calls, messages.ChatMessageToolCall{ID: "edit", Name: "write_file", Arguments: `{"path":"candidate.txt","content":"member edit\n"}`})
						}
						return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: calls}
					}
					found, gitOK := false, false
					var gitResult string
					for _, msg := range req.Messages {
						if msg.Role != messages.MessageRoleTool {
							continue
						}
						if msg.ToolCallID == "read" && strings.Contains(msg.Content, "uncommitted review input") {
							found = true
						}
						if msg.ToolCallID == "git" {
							gitResult = msg.Content
							gitOK = strings.Contains(msg.Content, "true")
						}
					}
					if !found || !gitOK {
						t.Errorf("member snapshot read=%t git status=%t: %s", found, gitOK, gitResult)
					}
					return answer("review complete")
				})
				r := runtimeTest(t, model, 4, 4)
				t.Cleanup(func() { once.Do(func() { close(release) }) })
				main := r.config.Root
				git := func(root string, args ...string) []byte {
					t.Helper()
					cmd := exec.Command("git", args...)
					cmd.Dir = root
					out, err := cmd.CombinedOutput()
					if err != nil {
						t.Fatalf("git %v: %v %s", args, err, out)
					}
					return out
				}
				git(main, "init", "-q")
				git(main, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-qm", "base")
				root := filepath.Join(t.TempDir(), "parent")
				git(main, "worktree", "add", "--detach", root, "HEAD")
				t.Chdir(root)
				if err := os.WriteFile(filepath.Join(root, "dirty.txt"), []byte("uncommitted review input\n"), 0600); err != nil {
					t.Fatal(err)
				}
				indexPath := strings.TrimSpace(string(git(root, "rev-parse", "--path-format=absolute", "--git-path", "index")))
				index, err := os.ReadFile(indexPath)
				if err != nil {
					t.Fatal(err)
				}
				cfg, err := sandbox.ParsePreset(preset)
				if err != nil {
					t.Fatal(err)
				}
				registry := tools.NewToolRegistry(nil, tools.WithSandboxFactory(sandbox.New, cfg))
				defer registry.Close()
				for _, name := range []string{"read_file", "write_file", "bash"} {
					if _, err := registry.LoadToolAuto(name); err != nil {
						t.Fatal(err)
					}
				}
				r.config.Root, r.config.Registry, r.config.MaxWorktrees = root, registry, 8
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				var children []subagent.Result
				for n := 0; n < 4; n++ {
					child, err := r.Spawn(ctx, subagent.Request{Task: "review llm tests", Label: fmt.Sprintf("reviewer %d", n), ReadOnly: readOnly, Background: true})
					if err != nil {
						t.Fatalf("spawn %d: %v", n, err)
					}
					children = append(children, child)
				}
				for range children {
					select {
					case <-entered:
					case <-ctx.Done():
						t.Fatal("four members did not start concurrently")
					}
				}
				once.Do(func() { close(release) })
				for _, child := range children {
					select {
					case <-child.Done:
					case <-ctx.Done():
						t.Fatal("members did not finish")
					}
				}
				s, err := r.State(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(s.Executions) != 4 || len(s.Contexts) != 4 {
					t.Fatalf("executions=%d contexts=%d", len(s.Executions), len(s.Contexts))
				}
				for _, e := range s.Executions {
					if e.Status != "completed" {
						t.Errorf("execution %s: %s %s", e.ID, e.Status, e.Error)
					}
				}
				for _, c := range s.Contexts {
					if c.Checkout == nil || c.Root == root {
						t.Error("member used a live checkout instead of a snapshot")
					}
					if !readOnly {
						if data, err := os.ReadFile(filepath.Join(c.Root, "candidate.txt")); err != nil || string(data) != "member edit\n" {
							t.Errorf("member edit: %q %v", data, err)
						}
					}
				}
				after, _ := os.ReadFile(indexPath)
				if !bytes.Equal(index, after) {
					t.Fatal("spawn modified parent index")
				}
				if _, err := os.Stat(filepath.Join(root, "candidate.txt")); !os.IsNotExist(err) {
					t.Fatal("member edit reached parent checkout")
				}
			})
		}
	}
}

func TestReadOnlyDoesNotHideBrokenGitSetup(t *testing.T) {
	skipIfWindows(t)
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		t.Error("model called after broken Git setup")
		return answer("unexpected")
	}), 1, 1)
	if err := os.WriteFile(filepath.Join(r.config.Root, ".git"), []byte("gitdir: /missing/polly-gitdir\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Spawn(context.Background(), subagent.Request{Task: "review", ReadOnly: true}); err == nil {
		t.Fatal("broken Git silently downgraded to live files")
	}
}
