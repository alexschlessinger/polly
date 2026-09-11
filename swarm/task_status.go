package swarm

import "strings"

// TaskStatus describes the remaining task work without changing its durable
// machine status. A submitted editing result can be accepted before integration.
func TaskStatus(task *Task) string {
	if task == nil {
		return ""
	}
	if task.Status == "awaiting_review" && acceptedTaskRevision(task) && task.Snapshot != "" {
		return "integration pending"
	}
	return strings.ReplaceAll(task.Status, "_", " ")
}

func acceptedTaskRevision(task *Task) bool {
	return task != nil && task.Revision > 0 && task.AcceptedRevision == task.Revision
}

// TaskStatusIn includes delivery and integration halts without storing display state.
func TaskStatusIn(s *State, task *Task) string {
	if deliveringTask(s, task) {
		return "delivering"
	}
	if task != nil && task.Status == "awaiting_review" && editingTask(s, task) {
		if c := currentCandidate(s, task); c != nil && c.Status == "conflicted" {
			return "integration halted"
		}
	}
	return TaskStatus(task)
}
