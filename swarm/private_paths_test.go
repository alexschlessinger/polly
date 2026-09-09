package swarm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestPrivatePathsAreNotCopiedIntoMember(t *testing.T) {
	skipIfWindows(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	var private string
	var relative, absolute string
	model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		if req.Messages[len(req.Messages)-1].Role != messages.MessageRoleTool {
			return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{
				{ID: "relative", Name: "read_file", Arguments: `{"path":"session-private.txt"}`},
				{ID: "absolute", Name: "read_file", Arguments: tools.Result(map[string]any{"path": private})},
			}}
		}
		for _, msg := range req.Messages {
			if msg.Role == messages.MessageRoleTool {
				if msg.ToolCallID == "relative" {
					relative = msg.Content
				}
				if msg.ToolCallID == "absolute" {
					absolute = msg.Content
				}
			}
		}
		return answer("done")
	})
	r := runtimeTest(t, model, 1, 1)
	private = filepath.Join(r.config.Root, "session-private.txt")
	if err := os.WriteFile(private, []byte("private-session-content"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-qm", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = r.config.Root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
	}
	registry := tools.NewToolRegistry(nil, tools.WithUnsafeNoSandbox())
	defer registry.Close()
	if _, err := registry.LoadToolAuto("read_file"); err != nil {
		t.Fatal(err)
	}
	r.config.Registry = registry
	r.config.PrivatePaths = []string{private}
	r.config.MaxWorktrees = 4
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := r.Agent(ctx, "", AgentRequest{Task: "read fixture", Tools: []string{"read_file"}, ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(absolute, "blocked") {
		t.Fatalf("original private path was not denied: %s", absolute)
	}
	if strings.Contains(relative, "private-session-content") || !strings.Contains(relative, "no such file") {
		t.Fatalf("private file was not excluded from member snapshot: %q", relative)
	}
}
