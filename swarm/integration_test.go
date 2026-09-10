package swarm

import (
	"bytes"
	"context"
	"errors"
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
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/workflow"
)

func skipIfWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
}

func TestWorkflowExampleFixRepairReviewAndParentApply(t *testing.T) {
	skipIfWindows(t)
	var reviews atomic.Int32
	model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		var brief string
		for _, msg := range req.Messages {
			if msg.Role == messages.MessageRoleUser && !strings.HasPrefix(msg.Content, "<peer_messages>") {
				brief = msg.Content
			}
		}
		reviewing := strings.HasPrefix(brief, "Independently")
		if !reviewing && req.Messages[len(req.Messages)-1].Role != messages.MessageRoleTool {
			content := "first fix\n"
			if strings.HasPrefix(brief, "Repair") {
				content = "repaired\n"
			}
			return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "write", Name: "write_file", Arguments: tools.Result(map[string]any{"path": "a.txt", "content": content})}}}
		}
		if reviewing {
			verdict := "not_fixed"
			if reviews.Add(1) > 1 {
				verdict = "fixed"
			}
			return completion(tools.Result(map[string]any{"R1": map[string]any{"verdict": verdict, "reasoning": "checked exact candidate", "requiredChange": "repair once"}}))
		}
		return completion(`{"R1":{"status":"fixed","what":"updated a.txt"}}`)
	})
	r := runtimeTest(t, model, 2, 8)
	root := r.config.Root
	git := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
		return out
	}
	git("init", "-q")
	git("config", "user.name", "test")
	git("config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "base")
	if err := os.WriteFile(filepath.Join(root, "parent.txt"), []byte("parent draft\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "parent.txt")
	beforeIndex, _ := os.ReadFile(filepath.Join(root, ".git", "index"))
	head := git("rev-parse", "HEAD")
	for _, name := range []string{"bash", "write_file", "read_file"} {
		if _, err := r.config.Registry.LoadToolAuto(name); err != nil {
			t.Fatal(err)
		}
	}
	source, err := os.ReadFile("../examples/workflows/fix-review-findings.js")
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"groups": []any{map[string]any{"label": "fix", "source": root, "evidence": "review.md", "findings": []any{map[string]any{"id": "R1", "summary": "repair a"}}, "checks": []any{"test -s a.txt"}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := r.RunWorkflow(ctx, string(source), input)
	if err != nil {
		t.Fatalf("workflow: %v %+v", err, report)
	}
	if reviews.Load() != 2 {
		t.Fatalf("reviews=%d", reviews.Load())
	}
	rows := report.Output.([]any)
	summary := rows[0].(map[string]any)["value"].(map[string]any)
	if summary["repairAttempted"] != true {
		t.Fatal(summary)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Members) != 3 || len(s.Executions) != 4 {
		t.Fatalf("repair did not reuse one member: %d members %d executions", len(s.Members), len(s.Executions))
	}
	for _, task := range s.Tasks {
		if owner := s.Members[task.Owner]; owner != nil && !owner.ReadOnly {
			if err := r.Submit(ctx, owner.ID, task.ID, task.Revision, "omit candidate", ""); err == nil {
				t.Fatal("editing result erased its integration candidate")
			}
		}
		if task.Status == "done" {
			continue
		}
		if err := r.Review(ctx, task.ID, task.Revision, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	taskID := summary["task"].(string)
	candidate, err := r.PrepareIntegration(ctx, []TaskReference{{Task: taskID, Revision: s.Tasks[taskID].Revision}}, "paths")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.AcceptIntegration(ctx, candidate.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = r.ApplyIntegration(ctx, candidate.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(got) != "repaired\n" {
		t.Fatalf("result: %q", got)
	}
	afterIndex, _ := os.ReadFile(filepath.Join(root, ".git", "index"))
	if !bytes.Equal(beforeIndex, afterIndex) || !bytes.Equal(head, git("rev-parse", "HEAD")) {
		t.Fatal("parent index or HEAD changed")
	}
	if err := r.Settle(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Cleanup(ctx, ""); err != nil {
		t.Fatal(err)
	}
	refs := git("for-each-ref", "--format=%(refname)", "refs/polly/snapshots/")
	if len(refs) == 0 {
		t.Fatal("workspace cleanup deleted follow-up snapshots")
	}
	if err := r.Forget(ctx); err != nil {
		t.Fatal(err)
	}
	refs = git("for-each-ref", "--format=%(refname)", "refs/polly/snapshots/")
	if len(refs) != 0 {
		t.Fatalf("snapshot refs leaked: %s", refs)
	}
}

func TestWorkflowExecApprovalTimeoutAndOrdinaryExit(t *testing.T) {
	skipIfWindows(t)
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("unused") }), 1, 1)
	if _, err := r.config.Registry.LoadToolAuto("bash"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	source := `polly.defineWorkflow({name:"checks",inputSchema:polly.schema.object({}),async run(){const context=await polly.context({readOnly:true});return polly.scope({context}, w=>w.exec("exit 7",{check:false}));}});`
	r.config.Callbacks = func(context.Context, Member) *llm.AgentCallbacks {
		return &llm.AgentCallbacks{ApproveToolCalls: func([]messages.ChatMessageToolCall) []bool { return []bool{false} }}
	}
	_, err := r.RunWorkflow(ctx, source, map[string]any{})
	var failure *workflow.Error
	if !errors.As(err, &failure) || failure.Code != "tool_denied" {
		t.Fatalf("approval bypassed: %v", err)
	}
	r.config.Callbacks = nil
	report, err := r.RunWorkflow(ctx, source, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Output.(map[string]any)["exitCode"] != float64(7) {
		t.Fatal(report.Output)
	}
	r.config.Agent.ToolTimeout = 10 * time.Millisecond
	r.UpdateDefaults(r.config.Request, r.config.Agent, r.config.Instructions)
	_, err = r.RunWorkflow(ctx, strings.Replace(source, "exit 7", "sleep 5", 1), map[string]any{})
	if !errors.As(err, &failure) || failure.Code != "timeout" {
		t.Fatalf("timeout recovered as command exit: %v", err)
	}
}
