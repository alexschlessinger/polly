package swarm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/subagent"
)

func TestFollowupChecksExistingBaseAndExplicitRefresh(t *testing.T) {
	r := scratchRuntime(t, doneModel(), true)
	ctx := context.Background()
	suspendAutoRelease(t, r)
	path := filepath.Join(r.config.Root, "version.txt")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "inspect", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	admitParent(t, r)
	before, _ := r.read(ctx)
	original := before.Tasks[first.Task]
	if err := r.Cleanup(ctx, first.Context); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("refreshed"), 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := r.manager(ctx)
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := manager.Capture(ctx, r.config.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.pinIntegrationSnapshot(ctx, refreshed); err != nil {
		t.Fatal(err)
	}
	run := func(snapshot string) AgentResult {
		t.Helper()
		next, err := r.Followup(ctx, "", FollowupRequest{Task: first.Task, Question: "explain", Snapshot: snapshot})
		if err != nil {
			t.Fatal(err)
		}
		result, err := r.Agent(ctx, "", AgentRequest{Session: next.Owner, TaskID: next.ID, Task: "explain"})
		if err != nil {
			t.Fatal(err)
		}
		admitParent(t, r)
		return result
	}
	second := run(refreshed.ID)
	s, _ := r.read(ctx)
	count := len(s.Tasks)
	if _, err := r.Followup(ctx, "", FollowupRequest{Task: first.Task, Question: "implicit refresh?"}); err == nil || !strings.Contains(err.Error(), "requires") {
		t.Fatalf("omitted snapshot bypassed base: %v", err)
	}
	s, _ = r.read(ctx)
	if len(s.Tasks) != count {
		t.Fatal("refused source created a pending task")
	}
	third := run(refreshed.ID)
	if third.Context != second.Context {
		t.Fatal("matching workspace was needlessly recreated")
	}
	if err := r.Cleanup(ctx, ""); err != nil {
		t.Fatal(err)
	}
	fourth := run("")
	s, _ = r.read(ctx)
	c := s.Contexts[fourth.Context]
	if got, err := os.ReadFile(filepath.Join(c.Root, "version.txt")); err != nil || string(got) != "original" {
		t.Fatalf("default source drifted: %q %v", got, err)
	}
	if !reflect.DeepEqual(s.Tasks[first.Task], original) || s.Tasks[fourth.Task].Requirement != original.Requirement {
		t.Fatal("followup changed original result or requirement")
	}
	for _, result := range []AgentResult{second, third} {
		if s.Tasks[result.Task].StartingSnapshot != refreshed.ID {
			t.Fatal("explicit refresh not recorded")
		}
	}
	if s.Tasks[fourth.Task].StartingSnapshot != original.StartingSnapshot {
		t.Fatal("default refresh changed source")
	}
}

func TestLiveSourceSurvivesReleaseContinuationAndReopen(t *testing.T) {
	r := runtimeTest(t, doneModel(), 1, 5)
	ctx := context.Background()
	source := canonicalPath(t, t.TempDir())
	first, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "inspect", ReadOnly: true, Source: source})
	if err != nil {
		t.Fatal(err)
	}
	admitParent(t, r)
	awaitReleased(t, r, first.Session)
	for range 2 {
		next, err := r.Agent(ctx, "", AgentRequest{Session: first.Session, Task: "continue"})
		if err != nil {
			t.Fatal(err)
		}
		if next.Session != first.Session {
			t.Fatal("identity changed")
		}
		admitParent(t, r)
		awaitReleased(t, r, first.Session)
	}
	if err := r.Forget(ctx); err != nil {
		t.Fatal(err)
	} // Live provenance is independent of Git refs.
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(r.config)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	task, err := reopened.Followup(ctx, "", FollowupRequest{Task: first.Task, Question: "after reopen"})
	if err != nil {
		t.Fatal(err)
	}
	next, err := reopened.Agent(ctx, "", AgentRequest{Session: task.Owner, TaskID: task.ID, Task: "after reopen"})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := reopened.read(ctx)
	if s.Contexts[next.Context].Root != source {
		t.Fatal("restoration used parent root")
	}
	for _, e := range s.Executions {
		if e.SourceRoot != source || e.Base != "" {
			t.Fatalf("lost durable live source: %+v", e)
		}
		if e.ID != first.Execution && e.Request.Source != "" {
			t.Fatal("fixture did not exercise omitted source")
		}
	}
	for _, task := range s.Tasks {
		if task.SourceRoot != source {
			t.Fatal("task lost its live source")
		}
	}
	admitParent(t, reopened)
	awaitReleased(t, reopened, first.Session)
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Followup(ctx, "", FollowupRequest{Task: first.Task, Question: "missing root"}); err == nil || !strings.Contains(err.Error(), "saved live root is unavailable") {
		t.Fatalf("missing root fell back: %v", err)
	}
}

func TestForgottenAndPrunedSourcesRefuseBothRoles(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		for _, unavailable := range []string{"forgotten", "pruned"} {
			t.Run(map[bool]string{true: "research", false: "editor"}[readOnly]+"/"+unavailable, func(t *testing.T) {
				r, a, ref := noEditResult(t, readOnly)
				ctx := context.Background()
				suspendAutoRelease(t, r)
				if readOnly {
					if err := r.Review(ctx, ref.Task, ref.Revision, true, ""); err != nil {
						t.Fatal(err)
					}
				} else {
					integrateOK(t, r, IntegrateRequest{Tasks: []TaskReference{ref}})
				}
				if unavailable == "forgotten" {
					if err := r.Forget(ctx); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := r.update(ctx, func(s *State) error {
						task := s.Tasks[a.Task]
						base, _ := followupSource(task, readOnly, "")
						s.Snapshots[base].Commit = strings.Repeat("0", 40)
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := r.Followup(ctx, "", FollowupRequest{Task: a.Task, Question: "explain"}); err == nil || !strings.Contains(err.Error(), "snapshot is unavailable") {
					t.Fatalf("unavailable followup: %v", err)
				}
				if _, err := r.Agent(ctx, "", AgentRequest{Session: a.Session, Task: "explain"}); err == nil || !strings.Contains(err.Error(), "snapshot is unavailable") {
					t.Fatalf("unavailable continuation: %v", err)
				}
			})
		}
	}
}

func TestFollowupToolCallIsIdempotent(t *testing.T) {
	r := runtimeTest(t, doneModel(), 1, 4)
	ctx := context.Background()
	suspendAutoRelease(t, r)
	first, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "inspect", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	admitParent(t, r)
	r.RegisterParentTools(r.config.Registry)
	tool, _, _ := r.config.Registry.GetIfAllowed("swarm_followup")
	ctx = subagent.WithCallID(ctx, "same-call")
	args := map[string]any{"task": first.Task, "question": "explain"}
	if _, err := tool.Execute(ctx, args); err != nil {
		t.Fatal(err)
	}
	s, _ := r.read(ctx)
	beforeTasks, beforeExecutions := len(s.Tasks), len(s.Executions)
	if _, err := tool.Execute(ctx, args); err != nil {
		t.Fatal(err)
	}
	s, _ = r.read(ctx)
	if len(s.Tasks) != beforeTasks || len(s.Executions) != beforeExecutions {
		t.Fatal("replayed call duplicated work")
	}
}

func TestFailedDormantLaunchRollsBackFreshWorkspace(t *testing.T) {
	r := runtimeTest(t, doneModel(), 1, 3)
	ctx := context.Background()
	first, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "inspect", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	admitParent(t, r)
	awaitReleased(t, r, first.Session)
	// This is discovered by the launch transaction after restoration. It must
	// not leave a checkout or scratch allocation behind on refusal.
	blocked, err := r.CreateTask(ctx, "blocked by a dependency", "", []string{"missing"}, first.Session)
	if err == nil {
		t.Fatal("invalid dependency unexpectedly created", blocked)
	}
	if err := r.update(ctx, func(s *State) error {
		s.Tasks["blocked"] = &Task{ID: "blocked", Owner: first.Session, Run: s.Tasks[first.Task].Run, Requirement: RequirementDelivered, Status: "pending", Revision: 1, Dependencies: []string{"missing"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Agent(ctx, "", AgentRequest{Session: first.Session, TaskID: "blocked", Task: "try blocked work"}); err == nil {
		t.Fatal("blocked task launched")
	}
	s := awaitReleased(t, r, first.Session)
	if len(s.Contexts) != 0 || len(s.Executions) != 1 {
		t.Fatal("failed launch leaked restored resources")
	}
}

func TestEditingFollowupRefusesExtraEditsOnMatchingBase(t *testing.T) {
	r, a, ref := noEditResult(t, false)
	ctx := context.Background()
	suspendAutoRelease(t, r)
	if _, err := r.Integrate(ctx, IntegrateRequest{Tasks: []TaskReference{ref}}); err != nil {
		t.Fatal(err)
	}
	s, _ := r.read(ctx)
	c := s.Contexts[a.Context]
	if err := os.WriteFile(filepath.Join(c.Root, "extra.txt"), []byte("local edits"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Followup(ctx, "", FollowupRequest{Task: a.Task, Question: "next", Snapshot: c.Checkout.Base.ID}); err == nil || !strings.Contains(err.Error(), "additional edits") {
		t.Fatalf("editing followup lost local edits: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.Root, "extra.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestLiveSourceKindSurvivesParentGitInitialization(t *testing.T) {
	skipIfWindows(t)
	ctx := context.Background()
	r := runtimeTest(t, doneModel(), 1, 3)
	source := canonicalPath(t, t.TempDir())
	a, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "inspect", ReadOnly: true, Source: source})
	if err != nil {
		t.Fatal(err)
	}
	admitParent(t, r)
	awaitReleased(t, r, a.Session)
	cmd := exec.Command("git", "init", "-q", r.config.Root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	b, err := r.Agent(ctx, "", AgentRequest{Session: a.Session, Task: "continue live work"})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := r.read(ctx)
	c := s.Contexts[b.Context]
	if c.Checkout != nil || c.Root != source || s.Executions[b.Execution].Base != "" {
		t.Fatal("a live task silently became a Git snapshot")
	}
}
