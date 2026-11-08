package swarm

import "github.com/alexschlessinger/pollytool/internal/coordsem"

// TaskStatus describes the remaining task work without changing its durable
// machine status. A submitted editing result can be accepted before integration.
func TaskStatus(task *Task) string {
	return coordsem.TaskStatus(semanticTask(task))
}

func acceptedTaskRevision(task *Task) bool {
	return coordsem.Accepted(semanticTask(task))
}

// TaskStatusIn includes delivery and integration halts without storing display state.
func TaskStatusIn(s *State, task *Task) string {
	delivering := deliveringTask(s, task)
	editing, conflicted := false, false
	if !delivering && task != nil && task.Status == "awaiting_review" {
		editing = editingTask(s, task)
		if editing {
			candidate := currentCandidate(s, task)
			conflicted = candidate != nil && candidate.Status == "conflicted"
		}
	}
	return coordsem.TaskDisplay(semanticTask(task), delivering, editing, conflicted)
}
