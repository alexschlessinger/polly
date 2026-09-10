package swarm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/workflow"
	"github.com/alexschlessinger/pollytool/worktree"
	"sort"
)

func noEditResult(t *testing.T, readOnly bool) (*Runtime, AgentResult, TaskReference) {
	t.Helper()
	skipIfWindows(t) // Runtime worktree fixtures require audited POSIX Git.
	r := runtimeTest(t, nilModel(), 1, 8)
	if err := os.WriteFile(filepath.Join(r.config.Root, "source.txt"), []byte("untouched\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.name", "test"}, {"config", "user.email", "test@example.invalid"}, {"add", "."}, {"commit", "-qm", "source"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = r.config.Root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
	}
	result, err := r.Agent(context.Background(), "", AgentRequest{Task: "Review source only. Do not write files.", ReadOnly: readOnly, Review: readOnly})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	task := s.Tasks[result.Task]
	if !readOnly && !unchangedTask(s, task) {
		t.Fatal("execution did not submit an unchanged candidate")
	}
	return r, result, TaskReference{Task: task.ID, Revision: task.Revision}
}

func TestUnchangedTaskAcceptanceBeforeAndAfterCleanup(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		for _, cleanupFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("readOnly=%t/cleanupFirst=%t", readOnly, cleanupFirst), func(t *testing.T) {
				r, result, ref := noEditResult(t, readOnly)
				suspendAutoRelease(t, r)
				ctx := context.Background()
				if cleanupFirst {
					if err := r.Cleanup(ctx, result.Context); err != nil {
						t.Fatal(err)
					}
				}
				if err := assertSettleMatchesBlockers(t, r); err == nil || !strings.Contains(err.Error(), "awaiting parent review") || !strings.Contains(err.Error(), ref.Task) {
					t.Fatalf("unaccepted task settled or lost review guidance: %v", err)
				}
				// A later parent edit is outside this member's unchanged result.
				if err := os.WriteFile(filepath.Join(r.config.Root, "source.txt"), []byte("parent drift\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := r.Review(ctx, ref.Task, ref.Revision, true, ""); err != nil {
					t.Fatal(err)
				}
				s, _ := r.State(ctx)
				if task := s.Tasks[ref.Task]; task.Status != "done" || task.AcceptedRevision != ref.Revision {
					t.Fatalf("accepted unchanged task: %+v", task)
				}
				if !cleanupFirst {
					if err := r.Cleanup(ctx, result.Context); err != nil {
						t.Fatal(err)
					}
				}
				if err := r.Settle(ctx); err != nil {
					t.Fatal(err)
				}
				if data, _ := os.ReadFile(filepath.Join(r.config.Root, "source.txt")); string(data) != "parent drift\n" {
					t.Fatal("settlement overwrote parent changes")
				}
				s, _ = r.State(ctx)
				if len(s.Applies) != 0 || s.Members[result.Session].Control != MemberControlEnabled {
					t.Fatal("settlement required an apply or changed the member control")
				}
			})
		}
	}
}

func TestUnchangedTaskRecoversSavedAcceptance(t *testing.T) {
	r, result, ref := noEditResult(t, false)
	ctx := context.Background()
	// Record the old runtime's accepted-but-awaiting-review state.
	if err := r.update(ctx, func(s *State) error {
		s.Tasks[ref.Task].AcceptedRevision = ref.Revision
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Cleanup(ctx, result.Context); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := New(r.config)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := restored.Settle(ctx); err != nil {
		t.Fatal(err)
	}
	// Read the underlying store, not a normalized display projection.
	raw, err := r.parent.ReadCoordination(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s, err := decodeState(raw)
	if err != nil {
		t.Fatal(err)
	}
	if s.Tasks[ref.Task].Status != "done" || s.Tasks[ref.Task].AcceptedRevision != ref.Revision || len(s.Applies) != 0 {
		t.Fatalf("saved no-op recovery did not persist completion: %+v", s.Tasks[ref.Task])
	}
}

func unchangedTaskState() (*State, *Task) {
	base := worktree.Snapshot{ID: "base", Tree: "same-tree", Commit: "base-commit", Source: "/parent"}
	submitted := worktree.Snapshot{ID: "submitted", Tree: base.Tree, Commit: "result-commit", Source: "/member"}
	task := &Task{ID: "task", Owner: "member", Status: "awaiting_review", Revision: 2, AcceptedRevision: 2, Snapshot: submitted.ID, StartingSnapshot: base.ID}
	s := &State{
		Tasks:     map[string]*Task{task.ID: task},
		Members:   map[string]*Member{"member": {ID: "member", Context: "copy"}},
		Contexts:  map[string]*ExecutionContext{"copy": {Owner: "member", Root: "/member", Checkout: &worktree.Checkout{Path: "/member", Base: base}}},
		Snapshots: map[string]*worktree.Snapshot{base.ID: &base, submitted.ID: &submitted},
	}
	return s, task
}

func TestUnchangedTaskRequiresOriginalProvenance(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*State, *Task)
	}{
		{"missing candidate", func(s *State, task *Task) { delete(s.Snapshots, task.Snapshot) }},
		{"missing base", func(s *State, task *Task) { delete(s.Snapshots, "base") }},
		{"missing source", func(s *State, task *Task) { s.Snapshots[task.Snapshot].Source = "" }},
		{"foreign owner identity", func(s *State, task *Task) { s.Members[task.Owner].ID = "other" }},
		{"missing owner", func(s *State, task *Task) { delete(s.Members, task.Owner) }},
		{"snapshot id mismatch", func(s *State, task *Task) { s.Snapshots[task.Snapshot].ID = "other" }},
		{"empty tree", func(s *State, task *Task) { s.Snapshots[task.Snapshot].Tree = ""; s.Snapshots["base"].Tree = "" }},
		{"empty commit", func(s *State, task *Task) { s.Snapshots[task.Snapshot].Commit = "" }},
		{"changed candidate", func(s *State, task *Task) { s.Snapshots[task.Snapshot].Tree = "changed-tree" }},
		{"missing recorded base", func(s *State, task *Task) { task.StartingSnapshot = "" }},
		{"released missing starting snapshot", func(s *State, task *Task) {
			delete(s.Contexts, "copy")
			s.Members[task.Owner].Context = ""
			task.StartingSnapshot = ""
		}},
		{"released missing base", func(s *State, task *Task) {
			delete(s.Contexts, "copy")
			s.Members[task.Owner].Context = ""
			task.StartingSnapshot = "missing"
		}},
		{"released base id mismatch", func(s *State, task *Task) {
			delete(s.Contexts, "copy")
			s.Members[task.Owner].Context = ""
			task.StartingSnapshot = "base"
			s.Snapshots["base"].ID = "foreign"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, task := unchangedTaskState()
			tc.change(s, task)
			if unchangedTask(s, task) {
				t.Fatal("invalid or changed provenance qualified for no-op completion")
			}
			task.Status = "done"
			if integrationTask(s, task) {
				t.Fatal("invalid completed task qualified for integration")
			}
		})
	}
}

func TestUnchangedTaskRecoveryRequiresCurrentAcceptance(t *testing.T) {
	for _, mutation := range []string{"not accepted", "stale acceptance", "missing candidate", "missing candidate source", "uncertain apply"} {
		t.Run(mutation, func(t *testing.T) {
			r, _, ref := noEditResult(t, false)
			ctx := context.Background()
			if err := r.Review(ctx, ref.Task, ref.Revision-1, true, ""); err == nil {
				t.Fatal("stale review succeeded")
			}
			if err := r.update(ctx, func(s *State) error {
				task := s.Tasks[ref.Task]
				task.AcceptedRevision = task.Revision
				switch mutation {
				case "not accepted":
					task.AcceptedRevision = 0
				case "stale acceptance":
					task.AcceptedRevision--
				case "missing candidate":
					delete(s.Snapshots, task.Snapshot)
				case "missing candidate source":
					s.Snapshots[task.Snapshot].Source = ""
				case "uncertain apply":
					s.Applies["uncertain"] = &ApplyRecord{ID: "uncertain", Status: "applying"}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := r.Settle(ctx); err == nil {
				t.Fatal("unsafe state settled")
			}
			s, _ := r.State(ctx)
			if s.Tasks[ref.Task].Status != "awaiting_review" {
				t.Fatal("unsafe state completed its task")
			}
		})
	}
}

func TestUnchangedTaskPreservesExplicitMixedIntegration(t *testing.T) {
	for _, reviewFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("reviewFirst=%t", reviewFirst), func(t *testing.T) {
			r, plan := applyFixture(t, false)
			ctx := context.Background()
			noop := submittedInput(t, r, plan.Parent, nil)
			edited := submittedInput(t, r, plan.Parent, map[string]string{"a.txt": "changed\n"})
			refs := []TaskReference{noop, edited}
			review := func() {
				t.Helper()
				for _, ref := range refs {
					if err := r.Review(ctx, ref.Task, ref.Revision, true, ""); err != nil {
						t.Fatal(err)
					}
				}
			}
			if reviewFirst {
				review()
			}
			candidate, err := r.PrepareIntegration(ctx, refs, "")
			if err != nil {
				t.Fatal(err)
			}
			if !reviewFirst {
				review()
			}
			s, _ := r.State(ctx)
			if s.Tasks[noop.Task].Status != "done" || s.Tasks[edited.Task].Status != "awaiting_review" {
				t.Fatal("review did not distinguish unchanged and edited tasks")
			}
			// Even if the parent independently reaches the submitted tree,
			// the edited task still requires its explicit integration receipt.
			if err := os.WriteFile(filepath.Join(r.config.Root, "a.txt"), []byte("changed\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := r.Settle(ctx); err == nil || !strings.Contains(err.Error(), edited.Task) || !strings.Contains(err.Error(), "prepare, accept, and apply") {
				t.Fatalf("missing integration guidance: %v", err)
			}
			if err := os.WriteFile(filepath.Join(r.config.Root, "a.txt"), []byte("base\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := r.AcceptIntegration(ctx, candidate.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := r.ApplyIntegration(ctx, candidate.ID); err != nil {
				t.Fatal(err)
			}
			if err := r.Settle(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := r.PrepareIntegration(ctx, []TaskReference{edited}, ""); err == nil {
				t.Fatal("already-applied changed task was reusable as an integration input")
			}
		})
	}
}

func TestTaskSettlementDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		status   string
		accepted bool
		want     string
	}{
		{"awaiting_review", false, "accept this revision or request changes"},
		{"awaiting_review", true, "prepare, accept, and apply"},
		{"pending", false, "assign and run"},
		{"blocked", false, "update the task"},
		{"changes_requested", false, "resume the member"},
	} {
		t.Run(fmt.Sprintf("%s/accepted=%t", tc.status, tc.accepted), func(t *testing.T) {
			s, task := unchangedTaskState()
			task.Status = tc.status
			if !tc.accepted {
				task.AcceptedRevision = 0
			}
			err := taskSettlementError(s, task)
			if err == nil || !strings.Contains(err.Error(), "task task revision 2:") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("missing task revision or operation: %v", err)
			}
		})
	}
	s, task := unchangedTaskState()
	delete(s.Snapshots, task.Snapshot)
	if err := taskSettlementError(s, task); !strings.Contains(err.Error(), "snapshot provenance is unavailable") {
		t.Fatalf("missing recovery diagnostic: %v", err)
	}
	task.Status, task.Execution = "running", "execution"
	s.Executions = map[string]*Execution{"execution": {ID: "execution", Member: "member", Status: "failed"}}
	if err := taskSettlementError(s, task); !strings.Contains(err.Error(), "execution execution is failed") || !strings.Contains(err.Error(), "resume member member") {
		t.Fatalf("failed execution described as active work: %v", err)
	}
	s.Executions[task.Execution].Status = "paused"
	s.Executions[task.Execution].StopReason = messages.StopReasonMaxIterations
	var limit *IterationLimitError
	if err := taskSettlementError(s, task); !strings.Contains(err.Error(), "task task revision 2:") || !errors.As(err, &limit) {
		t.Fatalf("lost task identity or typed iteration grant requirement: %v", err)
	}
}

// A completed workflow's unreviewed consumed research is the first blocker
// settlement names, ahead of per-task blockers and failed reports.
func TestSettleLeadsWithCompletedWorkflow(t *testing.T) {
	r := runtimeTest(t, nilModel(), 2, 8)
	ctx := context.Background()
	report, err := r.RunWorkflow(ctx, `polly.defineWorkflow({name:"research",inputSchema:polly.schema.object({}),async run(){await polly.agent({task:"investigate a",readOnly:true});return await polly.agent({task:"investigate b",readOnly:true});}})`, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	task, err := r.CreateTask(ctx, "pending work", "review", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	failed, err := r.RunWorkflow(ctx, `polly.defineWorkflow({name:"failure",inputSchema:polly.schema.object({}),async run(){throw Error("verification failed");}});`, map[string]any{})
	if err == nil || failed == nil {
		t.Fatalf("missing failure: %+v %v", failed, err)
	}
	var blocker *workflow.Error
	want := "1 result awaits delivery"
	if err := assertSettleMatchesBlockers(t, r); err == nil || !strings.HasPrefix(err.Error(), want) || !errors.As(err, &blocker) || blocker.Code != "blocked" {
		t.Fatalf("settlement did not lead with the completed workflow: %v", err)
	}
	admitParent(t, r)
	_ = report
	if err := assertSettleMatchesBlockers(t, r); err == nil || !strings.HasPrefix(err.Error(), "task "+task.ID+" revision 1: pending;") {
		t.Fatalf("after acknowledging: %v", err)
	}
	if err := r.CancelTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := assertSettleMatchesBlockers(t, r); err == nil || !strings.Contains(err.Error(), failed.ID) {
		t.Fatalf("failed report not named: %v", err)
	}
	if err := r.AcknowledgeWorkflow(ctx, failed.ID); err != nil {
		t.Fatalf("acknowledging the failure = %d, %v", 0, err)
	}
	if err := assertSettleMatchesBlockers(t, r); err != nil {
		t.Fatalf("settled swarm still blocked: %v", err)
	}
}

// Research the script reviewed itself leaves nothing for acknowledgment to
// accept, so an unacknowledged completed report does not block settlement.
func TestCompletedWorkflowWithReviewedResearchSettles(t *testing.T) {
	r := runtimeTest(t, nilModel(), 1, 2)
	ctx := context.Background()
	report, err := r.RunWorkflow(ctx, `polly.defineWorkflow({name:"reviewed",inputSchema:polly.schema.object({}),async run(){const a=await polly.agent({task:"investigate",readOnly:true,review:true});const t=await polly.tasks.read(a.task);await polly.tasks.review({task:t.id,revision:t.revision,accept:true});return a;}})`, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	admitParent(t, r)
	if err := r.Settle(ctx); err != nil {
		t.Fatalf("reviewed research still blocked settlement: %v", err)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Workflows[report.ID].Acknowledged {
		t.Fatal("parent checkpoint did not acknowledge delivered output")
	}
}

func TestSettleReportsTaskCount(t *testing.T) {
	r := runtimeTest(t, idleModel(), 1, 1)
	ctx := context.Background()
	var ids []string
	for _, description := range []string{"first", "second", "third"} {
		task, err := r.CreateTask(ctx, description, "review", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, task.ID)
	}
	sort.Strings(ids)
	var blocker *workflow.Error
	admitParent(t, r)
	if err := assertSettleMatchesBlockers(t, r); err == nil || !strings.HasPrefix(err.Error(), "3 tasks unsettled; first: task "+ids[0]+" revision 1: pending; resolve its dependencies") || !errors.As(err, &blocker) || blocker.Code != "blocked" {
		t.Fatalf("task count missing: %v", err)
	}
	for _, id := range ids[1:] {
		if err := r.CancelTask(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := assertSettleMatchesBlockers(t, r); err == nil || !strings.HasPrefix(err.Error(), "task "+ids[0]+" revision 1:") {
		t.Fatalf("a single task carried a count: %v", err)
	}
	// Typed blockers survive the prefix.
	s, task := unchangedTaskState()
	task.Status, task.Execution = "running", "execution"
	s.Executions = map[string]*Execution{"execution": {ID: "execution", Member: "member", Status: "paused", StopReason: messages.StopReasonMaxIterations}}
	s.Tasks["zzz"] = &Task{ID: "zzz", Status: "pending", Revision: 1}
	var limit *IterationLimitError
	if err := unsettledTasksError(s, []*Task{task, s.Tasks["zzz"]}); err == nil || !strings.HasPrefix(err.Error(), "2 tasks unsettled; first: task task revision 2:") || !errors.As(err, &limit) {
		t.Fatalf("typed iteration blocker lost behind the count: %v", err)
	}
}
