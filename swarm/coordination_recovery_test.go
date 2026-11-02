package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/workflow"
)

func requireCoordinationError(t *testing.T, err error, code string) string {
	t.Helper()
	var detail *workflow.Error
	if !errors.As(err, &detail) || detail.Code != code {
		t.Fatalf("workflow error identity: %v", err)
	}
	text, ok := tools.FormatToolError(err)
	var model tools.ToolError
	if !ok || json.Unmarshal([]byte(text), &model) != nil || model.Code != code {
		t.Fatalf("structured model error: %s %v", text, err)
	}
	return model.Message
}

func TestFollowupDeliveryPendingRequiresNewCallAfterReceipt(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := runtimeTest(t, doneModel(), 1, 3)
	suspendAutoRelease(t, r)
	first, err := r.Agent(ctx, "", AgentRequest{TaskName: "inspection", Label: "Inspect", Task: "inspect", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"target": "/root/inspection", "message": "verify the result"}
	_, _, err = followupTool(t, r, "pending-result", args)
	message := requireCoordinationError(t, err, "delivery_pending")
	if !strings.Contains(message, "next input boundary") || !strings.Contains(message, "new call") || !strings.Contains(message, "did not start") {
		t.Fatalf("recovery is not actionable: %s", message)
	}
	s, _ := r.read(ctx)
	if len(s.Executions) != 1 || !deliveringTask(s, s.Tasks[first.Task]) {
		t.Fatal("refused followup bypassed the delivery gate")
	}
	if len(s.Followups) != 1 {
		t.Fatalf("refusal was not recorded: %+v", s.Followups)
	}
	for _, call := range s.Followups {
		if call.Phase != "failed" || call.ErrorCode != "delivery_pending" {
			t.Fatalf("wrong durable reason: %+v", call)
		}
	}
	admitParent(t, r)
	_, _, err = followupTool(t, r, "pending-result", args)
	requireCoordinationError(t, err, "delivery_pending")
	s, _ = r.read(ctx)
	if len(s.Executions) != 1 || s.Tasks[first.Task].Delivery == nil {
		t.Fatal("replayed refusal started work or lost the delivery receipt")
	}
	if _, _, err := followupTool(t, r, "after-result", args); err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, r, ctx)
	s, _ = r.read(ctx)
	if len(s.Executions) != 2 {
		t.Fatalf("new call did not start exactly once: %d executions", len(s.Executions))
	}
}

func TestTaskReadWorkerIdentityGivesDiscoveryWithoutMutation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := runtimeTest(t, nilModel(), 1, 1)
	first, err := r.Agent(ctx, "", AgentRequest{TaskName: "inspection", Label: "Inspect", Task: "inspect", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	r.RegisterParentTools(r.config.Registry)
	reader, _, _ := r.config.Registry.GetIfAllowed("swarm_read")
	before, _ := r.read(ctx)
	for _, name := range []string{"/root/inspection", "inspection", first.Session} {
		_, err := reader.Execute(ctx, map[string]any{"view": "tasks", "id": name, "section": "result"})
		message := requireCoordinationError(t, err, "task_id_required")
		if !strings.Contains(message, `list_agents({path_prefix:"/root/inspection",details:true})`) || !strings.Contains(message, `section:"result"`) {
			t.Fatalf("missing precise discovery action: %s", message)
		}
	}
	_, err = reader.Execute(ctx, map[string]any{"view": "tasks", "id": "not-a-task"})
	if message := requireCoordinationError(t, err, "unknown_task"); !strings.Contains(message, `swarm_read({view:"tasks"})`) {
		t.Fatalf("unknown ID lacks recovery: %s", message)
	}
	if _, err := reader.Execute(ctx, map[string]any{"view": "tasks", "id": first.Task, "section": "result"}); err != nil {
		t.Fatal(err)
	}
	after, _ := r.read(ctx)
	if after.Tasks[first.Task].Revision != before.Tasks[first.Task].Revision || after.Tasks[first.Task].Delivery != nil || len(after.Executions) != len(before.Executions) || len(after.Followups) != len(before.Followups) {
		t.Fatal("identity discovery mutated task state or acknowledged delivery")
	}
}
