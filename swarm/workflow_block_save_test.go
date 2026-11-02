package swarm

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/workflow"
)

// Hold the first completion checkpoint until a sibling blocks the workflow.
// Return the context error a recorder observes, rather than its typed cause.
// The transaction is released before waiting so the blocker can commit.
type blockDuringProgressSave struct {
	sessions.CoordinationSession
	saving chan struct{}
	once   atomic.Bool
}

func (s *blockDuringProgressSave) UpdateCoordination(ctx context.Context, fn func(*sessions.CoordinationState) error) error {
	progress := false
	err := s.CoordinationSession.UpdateCoordination(ctx, func(raw *sessions.CoordinationState) error {
		before, err := decodeState(raw)
		if err != nil {
			return err
		}
		if err := fn(raw); err != nil {
			return err
		}
		state, err := decodeState(raw)
		if err != nil {
			return err
		}
		for id, report := range state.Workflows {
			if report.Status == "running" {
				previous := before.Workflows[id]
				for i, step := range report.Steps {
					if step.Status == "completed" && (previous == nil || i >= len(previous.Steps) || previous.Steps[i].Status != "completed") {
						progress = true
					}
				}
			}
		}
		return nil
	})
	if err == nil && progress && !s.once.Swap(true) {
		close(s.saving)
		<-ctx.Done()
		return ctx.Err()
	}
	return err
}

func TestWorkflowBlockDuringProgressSave(t *testing.T) {
	t.Parallel()
	var r *Runtime
	saving := make(chan struct{})
	var requested atomic.Bool
	model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		for _, msg := range req.Messages {
			if msg.Role != messages.MessageRoleUser || !strings.Contains(msg.Content, "blocker assignment") {
				continue
			}
			if requested.Swap(true) {
				<-ctx.Done()
				return answer("canceled")
			}
			select {
			case <-saving:
			case <-ctx.Done():
				return answer("canceled")
			}
			s, err := r.State(ctx)
			if err != nil {
				t.Error(err)
				return answer("state unavailable")
			}
			for _, m := range s.Members {
				if m.Label == "blocker" {
					task := s.Tasks[m.Task]
					return iterationTool("block", "swarm_block", tools.Result(map[string]any{"task": task.ID, "revision": task.Revision, "reason": "missing prerequisite"}))
				}
			}
			t.Error("blocker member not found")
		}
		return answer("completed sibling")
	})
	r = runtimeTest(t, model, 2, 2)
	r.parent = &blockDuringProgressSave{CoordinationSession: r.parent, saving: saving}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	report, err := r.RunWorkflow(ctx, `polly.workflow("blocked checkpoint",polly.schema.obj({}),async()=>{
 try { return await polly.parallel(["sibling","blocker"],label=>polly.research(label,label+" assignment"),{errors:"throw_after_all"}); }
 catch (error) { return "swallowed"; }
});`, map[string]any{})
	var failure *workflow.Error
	if !errors.As(err, &failure) || failure.Code != "workflow_blocked" || report == nil || report.Error == nil || report.Status != "interrupted" || report.Output != nil {
		t.Fatalf("blocker cause lost: report=%+v err=%v", report, err)
	}
	if r.HasActive() {
		t.Fatal("workflow returned before workers settled")
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range []*workflow.Error{failure, report.Error, s.Workflows[report.ID].Error} {
		if got == nil || got.Code != "workflow_blocked" || got.Session == "" {
			t.Fatalf("blocker identity lost: %+v", got)
		}
		member := s.Members[got.Session]
		if member == nil || member.Label != "blocker" {
			t.Fatalf("wrong blocker session: %+v", got)
		}
		task := s.Tasks[member.Task]
		detail, ok := got.Result.(map[string]any)
		if !ok || detail["task"] != task.ID || detail["execution"] != task.Execution || (detail["revision"] != task.Revision && detail["revision"] != float64(task.Revision)) || detail["reason"] != "missing prerequisite" || task.Status != "blocked" {
			t.Fatalf("blocker provenance lost: error=%+v task=%+v", got, task)
		}
	}
	if len(report.Steps) != 2 || report.Steps[0].Status != "completed" || report.Steps[0].Value == nil {
		t.Fatalf("sibling completion lost: %+v", report.Steps)
	}
	if saved := s.Workflows[report.ID]; saved.Status != "interrupted" || saved.Steps[0].Status != "completed" {
		t.Fatalf("terminal checkpoint lost blocker or sibling completion: %+v", saved)
	}
}
