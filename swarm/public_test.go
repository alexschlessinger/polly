package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/workflow"
	"github.com/alexschlessinger/pollytool/worktree"
)

func TestPublicViewsKeepProvenancePrivateAndUserDataUntouched(t *testing.T) {
	r, plan := applyFixture(t, false)
	retainAlias(t, r, plan.Parent, plan.Parent.ID)
	retainAlias(t, r, plan.Merged, plan.Merged.ID)
	ctx := context.Background()
	user := map[string]any{"id": "user-id", "snapshot": "user-snapshot", "startingSnapshot": "user-source"}
	if err := r.update(ctx, func(s *State) error {
		s.Tasks["task"].StartingSnapshot = plan.Parent.ID
		s.Tasks["task"].Result = user
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s, _ := r.read(ctx)
	view := PresentTask(s, s.Tasks["task"])
	if view.BaseCommit != plan.Parent.Commit || view.ResultCommit != plan.Merged.Commit || !reflect.DeepEqual(view.Result, user) {
		t.Fatalf("task projection: %+v", view)
	}
	encoded := tools.Result(view)
	for _, id := range []string{plan.Parent.ID, plan.Merged.ID} {
		if strings.Contains(encoded, id) {
			t.Fatalf("task leaked capture ID: %s", encoded)
		}
	}
	if !strings.Contains(tools.Result(s.Tasks["task"]), plan.Parent.ID) {
		t.Fatal("Go serialization changed")
	}
	deleted := s.Snapshots
	s.Snapshots = map[string]*worktree.Snapshot{}
	missing := tools.Result(PresentTask(s, s.Tasks["task"]))
	if strings.Contains(missing, `"baseCommit"`) || strings.Contains(missing, `"resultCommit"`) {
		t.Fatal("missing capture replaced with a guess")
	}
	s.Snapshots = deleted

	candidate := &IntegrationCandidate{ID: "candidate-view", Parent: plan.Parent, Merged: plan.Merged, Plan: plan,
		Inputs:    []IntegrationInput{{TaskReference: TaskReference{Task: "task", Revision: 1}, Base: plan.Parent, Submitted: plan.Merged}},
		Conflicts: []worktree.Conflict{{Base: plan.Parent, Ours: plan.Parent, Theirs: plan.Merged}},
		Receipt:   &ApplyRecord{ID: "candidate-view", Plan: plan, ObservedParent: plan.Parent, Status: "applied"},
	}
	candidate.Repairs, candidate.Pending = candidate.Inputs, candidate.Inputs
	before := tools.Result(candidate)
	for _, raw := range []any{candidate, &IntegrationRefresh{IntegrationCandidate: candidate, Changed: true}, candidate.Receipt, &IntegrationOutcome{Status: "applied", Receipt: candidate.Receipt}, plan.Parent} {
		value, err := r.publicResult(ctx, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		text := tools.Result(value)
		for _, id := range []string{plan.Parent.ID, plan.Merged.ID} {
			if strings.Contains(text, id) {
				t.Fatalf("nested capture ID leaked: %s", text)
			}
		}
		if !strings.Contains(text, plan.Parent.Commit) {
			t.Fatalf("missing Git identity: %s", text)
		}
	}
	if tools.Result(candidate) != before {
		t.Fatal("projection mutated persisted candidate")
	}
	s.Snapshots = nil
	if text := tools.Result(publicValue(s, candidate)); strings.Contains(text, `"commit"`) || !strings.Contains(text, `"id":"candidate-view"`) {
		t.Fatal("forgotten nested capture advertised a commit or lost its candidate:", text)
	}
	s.Snapshots = deleted
	original := &workflow.Error{Code: "conflicts", Message: "repair", Result: candidate, Cause: context.Canceled}
	_, err := r.publicResult(ctx, nil, fmt.Errorf("wrapped: %w", original))
	var failure *workflow.Error
	if !errors.As(err, &failure) || !errors.Is(err, context.Canceled) || strings.Contains(tools.Result(failure.Result), plan.Parent.ID) {
		t.Fatalf("structured error lost semantics or leaked IDs: %v", err)
	}
	if original.Result != candidate {
		t.Fatal("rewrote original error")
	}
	for _, raw := range []any{user, &workflow.Report{Source: "saved snapshot source", Output: user}} {
		before, _ := json.Marshal(raw)
		value, err := r.publicResult(ctx, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		after, _ := json.Marshal(value)
		if string(before) != string(after) {
			t.Fatal("rewrote user data or historical workflow")
		}
	}
}

func TestPublicCommitReceiptRetentionAndForget(t *testing.T) {
	r, base, _ := integrateFixture(t)
	ctx := context.Background()
	ref := submittedInput(t, r, base, map[string]string{"a.txt": "candidate\n"})
	h := &workflowHost{runtime: r}
	defer h.close()
	value, err := h.Call(ctx, workflow.Operation{Kind: "integrate", Args: map[string]any{"tasks": []TaskReference{ref}}})
	if err != nil {
		t.Fatal(err)
	}
	out := value.(*integrationOutcomeView)
	if out.Receipt == nil || out.Receipt.ObservedParent.Commit == "" {
		t.Fatal("apply receipt omitted its retained observed parent")
	}
	for _, capture := range []CommitView{out.Receipt.Plan.Parent, out.Receipt.Plan.Merged, out.Receipt.ObservedParent} {
		if _, err := r.snapshotForCommit(ctx, capture.Commit); err != nil {
			t.Fatalf("receipt exposed an unresolvable commit: %+v %v", capture, err)
		}
	}
	if err := r.Forget(ctx); err != nil {
		t.Fatal(err)
	}
	value, err = h.Call(ctx, workflow.Operation{Kind: "integrate", Args: map[string]any{"candidate": out.Candidate}})
	if err != nil {
		t.Fatal(err)
	}
	forgotten := value.(*integrationOutcomeView)
	if forgotten.Status != "applied" || forgotten.Receipt == nil || strings.Contains(tools.Result(forgotten), `"commit"`) {
		t.Fatal("forgotten receipt exposed an unavailable capture or lost the outcome")
	}
	legacy, err := r.Integrate(ctx, IntegrateRequest{Candidate: out.Candidate})
	if err != nil || legacy.Receipt.ObservedParent.Commit != out.Receipt.ObservedParent.Commit {
		t.Fatal("Go receipt changed after forgetting captures")
	}
}

func TestPublicCommitReaderPointersPaginationAndArtifacts(t *testing.T) {
	r, plan := applyFixture(t, false)
	retainAlias(t, r, plan.Parent, plan.Parent.ID)
	retainAlias(t, r, plan.Merged, plan.Merged.ID)
	ctx := context.Background()
	if err := r.update(ctx, func(s *State) error {
		s.Tasks["task"].StartingSnapshot = plan.Parent.ID
		s.Tasks["task"].Criteria = strings.Repeat("large criteria ", 2000)
		s.Publications["first"] = &Publication{ID: "first", Snapshot: plan.Parent.ID, Text: strings.Repeat("Cache evidence ", 2000)}
		s.Publications["second"] = &Publication{ID: "second", Snapshot: plan.Merged.ID, Text: "cache second"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.RegisterParentTools(r.config.Registry)
	reader, _, _ := r.config.Registry.GetIfAllowed("swarm_read")
	for _, field := range []string{"baseCommit", "resultCommit"} {
		text, err := reader.Execute(ctx, tools.Args{"view": "tasks", "id": "task", "section": "details", "pointer": "/" + field})
		want := plan.Parent.Commit
		if field == "resultCommit" {
			want = plan.Merged.Commit
		}
		if err != nil || text != tools.Result(want) {
			t.Fatalf("%s: %s %v", field, text, err)
		}
	}
	for _, field := range []string{"snapshot", "startingSnapshot"} {
		if _, err := reader.Execute(ctx, tools.Args{"view": "tasks", "id": "task", "section": "details", "pointer": "/" + field}); err == nil {
			t.Fatal("legacy pointer still selects an ID")
		}
	}
	out, err := reader.(tools.OutputTool).ExecuteOutput(ctx, tools.Args{"view": "tasks", "id": "task", "section": "details"})
	if err != nil || len(out.Media) != 1 {
		t.Fatalf("large task artifact: %+v %v", out, err)
	}
	if text := string(out.Media[0].Data); strings.Contains(text, plan.Parent.ID) || !strings.Contains(text, plan.Parent.Commit) {
		t.Fatal("artifact used internal task serialization")
	}
	out, err = reader.(tools.OutputTool).ExecuteOutput(ctx, tools.Args{"view": "publications", "query": "CACHE", "limit": 1})
	if err != nil || len(out.Media) != 1 {
		t.Fatalf("publication artifact: %+v %v", out, err)
	}
	text := string(out.Media[0].Data)
	if strings.Contains(text, plan.Parent.ID) || !strings.Contains(text, plan.Parent.Commit) || !strings.Contains(text, `"next": 2`) {
		t.Fatal(text)
	}
	text, err = reader.Execute(ctx, tools.Args{"view": "publications", "query": "CACHE", "offset": 2})
	if err != nil || !strings.Contains(text, plan.Merged.Commit) || strings.Contains(text, `"snapshot"`) {
		t.Fatalf("next page: %s %v", text, err)
	}
}

func TestPublicCommitSnapshotTaskAndPublicationRoundTrip(t *testing.T) {
	r, base, _ := integrateFixture(t)
	retainAlias(t, r, base, base.ID)
	if _, err := r.config.Registry.LoadToolAuto("bash"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	source := `polly.defineWorkflow({name:"commit views",inputSchema:polly.schema.object({commit:polly.schema.string()}),async run(input){
 const context=await polly.context({commit:input.commit});
 const captured=await polly.snapshot(context);
 if(captured.id!==undefined) polly.fail("internal snapshot ID leaked");
 await polly.exec("git cat-file -e "+captured.commit+"^{commit}",{context});
 const task=await polly.tasks.create({description:"Inspect",review:true});
 const worker=await polly.agent({commit:captured.commit,taskID:task.id,label:"Inspect",task:"inspect",readOnly:true});
 const submitted=await polly.tasks.read(worker.task);
 if(submitted.snapshot!==undefined||submitted.startingSnapshot!==undefined||submitted.baseCommit!==captured.commit) polly.fail("wrong task projection",submitted);
 return {captured,task:await polly.tasks.review({task:submitted.id,revision:submitted.revision,accept:true})};
 }});`
	r.config.Client = doneModel()
	report, err := r.RunWorkflow(ctx, source, map[string]any{"commit": base.Commit})
	if err != nil {
		t.Fatal(err)
	}
	output := report.Output.(map[string]any)
	commit := output["captured"].(map[string]any)["commit"].(string)
	r.RegisterParentTools(r.config.Registry)
	text, err := execParentTool(t, r, "swarm_publish", map[string]any{"text": "captured evidence", "commit": commit})
	if err != nil || !strings.Contains(text, commit) || strings.Contains(text, `"snapshot"`) {
		t.Fatalf("publication: %s %v", text, err)
	}
}
