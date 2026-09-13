package swarm

import "errors"

// Eligibility rules read state and controls and never write. Callers hold the
// launch lock and re-run them inside the transaction that acts on the answer.

// launchRefusal says why a member cannot start a new logical execution now.
// An explicit resume may restart a stopped member or a failed execution; an
// ordinary launch may not.
func launchRefusal(s *State, m *Member, resume bool) error {
	if m == nil {
		return errors.New("unknown member session")
	}
	switch m.Control {
	case MemberControlStopped:
		if !resume {
			return errors.New("member is stopped; explicit resume required")
		}
	}
	if e := s.Executions[m.Execution]; e != nil {
		switch e.Status {
		case "queued", "running", "waiting":
			return fail("session_busy", "member already has an active execution")
		case "paused", "failed":
			if !resume {
				return errors.New("member is paused; explicit resume or takeover required")
			}
		}
	}
	return nil
}

// resumeTarget decides how an explicit resume proceeds: a paused execution
// continues in place with its remaining allowance; anything else needs a new
// logical execution, which spends a run start.
func resumeTarget(s *State, m *Member, additional int, workflowActive bool) (e *Execution, continues bool, err error) {
	if workflowActive {
		return nil, false, fail("session_busy", "member is reserved by an active workflow; wait for it to settle or cancel it before resuming")
	}
	e = s.Executions[m.Execution]
	continues = e != nil && e.Status == "paused"
	if additional > 0 && !continues {
		return nil, false, errors.New("additional iterations require a paused execution")
	}
	return e, continues, nil
}

// applyGrant adds logical starts to a run and reopens a run that budget
// exhaustion paused. It is applied inside the transaction that spends it.
func applyGrant(run *Run, grant int) error {
	if grant <= 0 {
		return nil
	}
	if grant > int(^uint(0)>>1)-run.Limit {
		return errors.New("execution grant exceeds the supported limit")
	}
	run.Limit += grant
	run.Status = "running"
	return nil
}

// wakeEligible starts only explicit follow-ups left pending by finalization.
func wakeEligible(s *State, m *Member) bool {
	if m == nil || m.Control != MemberControlEnabled || m.Controller != "" {
		return false
	}
	if e := s.Executions[m.Execution]; e != nil && e.Status != "completed" {
		if e.Status == "paused" {
			for _, mail := range s.Messages {
				if mail.To == m.ID && mail.Start && !mail.Delivered && mail.ResumeExecution == e.ID {
					return true
				}
			}
		}
		return false
	}
	return pendingFollowup(s, m.ID)
}

// stopRefusal says why a member cannot be stopped. Stopping twice is fine.
func stopRefusal(m *Member) error {
	if m == nil {
		return errors.New("unknown member")
	}
	return nil
}
