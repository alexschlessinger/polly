package swarm

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestOrdinaryFollowupPreservesTypedCompletion(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"awaiting_review", "changes_requested", "done", "paused"} {
		for _, toolFree := range []bool{false, true} {
			t.Run(status+"/"+map[bool]string{false: "typed-tool", true: "tool-free"}[toolFree], func(t *testing.T) {
				var calls atomic.Int32
				entered := make(chan struct{})
				r := runtimeTest(t, modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
					n := calls.Add(1)
					if n == 1 && status == "paused" {
						close(entered)
						<-ctx.Done()
						return answer("interrupted")
					}
					if toolFree {
						if len(req.Tools) != 0 || req.ResponseSchema == nil {
							t.Error("tool-free follow-up lost its result schema")
						}
					} else {
						found := false
						for _, tool := range req.Tools {
							found = found || tool.GetName() == completionToolName
						}
						if !found || req.ResponseSchema != nil {
							t.Error("follow-up lost its completion tool")
							return answer("lost schema")
						}
					}
					value := "true"
					if n == 2 {
						value = `"invalid boolean"`
					} else if n > 2 {
						value = "false"
					}
					if toolFree {
						return answer(value)
					}
					return completion(value)
				}), 1, 3)
				suspendAutoRelease(t, r)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				req := AgentRequest{Label: "Typed", Task: "inspect", ReadOnly: true, Review: true, Schema: boolResultSchema}
				if toolFree {
					req.Tools = []string{}
				}
				first, err := r.start(ctx, "", req)
				if err != nil {
					t.Fatal(err)
				}
				if status == "paused" {
					select {
					case <-entered:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					if _, err := r.InterruptAgent(ctx, first.member); err != nil {
						t.Fatal(err)
					}
				}
				awaitIdle(t, r, ctx)
				before, err := r.read(ctx)
				if err != nil {
					t.Fatal(err)
				}
				original := before.Tasks[before.Members[first.member].Task]
				if status == "done" || status == "changes_requested" {
					if err := r.Review(ctx, original.ID, original.Revision, status == "done", "inspect again"); err != nil {
						t.Fatal(err)
					}
				}
				v, _, err := followupTool(t, r, "typed-followup", map[string]any{"target": first.member, "message": "inspect again"})
				if err != nil {
					t.Fatal(err)
				}
				awaitIdle(t, r, ctx)
				s, err := r.read(ctx)
				if err != nil {
					t.Fatal(err)
				}
				e, task := s.Executions[v.Execution], s.Tasks[v.Task]
				if calls.Load() != 3 || e.Status != "completed" || e.Completion == nil || e.Completion.Value != false || e.Result.Value != false || e.ResultCorrections != 1 {
					t.Fatalf("follow-up did not validate a fresh typed result: calls=%d execution=%+v", calls.Load(), e)
				}
				if task.Requirement != RequirementReviewed || task.Status != "awaiting_review" || task.AcceptedRevision != 0 || task.Result != false {
					t.Fatalf("follow-up changed completion or acceptance: %+v", task)
				}
				if (task.ID != original.ID) != (status == "done") || status == "done" && (task.Follows != original.ID || s.Tasks[original.ID].Result != true) {
					t.Fatalf("follow-up changed assignment lineage: %+v", task)
				}
				if (e.ID == first.id) != (status == "paused") || e.Request.MaxIterations != before.Executions[first.id].Request.MaxIterations {
					t.Fatalf("follow-up changed execution identity or allowance: %+v", e)
				}
				if status == "paused" && (len(s.Executions) != 1 || s.Runs[e.Run].Starts != 1 || e.Iterations != 3) {
					t.Fatalf("paused follow-up restarted its allowance: %+v", e)
				}
			})
		}
	}
}

func TestOrdinaryFollowupPreservesEmptyResultSchema(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		return completion("null")
	}), 1, 2)
	suspendAutoRelease(t, r)
	ctx := context.Background()
	first, err := r.Agent(ctx, "", AgentRequest{Label: "Typed", Task: "inspect", ReadOnly: true, Review: true, Schema: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := followupTool(t, r, "empty-schema", map[string]any{"target": first.Session, "message": "inspect again"})
	if err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, r, ctx)
	s, err := r.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e := s.Executions[v.Execution]
	if !reflect.DeepEqual(e.Request.Schema, map[string]any{}) || e.Status != "completed" || e.Completion == nil || e.Completion.Value != nil {
		t.Fatalf("empty result schema became untyped: %+v", e)
	}
}
