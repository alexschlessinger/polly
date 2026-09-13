package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/workflow"
	"github.com/alexschlessinger/pollytool/worktree"
)

func integrateFixture(t *testing.T) (*Runtime, worktree.Snapshot, *countingSession) {
	t.Helper()
	r, plan := applyFixture(t, false)
	suspendAutoRelease(t, r)
	if err := r.update(context.Background(), func(s *State) error {
		clear(s.Tasks)
		clear(s.Integrations)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	counter := newCountingSession(r.config.Parent)
	r.parent = counter
	return r, plan.Parent, counter
}

func integrateOK(t *testing.T, r *Runtime, req IntegrateRequest) *IntegrationOutcome {
	t.Helper()
	out, err := r.Integrate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertIntegrationDone(t *testing.T, r *Runtime, refs []TaskReference) {
	t.Helper()
	s, err := r.read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if task := s.Tasks[ref.Task]; task.Status != "done" || task.AcceptedRevision != ref.Revision {
			t.Fatalf("task not accepted and done: %+v", task)
		}
	}
}

func TestIntegrateAppliesBatchInOneCall(t *testing.T) {
	r, base, _ := integrateFixture(t)
	ctx := context.Background()
	var refs []TaskReference
	for _, path := range []string{"one.txt", "two.txt", "three.txt"} {
		refs = append(refs, submittedInput(t, r, base, map[string]string{path: path + "\n"}))
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = r.config.Root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
		return string(out)
	}
	head, index := git("rev-parse", "HEAD"), git("ls-files", "--stage")
	out := integrateOK(t, r, IntegrateRequest{Tasks: refs})
	if out.Status != "applied" || out.Candidate == "" || out.Receipt == nil || !reflect.DeepEqual(out.Tasks, refs) || !reflect.DeepEqual(out.Receipt.Tasks, refs) {
		t.Fatalf("outcome %+v", out)
	}
	s, _ := r.read(ctx)
	if len(s.Integrations) != 1 || len(s.Applies) != 1 || !s.Integrations[out.Candidate].Accepted || s.Integrations[out.Candidate].Status != "applied" {
		t.Fatal("candidate/receipt not saved once")
	}
	for _, path := range []string{"one.txt", "two.txt", "three.txt"} {
		if data, _ := os.ReadFile(filepath.Join(r.config.Root, path)); string(data) != path+"\n" {
			t.Fatalf("missing %s", path)
		}
	}
	if git("rev-parse", "HEAD") != head || git("ls-files", "--stage") != index {
		t.Fatal("integration changed HEAD or index")
	}
	assertIntegrationDone(t, r, refs)
	if err := assertSettleMatchesBlockers(t, r); err != nil {
		t.Fatal(err)
	}
	if n, err := r.releaseWorkspaces(ctx); err != nil || n != 3 {
		t.Fatalf("release=%d, %v", n, err)
	}
	s, _ = r.read(ctx)
	for _, ref := range refs {
		if s.Members[ref.Task].Context != "" {
			t.Fatal("workspace retained")
		}
	}
}

func TestIntegrateUnchangedCompletesWithoutApply(t *testing.T) {
	r, base, counter := integrateFixture(t)
	ctx := context.Background()
	ref := submittedInput(t, r, base, nil)
	req := IntegrateRequest{Tasks: []TaskReference{ref}}
	before := counter.updates.Load()
	out := integrateOK(t, r, req)
	if out.Status != "done" || out.Candidate != "" || out.Receipt != nil || !reflect.DeepEqual(out.Unchanged, []string{ref.Task}) || counter.updates.Load()-before != 1 {
		t.Fatalf("outcome %+v", out)
	}
	assertIntegrationDone(t, r, req.Tasks)
	replay := func() {
		t.Helper()
		before := counter.updates.Load()
		if again := integrateOK(t, r, req); !reflect.DeepEqual(again, out) || counter.updates.Load() != before {
			t.Fatal("done replay changed outcome or wrote")
		}
	}
	replay()
	var cb llm.AgentCallbacks
	r.bindParent(&cb, nil)
	if _, err := cb.ContinueAfterFinal(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.releaseWorkspaces(ctx); err != nil {
		t.Fatal(err)
	}
	s, _ := r.read(ctx)
	if s.Runs[s.Tasks[ref.Task].Run].Status != "completed" || s.Members[ref.Task].Context != "" {
		t.Fatal("run/resources not ended")
	}
	replay()
	_, err := r.Integrate(ctx, IntegrateRequest{Tasks: []TaskReference{{Task: ref.Task, Revision: ref.Revision + 1}}})
	candidateError(t, err, "stale_task")
	_, err = r.Integrate(ctx, IntegrateRequest{Tasks: req.Tasks, Drift: "invalid"})
	candidateError(t, err, "invalid_args")
	if err := r.Forget(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = r.Integrate(ctx, req)
	candidateError(t, err, "stale_task")
	s, _ = r.read(ctx)
	if len(s.Applies) != 0 || len(s.Integrations) != 0 {
		t.Fatal("unchanged integration allocated a candidate or apply")
	}
}

func TestIntegrateUnchangedPreparedCandidateDoesNotBlockForget(t *testing.T) {
	for _, route := range []string{"tasks", "candidate", "restored"} {
		t.Run(route, func(t *testing.T) {
			r, base, counter := integrateFixture(t)
			ctx := context.Background()
			ref := submittedInput(t, r, base, nil)
			c, err := r.PrepareIntegration(ctx, []TaskReference{ref}, "")
			if err != nil {
				t.Fatal(err)
			}
			s, _ := r.read(ctx)
			if completedUnchangedCandidate(s, c) {
				t.Fatal("unaccepted no-op is outstanding")
			}
			switch route {
			case "tasks":
				integrateOK(t, r, IntegrateRequest{Tasks: []TaskReference{ref}})
			case "candidate":
				integrateOK(t, r, IntegrateRequest{Candidate: c.ID})
			case "restored":
				if err := r.update(ctx, func(s *State) error {
					s.Tasks[ref.Task].AcceptedRevision = ref.Revision
					s.Tasks[ref.Task].Status = "done"
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
				r, err = New(r.config)
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				suspendAutoRelease(t, r)
				counter = newCountingSession(r.config.Parent)
				r.parent = counter
			}
			s, _ = r.read(ctx)
			if !completedUnchangedCandidate(s, s.Integrations[c.ID]) || len(s.Applies) != 0 {
				t.Fatal("completed no-op still outstanding or applied")
			}
			before := counter.updates.Load()
			out := integrateOK(t, r, IntegrateRequest{Candidate: c.ID})
			if out.Status != "done" || out.Candidate != c.ID || out.Receipt != nil || counter.updates.Load() != before {
				t.Fatalf("candidate replay %+v", out)
			}
			if p := Present(s, r.ID, r.ID); len(p.Decisions) != 0 {
				t.Fatalf("completed candidate has decisions %+v", p)
			}
			if err := assertSettleMatchesBlockers(t, r); err != nil {
				t.Fatal(err)
			}
			if _, err := r.releaseWorkspaces(ctx); err != nil {
				t.Fatal(err)
			}
			if err := r.Forget(ctx); err != nil {
				t.Fatal(err)
			}
			s, _ = r.read(ctx)
			if len(s.Applies) != 0 || len(s.Snapshots) != 0 || s.Integrations[c.ID] == nil {
				t.Fatal("forget invented an apply or removed history")
			}
			_, err = r.Integrate(ctx, IntegrateRequest{Candidate: c.ID})
			candidateError(t, err, "stale_task")
		})
	}
}

func TestIntegrateMixedBatch(t *testing.T) {
	r, base, _ := integrateFixture(t)
	unchanged := submittedInput(t, r, base, nil)
	changed := submittedInput(t, r, base, map[string]string{"new.txt": "new\n"})
	refs := []TaskReference{unchanged, changed}
	out := integrateOK(t, r, IntegrateRequest{Tasks: refs})
	if out.Status != "applied" || !reflect.DeepEqual(out.Unchanged, []string{unchanged.Task}) || len(out.Receipt.Tasks) != 2 {
		t.Fatalf("outcome %+v", out)
	}
	assertIntegrationDone(t, r, refs)
	_, err := r.PrepareIntegration(context.Background(), []TaskReference{changed}, "")
	candidateError(t, err, "stale_task")
	_, err = r.Integrate(context.Background(), IntegrateRequest{Tasks: []TaskReference{changed}})
	candidateError(t, err, "stale_task")
}

func TestIntegrateRefusesWithCurrentRevision(t *testing.T) {
	cases := []struct {
		name, code, message string
		change              func(*State, *IntegrateRequest, TaskReference)
	}{
		{"unknown", "unknown_task", "unknown", func(s *State, q *IntegrateRequest, r TaskReference) { q.Tasks[0].Task = "missing" }},
		{"stale", "stale_task", "revision 1", func(s *State, q *IntegrateRequest, r TaskReference) { q.Tasks[0].Revision = 2 }},
		{"running", "stale_task", "running at revision 1", func(s *State, q *IntegrateRequest, r TaskReference) { s.Tasks[r.Task].Status = "running" }},
		{"done_changed", "stale_task", "applied candidate", func(s *State, q *IntegrateRequest, r TaskReference) {
			s.Tasks[r.Task].Status = "done"
			s.Tasks[r.Task].AcceptedRevision = 1
		}},
		{"delivered", "invalid_args", "delivery", func(s *State, q *IntegrateRequest, r TaskReference) {
			s.Tasks[r.Task].Requirement = RequirementDelivered
		}},
		{"reviewed", "invalid_args", "swarm_review", func(s *State, q *IntegrateRequest, r TaskReference) {
			s.Tasks[r.Task].Requirement = RequirementReviewed
		}},
		{"missing_snapshot", "stale_task", "followup_task", func(s *State, q *IntegrateRequest, r TaskReference) { s.Tasks[r.Task].Snapshot = "" }},
		{"missing_proof", "stale_task", "provenance", func(s *State, q *IntegrateRequest, r TaskReference) {
			delete(s.Snapshots, s.Tasks[r.Task].StartingSnapshot)
		}},
		{"both", "invalid_args", "task revisions or a candidate", func(s *State, q *IntegrateRequest, r TaskReference) { q.Candidate = "candidate" }},
		{"neither", "invalid_args", "task revisions or a candidate", func(s *State, q *IntegrateRequest, r TaskReference) { q.Tasks = nil }},
		{"empty_task", "invalid_args", "nonempty", func(s *State, q *IntegrateRequest, r TaskReference) { q.Tasks[0].Task = "" }},
		{"zero_revision", "invalid_args", "positive", func(s *State, q *IntegrateRequest, r TaskReference) { q.Tasks[0].Revision = 0 }},
		{"duplicate", "invalid_args", "unique", func(s *State, q *IntegrateRequest, r TaskReference) { q.Tasks = append(q.Tasks, r) }},
		{"invalid_drift", "invalid_args", "drift", func(s *State, q *IntegrateRequest, r TaskReference) { q.Drift = "invalid" }},
		{"applying", "recovery_required", "uncertain", func(s *State, q *IntegrateRequest, r TaskReference) {
			s.Applies["uncertain"] = &ApplyRecord{ID: "uncertain", Status: "applying"}
		}},
		{"recovery_required", "recovery_required", "reconcile", func(s *State, q *IntegrateRequest, r TaskReference) {
			s.Applies["uncertain"] = &ApplyRecord{ID: "uncertain", Status: "recovery_required"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, base, counter := integrateFixture(t)
			ref := submittedInput(t, r, base, map[string]string{"a.txt": "changed\n"})
			req := IntegrateRequest{Tasks: []TaskReference{ref}}
			if err := r.update(context.Background(), func(s *State) error { tc.change(s, &req, ref); return nil }); err != nil {
				t.Fatal(err)
			}
			before, _ := r.read(context.Background())
			writes := counter.updates.Load()
			_, err := r.Integrate(context.Background(), req)
			candidateError(t, err, tc.code)
			if !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("guidance %v", err)
			}
			after, _ := r.read(context.Background())
			if !reflect.DeepEqual(before, after) || counter.updates.Load() != writes {
				t.Fatal("refusal mutated state")
			}
		})
	}
	// Invalid drift cannot disappear through the no-op fast path.
	r, base, counter := integrateFixture(t)
	ref := submittedInput(t, r, base, nil)
	before := counter.updates.Load()
	_, err := r.Integrate(context.Background(), IntegrateRequest{Tasks: []TaskReference{ref}, Drift: "invalid"})
	candidateError(t, err, "invalid_args")
	if counter.updates.Load() != before {
		t.Fatal("invalid no-op drift wrote")
	}
}

func TestIntegrateCandidateReplayAndDrift(t *testing.T) {
	r, base, counter := integrateFixture(t)
	ctx := context.Background()
	ref := submittedInput(t, r, base, map[string]string{"a.txt": "new\n"})
	c, err := r.PrepareIntegration(ctx, []TaskReference{ref}, "tree")
	if err != nil {
		t.Fatal(err)
	}
	for _, drift := range []string{"paths", "tree", "invalid"} {
		before := counter.updates.Load()
		_, err = r.Integrate(ctx, IntegrateRequest{Candidate: c.ID, Drift: drift})
		candidateError(t, err, "invalid_args")
		if counter.updates.Load() != before {
			t.Fatal("candidate drift wrote")
		}
	}
	// The legacy accepted candidate is the saved decision after a crash.
	if _, err = r.AcceptIntegration(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(r.config.Root, "other.txt"), []byte("parent\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = r.Integrate(ctx, IntegrateRequest{Candidate: c.ID})
	candidateError(t, err, "parent_changed")
	if !strings.Contains(err.Error(), "refresh") || !strings.Contains(err.Error(), c.ID) {
		t.Fatal(err)
	}
	c, _ = r.ReadIntegration(ctx, c.ID)
	if !c.Accepted || c.Status != "ready" {
		t.Fatal("drift discarded candidate")
	}
	next, err := r.RefreshIntegration(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Integrate(ctx, IntegrateRequest{Candidate: c.ID})
	candidateError(t, err, "superseded")
	_, err = r.Integrate(ctx, IntegrateRequest{Candidate: "missing"})
	candidateError(t, err, "unknown_candidate")
	out := integrateOK(t, r, IntegrateRequest{Candidate: next.ID})
	replay := func() {
		t.Helper()
		before := counter.updates.Load()
		again := integrateOK(t, r, IntegrateRequest{Candidate: next.ID})
		if !reflect.DeepEqual(again, out) || counter.updates.Load() != before {
			t.Fatal("applied replay changed receipt or wrote")
		}
		for _, drift := range []string{"paths", "tree", "invalid"} {
			_, err := r.Integrate(ctx, IntegrateRequest{Candidate: next.ID, Drift: drift})
			candidateError(t, err, "invalid_args")
		}
		if counter.updates.Load() != before {
			t.Fatal("invalid replay wrote")
		}
	}
	replay()
	if _, err = r.releaseWorkspaces(ctx); err != nil {
		t.Fatal(err)
	}
	if err = r.Forget(ctx); err != nil {
		t.Fatal(err)
	}
	replay()
	other := submittedInput(t, r, base, map[string]string{"another.txt": "later\n"})
	if err = r.update(ctx, func(s *State) error {
		s.Applies["uncertain"] = &ApplyRecord{ID: "uncertain", Status: "recovery_required"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	replay()
	_, err = r.Integrate(ctx, IntegrateRequest{Tasks: []TaskReference{other}})
	candidateError(t, err, "recovery_required")
	s, _ := r.read(ctx)
	if s.Applies["uncertain"].Status != "recovery_required" {
		t.Fatal("replay resolved unrelated uncertainty")
	}
}

func TestIntegrateSupersedesEarlierCandidatesForTheSameTasks(t *testing.T) {
	r, base, _ := integrateFixture(t)
	ctx := context.Background()
	ref := submittedInput(t, r, base, map[string]string{"a.txt": "new\n"})
	first, err := r.PrepareIntegration(ctx, []TaskReference{ref}, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.PrepareIntegration(ctx, []TaskReference{ref}, "")
	if err != nil {
		t.Fatal(err)
	}
	old, _ := r.ReadIntegration(ctx, first.ID)
	if old.Status != "superseded" || old.Successor != second.ID {
		t.Fatal("independent preparation did not supersede")
	}
	integrateOK(t, r, IntegrateRequest{Candidate: second.ID})
	// A retained invalid conflict cannot be applied and must not pin snapshots forever.
	if err = r.update(ctx, func(s *State) error {
		c := *old
		c.ID = "stale"
		c.Status = "conflicted"
		c.Successor = ""
		s.Integrations[c.ID] = &c
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = r.releaseWorkspaces(ctx); err != nil {
		t.Fatal(err)
	}
	if err = r.Forget(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrateHoldsOneLockAndAccountsShutdown(t *testing.T) {
	r, base, counter := integrateFixture(t)
	ref := submittedInput(t, r, base, nil)
	unlock, err := r.gate.Exclusive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	before := counter.updates.Load()
	done := make(chan error, 1)
	go func() {
		_, err := r.Integrate(context.Background(), IntegrateRequest{Tasks: []TaskReference{ref}})
		done <- err
	}()
	closed := make(chan error, 1)
	go func() { closed <- r.Close() }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued integrate blocked shutdown")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	if counter.updates.Load() != before {
		t.Fatal("queued canceled integration accepted task")
	}
}

func TestIntegrateAuthorityAndStrictRequests(t *testing.T) {
	r, base, counter := integrateFixture(t)
	ref := submittedInput(t, r, base, nil)
	for _, key := range []string{"actor", "identity", "parent", "controller", "run", "IDentity"} {
		_, err := r.integrateOperation(context.Background(), map[string]any{"tasks": []TaskReference{ref}, key: r.ID})
		candidateError(t, err, "invalid_args")
	}
	_, err := r.integrateOperation(context.Background(), map[string]any{"tasks": []any{map[string]any{"task": ref.Task, "revision": ref.Revision, "actor": r.ID}}})
	candidateError(t, err, "invalid_args")
	r.RegisterParentTools(r.config.Registry)
	before := counter.updates.Load()
	text, err := execParentTool(t, r, "swarm_integrate", map[string]any{"tasks": []TaskReference{ref}})
	if err != nil {
		t.Fatal(err)
	}
	var out IntegrationOutcome
	if err = json.Unmarshal([]byte(text), &out); err != nil || out.Status != "done" || counter.updates.Load()-before != 1 {
		t.Fatalf("tool: %s %v", text, err)
	}
	// The trusted workflow operation uses the same strict contract and retry path.
	h := &workflowHost{runtime: r, controller: "workflow"}
	defer h.close()
	before = counter.updates.Load()
	v, err := h.Call(context.Background(), workflow.Operation{Kind: "integrate", Args: map[string]any{"tasks": []TaskReference{ref}}})
	encoded, encodeErr := json.Marshal(v)
	decodeErr := json.Unmarshal(encoded, &out)
	if err != nil || encodeErr != nil || decodeErr != nil || out.Status != "done" || counter.updates.Load() != before {
		t.Fatalf("host replay: %+v %v", v, err)
	}
}

func TestIntegrateConflictHaltsAsOneDecision(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "editors", true: "with_completed_unchanged"}[mixed], func(t *testing.T) {
			r, base, _ := integrateFixture(t)
			ctx := context.Background()
			refs := []TaskReference{submittedInput(t, r, base, map[string]string{"a.txt": "first\n"}), submittedInput(t, r, base, map[string]string{"a.txt": "second\n"})}
			var noop TaskReference
			if mixed {
				noop = submittedInput(t, r, base, nil)
				refs = append([]TaskReference{noop}, refs...)
			}
			_, err := r.Integrate(ctx, IntegrateRequest{Tasks: refs})
			var halt *workflow.Error
			if !errors.As(err, &halt) || halt.Code != "conflicts" {
				t.Fatalf("halt: %v", err)
			}
			c := halt.Result.(*IntegrationCandidate)
			if c.Status != "conflicted" || c.Accepted || !strings.Contains(halt.Message, c.ID) || !strings.Contains(halt.Message, c.Merged.Commit) || strings.Contains(halt.Message, c.Merged.ID) || !strings.Contains(halt.Message, "revise") || !strings.Contains(halt.Message, "polly.agent") || !strings.Contains(halt.Message, "a.txt") {
				t.Fatalf("halt %+v", halt)
			}
			s, _ := r.read(ctx)
			for _, ref := range refs {
				task := s.Tasks[ref.Task]
				if task.AcceptedRevision != ref.Revision {
					t.Fatal("conflict lost exact acceptance")
				}
				if ref == noop {
					if TaskStatusIn(s, task) != "done" {
						t.Fatal("completed input labeled halted")
					}
					if p := MemberState(s, s.Members[task.Owner]); p.Display != "idle · done" || p.Attention {
						t.Fatalf("completed member %+v", p)
					}
				} else if task.Status != "awaiting_review" || TaskStatusIn(s, task) != "integration halted" {
					t.Fatalf("unresolved task %+v", task)
				}
			}
			p := Present(s, r.ID, r.ID)
			if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindIntegration || p.Decisions[0].ID != c.ID || p.Decisions[0].Why != "halted · conflicted" || p.Decisions[0].Action != halt.Message {
				t.Fatalf("decisions %+v", p)
			}
			if p := r.ParentState(s); p.Display != "idle · integration halted" {
				t.Fatalf("parent %+v", p)
			}
			if err := assertSettleMatchesBlockers(t, r); err == nil || !strings.Contains(err.Error(), halt.Message) {
				t.Fatalf("settlement: %v", err)
			}
			var cb llm.AgentCallbacks
			r.bindParent(&cb, nil)
			nudge, err := cb.ContinueAfterFinal(ctx, nil)
			if err != nil || len(nudge) == 0 || !strings.Contains(nudge[0].Content, "halted · conflicted") {
				t.Fatalf("nudge %+v %v", nudge, err)
			}
			if err := r.Forget(ctx); err == nil {
				t.Fatal("mixed unresolved candidate allowed forgetting")
			}
			_, err = r.Integrate(ctx, IntegrateRequest{Candidate: c.ID})
			candidateError(t, err, "conflicts")
			repair := submittedInput(t, r, c.Merged, map[string]string{"a.txt": "resolved\n"})
			successor, err := r.ReviseIntegration(ctx, c.ID, repair)
			if err != nil {
				t.Fatal(err)
			}
			s, _ = r.read(ctx)
			p = Present(s, r.ID, r.ID)
			if len(p.Decisions) != 1 || p.Decisions[0].ID != successor.ID || p.Decisions[0].Why != "ready" || !strings.Contains(p.Next, "swarm_integrate") {
				t.Fatalf("repair decision %+v", p)
			}
			out := integrateOK(t, r, IntegrateRequest{Candidate: successor.ID})
			if out.Status != "applied" || len(out.Receipt.Tasks) != len(refs)+1 {
				t.Fatalf("repaired outcome %+v", out)
			}
			assertIntegrationDone(t, r, append(refs, repair))
			if err := assertSettleMatchesBlockers(t, r); err != nil {
				t.Fatal(err)
			}
			s, _ = r.read(ctx)
			if len(Present(s, r.ID, r.ID).Decisions) != 0 || s.Integrations[c.ID].Successor != successor.ID {
				t.Fatal("repaired conflict still outstanding")
			}
			if _, err := r.releaseWorkspaces(ctx); err != nil {
				t.Fatal(err)
			}
			if err := r.Forget(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReviewRefusesEditingAcceptance(t *testing.T) {
	r, base, counter := integrateFixture(t)
	ctx := context.Background()
	ref := submittedInput(t, r, base, map[string]string{"a.txt": "edit\n"})
	before := counter.updates.Load()
	err := r.Review(ctx, ref.Task, ref.Revision, true, "")
	candidateError(t, err, "invalid_args")
	if !strings.Contains(err.Error(), "swarm_integrate") || !strings.Contains(err.Error(), ref.Task) || counter.updates.Load() != before {
		t.Fatal("refusal lost guidance or wrote", err)
	}
	err = r.Review(ctx, ref.Task, ref.Revision+1, true, "")
	candidateError(t, err, "stale_task")
	report, err := r.RunWorkflow(ctx, `polly.defineWorkflow({name:"review refusal",inputSchema:polly.schema.object({task:polly.schema.string(),revision:polly.schema.integer()}),async run(input){try{await polly.tasks.review({...input,accept:true});return "accepted";}catch(e){return {code:e.code,message:e.message};}}})`, map[string]any{"task": ref.Task, "revision": ref.Revision})
	if err != nil {
		t.Fatal(err)
	}
	output := report.Output.(map[string]any)
	if output["code"] != "invalid_args" || !strings.Contains(output["message"].(string), "swarm_integrate") {
		t.Fatalf("JS acceptance %+v", output)
	}
	if err := r.Review(ctx, ref.Task, ref.Revision, false, "fix the edit"); err != nil {
		t.Fatal(err)
	}
	s, _ := r.read(ctx)
	task := s.Tasks[ref.Task]
	if task.Status != "changes_requested" || task.Revision != ref.Revision+1 || task.Feedback != "fix the edit" || task.AcceptedRevision != 0 {
		t.Fatalf("feedback %+v", task)
	}
	found := false
	for _, mail := range s.Messages {
		if mail.To == task.Owner && mail.Kind == "info" && !mail.Start && strings.Contains(mail.Text, "fix the edit") {
			found = true
		}
	}
	if !found {
		t.Fatal("feedback did not address owner")
	}
}

func TestIntegrateUnchangedReplayLeavesUnrelatedUncertainty(t *testing.T) {
	r, base, counter := integrateFixture(t)
	ctx := context.Background()
	ref := submittedInput(t, r, base, nil)
	c, err := r.PrepareIntegration(ctx, []TaskReference{ref}, "")
	if err != nil {
		t.Fatal(err)
	}
	integrateOK(t, r, IntegrateRequest{Tasks: []TaskReference{ref}})
	if err := r.update(ctx, func(s *State) error {
		s.Applies["uncertain"] = &ApplyRecord{ID: "uncertain", Status: "applying"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := counter.updates.Load()
	for _, req := range []IntegrateRequest{{Tasks: []TaskReference{ref}}, {Candidate: c.ID}} {
		out := integrateOK(t, r, req)
		if out.Status != "done" || out.Receipt != nil {
			t.Fatalf("replay %+v", out)
		}
	}
	if counter.updates.Load() != before {
		t.Fatal("no-op replay wrote during uncertain apply")
	}
	s, _ := r.read(ctx)
	if s.Applies["uncertain"].Status != "applying" {
		t.Fatal("no-op replay cleared uncertainty")
	}
}
