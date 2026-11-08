package swarm

import "github.com/alexschlessinger/pollytool/internal/coordsem"

func semanticTask(t *Task) *coordsem.Task {
	if t == nil {
		return nil
	}
	out := &coordsem.Task{ID: t.ID, Run: t.Run, Status: t.Status, Owner: t.Owner, Execution: t.Execution, Snapshot: t.Snapshot, StartingSnapshot: t.StartingSnapshot, Feedback: t.Feedback, Requirement: t.Requirement, Revision: t.Revision, AcceptedRevision: t.AcceptedRevision}
	if d := t.Deferral; d != nil {
		out.Deferral = &coordsem.Deferral{Workflow: d.Workflow, Execution: d.Execution, Owner: d.Owner, Revision: d.Revision, Generation: d.Generation, AcceptedRevision: d.AcceptedRevision, Status: d.Status, Note: d.Note}
	}
	return out
}
func semanticExecution(e *Execution) *coordsem.Execution {
	if e == nil {
		return nil
	}
	return &coordsem.Execution{ID: e.ID, Run: e.Run, Member: e.Member, Workflow: e.Workflow, Status: e.Status, StopReason: string(e.StopReason), Generation: e.Generation, Iterations: e.Iterations, MaxIterations: e.Request.MaxIterations, RetryPause: e.RetryPause != nil}
}
func semanticTaskDecision(s *State, t *Task) coordsem.TaskDecisionInputs {
	f := coordsem.TaskDecisionInputs{Task: *semanticTask(t), Execution: semanticExecution(s.Executions[t.Execution]), OwnerDisplay: memberDisplay(s, t.Owner), Delivering: deliveringTask(s, t), Editing: editingTask(s, t)}
	if owner := s.Members[t.Owner]; owner != nil {
		v := semanticMember(owner)
		f.Owner = &v
	}
	if base, _ := taskSnapshots(s, t); base != nil {
		f.Provenance = true
	}
	if c := currentCandidate(s, t); c != nil {
		f.CandidateID, f.CandidateStatus, f.CandidateAction = c.ID, c.Status, integrateCandidateAction(c.ID)
		if c.Status == "conflicted" {
			f.CandidateAction = integrationHalt(c).Message
		}
	}
	if e := s.Executions[t.Execution]; e != nil && e.RetryPause != nil {
		f.RetryDescription = e.RetryPause.description()
	}
	return f
}
func semanticMember(m *Member) coordsem.Member {
	return coordsem.Member{ID: m.ID, Control: string(m.Control), Execution: m.Execution, Task: m.Task, Controller: m.Controller}
}
