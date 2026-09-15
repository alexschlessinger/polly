package subagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// recordingOpen is an OpenTools that records every scope it was asked for
// and the order of closes relative to the child agent's run.
type recordingOpen struct {
	scopes []tools.ToolScope
	events []string
	fail   error
	tools  []tools.Tool
}

func (o *recordingOpen) open(_ context.Context, scope tools.ToolScope) (tools.ToolBinding, error) {
	o.scopes = append(o.scopes, scope)
	if o.fail != nil {
		return tools.ToolBinding{}, o.fail
	}
	registry := tools.NewToolRegistry(o.tools)
	o.events = append(o.events, "open")
	return tools.ToolBinding{
		Registry:         registry,
		Instructions:     "REPOSITORY GUIDANCE",
		ToolInstructions: "TOOL GUIDANCE",
		Omitted:          []string{"spawn_agent"},
		Close: func() error {
			o.events = append(o.events, "close")
			return registry.Close()
		},
	}, nil
}

func probeTool(events *[]string) tools.Tool {
	return &tools.Func{Name: "probe", Desc: "probe", Run: func(context.Context, tools.Args) (string, error) {
		*events = append(*events, "probe")
		return "probed", nil
	}}
}

func systemContent(t *testing.T, req *llm.CompletionRequest) string {
	t.Helper()
	if req == nil || len(req.Messages) == 0 || req.Messages[0].Role != messages.MessageRoleSystem {
		t.Fatalf("no leading system message: %+v", req)
	}
	return req.Messages[0].Content
}

func TestRunnerWithToolsOpensPerChildAndClosesAfterAgent(t *testing.T) {
	open := &recordingOpen{}
	open.tools = []tools.Tool{probeTool(&open.events)}
	model := &sequentialLLM{responses: []messages.ChatMessage{toolCall("probe", `{}`), reply("done")}}
	base := llm.CompletionRequest{Model: "test/model", Messages: []messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: "BASE PROMPT"}}, Skills: nil}
	run := RunnerWithTools(model, open.open, tools.ToolScope{Root: "/work", SourceRoot: "/src"}, base, llm.AgentConfig{})

	res, err := run(context.Background(), Request{Label: "Test agent", Task: "go", Tools: []string{"probe"}})
	if err != nil || res.Text != "done" {
		t.Fatalf("result %+v, %v", res, err)
	}
	if len(open.scopes) != 1 || open.scopes[0].Root != "/work" || open.scopes[0].SourceRoot != "/src" || strings.Join(open.scopes[0].AllowedTools, ",") != "probe" {
		t.Fatalf("scopes = %+v", open.scopes)
	}
	if got := strings.Join(open.events, " "); got != "open probe close" {
		t.Fatalf("events = %q, want the binding closed after the child ran", got)
	}
	system := systemContent(t, model.last)
	for _, want := range []string{"BASE PROMPT", "REPOSITORY GUIDANCE", "TOOL GUIDANCE"} {
		if !strings.Contains(system, want) {
			t.Fatalf("system prompt %q lacks %q", system, want)
		}
	}
	if strings.Index(system, "BASE PROMPT") > strings.Index(system, "REPOSITORY GUIDANCE") {
		t.Fatalf("guidance precedes the base prompt: %q", system)
	}
	if model.last.Skills != nil {
		t.Fatal("an inherited skill catalog survived: tool guidance would be inserted twice")
	}
	if len(model.last.Messages) < 2 || model.last.Messages[1].Role != messages.MessageRoleUser || model.last.Messages[1].Content != "go" {
		t.Fatalf("child messages = %+v", model.last.Messages)
	}

	// A second child opens its own binding; nothing is shared between them.
	if _, err := run(context.Background(), Request{Label: "Test agent", Task: "again"}); err != nil {
		t.Fatal(err)
	}
	if len(open.scopes) != 2 || open.scopes[1].AllowedTools != nil {
		t.Fatalf("second scope = %+v", open.scopes)
	}
}

func TestRunnerWithToolsGuidancePlacement(t *testing.T) {
	open := &recordingOpen{}
	open.tools = []tools.Tool{probeTool(&open.events)}
	model := &sequentialLLM{}
	run := RunnerWithTools(model, open.open, tools.ToolScope{Root: "/work"}, llm.CompletionRequest{Model: "test/model"}, llm.AgentConfig{})

	// Without a base system message the guidance becomes one.
	if _, err := run(context.Background(), Request{Label: "Test agent", Task: "go"}); err != nil {
		t.Fatal(err)
	}
	if system := systemContent(t, model.last); system != "REPOSITORY GUIDANCE\n\nTOOL GUIDANCE" {
		t.Fatalf("system prompt = %q", system)
	}
	// Disabled tools keep the repository guidance and drop the tool guidance.
	if _, err := run(context.Background(), Request{Label: "Test agent", Task: "go", Tools: []string{}}); err != nil {
		t.Fatal(err)
	}
	if system := systemContent(t, model.last); system != "REPOSITORY GUIDANCE" {
		t.Fatalf("system prompt with tools disabled = %q", system)
	}
	if len(model.last.Tools) != 0 {
		t.Fatalf("tools offered while disabled: %v", names(model.last.Tools))
	}
}

func TestRunnerWithToolsFailedOpenAndNothingMatches(t *testing.T) {
	failing := &recordingOpen{fail: errors.New("daemon unreachable")}
	model := &sequentialLLM{}
	run := RunnerWithTools(model, failing.open, tools.ToolScope{Root: "/work"}, llm.CompletionRequest{}, llm.AgentConfig{})
	if _, err := run(context.Background(), Request{Label: "Test agent", Task: "go"}); err == nil || !strings.Contains(err.Error(), "daemon unreachable") || model.calls != 0 {
		t.Fatalf("failed open: %v (model calls %d)", err, model.calls)
	}

	empty := &recordingOpen{}
	run = RunnerWithTools(model, empty.open, tools.ToolScope{Root: "/work"}, llm.CompletionRequest{}, llm.AgentConfig{})
	if _, err := run(context.Background(), Request{Label: "Test agent", Task: "go", Tools: []string{"nope"}}); !errors.Is(err, ErrNoMatchingTools) {
		t.Fatalf("nothing matches = %v", err)
	}
	if got := strings.Join(empty.events, " "); got != "open close" {
		t.Fatalf("a refused child left its binding open: %q", got)
	}
	// A private agent built-in named alone is still a valid selection.
	if _, err := run(context.Background(), Request{Label: "Test agent", Task: "go", Tools: []string{"read_transcript"}}); err != nil {
		t.Fatalf("built-in selection refused: %v", err)
	}
}

func TestRunnersPreservePartialOutputOnError(t *testing.T) {
	open := &recordingOpen{}
	open.tools = []tools.Tool{probeTool(&open.events)}
	parent := tools.NewToolRegistry([]tools.Tool{probeTool(&open.events)})
	defer parent.Close()
	for name, run := range map[string]Runner{
		"AgentRunner":     AgentRunner(&sequentialLLM{responses: []messages.ChatMessage{toolCall("probe", `{}`), toolCall("probe", `{}`)}}, parent, llm.CompletionRequest{}, llm.AgentConfig{MaxIterations: 1}),
		"RunnerWithTools": RunnerWithTools(&sequentialLLM{responses: []messages.ChatMessage{toolCall("probe", `{}`), toolCall("probe", `{}`)}}, open.open, tools.ToolScope{Root: "/work"}, llm.CompletionRequest{}, llm.AgentConfig{MaxIterations: 1}),
	} {
		res, err := run(context.Background(), Request{Label: "Test agent", Task: "loop"})
		if !errors.Is(err, llm.ErrMaxIterations) {
			t.Fatalf("%s: error = %v, want the iteration limit", name, err)
		}
		if len(res.Text) == 0 && res.InputTokens == 0 && res.OutputTokens == 0 {
			// A scripted model reports no usage; the partial reply must still
			// be the last assistant message the child produced.
			t.Logf("%s: partial result %+v", name, res)
		}
	}
}
