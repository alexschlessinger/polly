package swarm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/worktree"
)

func TestFailedCoordinationMutationsDoNotReportSuccess(t *testing.T) {
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), 1, 1)
	r.RegisterParentTools(r.config.Registry)
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"swarm_control", map[string]any{"action": "cleanup", "id": "execution-not-context"}},
		{"swarm_control", map[string]any{"action": "stop", "id": "missing"}},
		{"swarm_control", map[string]any{"action": "cancel_task", "id": "missing"}},
		{"swarm_review", map[string]any{"task": "missing", "revision": 1, "accept": true}},
		{"swarm_update_task", map[string]any{"task": "missing", "revision": 1}},
		{"swarm_claim", map[string]any{"task": "missing", "revision": 1}},
		{"swarm_submit", map[string]any{"task": "missing", "revision": 1}},
		{"swarm_block", map[string]any{"task": "missing", "revision": 1}},
		{"workflow_cancel", map[string]any{"id": "missing"}},
		{"workflow_acknowledge", map[string]any{"id": "missing"}},
	} {
		t.Run(tc.name+":"+stringValue(tc.args["action"]), func(t *testing.T) {
			tool, _, _ := r.config.Registry.GetIfAllowed(tc.name)
			out, err := tool.Execute(context.Background(), tc.args)
			if err == nil || out != "" {
				t.Fatalf("failed mutation reported output %q, error %v", out, err)
			}
			if tc.args["action"] == "cleanup" && (!strings.Contains(err.Error(), "execution-not-context") || !strings.Contains(err.Error(), "list_agents")) {
				t.Fatalf("cleanup error lacks the requested ID and recovery guidance: %v", err)
			}
		})
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func TestReviewToolReportsRemainingIntegration(t *testing.T) {
	ctx := context.Background()
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), 1, 1)
	if err := r.update(ctx, func(s *State) error {
		base := worktree.Snapshot{ID: "base", Tree: "base-tree", Commit: "base-commit", Source: "parent"}
		s.Tasks["changed"] = &Task{ID: "changed", Owner: "editor", Status: "awaiting_review", Revision: 2, Snapshot: "candidate", StartingSnapshot: base.ID}
		s.Members["editor"] = &Member{ID: "editor", Context: "copy"}
		s.Contexts["copy"] = &ExecutionContext{ID: "copy", Owner: "editor", Root: "child", Checkout: &worktree.Checkout{Path: "child", Base: base}}
		s.Snapshots[base.ID] = &base
		s.Snapshots["candidate"] = &worktree.Snapshot{ID: "candidate", Tree: "changed-tree", Commit: "changed-commit", Source: "child"}
		s.Tasks["research"] = &Task{ID: "research", Status: "awaiting_review", Revision: 3}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.RegisterParentTools(r.config.Registry)
	review, _, _ := r.config.Registry.GetIfAllowed("swarm_review")
	for _, tc := range []struct {
		id, status, display string
		revision            int
	}{
		{"changed", "awaiting_review", "integration pending", 2},
		{"research", "done", "done", 3},
	} {
		out, err := review.Execute(ctx, map[string]any{"task": tc.id, "revision": tc.revision, "accept": true})
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatal(err)
		}
		if result["status"] != tc.status || result["displayStatus"] != tc.display || result["acceptedRevision"] != float64(tc.revision) {
			t.Fatalf("misleading review result: %s", out)
		}
		if tc.id == "changed" && !strings.Contains(stringValue(result["nextAction"]), "swarm_integration") {
			t.Fatalf("missing integration recovery: %s", out)
		}
		if tc.id == "research" && result["nextAction"] != nil {
			t.Fatalf("completed review still asks for work: %s", out)
		}
	}
	list, _, _ := r.config.Registry.GetIfAllowed("swarm_tasks")
	out, err := list.Execute(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Items []taskToolView `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		t.Fatal(err)
	}
	tasks := map[string]taskToolView{}
	for _, task := range page.Items {
		tasks[task.ID] = task
	}
	if tasks["changed"].Status != "awaiting_review" || tasks["changed"].DisplayStatus != "integration pending" || tasks["research"].DisplayStatus != "done" {
		t.Fatalf("task list lost machine or display status: %s", out)
	}
}

func TestReviewToolGuidanceForRetiredOrMissingProvenance(t *testing.T) {
	ctx := context.Background()
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), 1, 1)
	if err := r.update(ctx, func(s *State) error {
		s.Members["retired"] = &Member{ID: "retired", Status: "retired", Context: "removed"}
		s.Tasks["task"] = &Task{ID: "task", Owner: "retired", Status: "awaiting_review", Revision: 2, Snapshot: "missing"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.RegisterParentTools(r.config.Registry)
	review, _, _ := r.config.Registry.GetIfAllowed("swarm_review")
	out, err := review.Execute(ctx, map[string]any{"task": "task", "revision": 2, "accept": true})
	if err != nil || !strings.Contains(out, "restore the original task snapshots") {
		t.Fatalf("missing provenance guidance: %q %v", out, err)
	}
	out, err = review.Execute(ctx, map[string]any{"task": "task", "revision": 2, "accept": false, "feedback": "revise the summary"})
	if err != nil || !strings.Contains(out, "swarm_update_task") || strings.Contains(out, "Wait for") {
		t.Fatalf("retired member cannot revise: %q %v", out, err)
	}
}

func TestCleanupToolReportsRetirementAfterSuccess(t *testing.T) {
	ctx := context.Background()
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), 1, 1)
	result, err := r.Spawn(ctx, subagent.Request{Task: "research", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	r.RegisterParentTools(r.config.Registry)
	state, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	control, _, _ := r.config.Registry.GetIfAllowed("swarm_control")
	out, err := control.Execute(ctx, map[string]any{"action": "cleanup", "id": state.Members[result.Session].Context})
	if err != nil || out != `"retired"` {
		t.Fatalf("cleanup: %q %v", out, err)
	}
	state, err = r.State(ctx)
	if err != nil || state.Members[result.Session].Status != "retired" {
		t.Fatalf("success did not retire member: %+v %v", state, err)
	}
}

func TestFailedWorkflowToolRetainsItsReport(t *testing.T) {
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), 1, 1)
	r.RegisterParentTools(r.config.Registry)
	tool, _, _ := r.config.Registry.GetIfAllowed("workflow_run")
	out, err := tool.Execute(context.Background(), map[string]any{
		"source": `polly.defineWorkflow({name:"failure",inputSchema:polly.schema.object({}),async run(){throw new Error("inspect this report")}})`,
		"input":  "{}",
	})
	if err == nil || !strings.Contains(out, `"status": "failed"`) || !strings.Contains(out, `"id":`) {
		t.Fatalf("failed workflow lost its inspectable report: %q %v", out, err)
	}
}
