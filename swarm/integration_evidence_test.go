package swarm

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"github.com/alexschlessinger/pollytool/workflow"
)

// The model is deterministic; Git, tool execution, worker transcripts, workflow
// persistence, validation, integration and the parent loop are production paths.
func TestIntegrationEvidenceExercise(t *testing.T) {
	skipIfWindows(t)
	source, err := os.ReadFile("../examples/workflows/integration-evidence-exercise.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, sandboxed := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "sandbox"}[sandboxed], func(t *testing.T) {
			if sandboxed && (os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" || runtime.GOOS != "darwin" && runtime.GOOS != "linux") {
				t.Skip("opt-in process sandbox")
			}
			t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			root := t.TempDir()
			t.Chdir(root)
			git := func(args ...string) []byte {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = root
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %s %v", args, out, err)
				}
				return out
			}
			write := func(path, value string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, path), []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			const path = "features' test.txt" // Exercise literal shell path quoting too.
			git("init", "-q")
			write(path, "features=base\n")
			write("parent.txt", "parent base\n")
			git("add", ".")
			git("-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-qm", "base")
			write("parent.txt", "parent draft\n")
			git("add", "parent.txt")
			write("untracked.txt", "untracked draft\n")
			head := git("rev-parse", "HEAD")
			indexPath := filepath.Join(root, ".git", "index")
			index, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			registry := tools.NewToolRegistry(nil, tools.WithUnsafeNoSandbox())
			if sandboxed {
				registry.Close()
				cfg, err := sandbox.ParsePreset("workspace")
				if err != nil {
					t.Fatal(err)
				}
				registry = tools.NewToolRegistry(nil, tools.WithSandboxFactory(sandbox.New, cfg))
			}
			defer registry.Close()
			for _, name := range []string{"read_file", "write_file", "bash"} {
				if _, err := registry.LoadToolAuto(name); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			store, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeMemory})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			parent, err := store.Acquire(ctx, "exercise-parent", sessions.AcquireOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()
			var r *Runtime
			var applied atomic.Int32
			model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				var brief string
				read, wrote := false, false
				for _, m := range req.Messages {
					if m.Role == messages.MessageRoleUser {
						brief += m.Content
					}
					read = read || m.ToolName == "read_file"
					wrote = wrote || m.ToolName == "write_file"
				}
				s, err := r.State(ctx)
				if err != nil {
					t.Error(err)
					return answer("state unavailable")
				}
				if len(s.Applies) != 0 || applied.Load() != 0 {
					t.Error("applied before repair, review and validation finished")
				}
				for _, task := range s.Tasks {
					if task.Requirement == RequirementApplied && (task.Status == "done" || task.AcceptedRevision != 0) {
						t.Error("editing work accepted before explicit integration")
					}
				}
				if strings.Contains(brief, "Review the integration evidence exercise") {
					if !read {
						return iterationTool("read", "read_file", tools.Result(map[string]any{"path": path}))
					}
					for _, m := range req.Messages {
						if m.ToolName == "read_file" && !strings.Contains(m.Content, "features=base,alpha,beta") {
							t.Error("reviewer did not receive repaired contents")
						}
					}
					return iterationTool("complete", "swarm_complete", `{"value":{"approved":true,"feedback":"Both contributions and exact contents checked"}}`)
				}
				if wrote {
					return answer("Updated only the exercise fixture; automatic capture may submit it for acceptance.")
				}
				content := "features=base,alpha\n"
				if strings.Contains(brief, "exercise beta") {
					content = "features=base,beta\n"
				} else if strings.Contains(brief, "exercise complete repair") {
					content = "features=base,alpha,beta\n"
					failed := 0
					for _, w := range s.Workflows {
						for _, step := range w.Steps {
							if step.Kind == "exec" && step.Status == "failed" && step.Error.Code == "command_failed" {
								failed++
								if step.Error.Result.(map[string]any)["exitCode"] != float64(1) {
									t.Error("repair was not triggered by a real content mismatch")
								}
							}
						}
					}
					if failed != 1 {
						t.Errorf("expected one failed content check before repair, got %d", failed)
					}
				}
				return iterationTool("write", "write_file", tools.Result(map[string]any{"path": path, "content": content}))
			})
			r, err = New(Config{Store: store, Parent: parent, Registry: registry, Client: model, Root: root,
				Directory: filepath.Join(t.TempDir(), "runtime"), MaxConcurrent: 2, MaxExecutions: 8, MaxWorktrees: 16,
				Agent: llm.AgentConfig{MaxIterations: 5}, OnEvent: func(e Event) {
					if e.Kind == "integration" && strings.HasPrefix(e.Text, "applied ") {
						applied.Add(1)
					}
				}})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			r.RegisterParentTools(registry)
			turns := 0
			parentModel := modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				turns++
				for _, m := range req.Messages {
					if m.Role == messages.MessageRoleUser && strings.Contains(m.Content, "Workflow integration-evidence-exercise") {
						t.Error("terminal output duplicated into parent provider input")
					}
				}
				if turns == 1 {
					return iterationTool("exercise", "workflow_run", tools.Result(map[string]any{"source": string(source), "input": tools.Result(map[string]any{"path": path})}))
				}
				for _, m := range req.Messages {
					if m.ToolName == "workflow_run" {
						if success, _ := m.ToolSucceeded(); !success {
							t.Errorf("exercise workflow failed: %s", m.Content)
						}
					}
				}
				return answer("Verified a failed assertion, repaired the candidate and applied the validated commit once.")
			})
			agent := llm.NewAgent(parentModel, registry, llm.AgentConfig{MaxIterations: 3, ArtifactStore: parent.ArtifactStore()})
			defer agent.Close()
			if _, err := r.RunParent(ctx, agent, &llm.CompletionRequest{}, nil, nil); err != nil {
				t.Fatal(err)
			}
			s, err := r.State(ctx)
			if err != nil || len(s.Workflows) != 1 || len(s.Applies) != 1 || applied.Load() != 1 {
				t.Fatalf("exercise state: workflows=%d applies=%d applied events=%d err=%v", len(s.Workflows), len(s.Applies), applied.Load(), err)
			}
			var report *workflow.Report
			for _, w := range s.Workflows {
				report = w
			}
			if report.Status != "completed" {
				t.Fatalf("exercise failed: %+v", report.Error)
			}
			output := report.Output.(map[string]any)
			validations := output["validations"].([]any)
			first, last := validations[0].(map[string]any), validations[1].(map[string]any)
			if first["passed"] != false || last["passed"] != true || first["commit"] == last["commit"] || last["commit"] != output["commit"] {
				t.Fatalf("validation did not follow the repaired commit: %+v", validations)
			}
			for _, receipt := range s.Applies {
				if receipt.Status != "applied" || receipt.Plan.Merged.Commit != output["commit"] {
					t.Fatalf("applied a different commit: %+v", receipt)
				}
			}
			for file, want := range map[string]string{path: "features=base,alpha,beta\n", "parent.txt": "parent draft\n", "untracked.txt": "untracked draft\n"} {
				data, err := os.ReadFile(filepath.Join(root, file))
				if err != nil || string(data) != want {
					t.Fatalf("%s: %q %v", file, data, err)
				}
			}
			afterIndex, err := os.ReadFile(indexPath)
			if err != nil || !bytes.Equal(index, afterIndex) || !bytes.Equal(head, git("rev-parse", "HEAD")) {
				t.Fatal("integration changed the parent index or HEAD")
			}
			// Every emitted capture still resolves in its intended Git context.
			for _, snapshot := range s.Snapshots {
				if got := strings.TrimSpace(string(git("rev-parse", snapshot.Commit+"^{tree}"))); got != snapshot.Tree {
					t.Errorf("unresolvable captured commit: %+v", snapshot)
				}
			}
			for _, task := range s.Tasks {
				if task.Status != "done" || task.Requirement == RequirementApplied && task.AcceptedRevision != task.Revision {
					t.Errorf("explicit integration did not complete task: %+v", task)
				}
			}
			for _, member := range s.Members {
				view, err := store.ReadView(ctx, sessions.ViewTarget{ID: member.ID}, "")
				if err != nil {
					t.Fatal(err)
				}
				finals, writes, completions := 0, 0, 0
				for _, m := range view.History {
					if m.Role == messages.MessageRoleAssistant && m.StopReason == messages.StopReasonEndTurn {
						finals++
					}
					if m.ToolName == "write_file" {
						writes++
					}
					if m.ToolName == "swarm_complete" {
						completions++
					}
				}
				if !member.ReadOnly && (finals != 1 || writes != 1 || completions != 0) {
					t.Errorf("worker %s did not use ordinary final submission: finals=%d writes=%d typed=%d", member.Label, finals, writes, completions)
				}
				if member.ReadOnly && completions != 1 {
					t.Error("reviewer typed completion missing")
				}
			}
			history, err := parent.GetHistory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			terminal := 0
			for _, m := range history {
				if _, ok := workflowDeliveryOf(m); ok {
					terminal++
				}
				if m.Role == messages.MessageRoleUser && strings.Contains(m.Content, "Workflow integration-evidence-exercise") {
					t.Error("terminal output duplicated into persisted parent transcript")
				}
			}
			if terminal != 1 || turns != 2 {
				t.Fatalf("foreground result count=%d provider turns=%d", terminal, turns)
			}
			assertWorkflowDelivery(t, r, true, true)
			data, _ := json.Marshal(map[string]any{"failedCommit": first["commit"], "validatedCommit": last["commit"], "candidate": output["candidate"], "applies": applied.Load(), "workers": len(s.Members), "parentResults": terminal})
			t.Log(string(data))
		})
	}
}
