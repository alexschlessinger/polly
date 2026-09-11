package swarm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartingSnapshotRecordedAtAssignment(t *testing.T) {
	r, result, ref := noEditResult(t, false)
	ctx := context.Background()
	s, err := r.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task, e := s.Tasks[ref.Task], s.Executions[s.Members[result.Session].Execution]
	c := s.Contexts[result.Context]
	if task.StartingSnapshot != c.Checkout.Base.ID || e.Base != task.StartingSnapshot || e.Workspace != c.ID || e.SourceRoot != "" {
		t.Fatalf("assignment lost snapshot provenance: task=%+v execution=%+v", task, e)
	}
	// Claim follows the same assignment path and cannot replace a required base.
	created, err := r.CreateTask(ctx, "next editing task", "integrate", nil, "", CreateTaskOptions{Requirement: RequirementApplied})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Claim(ctx, result.Session, created.ID, created.Revision); err != nil {
		t.Fatal(err)
	}
	s, _ = r.read(ctx)
	if s.Tasks[created.ID].StartingSnapshot != task.StartingSnapshot {
		t.Fatal("claim did not retain the assignee's actual base")
	}
}

func TestLiveSourceRecordedAcrossContinuation(t *testing.T) {
	r := runtimeTest(t, nilModel(), 1, 4)
	ctx := context.Background()
	first, err := r.Agent(ctx, "", AgentRequest{Task: "inspect", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Agent(ctx, "", AgentRequest{Session: first.Session, Task: "inspect again"})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := r.read(ctx)
	if s.Tasks[second.Task].SourceRoot != r.config.Root || s.Tasks[second.Task].StartingSnapshot != "" {
		t.Fatal("continuation lost the live source")
	}
	for _, e := range s.Executions {
		if e.SourceRoot != r.config.Root || e.Base != "" || e.Workspace == "" {
			t.Fatalf("execution lost its resolved source: %+v", e)
		}
	}
}

func TestAssignmentRefusesPresetSourceMismatch(t *testing.T) {
	s, task := unchangedTaskState()
	m := s.Members[task.Owner]
	task.StartingSnapshot = "required-other-base"
	if err := assignTask(s, task, m, s.Contexts[m.Context], "new-execution"); err == nil {
		t.Fatal("assignment replaced a preset snapshot")
	}
	if task.StartingSnapshot != "required-other-base" || task.Revision != 2 || task.Execution != "" {
		t.Fatal("refusal mutated task provenance")
	}
}

func TestTaskSnapshotsReadTaskRecordsOnly(t *testing.T) {
	s, task := unchangedTaskState()
	delete(s.Contexts, "copy")
	s.Members[task.Owner].Context = "new-workspace"
	base, candidate := taskSnapshots(s, task)
	if base == nil || candidate == nil || base.ID != task.StartingSnapshot || !unchangedTask(s, task) {
		t.Fatal("workspace removal or replacement invalidated task provenance")
	}
}

func TestSubmitRejectsAnotherSourceSnapshot(t *testing.T) {
	r, result, ref := noEditResult(t, false)
	ctx := context.Background()
	s, _ := r.read(ctx)
	task := s.Tasks[ref.Task]
	// The starting snapshot came from the parent, not the member's checkout.
	err := r.Submit(ctx, result.Session, task.ID, task.Revision, "foreign result", task.StartingSnapshot)
	if err == nil || !strings.Contains(err.Error(), "must come from") {
		t.Fatalf("foreign source submission: %v", err)
	}
	after, _ := r.read(ctx)
	if after.Tasks[task.ID].Snapshot != task.Snapshot || after.Tasks[task.ID].Revision != task.Revision {
		t.Fatal("refused submission changed the task")
	}
}

func TestCleanupKeepsSnapshotRefsUntilForget(t *testing.T) {
	r, _, ref := noEditResult(t, false)
	ctx := context.Background()
	suspendAutoRelease(t, r)
	if _, err := r.Integrate(ctx, IntegrateRequest{Tasks: []TaskReference{ref}}); err != nil {
		t.Fatal(err)
	}
	s, _ := r.read(ctx)
	base := s.Tasks[ref.Task].StartingSnapshot
	if err := r.Cleanup(ctx, ""); err != nil {
		t.Fatal(err)
	}
	after, _ := r.read(ctx)
	if len(after.Contexts) != 0 || len(after.Snapshots) != len(s.Snapshots) || after.Tasks[ref.Task].StartingSnapshot != base {
		t.Fatal("workspace cleanup erased task-owned snapshots")
	}
	refExists := func() bool {
		cmd := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/polly/snapshots/"+base)
		cmd.Dir = r.config.Root
		return cmd.Run() == nil
	}
	if !refExists() {
		t.Fatal("cleanup deleted the snapshot's Git reference")
	}
	copy, err := r.makeContext(ctx, r.ID, AgentRequest{ReadOnly: true, Snapshot: base})
	if err != nil {
		t.Fatal("retained snapshot could not recreate a workspace:", err)
	}
	if text, err := os.ReadFile(filepath.Join(copy.Root, "source.txt")); err != nil || string(text) != "untouched\n" {
		t.Fatalf("restored source: %q %v", text, err)
	}
	if err := r.Forget(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ = r.read(ctx)
	if len(after.Snapshots) != 0 || refExists() || after.Tasks[ref.Task].StartingSnapshot != base {
		t.Fatal("forget must remove refs, while preserving historical task provenance")
	}
	if _, err := r.makeContext(ctx, r.ID, AgentRequest{ReadOnly: true, Snapshot: base}); err == nil {
		t.Fatal("forgotten snapshot was silently recaptured")
	}
}

func TestForgetRefusesOutstandingIntegration(t *testing.T) {
	r, _ := applyFixture(t, false)
	if err := r.Forget(context.Background()); err == nil {
		t.Fatal("forget discarded an integration candidate's snapshots")
	}
	r, _, _ = noEditResult(t, false)
	if err := r.Forget(context.Background()); err == nil {
		t.Fatal("forget discarded a submitted task's snapshots")
	}
}
