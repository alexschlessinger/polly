package swarm

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/workflow"
)

var removedCoordinationTools = []string{
	"swarm_status", "swarm_wait", "swarm_followup", "swarm_tasks", "read_messages", "swarm_search", "workflow_read",
	"workflow_start", "workflow_cancel", "workflow_acknowledge", "swarm_create_task", "swarm_update_task",
	"swarm_claim", "swarm_submit", "swarm_snapshot", "swarm_integration", "swarm_read_artifact",
}

var parentCoordinationTools = []string{"swarm_read", "send_message", "wait_agent", "list_agents", "interrupt_agent", "swarm_publish", "spawn_agent", "followup_task", "swarm_review", "swarm_integrate", "swarm_control", "workflow_run", "swarm_help", "workflow_help"}
var childCoordinationTools = []string{"swarm_read", "send_message", "wait_agent", "list_agents", "swarm_publish", "swarm_block"}

func assertCoordinationTools(t *testing.T, schemas []tools.Tool, want []string) {
	t.Helper()
	var got []string
	for _, s := range schemas {
		name := s.GetName()
		if strings.HasPrefix(name, "swarm_") || strings.HasPrefix(name, "workflow_") || name == "spawn_agent" || name == "send_message" || name == "list_agents" || name == "wait_agent" || name == "followup_task" || name == "interrupt_agent" || slices.Contains(removedCoordinationTools, name) {
			got = append(got, name)
		}
	}
	want = slices.Clone(want)
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("coordination tools = %v, want %v", got, want)
	}
}

func TestExactCoordinationToolSets(t *testing.T) {
	r := runtimeTest(t, nilModel(), 1, 2)
	r.RegisterParentTools(r.config.Registry)
	assertCoordinationTools(t, r.config.Registry.All(), parentCoordinationTools)
	for _, name := range removedCoordinationTools {
		if _, exists, _ := r.config.Registry.GetIfAllowed(name); exists {
			t.Errorf("removed tool still dispatchable: %s", name)
		}
	}
	for _, tool := range r.config.Registry.All() {
		text := tool.GetSchema().Description()
		if tool.GetName() == "swarm_help" {
			text += coordinationGuide
		} else if tool.GetName() == "workflow_help" {
			text += workflowGuide
		}
		for _, old := range removedCoordinationTools {
			if strings.Contains(text, old) {
				t.Errorf("%s suggests removed tool %s", tool.GetName(), old)
			}
		}
	}
	for _, name := range []string{"swarm_control", "workflow_run"} {
		tool, _, _ := r.config.Registry.GetIfAllowed(name)
		if !tool.(interface{ Coordinates() bool }).Coordinates() || !tool.(tools.UntimedTool).Untimed() {
			t.Errorf("%s lost coordination or timeout traits", name)
		}
	}
	for _, typed := range []bool{false, true} {
		t.Run(fmt.Sprint("typed=", typed), func(t *testing.T) {
			model := modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				want := slices.Clone(childCoordinationTools)
				if typed {
					want = append(want, completionToolName)
				}
				assertCoordinationTools(t, req.Tools, want)
				if typed {
					return completion(`{"answer":"complete"}`)
				}
				return answer("complete")
			})
			r := runtimeTest(t, model, 1, 1)
			req := AgentRequest{Label: "Inspect tool set", Task: "Report completion", ReadOnly: true}
			if typed {
				req.Schema = map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}}, "required": []string{"answer"}}
			}
			if _, err := r.Agent(context.Background(), "", req); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSwarmReadPrivacySelectionsAndNoMutations(t *testing.T) {
	r := runtimeTest(t, nilModel(), 1, 2)
	ctx := context.Background()
	large := strings.Repeat("private addressed evidence\n", 2000)
	if err := r.update(ctx, func(s *State) error {
		s.Members["child"] = &Member{ID: "child"}
		s.Messages["mine"] = &Mail{ID: "mine", To: "child", From: r.ID, Text: large}
		s.Messages["parent-only"] = &Mail{ID: "parent-only", To: r.ID, From: "child", Text: "parent secret"}
		s.Tasks["task"] = &Task{ID: "task", Status: "awaiting_review", Revision: 3, Criteria: "evidence", Result: map[string]any{"value": []string{"saved"}}}
		s.Workflows["workflow"] = &workflow.Report{ID: "workflow", Status: "completed", Source: "saved source", Input: map[string]any{"key": "input"}, Output: "output"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := r.State(ctx)
	registry := tools.NewToolRegistry(nil, tools.WithUnsafeNoSandbox())
	defer registry.Close()
	r.registerMemberTools(registry, "child")
	assertCoordinationTools(t, registry.All(), childCoordinationTools)
	child, _, _ := registry.GetIfAllowed("swarm_read")
	if text := tools.Result(child.GetSchema()); strings.Contains(text, "workflow") || strings.Contains(text, `"step"`) {
		t.Fatalf("child schema advertises workflow inspection: %s", text)
	}
	for _, args := range []map[string]any{
		{"view": "workflows", "id": "workflow", "actor": r.ID},
		{"view": "messages", "id": "parent-only", "actor": r.ID},
		{"view": "unknown"},
	} {
		if _, err := child.Execute(ctx, args); err == nil {
			t.Fatalf("child accepted forbidden selection: %v", args)
		}
	}
	out, err := child.(tools.OutputTool).ExecuteOutput(ctx, map[string]any{"view": "messages", "id": "mine"})
	if err != nil || len(out.Media) != 1 || !strings.Contains(string(out.Media[0].Data), "private addressed evidence") {
		t.Fatalf("addressed message artifact missing: %v %v", out, err)
	}
	text, err := child.Execute(ctx, map[string]any{"view": "messages"})
	if err != nil || strings.Contains(text, "parent secret") || !strings.Contains(text, `"total": 1`) {
		t.Fatalf("inbox = %s, %v", text, err)
	}
	r.RegisterParentTools(r.config.Registry)
	parent, _, _ := r.config.Registry.GetIfAllowed("swarm_read")
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"view": "tasks", "id": "task", "section": "result", "pointer": "/value/0"}, `"saved"`},
		{map[string]any{"view": "tasks", "id": "task", "section": "details", "pointer": "/criteria"}, `"evidence"`},
		{map[string]any{"view": "workflows", "id": "workflow", "section": "source"}, `"saved source"`},
		{map[string]any{"view": "workflows", "id": "workflow", "section": "input", "pointer": "/key"}, `"input"`},
		{map[string]any{"view": "workflows", "id": "workflow", "section": "output"}, `"output"`},
	} {
		if out, err := parent.Execute(ctx, tc.args); err != nil || out != tc.want {
			t.Fatalf("selection %v = %s, %v", tc.args, out, err)
		}
	}
	if _, err := parent.Execute(ctx, map[string]any{"view": "workflows"}); err == nil {
		t.Fatal("workflow view accepted missing id")
	}
	after, _ := r.State(ctx)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("reading changed task state, acknowledgment or delivery")
	}
}

func TestSwarmReadPublicationAndMessagePagination(t *testing.T) {
	r := runtimeTest(t, nilModel(), 1, 2)
	ctx := context.Background()
	large := strings.Repeat("CaChe evidence\n", 2000)
	if err := r.update(ctx, func(s *State) error {
		for i := range 125 {
			id := fmt.Sprintf("%03d", i)
			s.Publications[id] = &Publication{ID: id, Text: "CaChe evidence"}
			s.Messages[id] = &Mail{ID: id, To: r.ID, Text: "addressed evidence"}
		}
		s.Publications["000"].Text = large
		s.Publications["filtered"] = &Publication{ID: "filtered", Text: "unrelated"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.RegisterParentTools(r.config.Registry)
	reader, _, _ := r.config.Registry.GetIfAllowed("swarm_read")
	for _, view := range []string{"publications", "messages"} {
		seen := map[string]bool{}
		for offset := 1; offset != 0; {
			out, err := reader.(tools.OutputTool).ExecuteOutput(ctx, map[string]any{"view": view, "query": "cache", "offset": offset, "limit": 17})
			if err != nil || len(out.Text) > inspectionBytes {
				t.Fatalf("page %s: %v", view, err)
			}
			data := []byte(out.Text)
			if len(out.Media) > 0 {
				data = out.Media[0].Data
			}
			var page struct {
				Items []Publication `json:"items"`
				Total int           `json:"total"`
				Next  int           `json:"next"`
			}
			if err := json.Unmarshal(data, &page); err != nil || page.Total != 125 || len(page.Items) == 0 || page.Next != 0 && page.Next <= offset {
				t.Fatalf("invalid page %s: %s, %v", view, data, err)
			}
			for _, item := range page.Items {
				if seen[item.ID] {
					t.Fatal("pagination repeated an entry")
				}
				seen[item.ID] = true
				if view == "publications" && item.ID == "000" && item.Text != large {
					t.Fatal("large publication lost evidence")
				}
			}
			offset = page.Next
		}
		if len(seen) != 125 {
			t.Fatalf("lost %s entries: %d", view, len(seen))
		}
		for _, args := range []map[string]any{{"view": view, "offset": 0}, {"view": view, "limit": 101}} {
			if _, err := reader.Execute(ctx, args); err == nil {
				t.Fatal("invalid pagination accepted")
			}
		}
	}
}

func TestBlockToolChecksOwnerRevisionAndReason(t *testing.T) {
	r := runtimeTest(t, nilModel(), 1, 2)
	ctx := context.Background()
	if err := r.update(ctx, func(s *State) error {
		s.Tasks["mine"] = &Task{ID: "mine", Owner: "child", Status: "running", Revision: 2}
		s.Tasks["theirs"] = &Task{ID: "theirs", Owner: "other", Status: "running", Revision: 2}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	registry := tools.NewToolRegistry(nil, tools.WithUnsafeNoSandbox())
	defer registry.Close()
	r.registerMemberTools(registry, "child")
	block, _, _ := registry.GetIfAllowed("swarm_block")
	before, _ := r.State(ctx)
	for _, args := range []map[string]any{
		{"task": "theirs", "revision": 2, "reason": "blocked", "actor": "other"},
		{"task": "mine", "revision": 1, "reason": "blocked"},
		{"task": "mine", "revision": 2, "reason": " "},
	} {
		if out, err := block.Execute(ctx, args); err == nil || out != "" {
			t.Fatalf("invalid blocker succeeded: %s, %v", out, err)
		}
	}
	after, _ := r.State(ctx)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("invalid blocker changed state")
	}
}
