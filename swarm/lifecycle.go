package swarm

import (
	"context"
	"fmt"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

// Lifecycle is the one activity vocabulary shared by members and the parent.
// It is derived from the execution record and the member's control on every
// read; nothing persists it.
type Lifecycle string

const (
	LifecycleIdle    Lifecycle = "idle"    // no execution, or a completed one
	LifecycleActive  Lifecycle = "active"  // queued or running
	LifecycleWaiting Lifecycle = "waiting" // parked until an addressed event
	LifecyclePaused  Lifecycle = "paused"  // continues only on an explicit decision
)

// MemberControl is the host's standing instruction for a member. It is the
// only member activity that persists; execution status carries the rest.
type MemberControl string

const (
	MemberControlEnabled MemberControl = ""
	MemberControlStopped MemberControl = "stopped"
	MemberControlRetired MemberControl = "retired"
)

// AgentPresentation is what a view knows about an agent. Consumers decide on
// the typed fields; Display is for people. Every field is a scalar so a view
// can compare two presentations to detect a change.
type AgentPresentation struct {
	Lifecycle Lifecycle `json:"lifecycle"`
	// Busy is true for active and waiting agents.
	Busy bool `json:"busy"`
	// Outcome is the raw execution status, or "" without an execution.
	Outcome       string        `json:"outcome,omitempty"`
	Control       MemberControl `json:"control,omitempty"`
	StopReason    string        `json:"stopReason,omitempty"`
	Iterations    int           `json:"iterations,omitempty"`
	MaxIterations int           `json:"maxIterations,omitempty"`
	TaskStatus    string        `json:"taskStatus,omitempty"`
	Deferred      bool          `json:"deferred"`
	// Attention marks open work that no execution is advancing.
	Attention bool   `json:"attention"`
	Workflow  string `json:"workflow,omitempty"`
	Detail    string `json:"detail,omitempty"`
	// Display is "<lifecycle>[ · <detail>][ · deferred]".
	Display string `json:"display"`
}

// DisplayLabel composes the label grammar shared by every agent view.
func DisplayLabel(lifecycle Lifecycle, detail string, deferred bool) string {
	label := string(lifecycle)
	if detail != "" {
		label += " · " + detail
	}
	if deferred {
		label += " · deferred"
	}
	return label
}

// ReadStateView observes a root's coordination state without constructing a
// runtime, repairing records, claiming a lease or reading private transcripts.
func ReadStateView(ctx context.Context, store sessions.CoordinationViewStore, rootID string) (*State, error) {
	return new(StateCache).ReadView(ctx, store, rootID)
}

// ReadView is ReadStateView for a display that polls: an unchanged root
// returns the previous decode, which the caller must treat as read-only.
func (c *StateCache) ReadView(ctx context.Context, store sessions.CoordinationViewStore, rootID string) (*State, error) {
	raw, err := store.ReadCoordinationView(ctx, rootID)
	if err != nil {
		return nil, err
	}
	return c.decode(raw)
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

// MemberState derives a member's presentation. Reading never repairs records,
// accepts tasks or claims an execution.
func MemberState(s *State, m *Member) AgentPresentation {
	p := AgentPresentation{}
	if m == nil {
		return p
	}
	e := s.Executions[m.Execution]
	if e != nil {
		p.Outcome, p.Workflow, p.StopReason = e.Status, ExecutionWorkflow(s, e), string(e.StopReason)
		p.Iterations, p.MaxIterations = e.Iterations, e.Request.MaxIterations
	}
	p.Control = m.Control
	open := false
	if t := s.Tasks[m.Task]; t != nil {
		p.TaskStatus, p.Deferred = TaskStatus(t), TaskDeferred(s, t)
		open = t.Status != "done" && t.Status != "canceled"
	}
	switch {
	case p.Control == MemberControlRetired:
		p.Lifecycle, p.Detail = LifecycleIdle, "retired"
	case p.Control == MemberControlStopped:
		p.Lifecycle, p.Detail = LifecyclePaused, "stopped"
	case e == nil || e.Status == "completed":
		p.Lifecycle, p.Detail = LifecycleIdle, p.TaskStatus
	case e.Status == "queued":
		p.Lifecycle, p.Detail = LifecycleActive, "queued"
	case e.Status == "running":
		p.Lifecycle = LifecycleActive
	case e.Status == "waiting":
		p.Lifecycle = LifecycleWaiting
	case e.Status == "failed":
		p.Lifecycle, p.Detail = LifecyclePaused, "failed"
	case e.StopReason == messages.StopReasonMaxIterations:
		p.Lifecycle, p.Detail = LifecyclePaused, fmt.Sprintf("iteration limit (%d/%d)", e.Iterations, e.Request.MaxIterations)
	default:
		p.Lifecycle, p.Detail = LifecyclePaused, "interrupted"
	}
	p.Busy = p.Lifecycle == LifecycleActive || p.Lifecycle == LifecycleWaiting
	p.Attention = !p.Deferred && !p.Busy && open
	// A control hides the execution, not the work the member leaves behind.
	if p.Control != MemberControlEnabled && p.Attention && p.TaskStatus != "" {
		p.Detail += " · " + p.TaskStatus
	}
	p.Display = DisplayLabel(p.Lifecycle, p.Detail, p.Deferred)
	return p
}
