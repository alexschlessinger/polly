package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/alexschlessinger/pollytool/swarm"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

type integrationModel func(context.Context, *llm.CompletionRequest) messages.ChatMessage

func (f integrationModel) ChatCompletionStream(ctx context.Context, req *llm.CompletionRequest, p llm.EventStreamProcessor) <-chan *messages.StreamEvent {
	ch := make(chan messages.ChatMessage, 1)
	ch <- f(ctx, req)
	close(ch)
	return p.ProcessMessagesToEvents(ch)
}

func TestIntegrationWorkflowClientsInSandboxedLinkedCheckout(t *testing.T) {
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" {
		t.Skip("opt-in process sandbox")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("sandbox platform")
	}
	source, err := os.ReadFile("../../examples/workflows/integrate-results.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, client := range []string{"library", "cli tools with blocking spawn", "tui command"} {
		t.Run(client, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			main, root := t.TempDir(), filepath.Join(t.TempDir(), "linked")
			git := func(dir string, args ...string) []byte {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = dir
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
				return out
			}
			git(main, "init", "-q")
			git(main, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-qm", "base")
			git(main, "worktree", "add", "--detach", root, "HEAD")
			t.Chdir(root)
			if err := os.WriteFile(filepath.Join(root, "parent.txt"), []byte("parent draft\n"), 0600); err != nil {
				t.Fatal(err)
			}
			git(root, "add", "parent.txt")
			indexPath := strings.TrimSpace(string(git(root, "rev-parse", "--path-format=absolute", "--git-path", "index")))
			index, _ := os.ReadFile(indexPath)
			head := git(root, "rev-parse", "HEAD")
			cfg, err := sandbox.ParsePreset("workspace")
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
			store := testOpenMemoryStore(t, nil)
			parent := testAcquireSession(t, store, "integration-parent")
			applied := make(chan struct{})
			var once sync.Once
			model := integrationModel(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				var brief string
				for _, m := range req.Messages {
					if m.Role == messages.MessageRoleUser {
						brief = m.Content
					}
				}
				if strings.Contains(brief, "HOLD_UNTIL_APPLY") {
					select {
					case <-applied:
					case <-ctx.Done():
					}
					return spawnTestReply("hold finished")
				}
				if strings.Contains(brief, "Independently review") {
					return spawnTestReply(`{"approved":true,"feedback":"both contributions verified"}`)
				}
				if req.Messages[len(req.Messages)-1].Role != messages.MessageRoleTool {
					name := "a.txt"
					if strings.Contains(brief, "WRITE_B") {
						name = "b.txt"
					}
					return spawnTestToolCall("write_file", tools.Result(map[string]any{"path": name, "content": "candidate\n"}))
				}
				return spawnTestReply("editing finished")
			})
			r, err := swarm.New(swarm.Config{Store: store, Parent: parent, Registry: registry, Client: model, Root: root, Directory: filepath.Join(t.TempDir(), "runtime"), MaxWorktrees: 16, MaxConcurrent: 4, Agent: llm.AgentConfig{MaxIterations: 20}, OnEvent: func(e swarm.Event) {
				if e.Kind == "integration" && strings.HasPrefix(e.Text, "applied ") {
					once.Do(func() { close(applied) })
				}
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			r.RegisterParentTools(registry)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			refs := []swarm.TaskReference{}
			for _, brief := range []string{"WRITE_A", "WRITE_B"} {
				result, err := r.Agent(ctx, "", swarm.AgentRequest{Task: brief, Tools: []string{"write_file"}})
				if err != nil {
					t.Fatal(err)
				}
				task, err := r.ReadTask(ctx, result.Task)
				if err != nil {
					t.Fatal(err)
				}
				refs = append(refs, swarm.TaskReference{Task: task.ID, Revision: task.Revision})
			}
			input := map[string]any{"tasks": refs, "checks": []string{"test -s a.txt && test -s b.txt", "test -s parent.txt"}}
			switch client {
			case "library":
				if _, err := r.RunWorkflow(ctx, string(source), input); err != nil {
					t.Fatal(err)
				}
			case "cli tools with blocking spawn":
				parentModel := integrationModel(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
					if len(req.Messages) > 0 && req.Messages[len(req.Messages)-1].Role == messages.MessageRoleTool {
						return spawnTestReply("integrated")
					}
					return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{
						{ID: "hold", Name: "spawn_agent", Arguments: `{"task":"HOLD_UNTIL_APPLY","read_only":true}`},
						{ID: "integrate", Name: "workflow_run", Arguments: tools.Result(map[string]any{"source": string(source), "input": tools.Result(input)})},
					}}
				})
				response, err := llm.NewAgent(parentModel, registry, llm.AgentConfig{MaxIterations: 3}).Run(ctx, &llm.CompletionRequest{}, nil)
				if err != nil {
					t.Fatal("foreground orchestration deadlocked or failed", err)
				}
				for _, msg := range response.AllMessages {
					if msg.Role == messages.MessageRoleTool {
						if success, known := msg.ToolSucceeded(); known && !success {
							t.Fatal(msg.Content)
						}
					}
				}
			case "tui command":
				dir := t.TempDir()
				scriptPath, inputPath := filepath.Join(dir, "integration.js"), filepath.Join(dir, "input.json")
				if err := os.WriteFile(scriptPath, source, 0600); err != nil {
					t.Fatal(err)
				}
				data, _ := json.Marshal(input)
				if err := os.WriteFile(inputPath, data, 0600); err != nil {
					t.Fatal(err)
				}
				command := &replCommandContext{ctx: ctx, state: &conversationState{swarm: r}}
				replies := dispatchDefaultCommandForTest(t, "/workflow "+scriptPath+" "+inputPath, command)
				if len(replies) != 1 || !strings.Contains(replies[0], "Workflow started:") {
					t.Fatal(replies)
				}
				for {
					state, err := r.State(ctx)
					if err != nil {
						t.Fatal(err)
					}
					finished := false
					for _, report := range state.Workflows {
						if report.Status != "running" {
							if report.Status != "completed" {
								t.Fatalf("workflow %s: %+v", report.Status, report.Error)
							}
							finished = true
						}
					}
					if finished {
						break
					}
					select {
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					case <-time.After(10 * time.Millisecond):
					}
				}
				state, err := r.State(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(swarmInspectorText(state, "integrations"), "Apply: applied") {
					t.Fatal("TUI inspector missing receipt")
				}
			}
			state, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, ref := range refs {
				if state.Tasks[ref.Task].Status != "done" {
					t.Fatal("contribution not integrated")
				}
			}
			for _, name := range []string{"a.txt", "b.txt"} {
				data, _ := os.ReadFile(filepath.Join(root, name))
				if string(data) != "candidate\n" {
					t.Fatalf("missing %s: %s", name, data)
				}
			}
			afterIndex, _ := os.ReadFile(indexPath)
			if !bytes.Equal(index, afterIndex) || !bytes.Equal(head, git(root, "rev-parse", "HEAD")) {
				t.Fatal("parent index or branch changed")
			}
		})
	}
}
