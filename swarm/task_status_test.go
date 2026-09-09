package swarm

import "testing"

func TestTaskStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		task *Task
		want string
	}{
		{"nil", nil, ""},
		{"review", &Task{Status: "awaiting_review", Revision: 2}, "awaiting review"},
		{"accepted", &Task{Status: "awaiting_review", Revision: 2, AcceptedRevision: 2, Snapshot: "candidate"}, "accepted · integration pending"},
		{"stale", &Task{Status: "awaiting_review", Revision: 3, AcceptedRevision: 2, Snapshot: "candidate"}, "awaiting review"},
		{"zero", &Task{Status: "awaiting_review", Snapshot: "candidate"}, "awaiting review"},
		{"no snapshot", &Task{Status: "awaiting_review", Revision: 2, AcceptedRevision: 2}, "awaiting review"},
		{"done", &Task{Status: "done", Revision: 2, AcceptedRevision: 2, Snapshot: "candidate"}, "done"},
		{"changes requested", &Task{Status: "changes_requested"}, "changes requested"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := TaskStatus(tc.task); got != tc.want {
				t.Fatalf("TaskStatus = %q, want %q", got, tc.want)
			}
		})
	}
}
