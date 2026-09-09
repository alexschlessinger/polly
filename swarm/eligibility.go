package swarm

import "errors"

// Eligibility rules read state and controls and never write. Callers hold the
// launch lock and re-run them inside the transaction that acts on the answer.

// launchRefusal says why a member cannot start a new logical execution now.
func launchRefusal(s *State, m *Member) error {
	if m == nil {
		return errors.New("unknown member session")
	}
	switch m.Control {
	case MemberControlRetired:
		return errors.New("member has been retired; reassign its task")
	case MemberControlStopped:
		return errors.New("member is stopped; explicit resume required")
	}
	if e := s.Executions[m.Execution]; e != nil {
		switch e.Status {
		case "queued", "running", "waiting":
			return fail("session_busy", "member already has an active execution")
		case "paused", "failed":
			return errors.New("member is paused; explicit resume or takeover required")
		}
	}
	return nil
}

// wakeEligible reports whether addressed mail may start a member. Only an
// enabled, unreserved member whose last execution completed is woken; paused,
// failed, stopped and retired members wait for an explicit decision, and
// informational mail never starts anyone.
func wakeEligible(s *State, m *Member) bool {
	if m == nil || m.Control != MemberControlEnabled || m.Controller != "" {
		return false
	}
	if e := s.Executions[m.Execution]; e != nil && e.Status != "completed" {
		return false
	}
	return hasWakeMail(s, m.ID)
}

// stopRefusal says why a member cannot be stopped. Stopping twice is fine;
// a retired member keeps its retirement and the provenance it pins.
func stopRefusal(m *Member) error {
	if m == nil {
		return errors.New("unknown member")
	}
	if m.Control == MemberControlRetired {
		return errors.New("member has been retired; nothing to stop")
	}
	return nil
}
