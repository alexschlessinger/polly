package swarm

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/workflow"
)

func TestInspectionLargeReportAndPagination(t *testing.T) {
	r := runtimeTest(t, nilModel(), 1, 4)
	ctx := context.Background()
	large := strings.Repeat("full evidence\n", 150000)
	if err := r.update(ctx, func(s *State) error {
		w := &workflow.Report{ID: "report", Name: "large audit", Status: "failed", Source: "workflow source", Input: map[string]any{"evidence": large}, Error: &workflow.Error{Message: large}}
		s.Workflows[w.ID] = w
		for i := range 180 {
			id := fmt.Sprintf("task-%03d", i)
			s.Tasks[id] = &Task{ID: id, Owner: id, Execution: id, Description: strings.Repeat("description", 90), Result: map[string]any{"claim": "small"}, Status: "awaiting_review"}
			s.Members[id] = &Member{ID: id, Execution: id, Task: id, Status: "paused", Label: strings.Repeat("long title", 80)}
			s.Executions[id] = &Execution{ID: id, Member: id, Workflow: w.ID, Status: "completed", Error: ""}
			w.Steps = append(w.Steps, workflow.Step{Operation: workflow.Operation{ID: id, Kind: "agent"}, Status: "completed", Value: map[string]any{"claim": "small"}})
		}
		w.Steps[17].Value = map[string]any{"claim": large}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.RegisterParentTools(r.config.Registry)
	for _, name := range []string{"swarm_tasks", "list_agents", "workflow_read"} {
		tool, _, _ := r.config.Registry.GetIfAllowed(name)
		offset, seen := 1, 0
		for {
			args := map[string]any{"offset": offset, "limit": 100}
			if name == "workflow_read" {
				args["id"] = "report"
				args["section"] = "steps"
			}
			out, err := tool.(tools.OutputTool).ExecuteOutput(ctx, args)
			if err != nil || len(out.Text) > inspectionBytes || len(out.Media) != 0 {
				t.Fatalf("unbounded %s page: %d bytes, %v", name, len(out.Text), err)
			}
			var page inspectionPage
			if err := json.Unmarshal([]byte(out.Text), &page); err != nil {
				t.Fatal(err)
			}
			if len(page.Items) == 0 {
				t.Fatal("pagination made no progress")
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
			t.Fatalf("%s lost entries: %d", name, seen)
		}
	}
	tool, _, _ := r.config.Registry.GetIfAllowed("workflow_read")
	out, err := tool.(tools.OutputTool).ExecuteOutput(ctx, map[string]any{"id": "report"})
	if err != nil || len(out.Text) > inspectionBytes || len(out.Media) != 0 {
		t.Fatalf("summary: %d bytes %v", len(out.Text), err)
	}
	out, err = tool.(tools.OutputTool).ExecuteOutput(ctx, map[string]any{"id": "report", "section": "step", "step": "task-017", "pointer": "/value/claim"})
	if err != nil || len(out.Text) > inspectionBytes || len(out.Media) != 1 {
		t.Fatalf("selected artifact: %+v %v", out, err)
	}
	var recovered string
	if err := json.Unmarshal(out.Media[0].Data, &recovered); err != nil || recovered != large {
		t.Fatal("artifact lost selected evidence")
	}
	for _, args := range []map[string]any{{"id": "foreign"}, {"id": "report", "section": "step", "step": "foreign"}, {"id": "report", "section": "input", "pointer": "/foreign"}} {
		if _, err := tool.Execute(ctx, args); err == nil {
			t.Fatalf("foreign selection accepted: %v", args)
		}
	}
}

func TestInspectionPointerExactValues(t *testing.T) {
	v := map[string]any{"a/b": map[string]any{"~": [2]any{json.Number("9007199254740993"), nil}}}
	value, err := selectInspection(v, "/a~1b/~0/0")
	if err != nil || value != json.Number("9007199254740993") {
		t.Fatalf("integer rounded: %v %v", value, err)
	}
	value, err = selectInspection(v, "/a~1b/~0/1")
	if err != nil || value != nil {
		t.Fatalf("null missing: %v %v", value, err)
	}
	for _, pointer := range []string{"a", "/bad~2", "/a~1b/~0/01", "/a~1b/~0/-1", "/a~1b/~0/2", "/a~1b/~0/1/x"} {
		if _, err := selectInspection(v, pointer); err == nil {
			t.Fatalf("invalid pointer %q accepted", pointer)
		}
	}
}

func TestSelectedTextArtifactWorksThroughRealAgentLoop(t *testing.T) {
	r := runtimeTest(t, nilModel(), 1, 4)
	ctx := context.Background()
	large := strings.Repeat("evidence line\n", 5000)
	if err := r.update(ctx, func(s *State) error {
		s.Tasks["selected"] = &Task{ID: "selected", Result: map[string]any{"evidence": large}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.RegisterParentTools(r.config.Registry)
	model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		for _, msg := range req.Messages {
			if msg.ToolName == "read_artifact" {
				return answer("inspected")
			}
		}
		for _, msg := range req.Messages {
			if msg.ToolName == "swarm_tasks" {
				if len(msg.Content) > inspectionBytes {
					t.Error("large result entered model context")
				}
				id := regexp.MustCompile(`sha256:[0-9a-f]{64}`).FindString(msg.Content)
				if id != "" {
					return iterationTool("read", "read_artifact", tools.Result(map[string]any{"id": id, "query": "evidence"}))
				}
				t.Error("missing authorized artifact receipt")
				return answer("missing")
			}
		}
		return iterationTool("result", "swarm_tasks", `{"task":"selected","section":"result"}`)
	})
	agent := llm.NewAgent(model, r.config.Registry, llm.AgentConfig{MaxIterations: 3, ArtifactStore: r.config.Parent.ArtifactStore()})
	response, err := agent.Run(ctx, &llm.CompletionRequest{Messages: []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "inspect saved result"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil {
		t.Fatal("missing response")
	}
}
