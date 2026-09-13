package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/workflow"
)

func TestCandidateReceiptJavaScriptContract(t *testing.T) {
	r, base, _ := integrateFixture(t)
	ref := submittedInput(t, r, base, map[string]string{"a.txt": "updated\n"})
	source := `polly.defineWorkflow({name:"receipts",inputSchema:polly.schema.object({task:polly.schema.string(),revision:polly.schema.integer()}),async run(input){
 function noAttempt(c) { if(!Object.prototype.hasOwnProperty.call(c,"receipt") || c.receipt!==null) polly.fail("missing explicit null receipt",c); }
 const prepared=await polly.integration.prepare({tasks:[input]}); noAttempt(prepared);
 const refreshed=await polly.integration.refresh(prepared.id); noAttempt(refreshed);
 const before=await polly.integration.read(prepared.id); noAttempt(before);
 const applied=await polly.integrate({candidate:prepared.id});
 const after=await polly.integration.read(prepared.id);
 if(applied.receipt.status!=="applied" || after.receipt.status!=="applied" || after.receipt.id!==prepared.id) polly.fail("missing applied receipt",after);
 return {before,after,applied,user:{applyReceipt:"user-owned",snapshot:"unchanged"}};
 }});`
	report, err := r.RunWorkflow(context.Background(), source, ref)
	if err != nil {
		t.Fatal(err)
	}
	output := report.Output.(map[string]any)
	if output["before"].(map[string]any)["receipt"] != nil || output["user"].(map[string]any)["applyReceipt"] != "user-owned" {
		t.Fatalf("unexpected output: %+v", output)
	}
}

func TestCandidateReceiptInJavaScriptErrorAndWorkflowAttachment(t *testing.T) {
	r, base, _ := integrateFixture(t)
	ctx := context.Background()
	a := submittedInput(t, r, base, map[string]string{"a.txt": "alpha\n"})
	b := submittedInput(t, r, base, map[string]string{"a.txt": "beta\n"})
	source := `polly.defineWorkflow({name:"conflict-receipt",inputSchema:polly.schema.object({tasks:polly.schema.array(polly.schema.object({task:polly.schema.string(),revision:polly.schema.integer()}))}),async run(input){
 try { await polly.integrate(input); }
 catch(error) {
  if(error.code!=="conflicts" || !Object.prototype.hasOwnProperty.call(error.result,"receipt") || error.result.receipt!==null) throw error;
  return {candidate:error.result,user:{applyReceipt:"untouched",receipt:"user receipt",snapshot:"user snapshot"},padding:"full evidence".repeat(2000)};
 }
 polly.fail("expected a conflict");
}});`
	r.RegisterParentTools(r.config.Registry)
	runner, _, _ := r.config.Registry.GetIfAllowed("workflow_run")
	output, err := runner.(tools.OutputTool).ExecuteOutput(ctx, map[string]any{"source": source, "input": tools.Result(map[string]any{"tasks": []TaskReference{a, b}})})
	if err != nil || len(output.Media) != 1 {
		t.Fatalf("workflow attachment: %+v %v", output, err)
	}
	var summary struct {
		Output struct {
			Candidate map[string]any `json:"candidate"`
			User      map[string]any `json:"user"`
		} `json:"output"`
	}
	if err := json.Unmarshal(output.Media[0].Data, &summary); err != nil {
		t.Fatal(err)
	}
	if receipt, exists := summary.Output.Candidate["receipt"]; !exists || receipt != nil {
		t.Fatal("actual JavaScript error/attachment lost its explicit null receipt")
	}
	if summary.Output.User["receipt"] != "user receipt" || summary.Output.User["applyReceipt"] != "untouched" || summary.Output.User["snapshot"] != "user snapshot" {
		t.Fatal("public projection rewrote user-owned objects")
	}
	if output.Data != nil {
		t.Fatal("direct Go tool call without a parent origin acquired terminal delivery metadata")
	}
	assertWorkflowDelivery(t, r, false, false)
}

func TestCandidateReceiptUnchangedRefreshKeepsRecordedAttempt(t *testing.T) {
	r, base, _ := integrateFixture(t)
	ctx := context.Background()
	ref := submittedInput(t, r, base, map[string]string{"a.txt": "updated\n"})
	candidate, err := r.PrepareIntegration(ctx, []TaskReference{ref}, "paths")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.update(ctx, func(s *State) error {
		s.Applies[candidate.ID] = &ApplyRecord{ID: candidate.ID, Status: "not_applied", Plan: candidate.Plan}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	source := `polly.defineWorkflow({name:"refresh-receipt",inputSchema:polly.schema.object({id:polly.schema.string()}),async run({id}){
 const before=await polly.integration.read(id);
 const after=await polly.integration.refresh(id);
 if(after.changed || before.receipt.status!=="not_applied" || after.receipt===null || after.receipt.status!=="not_applied") polly.fail("unchanged refresh lost its recorded attempt",after);
 return after;
}});`
	if _, err := r.RunWorkflow(ctx, source, map[string]any{"id": candidate.ID}); err != nil {
		t.Fatalf("recorded attempt: %v", err)
	}
	// Error candidates can also be raw records rather than ReadIntegration's
	// hydrated copy. The public boundary must use the same authoritative receipt.
	_, err = r.publicResult(ctx, nil, &workflow.Error{Code: "test", Message: "inspect", Result: candidate})
	var failure *workflow.Error
	if !errors.As(err, &failure) || failure.Result.(*IntegrationView).Receipt == nil || failure.Result.(*IntegrationView).Receipt.Status != "not_applied" {
		t.Fatalf("error candidate lost its recorded attempt: %v", err)
	}
}

func TestCandidateReceiptProjectionErrorsAndArtifacts(t *testing.T) {
	r, plan := applyFixture(t, false)
	ctx := context.Background()
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	candidate := s.Integrations[plan.ID]
	if strings.Contains(tools.Result(candidate), `"receipt"`) {
		t.Fatal("Go candidate serialization gained a null receipt")
	}
	before := tools.Result(candidate)
	_, err = r.publicResult(ctx, nil, &workflow.Error{Code: "test", Message: "candidate needs inspection", Result: candidate})
	var failure *workflow.Error
	if !errors.As(err, &failure) || !strings.Contains(tools.Result(failure.Result), `"receipt":null`) {
		t.Fatalf("structured error omitted receipt: %v", err)
	}
	if tools.Result(candidate) != before {
		t.Fatal("public error projection mutated stored candidate")
	}
	selected, err := selectInspection(failure.Result, "/receipt")
	if err != nil || selected != nil {
		t.Fatalf("null receipt pointer: %#v, %v", selected, err)
	}
	// Large public results use the same projection in the complete attachment.
	candidate.Drift = strings.Repeat("large candidate ", 3000)
	value, err := r.publicResult(ctx, candidate, nil)
	if err != nil {
		t.Fatal(err)
	}
	text, err := coordinationToolResult(value, nil)
	if err != nil {
		t.Fatal(err)
	}
	output := inspectionOutput("candidate", text)
	if len(output.Media) != 1 || !strings.Contains(string(output.Media[0].Data), `"receipt": null`) {
		t.Fatal("candidate attachment lost its null receipt")
	}
	for _, status := range []string{"applied", "not_applied", "applying", "recovery_required"} {
		t.Run(status, func(t *testing.T) {
			if err := r.update(ctx, func(s *State) error {
				s.Applies[plan.ID] = &ApplyRecord{ID: plan.ID, Status: status, Plan: plan}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			report, err := r.RunWorkflow(ctx, `polly.defineWorkflow({name:"read-receipt",inputSchema:polly.schema.object({id:polly.schema.string()}),async run({id}){return (await polly.integration.read(id)).receipt}})`, map[string]any{"id": plan.ID})
			if err != nil || report.Output.(map[string]any)["status"] != status {
				t.Fatalf("receipt status: %+v %v", report, err)
			}
		})
	}
	if text := tools.Result(&integrationOutcomeView{Status: "done"}); strings.Contains(text, `"receipt"`) {
		t.Fatal("unchanged outcome requires an apply receipt")
	}
}
