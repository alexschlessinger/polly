package swarm

import "strings"

// TaskStatus describes the remaining task work without changing its durable
// machine status. A submitted editing result can be accepted before integration.
func TaskStatus(task *Task) string {
	if task == nil {
		return ""
	}
	if task.Status == "awaiting_review" && acceptedTaskRevision(task) && task.Snapshot != "" {
		return "accepted · integration pending"
	}
	return strings.ReplaceAll(task.Status, "_", " ")
}

func acceptedTaskRevision(task *Task) bool {
	return task != nil && task.Revision > 0 && task.AcceptedRevision == task.Revision
}
