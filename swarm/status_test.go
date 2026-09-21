package swarm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/tools"
)

// swarm_read exists for the parent and for members, always allowed, with a
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
	out, err := execParentTool(t, r, "swarm_read", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var parent statusSummary
	if err := json.Unmarshal([]byte(out), &parent); err != nil || parent.Next != "No swarm work yet." || parent.Counts.Dormant != 1 {
		t.Fatalf("parent status = %s (%v)", out, err)
	}
	parentTool, _, _ := r.config.Registry.GetIfAllowed("swarm_read")
	if desc := parentTool.GetSchema().Description(); !strings.Contains(desc, "needs your decision") {
		t.Fatalf("parent description: %s", desc)
	}
	registry := tools.NewToolRegistry(nil, tools.WithNativeTools(), tools.WithUnsafeNoSandbox())
	defer registry.Close()
	r.registerMemberTools(registry, "m")
	memberTool, _, _ := registry.GetIfAllowed("swarm_read")
	if memberTool == nil {
		t.Fatal("member registry lacks swarm_read")
	}
	if desc := memberTool.GetSchema().Description(); !strings.Contains(desc, "awaiting your reply") {
		t.Fatalf("member description: %s", desc)
	}
	out, err = memberTool.Execute(ctx, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var member statusSummary
	if err := json.Unmarshal([]byte(out), &member); err != nil || len(member.NeedsDecision) != 1 || member.NeedsDecision[0].ID != "q" || !strings.Contains(member.NeedsDecision[0].Action, `message: <reply>`) {
		t.Fatalf("member status = %s (%v)", out, err)
	}
	if _, err := memberTool.Execute(ctx, map[string]any{"section": "nope"}); err == nil {
		t.Fatal("unknown section accepted")
	}
}

// Waiting reports notifications; inspection remains an explicit read.
func TestParentWaitReturnsNotification(t *testing.T) {
	r := runtimeTest(t, nilModel(), 1, 2)
	out, err := execParentTool(t, r, "wait_agent", map[string]any{})
	var result struct {
		Message  string `json:"message"`
		TimedOut bool   `json:"timed_out"`
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil || result.Message == "" || result.TimedOut {
		t.Fatalf("wait = %s (%v)", out, err)
	}
}

// A wake says which of the two things happened. The last agent finishing is
// both news and an empty swarm, so that case must report both rather than
// choosing: the old combined string said "or" and left the parent guessing.
func TestWakeTextDistinguishesNewsFromAnEmptySwarm(t *testing.T) {
	news := wakeText(parentWake{News: true})
	idle := wakeText(parentWake{Idle: true})
	both := wakeText(parentWake{News: true, Idle: true})
	if strings.Contains(news, "no workers remain") || !strings.Contains(news, "update is available") {
		t.Fatalf("news = %q", news)
	}
	if strings.Contains(idle, "update is available") || !strings.Contains(idle, "No workers remain") {
		t.Fatalf("idle = %q", idle)
	}
	if !strings.Contains(both, "update is available") || !strings.Contains(both, "no workers remain") {
		t.Fatalf("both = %q", both)
	}
}

// An expired park names what is still in flight, so the parent does not have
// to spend a second call on swarm_read to learn it.
func TestWaitTextNamesRunningWorkAndDecisions(t *testing.T) {
	p := Presentation{
		Working: []WorkingItem{
			{Kind: "workflow", ID: "w1", Label: "theme wave 3", State: "running", Agents: 8},
			{Kind: "member", ID: "m1", Label: "docs pass", State: "running"},
		},
		Counts: Counts{Working: 2, NeedsDecision: 3},
	}
	text := waitText(p)
	for _, want := range []string{"theme wave 3", "8 agents", "docs pass", "3 need your decision", "Continue your own work"} {
		if !strings.Contains(text, want) {
			t.Fatalf("waitText missing %q: %q", want, text)
		}
	}
	empty := waitText(Presentation{})
	if !strings.Contains(empty, "Nothing is running") || !strings.Contains(empty, "No decisions are waiting") {
		t.Fatalf("empty = %q", empty)
	}
	// The list is bounded: a large swarm must not turn the notice into a dump.
	var many Presentation
	for i := range 40 {
		many.Working = append(many.Working, WorkingItem{Kind: "member", ID: fmt.Sprint(i), Label: fmt.Sprintf("worker %d", i), State: "running"})
	}
	if bounded := waitText(many); len(bounded) > waitTextBytes || !strings.Contains(bounded, "and 35 more") {
		t.Fatalf("unbounded or miscounted (%d bytes): %q", len(bounded), bounded)
	}
	// The byte cap holds on its own: a few long labels stop the list before
	// the item cap does.
	var long Presentation
	for i := range waitTextItems {
		long.Working = append(long.Working, WorkingItem{Kind: "member", ID: fmt.Sprint(i), Label: strings.Repeat("x", 300), State: "running"})
	}
	if bounded := waitText(long); len(bounded) > waitTextBytes+64 || !strings.Contains(bounded, "more") {
		t.Fatalf("byte cap ignored (%d bytes): %q", len(bounded), bounded)
	}
}

// Only a park long enough that it cannot be polling earns the summary: the
// floor on timeout_ms is ten seconds, and a summary on every expiry would make
// wait_agent a cheaper swarm_read.
func TestTimeoutMessageSummarisesOnlyLongParks(t *testing.T) {
	r := runtimeTest(t, nilModel(), 1, 2)
	ctx := context.Background()
	if short := r.timeoutMessage(ctx, 30*time.Second); short != waitNoUpdate {
		t.Fatalf("short park summarised: %q", short)
	}
	long := r.timeoutMessage(ctx, waitSummaryFloor)
	if long == waitNoUpdate || !strings.Contains(long, "Continue your own work") {
		t.Fatalf("long park not summarised: %q", long)
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
	tool, _, _ := r.config.Registry.GetIfAllowed("swarm_read")
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
