package swarm

import "testing"

func TestRequirementFromRequest(t *testing.T) {
	for _, tc := range []struct {
		name             string
		review, readOnly bool
		want             string
	}{
		{"research", false, true, RequirementDelivered},
		{"reviewed research", true, true, RequirementReviewed},
		{"editing", false, false, RequirementApplied},
		{"reviewed editing refused", true, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := requirementFor(tc.review, tc.readOnly)
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("requirement = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestRequirementCompatibilityDoesNotConvert(t *testing.T) {
	for _, requirement := range []string{RequirementDelivered, RequirementReviewed, RequirementApplied} {
		for _, readOnly := range []bool{true, false} {
			task := &Task{ID: "task", Requirement: requirement, Revision: 3}
			err := validateRequirement(task, readOnly)
			compatible := readOnly == (requirement != RequirementApplied)
			if (err == nil) != compatible || task.Requirement != requirement || task.Revision != 3 {
				t.Fatalf("requirement=%s readOnly=%v: task=%+v err=%v", requirement, readOnly, task, err)
			}
		}
	}
}

func TestDeliveryIdentifiesExactExecutionAndRevision(t *testing.T) {
	task := &Task{Execution: "second", Revision: 3, Delivery: &TaskDelivery{Execution: "first", Revision: 3}}
	if deliveredTask(task) {
		t.Fatal("another execution's result counted as delivered")
	}
	task.Delivery.Execution = "second"
	task.Delivery.Revision = 2
	if deliveredTask(task) {
		t.Fatal("an earlier revision's result counted as delivered")
	}
	task.Delivery.Revision = 3
	if !deliveredTask(task) {
		t.Fatal("the exact result was not delivered")
	}
}

func TestTaskCreationRequirement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options CreateTaskOptions
		owner   *Member
		want    string
	}{
		{"unowned", CreateTaskOptions{}, nil, RequirementDelivered},
		{"editing", CreateTaskOptions{}, &Member{}, RequirementApplied},
		{"research", CreateTaskOptions{}, &Member{ReadOnly: true}, RequirementDelivered},
		{"review", CreateTaskOptions{Review: true}, nil, RequirementReviewed},
		{"explicit", CreateTaskOptions{Requirement: RequirementApplied}, nil, RequirementApplied},
		{"conflict", CreateTaskOptions{Review: true, Requirement: RequirementDelivered}, nil, ""},
		{"incompatible", CreateTaskOptions{Requirement: RequirementApplied}, &Member{ReadOnly: true}, ""},
		{"unknown", CreateTaskOptions{Requirement: "accepted"}, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := creationRequirement(tc.options, tc.owner)
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
