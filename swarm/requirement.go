package swarm

import "time"

// CreateTaskOptions selects the obligation once, when the task is created.
type CreateTaskOptions struct {
	Review      bool
	Requirement string
}

func creationRequirement(options CreateTaskOptions, owner *Member) (string, error) {
	requirement := options.Requirement
	if requirement == "" {
		if options.Review {
			requirement = RequirementReviewed
		} else if owner != nil {
			requirement, _ = requirementFor(false, owner.ReadOnly)
		} else {
			requirement = RequirementDelivered
		}
	}
	if requirement != RequirementDelivered && requirement != RequirementReviewed && requirement != RequirementApplied {
		return "", fail("invalid_args", "unknown task completion requirement")
	}
	if options.Review && requirement != RequirementReviewed {
		return "", fail("invalid_args", "review conflicts with the task requirement")
	}
	if owner != nil {
		if err := validateRequirement(&Task{Requirement: requirement}, owner.ReadOnly); err != nil {
			return "", err
		}
	}
	return requirement, nil
}

const (
	RequirementDelivered = "delivered"
	RequirementReviewed  = "reviewed"
	RequirementApplied   = "applied"

	WorkspaceReleasing = "releasing"
	WorkspaceRetained  = "retained"
)

// TaskDelivery identifies the exact result durably returned to a parent or workflow.
type TaskDelivery struct {
	Via       string    `json:"via"`
	Ref       string    `json:"ref"`
	Revision  int       `json:"revision"`
	Execution string    `json:"execution"`
	Inline    bool      `json:"inline"`
	At        time.Time `json:"at"`
}

func requirementFor(review, readOnly bool) (string, error) {
	if review && !readOnly {
		return "", fail("invalid_args", "editing work is reviewed by integrating it; review requires read-only work")
	}
	if review {
		return RequirementReviewed, nil
	}
	if readOnly {
		return RequirementDelivered, nil
	}
	return RequirementApplied, nil
}

// Empty requirements exist only in hand-built fixtures; persisted tasks set one
// when created. Keep those fixtures' original review and integration obligations.
func requirementOf(s *State, t *Task) string {
	if t == nil {
		return ""
	}
	if t.Requirement != "" {
		return t.Requirement
	}
	if m := s.Members[t.Owner]; m != nil && !m.ReadOnly {
		return RequirementApplied
	}
	return RequirementReviewed
}

// Assignment validates a task's obligation without changing it.
func validateRequirement(t *Task, readOnly bool) error {
	if t.Requirement == "" { // fixture compatibility, never a creation default
		return nil
	}
	switch t.Requirement {
	case RequirementDelivered, RequirementReviewed:
		if readOnly {
			return nil
		}
	case RequirementApplied:
		if !readOnly {
			return nil
		}
	default:
		return fail("invalid_args", "unknown task completion requirement")
	}
	return fail("requirement_mismatch", "task "+t.ID+" requires "+t.Requirement+"; assign a compatible member or create a separate task")
}

func criteriaFor(requirement string) string {
	switch requirement {
	case RequirementDelivered:
		return "Result delivered to the parent"
	case RequirementApplied:
		return "Parent integrates the candidate"
	default:
		return "Parent reviews and accepts the submitted result"
	}
}

func deliveredTask(t *Task) bool {
	return t != nil && t.Delivery != nil && t.Delivery.Revision == t.Revision && t.Delivery.Execution == t.Execution
}

func deliveringTask(s *State, t *Task) bool {
	if t == nil || t.Status != "running" || requirementOf(s, t) != RequirementDelivered || deliveredTask(t) {
		return false
	}
	e := s.Executions[t.Execution]
	return e != nil && e.Status == "completed"
}

func resultNotice(s *State, t *Task) *Mail {
	for _, id := range sortedInspectionIDs(s.Messages) {
		m := s.Messages[id]
		if m.Task == t.ID && m.Revision == t.Revision && m.Execution == t.Execution {
			return m
		}
	}
	return nil
}
