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

// Read the examples from the tool result, never from source files. Each caller
// changes into an empty directory before loading help or running a workflow.
func workflowHelpExamples(t *testing.T) []string {
	t.Helper()
	registry := tools.NewToolRegistry(nil)
	defer registry.Close()
	registerHelpTools(registry)
	helper, _ := registry.Get("workflow_help")
	guide, err := helper.Execute(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var examples []string
	for _, block := range strings.Split(guide, "```js\n")[1:] {
		source, _, ok := strings.Cut(block, "\n```")
		if !ok {
			t.Fatal("unclosed JavaScript example")
		}
		examples = append(examples, source)
	}
	if len(examples) != 4 {
		t.Fatalf("guide has %d examples; expected research, editing, tasks and reconciliation", len(examples))
	}
	return examples
}

func TestWorkflowHelpResearchExampleOutsideCheckout(t *testing.T) {
	t.Chdir(t.TempDir())
	source := workflowHelpExamples(t)[0]
	r := runtimeTest(t, modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		var question string
		for _, msg := range req.Messages {
			if msg.Role == messages.MessageRoleUser {
				question, _, _ = strings.Cut(msg.Content, "\n\nCompletion:")
			}
		}
		return completion(tools.Result(map[string]any{"answer": question, "evidence": []string{"fixture evidence"}}))
	}), 2, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	questions := []any{"How is the cache keyed?", "How are failures retried?"}
	report, err := r.RunWorkflow(ctx, source, map[string]any{"questions": questions})
	if err != nil {
		t.Fatalf("research example: %v", err)
	}
	rows, ok := report.Output.([]any)
	if !ok || len(rows) != len(questions) {
		t.Fatalf("research output: %#v", report.Output)
	}
	for i, row := range rows {
		if row.(map[string]any)["answer"] != questions[i] {
			t.Fatalf("parallel results lost order or retained wrappers: %#v", rows)
		}
	}
	state, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Tasks) != 2 {
		t.Fatalf("research tasks: %+v", state.Tasks)
	}
	for _, task := range state.Tasks {
		if task.Requirement != RequirementDelivered || task.Status != "done" {
			t.Fatalf("ordinary research did not complete on delivery: %+v", task)
		}
	}
}

func TestWorkflowHelpEditingExampleOutsideCheckout(t *testing.T) {
	skipIfWindows(t)
	t.Chdir(t.TempDir())
	source := workflowHelpExamples(t)[1]
	for _, tc := range []struct {
		name     string
		approved bool
		check    string
		wantErr  string
	}{
		{name: "integrates checked candidate", approved: true, check: `test "$(cat a.txt)" = fixed`},
		{name: "rejected review retains work", check: "exit 0", wantErr: "Review needs changes"},
		{name: "failed check retains work", approved: true, check: "exit 1", wantErr: "command failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runtimeTest(t, modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				var brief string
				for _, msg := range req.Messages {
					if msg.Role == messages.MessageRoleUser {
						brief = msg.Content
					}
				}
				if strings.HasPrefix(brief, "Compare this candidate") {
					for _, field := range []string{`"baseline"`, `"commit"`, `"paths"`, `"a.txt"`} {
						if !strings.Contains(brief, field) {
							t.Errorf("review omitted comparison evidence %s: %s", field, brief)
						}
					}
					return completion(tools.Result(map[string]any{"approved": tc.approved, "feedback": "fixture review"}))
				}
				if req.Messages[len(req.Messages)-1].Role != messages.MessageRoleTool {
					return iterationTool("write", "write_file", tools.Result(map[string]any{"path": "a.txt", "content": "fixed\n"}))
				}
				return answer("Fixed a.txt.")
			}), 2, 4)
			root := r.config.Root
			if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("base\n"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"init", "-q"}, {"add", "a.txt"}, {"-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-qm", "base"}} {
				cmd := exec.Command("git", args...)
				cmd.Dir = root
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v %s", args, err, output)
				}
			}
			for _, name := range []string{"write_file", "bash"} {
				if _, err := r.config.Registry.LoadToolAuto(name); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			report, err := r.RunWorkflow(ctx, source, map[string]any{"task": "Fix a.txt.", "checks": []any{tc.check}})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("editing example: %v", err)
				}
				out := report.Output.(map[string]any)
				integration := out["integration"].(map[string]any)
				if integration["status"] != "applied" || integration["receipt"].(map[string]any)["status"] != "applied" {
					t.Fatalf("missing applied receipt: %#v", out)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) || report.Status != "failed" {
				t.Fatalf("want %q, got report=%+v err=%v", tc.wantErr, report, err)
			}
			want := "fixed\n"
			if tc.wantErr != "" {
				want = "base\n"
			}
			data, err := os.ReadFile(filepath.Join(root, "a.txt"))
			if err != nil || string(data) != want {
				t.Fatalf("parent file = %q, want %q: %v", data, want, err)
			}
			state, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, task := range state.Tasks {
				if task.Requirement == RequirementApplied {
					if tc.wantErr == "" && task.Status != "done" || tc.wantErr != "" && (task.Status != "awaiting_review" || task.Snapshot == "") {
						t.Fatalf("editing disposition or retained snapshot: %+v", task)
					}
				}
			}
		})
	}
}
