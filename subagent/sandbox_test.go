package subagent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestSpawnSandboxArgument(t *testing.T) {
	t.Parallel()
	var got Request
	calls := 0
	tool := NewTool(func(_ context.Context, req Request) (Result, error) {
		calls++
		got = req
		return Result{Text: "done"}, nil
	})
	for _, key := range []string{"sandbox", "model", "model_host"} {
		property, ok := tool.GetSchema().Properties()[key].(map[string]any)
		if !ok || property["type"] != "string" || slices.Contains(tool.GetSchema().Required(), key) {
			t.Fatalf("%s is not an optional string: %+v", key, tool.GetSchema())
		}
	}
	for _, value := range []any{"", " ", "no-such-preset", false, true, nil, 1, []any{"workspace"}} {
		_, err := tool.Execute(context.Background(), map[string]any{"label": "Inspect", "task": "inspect", "sandbox": value})
		var toolErr *tools.ToolError
		if !errors.As(err, &toolErr) || toolErr.Code != "INVALID_ARGS" || calls != 0 {
			t.Fatalf("sandbox %v: err=%v calls=%d", value, err, calls)
		}
	}
	for _, spec := range []string{"", "workspace+net"} {
		args := map[string]any{"label": "Inspect", "task": "inspect", "model": "provider/model", "model_host": "route"}
		if spec != "" {
			args["sandbox"] = spec
		}
		if _, err := tool.Execute(context.Background(), args); err != nil {
			t.Fatal(err)
		}
		if got.Sandbox != spec || got.Model != "provider/model" || got.ModelHost != "route" {
			t.Fatalf("request not forwarded: %+v", got)
		}
	}
}

func TestRunnerWithToolsSandboxRestrictions(t *testing.T) {
	t.Parallel()
	open := &recordingOpen{}
	model := &sequentialLLM{}
	// Spare capacity catches append aliasing across runs.
	floor := make([]string, 1, 4)
	floor[0] = "workspace"
	run := RunnerWithTools(model, open.open, tools.ToolScope{Grant: tools.ExecutionGrant{SandboxPresets: floor}}, llm.CompletionRequest{Model: "parent/model", ModelHost: "parent-route"}, llm.AgentConfig{})
	for _, spec := range []string{"workspace+net", ""} {
		_, err := run(context.Background(), Request{Label: "Inspect", Task: "inspect", Sandbox: spec})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(open.scopes) != 2 || !slices.Equal(open.scopes[0].Grant.SandboxPresets, []string{"workspace", "workspace+net"}) || !slices.Equal(open.scopes[1].Grant.SandboxPresets, []string{"workspace"}) {
		t.Fatalf("scopes = %+v", open.scopes)
	}
	if model.last.Model != "parent/model" || model.last.ModelHost != "parent-route" {
		t.Fatalf("sandbox changed route: %+v", model.last)
	}
	if _, err := run(context.Background(), Request{Label: "Inspect", Task: "inspect", Sandbox: "bad-preset"}); err == nil || len(open.scopes) != 2 {
		t.Fatalf("invalid host request opened tools: %v", err)
	}
}

func TestAgentRunnerRefusesUnboundSandbox(t *testing.T) {
	t.Parallel()
	registry := tools.NewToolRegistry(nil)
	defer registry.Close()
	model := &sequentialLLM{}
	run := AgentRunner(model, registry, llm.CompletionRequest{}, llm.AgentConfig{})
	if _, err := run(context.Background(), Request{Label: "Inspect", Task: "inspect", Sandbox: "workspace"}); err == nil || !strings.Contains(err.Error(), "RunnerWithTools") || model.calls != 0 {
		t.Fatalf("shared registry ignored sandbox: %v calls=%d", err, model.calls)
	}
	if _, err := run(context.Background(), Request{Label: "Inspect", Task: "inspect"}); err != nil || model.calls != 1 {
		t.Fatalf("omission changed behavior: %v calls=%d", err, model.calls)
	}
}
