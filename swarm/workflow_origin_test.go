package swarm

import (
	"context"
	"testing"
)

func TestWorkflowRetainsLaunchCallAcrossCheckpoints(t *testing.T) {
	r := runtimeTest(t, nilModel(), 1, 3)
	ctx := WithWorkflowCallID(context.Background(), "parent-tool-call")
	report, err := r.RunWorkflow(ctx, `polly.defineWorkflow({name:"origin",inputSchema:polly.schema.object({}),async run(){await polly.log("checkpoint");return "done";}});`, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	state, err := r.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Workflows[report.ID]; got.CallID != "parent-tool-call" || got.Status != "completed" || len(got.Steps) != 1 {
		t.Fatalf("workflow lost launch provenance: %+v", got)
	}
	// The normal runner report does not carry host metadata. Later checkpoints
	// must retain the originally recorded origin even if a caller supplies one.
	report.CallID = "different-call"
	if err := r.SaveWorkflow(context.Background(), *report); err != nil {
		t.Fatal(err)
	}
	state, err = r.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Workflows[report.ID].CallID != "parent-tool-call" {
		t.Fatal("checkpoint redirected workflow origin")
	}
}
