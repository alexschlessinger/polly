package swarm

import (
	"context"
	"fmt"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

// ReadStateView observes a root's coordination state without constructing a
// runtime, repairing records, claiming a lease or reading private transcripts.
func ReadStateView(ctx context.Context, store sessions.CoordinationViewStore, rootID string) (*State, error) {
	raw, err := store.ReadCoordinationView(ctx, rootID)
	if err != nil {
		return nil, err
	}
	return decodeState(raw)
}

// MemberPresentation separates execution outcome from review and deferral.
// It is derived only; reading old records never repairs or accepts tasks.
type MemberPresentation struct {
	Outcome    string `json:"outcome"`
	TaskStatus string `json:"taskStatus,omitempty"`
	Deferred   bool   `json:"deferred"`
	Active     bool   `json:"active"`
	Attention  bool   `json:"attention"`
	Workflow   string `json:"workflow,omitempty"`
	Display    string `json:"display"`
}

// ExecutionWorkflow also supports pre-binding records for display only.
// Deferral authority always requires the host-authored Execution.Workflow.
func ExecutionWorkflow(s *State, e *Execution) string {
	if e == nil {
		return ""
	}
	if e.Workflow != "" {
		return e.Workflow
	}
	if m := s.Members[e.Member]; m != nil && m.Execution == e.ID && m.Controller != "" {
		return m.Controller
	}
	if e.Request.CallID != "" {
		for id, w := range s.Workflows {
			for _, step := range w.Steps {
				if step.Kind == "agent" && step.ID == e.Request.CallID {
					return id
				}
			}
		}
	}
	return ""
}

func MemberState(s *State, m *Member) MemberPresentation {
	p := MemberPresentation{}
	if m == nil {
		return p
	}
	p.Outcome = m.Status
	e := s.Executions[m.Execution]
	if e != nil {
		p.Outcome, p.Workflow = e.Status, ExecutionWorkflow(s, e)
		// Waiting is a live parked invocation, whose persisted execution may
		// still be running. Resource retirement/stopping remain explicit.
		if m.Status == "waiting" && e.Status == "running" {
			p.Outcome = "waiting"
		}
	}
	if m.Status == "retired" || m.Status == "stopped" {
		p.Outcome = m.Status
	}
	t := s.Tasks[m.Task]
	if t != nil {
		p.TaskStatus, p.Deferred = TaskStatus(t), TaskDeferred(s, t)
	}
	p.Active = p.Outcome == "running" || p.Outcome == "queued" || p.Outcome == "waiting"
	p.Attention = !p.Deferred && !p.Active && t != nil && t.Status != "done" && t.Status != "canceled"
	p.Display = p.Outcome
	if p.Outcome == "completed" || p.Outcome == "idle" {
		if p.TaskStatus != "" {
			p.Display = p.TaskStatus
		}
	} else if p.Outcome == "retired" && p.Attention {
		p.Display += " · " + p.TaskStatus
	}
	if p.Outcome == "paused" && e != nil && e.StopReason == messages.StopReasonMaxIterations {
		p.Display = fmt.Sprintf("paused · iteration limit reached (%d/%d)", e.Iterations, e.Request.MaxIterations)
	}
	if p.Deferred {
		p.Display += " · deferred"
	}
	return p
}
