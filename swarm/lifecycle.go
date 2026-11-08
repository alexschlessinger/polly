package swarm

import (
	"context"

	"github.com/alexschlessinger/pollytool/internal/coordsem"
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
	RetryPause    RetryPause    `json:"retryPause,omitzero"`
	TaskStatus    string        `json:"taskStatus,omitempty"`
	Deferred      bool          `json:"deferred"`
	Delivering    bool          `json:"delivering,omitempty"`
	// Attention marks open work that no execution is advancing.
	Attention bool   `json:"attention"`
	Workflow  string `json:"workflow,omitempty"`
	Detail    string `json:"detail,omitempty"`
	// Display is "<lifecycle>[ · <detail>][ · deferred]".
	Display string `json:"display"`
}

// DisplayLabel composes the label grammar shared by every agent view.
func DisplayLabel(lifecycle Lifecycle, detail string, deferred bool) string {
	return coordsem.Display(string(lifecycle), detail, deferred)
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

// workflowControlled reports whether a running workflow still owns an
// execution's progress. The parent hears about such work once, when the
// workflow reaches a terminal status; afterwards the member is an ordinary
// member again and its transitions and outcomes reach the parent directly.
// Only the host-authored Execution.Workflow counts, as for deferral.
func workflowControlled(s *State, e *Execution) bool {
	if e == nil || e.Workflow == "" {
		return false
	}
	w := s.Workflows[e.Workflow]
	return w != nil && w.Status == "running"
}

// memberControlled extends workflowControlled to the member record: a member
// reserved by a running workflow is controlled from the moment it is created,
// including the window before its first execution is recorded, so a parked
// parent does not wake on the workflow's own launches.
func memberControlled(s *State, m *Member) bool {
	if m == nil {
		return false
	}
	if workflowControlled(s, s.Executions[m.Execution]) {
		return true
	}
	if m.Controller == "" {
		return false
	}
	w := s.Workflows[m.Controller]
	return w != nil && w.Status == "running"
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
		if e.RetryPause != nil {
			p.RetryPause = *e.RetryPause
		}
	}
	p.Control = m.Control
	open := false
	if t := s.Tasks[m.Task]; t != nil {
		p.TaskStatus, p.Deferred = TaskStatusIn(s, t), TaskDeferred(s, t)
		p.Delivering = deliveringTask(s, t) && !p.Deferred
		open = t.Status != "done" && t.Status != "canceled"
	}
	f := coordsem.DescribeMember(string(m.Control), semanticExecution(e), p.TaskStatus, open, p.Deferred, p.Delivering)
	p.Lifecycle, p.Detail, p.Display = Lifecycle(f.Lifecycle), f.Detail, f.Display
	p.Busy, p.Attention = f.Busy, f.Attention
	return p
}

// memberActivity is shared with the first-assignment roster. Its lifecycle
// depends only on the member's control and current execution, not task history.
func memberActivity(m *Member, e *Execution, taskStatus string) (Lifecycle, string) {
	lifecycle, detail := coordsem.Activity(string(m.Control), semanticExecution(e), taskStatus)
	return Lifecycle(lifecycle), detail
}
