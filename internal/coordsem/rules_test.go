package coordsem

import "testing"

func TestClassification(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		in     Classification
		bucket Bucket
		reason string
	}{
		{"controlled before unchanged", Classification{Controlled: true, Unchanged: true}, FoldedWorking, ""},
		{"uncertain apply", Classification{Unchanged: true, FirstApply: "a"}, Working, "accepted · settles after integration a is reconciled"},
		{"accepted unchanged", Classification{Unchanged: true}, Done, ""},
		{"notice", Classification{Delivering: true, Advancing: true}, Working, "delivering"},
		{"request before running", Classification{Parked: true, Asking: true, Running: true}, FoldedDecision, ""},
		{"waiting peer", Classification{Parked: true, Running: true}, FoldedWorking, ""},
		{"parked alone", Classification{Parked: true}, Decision, ""},
		{"canceled first", Classification{Status: "pending", Dependencies: []Dependency{{ID: "outside"}, {ID: "gone", Canceled: true}}}, Decision, "gone"},
		{"outside run", Classification{Status: "pending", Dependencies: []Dependency{{ID: "outside"}}}, Decision, ""},
		{"cycles do not invent readiness", Classification{Status: "pending", Dependencies: []Dependency{{ID: "self", Open: true}, {ID: "decision", Open: true}}}, Working, "pending · waiting on self, decision"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, r := Classify(tc.in)
			if b != tc.bucket || r != tc.reason {
				t.Fatalf("got %v %q; want %v %q", b, r, tc.bucket, tc.reason)
			}
		})
	}
}

func TestActivityAndGates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ status, control, want string }{
		{"queued", "", "active"}, {"running", "", "active"}, {"waiting", "", "waiting"}, {"completed", "", "idle"}, {"failed", "", "paused"}, {"running", "stopped", "paused"},
	} {
		got, _ := Activity(tc.control, &Execution{Status: tc.status}, "")
		if got != tc.want {
			t.Fatalf("%+v: %s", tc, got)
		}
	}
	if WorkStepRunning("followup", "running") || !DisplayStepRunning("followup", "running") {
		t.Fatal("followup gates were conflated")
	}
}

func TestNewsValuesAndDeferral(t *testing.T) {
	t.Parallel()
	m := Member{ID: "m", Execution: "e"}
	e := &Execution{ID: "e", Status: "queued", Generation: 3}
	a := NewsMember(m, e)
	e.Status = "running"
	if a != NewsMember(m, e) {
		t.Fatal("queued to running is not news")
	}
	e.Generation++
	if a == NewsMember(m, e) {
		t.Fatal("generation is news")
	}
	task := &Task{ID: "t", Run: "r", Status: "blocked", Owner: "m", Execution: "e", Revision: 2, Deferral: &Deferral{Workflow: "w", Execution: "e", Owner: "m", Revision: 2, Generation: 4, Status: "blocked"}}
	e.Status, e.Workflow, e.Run, e.Member = "failed", "w", "r", "m"
	w := &Workflow{ID: "w", Run: "r", Status: "failed", Acknowledged: true}
	if !Deferred(task, e, w) {
		t.Fatal("valid deferral rejected")
	}
	e.Generation++
	if Deferred(task, e, w) {
		t.Fatal("stale deferral accepted")
	}
}
