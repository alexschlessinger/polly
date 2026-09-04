package subagent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// sequentialLLM answers each call with the next scripted message and
// remembers the last request it saw.
type sequentialLLM struct {
	responses []messages.ChatMessage
	calls     int
	last      *llm.CompletionRequest
}

func (s *sequentialLLM) ChatCompletionStream(_ context.Context, req *llm.CompletionRequest, processor llm.EventStreamProcessor) <-chan *messages.StreamEvent {
	s.last = req
	idx := s.calls
	s.calls++
	msg := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done", StopReason: messages.StopReasonEndTurn}
	if idx < len(s.responses) {
		msg = s.responses[idx]
	}
	input := make(chan messages.ChatMessage, 1)
	input <- msg
	close(input)
	return processor.ProcessMessagesToEvents(input)
}

func toolCall(name, args string) messages.ChatMessage {
	return messages.ChatMessage{
		Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
		ToolCalls: []messages.ChatMessageToolCall{{ID: "call-1", Name: name, Arguments: args}},
	}
}

func reply(text string) messages.ChatMessage {
	return messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: text, StopReason: messages.StopReasonEndTurn}
}

func names(ts []tools.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.GetName())
	}
	return out
}

func TestToolParsesTheBriefAndFormatsTheReply(t *testing.T) {
	var got Request
	tool := NewTool(func(_ context.Context, req Request) (Result, error) {
		got = req
		return Result{Text: " found it \n", Session: "purple-owl", InputTokens: 10, OutputTokens: 2}, nil
	})
	out, err := tool.Execute(context.Background(), map[string]any{
		"task": " look around ", "label": "explore",
		"tools": []any{"read_file", " ", "git__*"},
		"model": "openai/gpt-5.4", "max_iterations": float64(3),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := Request{Task: "look around", Label: "explore", Tools: []string{"read_file", "git__*"}, Model: "openai/gpt-5.4", MaxIterations: 3}
	if got.Task != want.Task || got.Label != want.Label || !slices.Equal(got.Tools, want.Tools) || got.Model != want.Model || got.MaxIterations != want.MaxIterations {
		t.Fatalf("request = %+v, want %+v", got, want)
	}
	if out != "found it\n\n(agent session purple-owl · 10 in / 2 out)" {
		t.Fatalf("result = %q", out)
	}
	if (Result{Text: "x"}).String() != "x" || (Result{Session: "s"}).String() != "(the agent returned no reply)\n\n(agent session s)" {
		t.Fatal("result formatting without a session or reply")
	}
	schema := tool.GetSchema()
	if schema.Title() != ToolName || !tool.Untimed() || tool.GetType() != "native" {
		t.Fatalf("tool identity: %s %v %s", schema.Title(), tool.Untimed(), tool.GetType())
	}
}

func TestToolRefusesABriefWithoutATask(t *testing.T) {
	called := false
	tool := NewTool(func(context.Context, Request) (Result, error) {
		called = true
		return Result{}, nil
	})
	_, err := tool.Execute(context.Background(), map[string]any{"label": "nothing"})
	var toolErr *tools.ToolError
	if !errors.As(err, &toolErr) || toolErr.Code != "INVALID_ARGS" || called {
		t.Fatalf("err = %v, called %v; want an INVALID_ARGS refusal", err, called)
	}
}

func TestToolReportsAFailedChildWithItsSession(t *testing.T) {
	tool := NewTool(func(context.Context, Request) (Result, error) {
		return Result{Session: "purple-owl"}, errors.New("provider exploded")
	})
	_, err := tool.Execute(context.Background(), map[string]any{"task": "look"})
	var toolErr *tools.ToolError
	if !errors.As(err, &toolErr) || toolErr.Code != "AGENT_FAILED" || toolErr.Message != "agent failed: provider exploded (session purple-owl)" {
		t.Fatalf("err = %v", err)
	}
}

func TestToolPassesCancellationThrough(t *testing.T) {
	cause := errors.New("user left")
	tool := NewTool(func(ctx context.Context, _ Request) (Result, error) {
		<-ctx.Done()
		return Result{}, context.Cause(ctx)
	})
	ctx, cancel := context.WithCancelCause(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel(cause)
	}()
	if _, err := tool.Execute(ctx, map[string]any{"task": "look"}); !errors.Is(err, cause) {
		t.Fatalf("err = %v, want the cancellation cause", err)
	}
}

func TestToolBoundsConcurrentChildren(t *testing.T) {
	var running, peak atomic.Int32
	release := make(chan struct{})
	tool := NewTool(func(ctx context.Context, _ Request) (Result, error) {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		defer running.Add(-1)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return Result{Text: "ok"}, nil
	}, WithMaxConcurrent(1))
	done := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := tool.Execute(context.Background(), map[string]any{"task": "look"})
			done <- err
		}()
	}
	time.Sleep(50 * time.Millisecond)
	if got := running.Load(); got != 1 {
		t.Fatalf("%d children running, want 1", got)
	}
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if peak.Load() != 1 {
		t.Fatalf("peak concurrency %d, want 1", peak.Load())
	}
}

func TestChildRegistryNeverHandsOutSpawnAgent(t *testing.T) {
	parent := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "alpha", Desc: "a"}, &tools.Func{Name: "beta", Desc: "b"}})
	parent.Register(NewTool(nil))
	if got := names(ChildRegistry(parent, nil).All()); !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Fatalf("unfiltered child sees %v", got)
	}
	if got := names(ChildRegistry(parent, []string{"alpha", ToolName}).All()); !slices.Equal(got, []string{"alpha"}) {
		t.Fatalf("filtered child sees %v", got)
	}
	if got := names(parent.All()); !slices.Contains(got, ToolName) {
		t.Fatalf("parent lost its spawn tool: %v", got)
	}
}

func TestAgentRunnerRunsAChildOverTheParentsTools(t *testing.T) {
	hits := 0
	parent := tools.NewToolRegistry([]tools.Tool{
		&tools.Func{Name: "probe", Desc: "probe", Run: func(context.Context, tools.Args) (string, error) { hits++; return "pong", nil }},
		&tools.Func{Name: "other", Desc: "other"},
	})
	fake := &sequentialLLM{responses: []messages.ChatMessage{toolCall("probe", `{}`), reply("found it")}}
	base := llm.CompletionRequest{Model: "test/model", Messages: []messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: "be brief"}}}
	run := AgentRunner(fake, parent, base, llm.AgentConfig{})

	res, err := run(context.Background(), Request{Task: "look around", Tools: []string{"probe"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "found it" || res.Session != "" || hits != 1 {
		t.Fatalf("result = %+v, hits %d", res, hits)
	}
	if fake.last.Model != "test/model" || len(fake.last.Messages) < 2 || fake.last.Messages[1].Content != "look around" {
		t.Fatalf("child request = %+v", fake.last.Messages)
	}
	if got := names(fake.last.Tools); !slices.Contains(got, "probe") || slices.Contains(got, "other") {
		t.Fatalf("child tools = %v, want probe without other", got)
	}
	if got := names(parent.All()); len(got) != 2 {
		t.Fatalf("the child changed the parent's tools: %v", got)
	}

	if _, err := run(context.Background(), Request{Task: "look", Tools: []string{"nope"}}); !errors.Is(err, ErrNoMatchingTools) {
		t.Fatalf("err = %v, want ErrNoMatchingTools", err)
	}
	if _, err := run(context.Background(), Request{Task: "look", Model: "other/model"}); err != nil || fake.last.Model != "other/model" {
		t.Fatalf("model override: err %v, model %q", err, fake.last.Model)
	}
	if !strings.Contains(fake.last.Messages[0].Content, "be brief") {
		t.Fatal("the child lost the base system prompt")
	}
}
