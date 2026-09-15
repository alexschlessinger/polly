package swarm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/workflow"
)

// Read the examples from the tool result, never from source files. Each caller
// changes into an empty directory before loading help or running a workflow.
func workflowHelp(t *testing.T, args map[string]any) (string, error) {
	t.Helper()
	registry := tools.NewToolRegistry(nil, tools.WithNativeTools())
	defer registry.Close()
	registerHelpTools(registry)
	helper, _ := registry.Get("workflow_help")
	return helper.Execute(context.Background(), args)
}

func workflowHelpExample(t *testing.T, name string) string {
	t.Helper()
	body, err := workflowHelp(t, map[string]any{"example": name})
	if err != nil {
		t.Fatal(err)
	}
	_, block, ok := strings.Cut(body, "```js\n")
	source, _, closed := strings.Cut(block, "\n```")
	if !ok || !closed || strings.Contains(source, "```") || !strings.Contains(source, `polly.workflow("`+name+`"`) {
		t.Fatalf("example %s is not one fenced script defining that workflow: %s", name, body)
	}
	return source
}

func TestWorkflowHelpReferenceIndexesExamples(t *testing.T) {
	reference, err := workflowHelp(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(reference, "```js") {
		t.Fatal("reference carries example code")
	}
	want := []string{"api-tour", "parallel-research", "fix-and-verify", "integrate-results", "task-dependencies", "reconcile-integration"}
	if len(workflowExamples) != len(want) {
		t.Fatalf("guide has %d examples, want %v", len(workflowExamples), want)
	}
	for i, name := range want {
		if workflowExamples[i].name != name || !strings.Contains(reference, "`"+name+"`: "+workflowExamples[i].title) {
			t.Errorf("example %d = %+v; reference index lacks %s", i, workflowExamples[i], name)
		}
		workflowHelpExample(t, name)
	}
	if _, err := workflowHelp(t, map[string]any{"example": "missing"}); err == nil || !strings.Contains(err.Error(), "api-tour") {
		t.Fatalf("unknown example: %v", err)
	}
}

func TestWorkflowHelpTourExampleOutsideCheckout(t *testing.T) {
	skipIfWindows(t)
	t.Chdir(t.TempDir())
	source := workflowHelpExample(t, "api-tour")
	for _, tc := range []struct {
		name    string
		files   []any
		edit    string
		wantErr string
		tasks   int
	}{
		{name: "read-only tour", files: []any{"a.txt", "b.txt"}, tasks: 4},
		{name: "tour integrates edit", files: []any{"a.txt", "b.txt"}, edit: "Rewrite b.txt.", tasks: 5},
		{name: "missing file stops before agents", files: []any{"a.txt", "missing.txt"}, wantErr: "Missing input files"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runtimeTest(t, modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				var brief string
				for _, msg := range req.Messages {
					if msg.Role == messages.MessageRoleUser {
						brief = msg.Content
					}
				}
				switch {
				case strings.HasPrefix(brief, "Summarize "):
					return completion(tools.Result(map[string]any{"summary": brief, "risk": "low", "lines": 1}))
				case strings.HasPrefix(brief, "Rank the files"):
					for _, fact := range []string{"alpha-marker", `"sizes"`, "Summarize b.txt"} {
						if !strings.Contains(brief, fact) {
							t.Errorf("ranker input omitted %s: %s", fact, brief)
						}
					}
					return completion(tools.Result(map[string]any{"a.txt": "low", "b.txt": "high"}))
				case strings.HasPrefix(brief, "Quote"):
					return answer("alpha-marker")
				case req.Messages[len(req.Messages)-1].Role != messages.MessageRoleTool:
					return iterationTool("write", "write_file", tools.Result(map[string]any{"path": "b.txt", "content": "edited\n"}))
				}
				return answer("Rewrote b.txt.")
			}), 2, 6)
			root := r.config.Root
			for name, content := range map[string]string{"a.txt": "alpha-marker\n", "b.txt": "second\n"} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			localCommitGit(t, root, "init", "-q")
			localCommitGit(t, root, "add", ".")
			localCommitGit(t, root, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-qm", "base")
			for _, name := range []string{"read_file", "write_file", "bash"} {
				if _, err := r.config.Registry.LoadToolAuto(name); err != nil {
					t.Fatal(err)
				}
			}
			input := map[string]any{"files": tc.files}
			if tc.edit != "" {
				input["edit"] = tc.edit
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			report, err := r.RunWorkflow(ctx, source, input)
			state, stateErr := r.State(ctx)
			if stateErr != nil {
				t.Fatal(stateErr)
			}
			if len(state.Tasks) != tc.tasks {
				t.Fatalf("tasks = %d, want %d: %+v", len(state.Tasks), tc.tasks, state.Tasks)
			}
			want := "second\n"
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want %q, got report=%+v err=%v", tc.wantErr, report, err)
				}
			} else {
				if err != nil {
					t.Fatalf("tour example: %+v %v", report, err)
				}
				out := report.Output.(map[string]any)
				commit, _ := out["commit"].(string)
				ranking, _ := out["ranking"].(map[string]any)
				if len(commit) != 40 || len(ranking) != 2 || out["status"] != "done" || out["detail"] != "alpha-marker" {
					t.Fatalf("tour output: %#v", out)
				}
				integration, _ := out["integration"].(map[string]any)
				if tc.edit != "" {
					want = "edited\n"
					if integration["status"] != "applied" {
						t.Fatalf("edit not applied: %#v", out)
					}
				} else if out["integration"] != nil {
					t.Fatalf("read-only tour integrated: %#v", out)
				}
				for _, task := range state.Tasks {
					if task.Status != "done" {
						t.Fatalf("tour left undecided work: %+v", task)
					}
				}
			}
			data, err := os.ReadFile(filepath.Join(root, "b.txt"))
			if err != nil || string(data) != want {
				t.Fatalf("parent b.txt = %q, want %q: %v", data, want, err)
			}
		})
	}
}

func TestWorkflowHelpResearchExampleOutsideCheckout(t *testing.T) {
	t.Chdir(t.TempDir())
	source := workflowHelpExample(t, "parallel-research")
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

func TestWorkflowHelpFixVerifyExampleOutsideCheckout(t *testing.T) {
	skipIfWindows(t)
	t.Chdir(t.TempDir())
	source := workflowHelpExample(t, "fix-and-verify")
	for _, tc := range []struct {
		name    string
		fixable bool
		wantErr string
	}{
		{name: "repairs once then integrates", fixable: true},
		{name: "unresolved findings retain work", wantErr: "Findings or checks remain unresolved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reviews atomic.Int32
			r := runtimeTest(t, modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				var brief string
				for _, msg := range req.Messages {
					if msg.Role == messages.MessageRoleUser {
						brief = msg.Content
					}
				}
				switch {
				case strings.HasPrefix(brief, "Independently verify"):
					verdict := "not_fixed"
					if reviews.Add(1) > 1 && tc.fixable {
						verdict = "fixed"
					}
					return completion(tools.Result(map[string]any{"R1": map[string]any{"verdict": verdict, "reasoning": "checked exact candidate"}}))
				case req.Messages[len(req.Messages)-1].Role != messages.MessageRoleTool:
					content := "first fix\n"
					if strings.HasPrefix(brief, "Repair") {
						content = "fixed\n"
					}
					return iterationTool("write", "write_file", tools.Result(map[string]any{"path": "a.txt", "content": content}))
				}
				return completion(tools.Result(map[string]any{"R1": map[string]any{"status": "fixed", "what": "updated a.txt"}}))
			}), 2, 8)
			root := r.config.Root
			if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("base\n"), 0600); err != nil {
				t.Fatal(err)
			}
			localCommitGit(t, root, "init", "-q")
			localCommitGit(t, root, "add", ".")
			localCommitGit(t, root, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-qm", "base")
			for _, name := range []string{"read_file", "write_file", "bash"} {
				if _, err := r.config.Registry.LoadToolAuto(name); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			input := map[string]any{"source": root, "findings": []any{map[string]any{"id": "R1", "summary": "repair a"}}, "checks": []any{`test "$(cat a.txt)" = fixed`}}
			report, err := r.RunWorkflow(ctx, source, input)
			want := "fixed\n"
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("fix-verify example: %+v %v", report, err)
				}
				out := report.Output.(map[string]any)
				integration, _ := out["integration"].(map[string]any)
				reports, _ := out["reports"].([]any)
				if integration["status"] != "applied" || len(reports) != 2 || out["passed"] != true {
					t.Fatalf("fix-verify output: %#v", out)
				}
			} else {
				want = "base\n"
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || report.Status != "failed" {
					t.Fatalf("want %q, got report=%+v err=%v", tc.wantErr, report, err)
				}
				var failure *workflow.Error
				if !errors.As(err, &failure) || failure.Result.(map[string]any)["session"] == "" || len(failure.Result.(map[string]any)["rejected"].([]any)) != 1 {
					t.Fatalf("failure lost its structured summary: %v", err)
				}
			}
			if reviews.Load() != 2 {
				t.Fatalf("reviews = %d, want one rejection and one re-verification", reviews.Load())
			}
			data, err := os.ReadFile(filepath.Join(root, "a.txt"))
			if err != nil || string(data) != want {
				t.Fatalf("parent a.txt = %q, want %q: %v", data, want, err)
			}
			state, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(state.Members) != 3 || len(state.Executions) != 4 {
				t.Fatalf("repair did not continue the fixer's session: %d members %d executions", len(state.Members), len(state.Executions))
			}
			for _, task := range state.Tasks {
				switch {
				case tc.wantErr == "" && task.Status != "done":
					t.Fatalf("fix-verify left undecided work: %+v", task)
				case tc.wantErr != "" && task.Requirement == RequirementApplied && (task.Status != "awaiting_review" || task.Snapshot == ""):
					t.Fatalf("rejected editing work lost its retained snapshot: %+v", task)
				}
			}
		})
	}
}

func TestWorkflowHelpIntegrateExampleOutsideCheckout(t *testing.T) {
	skipIfWindows(t)
	t.Chdir(t.TempDir())
	source := workflowHelpExample(t, "integrate-results")
	r, base, _ := integrateFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a := submittedInput(t, r, base, map[string]string{"a.txt": "first\n"})
	b := submittedInput(t, r, base, map[string]string{"a.txt": "second\n"})
	var reviews atomic.Int32
	r.config.Client = modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		var brief string
		for _, msg := range req.Messages {
			if msg.Role == messages.MessageRoleUser {
				brief = msg.Content
			}
		}
		switch {
		case strings.HasPrefix(brief, "Independently review this combined"):
			if reviews.Add(1) == 1 {
				// The parent moves while the first validation runs, so the
				// first integrate attempt must refresh instead of applying.
				if err := os.WriteFile(filepath.Join(r.config.Root, "parent.txt"), []byte("parent drift\n"), 0600); err != nil {
					t.Error(err)
				}
			}
			return completion(tools.Result(map[string]any{"approved": true, "feedback": "both contributions present"}))
		case req.Messages[len(req.Messages)-1].Role != messages.MessageRoleTool:
			return iterationTool("repair", "write_file", tools.Result(map[string]any{"path": "a.txt", "content": "resolved\n"}))
		}
		return completion(tools.Result(map[string]any{"summary": "resolved a.txt"}))
	})
	for _, name := range []string{"write_file", "bash"} {
		if _, err := r.config.Registry.LoadToolAuto(name); err != nil {
			t.Fatal(err)
		}
	}
	input := map[string]any{"tasks": []any{map[string]any{"task": a.Task, "revision": a.Revision}, map[string]any{"task": b.Task, "revision": b.Revision}}, "checks": []any{`test "$(cat a.txt)" = resolved`}, "drift": "tree"}
	report, err := r.RunWorkflow(ctx, source, input)
	if err != nil {
		t.Fatalf("integrate example: %+v %v", report, err)
	}
	out := report.Output.(map[string]any)
	if out["status"] != "applied" || out["repairs"] != float64(1) || out["refreshes"] != float64(1) || reviews.Load() != 2 {
		t.Fatalf("integrate output: %#v reviews=%d", out, reviews.Load())
	}
	for file, want := range map[string]string{"a.txt": "resolved\n", "parent.txt": "parent drift\n"} {
		data, err := os.ReadFile(filepath.Join(r.config.Root, file))
		if err != nil || string(data) != want {
			t.Fatalf("parent %s = %q, want %q: %v", file, data, want, err)
		}
	}
	state, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range state.Tasks {
		if task.Status != "done" {
			t.Fatalf("integrate example left undecided work: %+v", task)
		}
	}
}
