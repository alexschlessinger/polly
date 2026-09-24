package swarm

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/workflow"
)

// A workflow may continue its own member after its typed result failed: the
// conversation and the result schema carry into a new execution with fresh
// corrections, one start is spent, and the blocked task completes.
func TestWorkflowContinuesItsOwnFailedTypedMember(t *testing.T) {
	for _, restated := range []bool{false, true} {
		name := "inherited schema"
		if restated {
			name = "restated schema"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			r := runtimeTest(t, modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				calls.Add(1)
				for _, m := range req.Messages {
					if m.Role == messages.MessageRoleUser && strings.HasPrefix(m.Content, "Retry:") {
						return completion(`{"answer":"verified"}`)
					}
				}
				return completion(`{"Answer":"verified"}`)
			}), 1, 4)
			suspendAutoRelease(t, r)
			again := `{session: refused.session, task: "Retry: " + refused.message}`
			if restated {
				again = `{session: refused.session, task: "Retry: " + refused.message, schema}`
			}
			source := `polly.defineWorkflow({name:"retry",inputSchema:polly.schema.object({}),async run(){
  const schema = polly.schema.object({answer: polly.schema.string()});
  let refused;
  try { return await polly.agent({label:"Researcher",task:"report",readOnly:true,schema}); }
  catch (error) { refused = error; }
  const again = await polly.agent(` + again + `);
  return {code: refused.code, reason: refused.message, session: refused.session, task: again.task, value: again.value};
}});`
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			report, err := r.RunWorkflow(ctx, source, map[string]any{})
			if err != nil {
				t.Fatalf("workflow: %v %+v", err, report)
			}
			output := report.Output.(map[string]any)
			reason := fmt.Sprint(output["reason"])
			if calls.Load() != 5 || output["code"] != "agent_failed" || !strings.Contains(reason, "after three corrections") || !strings.Contains(reason, `(did you mean "answer"?)`) {
				t.Fatalf("calls=%d output=%+v", calls.Load(), output)
			}
			s, _ := r.State(ctx)
			if len(s.Members) != 1 || len(s.Executions) != 2 || len(s.Tasks) != 1 {
				t.Fatalf("members=%d executions=%d tasks=%d", len(s.Members), len(s.Executions), len(s.Tasks))
			}
			var failed, retried *Execution
			for _, e := range s.Executions {
				switch e.Status {
				case "failed":
					failed = e
				case "completed":
					retried = e
				}
			}
			if failed == nil || retried == nil {
				t.Fatalf("executions: %+v", s.Executions)
			}
			if failed.ResultCorrections != 3 || failed.PendingResultCorrection != "" || retried.ResultCorrections != 0 || retried.Completion == nil ||
				retried.Workflow == "" || retried.Workflow != failed.Workflow || !reflect.DeepEqual(retried.Request.Schema, failed.Request.Schema) {
				t.Fatalf("failed=%+v retried=%+v", failed, retried)
			}
			member := s.Members[fmt.Sprint(output["session"])]
			if member == nil || member.Execution != retried.ID || member.Controller != "" || s.Runs[retried.Run].Starts != 2 {
				t.Fatalf("member=%+v starts=%+v", member, s.Runs)
			}
			task := s.Tasks[fmt.Sprint(output["task"])]
			want := map[string]any{"answer": "verified"}
			if task == nil || task.Status != "done" || task.Execution != retried.ID || !reflect.DeepEqual(task.Result, want) || !reflect.DeepEqual(output["value"], want) {
				t.Fatalf("task=%+v output=%+v", task, output)
			}
		})
	}
}

// The retry is the launching workflow's alone: another workflow, a paused
// member and a stopped member are refused, and the refusal starts nothing.
func TestWorkflowRetryRefusesOtherMembers(t *testing.T) {
	for _, kind := range []string{"same workflow", "other workflow", "paused", "stopped"} {
		t.Run(kind, func(t *testing.T) {
			var valid atomic.Bool
			r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
				if valid.Load() {
					return completion("true")
				}
				return completion(`"wrong"`)
			}), 1, 4)
			if kind == "paused" {
				r.UpdateDefaults(r.config.Request, llm.AgentConfig{MaxIterations: 1}, nil)
			}
			h := &workflowHost{runtime: r, controller: "wf"}
			defer h.close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, err := h.Call(ctx, workflow.Operation{Kind: "agent", Args: map[string]any{"label": "Test agent", "task": "check", "readOnly": true, "schema": boolResultSchema}})
			var refused *workflow.Error
			if !errors.As(err, &refused) || refused.Session == "" {
				t.Fatalf("first launch: %v", err)
			}
			wantCode := "agent_failed"
			if kind == "paused" {
				wantCode = "iteration_limit"
			}
			if refused.Code != wantCode {
				t.Fatalf("first launch code %q, want %q: %v", refused.Code, wantCode, err)
			}
			id := refused.Session
			valid.Store(true)
			host, want := h, ""
			switch kind {
			case "other workflow":
				host = &workflowHost{runtime: r, controller: "other"}
				defer host.close()
				want = "last execution failed"
			case "paused":
				want = "member is paused"
			case "stopped":
				if err := r.StopMember(ctx, id); err != nil {
					t.Fatal(err)
				}
				want = "member is stopped"
			}
			result, err := host.Call(ctx, workflow.Operation{Kind: "agent", Args: map[string]any{"session": id, "task": "Retry: fix the value"}})
			s, _ := r.State(ctx)
			if want != "" {
				if err == nil || !strings.Contains(err.Error(), want) || len(s.Executions) != 1 {
					t.Fatalf("retry: result=%+v err=%v executions=%d, want %q", result, err, len(s.Executions), want)
				}
				return
			}
			// The direct host delivers nothing, so the execution, not the task,
			// shows the retry finished.
			if err != nil || len(s.Executions) != 2 || result.(AgentResult).Value != true || s.Executions[result.(AgentResult).Execution].Status != "completed" {
				t.Fatalf("retry: result=%+v err=%v executions=%d", result, err, len(s.Executions))
			}
		})
	}
}
