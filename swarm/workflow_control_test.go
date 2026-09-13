package swarm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestWorkflowRunForegroundAndInvalidInput(t *testing.T) {
	r := runtimeTest(t, nilModel(), 1, 2)
	source := `polly.defineWorkflow({name:"echo",inputSchema:polly.schema.object({text:polly.schema.string()}),async run(input){return input.text}})`
	out, err := execParentTool(t, r, "workflow_run", map[string]any{"source": source, "input": `{"text":"foreground"}`})
	if err != nil || !strings.Contains(out, `"output": "foreground"`) || !strings.Contains(out, `"status": "completed"`) {
		t.Fatalf("foreground launch = %s, %v", out, err)
	}
	for _, background := range []bool{false, true} {
		if _, err := execParentTool(t, r, "workflow_run", map[string]any{"source": source, "input": "{", "background": background}); err == nil {
			t.Fatal("invalid JSON input started a workflow")
		}
	}
	s, _ := r.State(context.Background())
	if len(s.Workflows) != 1 {
		t.Fatal("invalid input persisted a workflow")
	}
}

func TestWorkflowControlCancellationWakesParentAndDefersUnresolvedWork(t *testing.T) {
	r := runtimeTest(t, modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		<-ctx.Done()
		return answer("interrupted work must not complete")
	}), 1, 2)
	r.RegisterParentTools(r.config.Registry)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	launchCtx, cancelLaunch := context.WithCancel(ctx)
	runner, _, _ := r.config.Registry.GetIfAllowed("workflow_run")
	out, err := runner.Execute(launchCtx, map[string]any{
		"source": `polly.defineWorkflow({name:"background",inputSchema:polly.schema.object({}),async run(){return await polly.agent({label:"Wait for input",task:"Wait for input",readOnly:true})}})`,
		"input":  "{}", "background": true,
	})
	cancelLaunch()
	if err != nil {
		t.Fatal(err)
	}
	var started struct{ ID, Status string }
	if err := json.Unmarshal([]byte(out), &started); err != nil || started.ID == "" || started.Status != "started" {
		t.Fatalf("background result = %s, %v", out, err)
	}
	awaitState(t, r, ctx, func(s *State) bool { return runningExecutions(s) == 1 })
	wait, _, _ := r.config.Registry.GetIfAllowed("wait_agent")
	woke := make(chan error, 1)
	go func() { _, err := wait.Execute(ctx, nil); woke <- err }()
	control, _, _ := r.config.Registry.GetIfAllowed("swarm_control")
	if _, err := control.Execute(ctx, map[string]any{"action": "cancel_workflow", "id": started.ID}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-woke:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("cancellation never woke the parent")
	}
	s := waitWorkflowIdle(t, r)
	if s.Workflows[started.ID].Status == "completed" || s.Workflows[started.ID].Status == "running" {
		t.Fatal("canceled workflow completed")
	}
	for _, task := range s.Tasks {
		if task.Status == "done" || task.Status == "awaiting_review" || task.Delivery != nil {
			t.Fatalf("canceled work was submitted or delivered: %+v", task)
		}
	}
	if _, err := control.Execute(ctx, map[string]any{"action": "acknowledge_workflow", "id": started.ID, "defer": true, "note": " "}); err == nil {
		t.Fatal("blank deferral note accepted")
	}
	if _, err := control.Execute(ctx, map[string]any{"action": "acknowledge_workflow", "id": started.ID, "defer": true, "note": "Reported interruption; retain the unfinished assignment"}); err != nil {
		t.Fatal(err)
	}
	s, _ = r.State(ctx)
	if !s.Workflows[started.ID].Acknowledged {
		t.Fatal("deferral did not acknowledge terminal report")
	}
	for _, task := range s.Tasks {
		if !TaskDeferred(s, task) || task.Status == "done" || task.AcceptedRevision != 0 {
			t.Fatalf("deferral accepted or lost work: %+v", task)
		}
	}
}
