package swarm

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/workflow"
)

func TestManagedSpawnRequiresExplicitModeBeforeEffects(t *testing.T) {
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		t.Error("invalid spawn called the model")
		return answer("unexpected")
	}), 1, 2)
	r.RegisterParentTools(r.config.Registry)
	spawn, _, _ := r.config.Registry.GetIfAllowed("spawn_agent")
	if !slices.Contains(spawn.GetSchema().Required(), "read_only") {
		t.Fatal("schema permits an implicit editing assignment")
	}
	ctx := context.Background()
	before, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, directoryBefore := os.Stat(r.config.Directory)
	for _, tc := range []struct {
		name    string
		present bool
		value   any
	}{
		{name: "missing"},
		{name: "null", present: true},
		{name: "string", present: true, value: "false"},
		{name: "number", present: true, value: 0},
		{name: "object", present: true, value: map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := tools.Args{"task_name": "research", "message": "Inspect only", "commit": "invalid-selector"}
			if tc.present {
				a["read_only"] = tc.value
			}
			if _, err := spawn.Execute(ctx, a); err == nil || !strings.Contains(err.Error(), "read_only is required and must be a boolean") {
				t.Fatalf("mode was not checked before commit selection: %v", err)
			}
		})
	}
	if _, err := spawn.Execute(ctx, tools.Args{"task_name": "editing", "message": "Edit", "read_only": false, "review": true}); err == nil || !strings.Contains(err.Error(), "review requires read-only") {
		t.Fatalf("reviewed editing accepted: %v", err)
	}
	after, err := r.State(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("invalid mode changed tasks, captures or executions: %v", err)
	}
	if _, err := os.Stat(r.config.Directory); os.IsNotExist(directoryBefore) && !os.IsNotExist(err) {
		t.Fatalf("invalid mode allocated runtime files: %v", err)
	}
}

func TestManagedSpawnFinalDeliveryAndAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name, requirement string
		readOnly, review  bool
	}{
		{"research", RequirementDelivered, true, false},
		{"reviewed_research", RequirementReviewed, true, true},
		{"editing", RequirementApplied, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, _ := integrateFixture(t)
			r.config.Client = modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
				return answer("Plain final finding; no changes needed.")
			})
			r.RegisterParentTools(r.config.Registry)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			text, err := execParentTool(t, r, "spawn_agent", tools.Args{"task_name": tc.name, "message": "Inspect assigned files", "read_only": tc.readOnly, "review": tc.review})
			if err != nil {
				t.Fatal(err)
			}
			var receipt struct{ Member string }
			if err := json.Unmarshal([]byte(text), &receipt); err != nil {
				t.Fatal(err)
			}
			awaitIdle(t, r, ctx)
			s, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			task := s.Tasks[s.Members[receipt.Member].Task]
			if task.Requirement != tc.requirement || task.AcceptedRevision != 0 || task.Status == "done" || task.Result == nil {
				t.Fatalf("wrong automatic result: %+v", task)
			}
			if !tc.readOnly && task.Snapshot == "" {
				t.Fatal("editing final omitted automatic capture")
			}
			input := admitParent(t, r)
			if len(input) != 1 || !strings.Contains(input[0].Content, "Plain final finding") {
				t.Fatalf("result not delivered: %+v", input)
			}
			s, err = r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			task = s.Tasks[task.ID]
			if tc.requirement == RequirementDelivered {
				if task.Status != "done" || task.Delivery == nil {
					t.Fatalf("research needs unnecessary acceptance: %+v", task)
				}
			} else {
				if task.Status != "awaiting_review" || task.AcceptedRevision != 0 {
					t.Fatalf("delivery accepted reviewed work: %+v", task)
				}
				name := "swarm_review"
				args := tools.Args{"task": task.ID, "revision": task.Revision, "accept": true}
				stale := tools.Args{"task": task.ID, "revision": task.Revision - 1, "accept": true}
				if !tc.readOnly {
					name = "swarm_integrate"
					args = tools.Args{"tasks": []any{map[string]any{"task": task.ID, "revision": task.Revision}}}
					stale = tools.Args{"tasks": []any{map[string]any{"task": task.ID, "revision": task.Revision - 1}}}
				}
				if !strings.Contains(input[0].Content, name) {
					t.Fatalf("notice omitted exact acceptance action: %s", input[0].Content)
				}
				if _, err := execParentTool(t, r, name, stale); err == nil {
					t.Fatal("stale acceptance succeeded")
				}
				if _, err := execParentTool(t, r, name, args); err != nil {
					t.Fatal(err)
				}
				s, _ = r.State(ctx)
				if s.Tasks[task.ID].Status != "done" {
					t.Fatalf("explicit acceptance did not finish task: %+v", s.Tasks[task.ID])
				}
			}
			// The documented details lookup still supplies a usable release ID.
			text, err = execParentTool(t, r, "list_agents", tools.Args{"path_prefix": "/root/" + tc.name, "details": true})
			if err != nil {
				t.Fatal(err)
			}
			var page struct{ Items []struct{ Context string } }
			if err := json.Unmarshal([]byte(text), &page); err != nil || len(page.Items) != 1 || page.Items[0].Context == "" {
				t.Fatalf("missing workspace provenance: %s %v", text, err)
			}
			if text, err := execParentTool(t, r, "swarm_control", tools.Args{"action": "release", "id": page.Items[0].Context}); err != nil || !strings.Contains(text, `"released"`) {
				t.Fatalf("details release failed: %s %v", text, err)
			}
		})
	}
}

func TestAgentInspectionSummaryDispositions(t *testing.T) {
	r := runtimeTest(t, nilModel(), 1, 2)
	r.RegisterParentTools(r.config.Registry)
	ctx := context.Background()
	for _, tc := range []struct {
		name, execution, task, requirement string
		lifecycle                          Lifecycle
		work, detail                       string
		attention                          bool
	}{
		{"running", "running", "running", RequirementReviewed, LifecycleActive, "running", "", false},
		{"waiting", "waiting", "running", RequirementReviewed, LifecycleWaiting, "running", "", false},
		{"blocked", "completed", "blocked", RequirementReviewed, LifecycleIdle, "blocked", "", true},
		{"failed", "failed", "blocked", RequirementReviewed, LifecyclePaused, "blocked", "failed", true},
		{"interrupted", "paused", "running", RequirementReviewed, LifecyclePaused, "running", "interrupted", true},
		{"delivering", "completed", "running", RequirementDelivered, LifecycleIdle, "delivering", "", false},
		{"review", "completed", "awaiting_review", RequirementReviewed, LifecycleIdle, "awaiting review", "", true},
		{"integration", "completed", "awaiting_review", RequirementApplied, LifecycleIdle, "integration pending", "", true},
		{"done", "completed", "done", RequirementDelivered, LifecycleIdle, "done", "", false},
		{"deferred", "failed", "blocked", RequirementReviewed, LifecyclePaused, "blocked", "failed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.update(ctx, func(s *State) error {
				m := &Member{ID: "worker", Label: "Inspect cache", Task: "task", Execution: "execution", Context: "context", ReadOnly: tc.requirement != RequirementApplied}
				task := &Task{ID: m.Task, Owner: m.ID, Status: tc.task, Execution: m.Execution, Revision: 2, Requirement: tc.requirement}
				e := &Execution{ID: m.Execution, Member: m.ID, Status: tc.execution, Iterations: 3, Request: AgentRequest{MaxIterations: 20}, Result: &AgentResult{Task: task.ID, Execution: m.Execution, Revision: task.Revision}}
				if tc.requirement == RequirementApplied {
					task.AcceptedRevision, task.Snapshot = task.Revision, "capture"
				}
				if tc.name == "deferred" {
					task.Run, e.Run, e.Workflow, e.Generation = "run", "run", "workflow", 1
					s.Workflows[e.Workflow] = &workflow.Report{ID: e.Workflow, Run: e.Run, Status: "failed", Acknowledged: true}
					task.Deferral = &TaskDeferral{Workflow: e.Workflow, Execution: e.ID, Owner: m.ID, Revision: task.Revision, Generation: e.Generation, Status: task.Status}
				}
				s.Members[m.ID], s.Tasks[task.ID], s.Executions[e.ID] = m, task, e
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			text, err := execParentTool(t, r, "list_agents", tools.Args{})
			if err != nil {
				t.Fatal(err)
			}
			var page struct {
				Items []struct {
					ID        string
					AgentName string `json:"agent_name"`
					State     agentStateSummary
				}
				Self, Parent string
				ParentState  agentStateSummary
			}
			if err := json.Unmarshal([]byte(text), &page); err != nil || len(page.Items) != 1 {
				t.Fatalf("bad roster: %s %v", text, err)
			}
			got := page.Items[0].State
			if got.Lifecycle != tc.lifecycle || got.TaskStatus != tc.work || got.Detail != tc.detail || got.Attention != tc.attention || got.Deferred != (tc.name == "deferred") {
				t.Fatalf("lost disposition: %+v", got)
			}
			if page.Items[0].ID != "worker" || page.Items[0].AgentName != "/root/agent_worker" {
				t.Fatalf("lost usable worker identity: %s", text)
			}
			if page.Self != r.ID || page.Parent != r.ID || page.ParentState.Lifecycle == "" {
				t.Fatalf("lost caller/parent state: %s", text)
			}
			for _, field := range []string{"context", "execution", "task", "name", "agent_status", "iterations", "maxIterations", "display", "outcome"} {
				if strings.Contains(text, `"`+field+`":`) {
					t.Errorf("summary exposes detailed %s", field)
				}
			}
			details, err := execParentTool(t, r, "list_agents", tools.Args{"details": true})
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"context", "execution", "task", "agent_status", "iterations", "maxIterations", "display", "outcome"} {
				if !strings.Contains(details, `"`+field+`":`) {
					t.Errorf("details omit %s: %s", field, details)
				}
			}
			if len(text) >= len(details) {
				t.Fatal("summary is not smaller than details")
			}
		})
	}
	for _, invalid := range []any{nil, "true", 1} {
		if _, err := execParentTool(t, r, "list_agents", tools.Args{"details": invalid}); err == nil {
			t.Fatalf("accepted invalid details: %v", invalid)
		}
	}
}

func TestCompactTaskSummaryRetainsDetailedEvidence(t *testing.T) {
	r, plan := applyFixture(t, false)
	retainAlias(t, r, plan.Parent, plan.Parent.ID)
	retainAlias(t, r, plan.Merged, plan.Merged.ID)
	ctx := context.Background()
	if err := r.update(ctx, func(s *State) error {
		task := s.Tasks["task"]
		task.StartingSnapshot, task.Execution, task.Run, task.Follows = plan.Parent.ID, "execution", "run", "prior"
		task.Delivery = &TaskDelivery{Via: "mail", Ref: "receipt", Revision: task.Revision, Execution: task.Execution}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.RegisterParentTools(r.config.Registry)
	before, _ := r.State(ctx)
	text, err := execParentTool(t, r, "swarm_read", tools.Args{"view": "tasks", "id": "task"})
	if err != nil {
		t.Fatal(err)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(text), &summary); err != nil {
		t.Fatal(err)
	}
	want := []string{"id", "owner", "description", "status", "displayStatus", "revision", "requirement", "deferred", "read"}
	slices.Sort(want)
	if !slices.Equal(slices.Sorted(maps.Keys(summary)), want) {
		t.Fatalf("summary contract: %s", text)
	}
	for _, field := range []string{"baseCommit", "resultCommit", "execution", "run", "follows", "delivery", "acceptedRevision"} {
		if _, err := execParentTool(t, r, "swarm_read", tools.Args{"view": "tasks", "id": "task", "pointer": "/" + field}); err == nil {
			t.Fatalf("summary still exposes %s", field)
		}
		if _, err := execParentTool(t, r, "swarm_read", tools.Args{"view": "tasks", "id": "task", "section": "details", "pointer": "/" + field}); err != nil {
			t.Fatalf("details lost %s: %v", field, err)
		}
	}
	after, err := r.State(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("inspection changed evidence: %v", err)
	}
}
