package swarm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools"
)

// swarm_status exists for the parent and for members, always allowed, with a
// view scoped to the actor.
func TestSwarmStatusToolRegisteredForBothActors(t *testing.T) {
	r := runtimeTest(t, nilModel(), 1, 2)
	ctx := context.Background()
	if err := r.update(ctx, func(s *State) error {
		s.Members["m"] = &Member{ID: "m", Name: "m", Label: "worker"}
		s.Messages["q"] = &Mail{ID: "q", From: r.ID, To: "m", Kind: "request", Text: "status?"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out, err := execParentTool(t, r, "swarm_status", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var parent statusSummary
	if err := json.Unmarshal([]byte(out), &parent); err != nil || parent.Next != "No swarm work yet." || parent.Counts.Dormant != 1 {
		t.Fatalf("parent status = %s (%v)", out, err)
	}
	parentTool, _, _ := r.config.Registry.GetIfAllowed("swarm_status")
	if desc := parentTool.GetSchema().Description(); !strings.Contains(desc, "needs your decision") {
		t.Fatalf("parent description: %s", desc)
	}
	registry := tools.NewToolRegistry(nil, tools.WithUnsafeNoSandbox())
	defer registry.Close()
	r.registerMemberTools(registry, "m", "", false)
	memberTool, _, _ := registry.GetIfAllowed("swarm_status")
	if memberTool == nil {
		t.Fatal("member registry lacks swarm_status")
	}
	if desc := memberTool.GetSchema().Description(); !strings.Contains(desc, "awaiting your reply") {
		t.Fatalf("member description: %s", desc)
	}
	out, err = memberTool.Execute(ctx, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var member statusSummary
	if err := json.Unmarshal([]byte(out), &member); err != nil || len(member.NeedsDecision) != 1 || member.NeedsDecision[0].ID != "q" || !strings.Contains(member.NeedsDecision[0].Action, `reply_to: "q"`) {
		t.Fatalf("member status = %s (%v)", out, err)
	}
	if _, err := memberTool.Execute(ctx, map[string]any{"section": "nope"}); err == nil {
		t.Fatal("unknown section accepted")
	}
}

// The parent's swarm_wait returns the status summary, so no separate status
// call is needed after it wakes.
func TestParentSwarmWaitReturnsStatus(t *testing.T) {
	r := runtimeTest(t, nilModel(), 1, 2)
	ctx := context.Background()
	out, err := execParentTool(t, r, "swarm_wait", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var status statusSummary
	if err := json.Unmarshal([]byte(out), &status); err != nil || status.Next != "No swarm work yet." {
		t.Fatalf("idle wait = %s (%v)", out, err)
	}
	task, err := r.CreateTask(ctx, "pending work", "review", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	out, err = execParentTool(t, r, "swarm_wait", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out), &status); err != nil || status.Next != "task "+task.ID+" revision 1: assign and run the task or cancel it" || status.Counts.NeedsDecision != 1 || status.Budget == nil || status.Budget.Unit != "starts" {
		t.Fatalf("wait with a pending task = %s (%v)", out, err)
	}
}

// Counts are totals; the summary pages its lists within the inspection
// budget and the sections page through everything.
func TestStatusCountsAreTotals(t *testing.T) {
	r := runtimeTest(t, nilModel(), 1, 2)
	ctx := context.Background()
	if err := r.update(ctx, func(s *State) error {
		s.Runs["run"] = &Run{ID: "run", Status: "running", Starts: 1, Limit: 8}
		for i := range 180 {
			id := fmt.Sprintf("task-%03d", i)
			s.Tasks[id] = &Task{ID: id, Run: "run", Status: "pending", Revision: 1, Description: strings.Repeat("d", 200)}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.RegisterParentTools(r.config.Registry)
	tool, _, _ := r.config.Registry.GetIfAllowed("swarm_status")
	out, err := tool.(tools.OutputTool).ExecuteOutput(ctx, map[string]any{})
	if err != nil || len(out.Text) > inspectionBytes || len(out.Media) != 0 {
		t.Fatalf("summary: %d bytes, %d media, %v", len(out.Text), len(out.Media), err)
	}
	var status statusSummary
	if err := json.Unmarshal([]byte(out.Text), &status); err != nil {
		t.Fatal(err)
	}
	if status.Counts.NeedsDecision != 180 || len(status.NeedsDecision) >= 180 || len(status.NeedsDecision) == 0 || status.NeedsDecisionNext != len(status.NeedsDecision)+1 || !strings.HasPrefix(status.Next, "task task-000 revision 1:") {
		t.Fatalf("summary totals: counts %+v, page %d, next %d, %q", status.Counts, len(status.NeedsDecision), status.NeedsDecisionNext, status.Next)
	}
	offset, seen := 1, 0
	for {
		out, err := tool.(tools.OutputTool).ExecuteOutput(ctx, map[string]any{"section": "decisions", "offset": offset, "limit": 100})
		if err != nil || len(out.Text) > inspectionBytes || len(out.Media) != 0 {
			t.Fatalf("decisions page: %d bytes, %v", len(out.Text), err)
		}
		var page inspectionPage
		if err := json.Unmarshal([]byte(out.Text), &page); err != nil {
			t.Fatal(err)
		}
		if page.Total != 180 || len(page.Items) == 0 {
			t.Fatalf("page total %d, items %d", page.Total, len(page.Items))
		}
		seen += len(page.Items)
		if page.Next == 0 {
			break
		}
		if page.Next <= offset {
			t.Fatal("repeated page")
		}
		offset = page.Next
	}
	if seen != 180 {
		t.Fatalf("paging lost entries: %d", seen)
	}
	if c := StatusCounts(func() *State { s, _ := r.read(ctx); return s }(), r.ID); c.NeedsDecision != 180 {
		t.Fatalf("StatusCounts = %+v", c)
	}
}
