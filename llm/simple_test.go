package llm

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// TestExecuteWithToolsAppendsAssistantOnce: a response carrying several tool
// calls must land in history as one assistant message followed by one result
// per call — not appended again for every call.
func TestExecuteWithToolsAppendsAssistantOnce(t *testing.T) {
	fake := &sequentialLLM{
		responses: []messages.ChatMessage{
			{
				Role: messages.MessageRoleAssistant,
				ToolCalls: []messages.ChatMessageToolCall{
					{ID: "tc1", Name: "noop", Arguments: "{}"},
					{ID: "tc2", Name: "noop", Arguments: "{}"},
				},
				StopReason: messages.StopReasonToolUse,
			},
			{
				Role:       messages.MessageRoleAssistant,
				Content:    "done",
				StopReason: messages.StopReasonEndTurn,
			},
		},
	}
	noop := &tools.Func{
		Name: "noop",
		Run:  func(context.Context, tools.Args) (string, error) { return "ok", nil },
	}
	registry := tools.NewToolRegistry([]tools.Tool{noop})

	builder := NewCompletionBuilder("fake/model").WithUserMessage("go")
	response, err := builder.ExecuteWithTools(context.Background(), fake, registry)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if response == nil || response.Content != "done" {
		t.Fatalf("response = %#v, want final content %q", response, "done")
	}

	var roles []string
	for _, m := range builder.req.Messages {
		roles = append(roles, m.Role)
	}
	want := []string{
		messages.MessageRoleUser,
		messages.MessageRoleAssistant,
		messages.MessageRoleTool,
		messages.MessageRoleTool,
	}
	if len(roles) != len(want) {
		t.Fatalf("history roles = %v, want %v", roles, want)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("history roles = %v, want %v", roles, want)
		}
	}
	if builder.req.Messages[2].ToolCallID != "tc1" || builder.req.Messages[3].ToolCallID != "tc2" {
		t.Fatalf("tool results out of order: %#v", builder.req.Messages[2:])
	}
}

// TestExecuteWithToolsCapsRounds: a model that never stops calling tools must
// hit the round cap instead of looping forever.
func TestExecuteWithToolsCapsRounds(t *testing.T) {
	noop := &tools.Func{
		Name: "noop",
		Run:  func(context.Context, tools.Args) (string, error) { return "ok", nil },
	}
	registry := tools.NewToolRegistry([]tools.Tool{noop})
	fake := &alwaysToolUseLLM{}

	response, err := NewCompletionBuilder("fake/model").
		WithUserMessage("go").
		ExecuteWithTools(context.Background(), fake, registry)

	if !errors.Is(err, ErrMaxIterations) {
		t.Fatalf("err = %v, want ErrMaxIterations", err)
	}
	if response == nil || response.StopReason != messages.StopReasonMaxIterations {
		t.Fatalf("response = %#v, want stop reason %q", response, messages.StopReasonMaxIterations)
	}
	if fake.calls != 1024 {
		t.Fatalf("LLM calls = %d, want 1024", fake.calls)
	}
}

func TestExecuteWithToolsPreservesOrderSchemasAndRegistry(t *testing.T) {
	var order []string
	first := &tools.Func{Name: "first", Run: func(context.Context, tools.Args) (string, error) {
		order = append(order, "first")
		return "ok", nil
	}}
	second := &tools.Func{Name: "second", Run: func(context.Context, tools.Args) (string, error) {
		order = append(order, "second")
		return "ok", nil
	}}
	registry := tools.NewToolRegistry([]tools.Tool{first, second})
	defer registry.Close()
	model := &promptCacheRecordingLLM{responses: []messages.ChatMessage{{
		Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
		ToolCalls: []messages.ChatMessageToolCall{{ID: "1", Name: "first", Arguments: "{}"}, {ID: "2", Name: "second", Arguments: "{}"}},
	}}}
	_, err := NewCompletionBuilder("test").WithUserMessage("go").WithTools([]tools.Tool{first}).ExecuteWithTools(context.Background(), model, registry)
	if err != nil || !reflect.DeepEqual(order, []string{"first", "second"}) {
		t.Fatalf("order=%v error=%v", order, err)
	}
	for _, req := range model.requests {
		if len(req.Tools) != 1 || req.Tools[0].GetName() != "first" {
			t.Fatalf("explicit schemas changed: %v", req.Tools)
		}
	}
	if len(registry.All()) != 2 {
		t.Fatal("builder changed or closed the caller's registry")
	}
}

func TestExecuteWithToolsKeepsRichResults(t *testing.T) {
	rich := &testRichTool{name: "rich", output: tools.ToolOutput{Text: "image", Data: 42,
		Media: []tools.ToolMedia{{Data: []byte("image bytes"), MIMEType: "image/png"}},
	}}
	registry := tools.NewToolRegistry([]tools.Tool{rich})
	defer registry.Close()
	model := &promptCacheRecordingLLM{responses: []messages.ChatMessage{{
		Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
		Reasoning: "make an image", ToolCalls: []messages.ChatMessageToolCall{{ID: "1", Name: "rich", Arguments: "{}"}},
	}}}
	builder := NewCompletionBuilder("test").WithUserMessage("go")
	_, err := builder.ExecuteWithTools(context.Background(), model, registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(builder.req.Messages) != 3 || builder.req.Messages[1].Reasoning != "make an image" {
		t.Fatalf("history=%+v", builder.req.Messages)
	}
	result := builder.req.Messages[2]
	if result.Content != "image" || len(result.Parts) != 1 || result.Parts[0].Type != "image_base64" || result.Metadata["tool_data"] != float64(42) {
		t.Fatalf("rich result lost: %+v", result)
	}
	if success, known := result.ToolSucceeded(); !known || !success {
		t.Fatalf("missing success receipt: %+v", result)
	}
	if len(model.requests[0].Tools) != 1 || model.requests[0].Tools[0].GetName() != "rich" {
		t.Fatal("builder advertised extra tools")
	}
}

func TestExecuteWithToolsCanCorrectInvalidCalls(t *testing.T) {
	registry := tools.NewToolRegistry(nil)
	defer registry.Close()
	model := &promptCacheRecordingLLM{responses: []messages.ChatMessage{{
		Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
		ToolCalls: []messages.ChatMessageToolCall{{ID: "bad", Name: "missing", Arguments: "{"}, {ID: "missing", Name: "missing", Arguments: "{}"}},
	}}}
	builder := NewCompletionBuilder("test").WithUserMessage("go")
	response, err := builder.ExecuteWithTools(context.Background(), model, registry)
	if err != nil || response == nil || response.Content != "done" || len(model.requests) != 2 {
		t.Fatalf("recovery: response=%+v error=%v calls=%d", response, err, len(model.requests))
	}
	if len(model.requests[0].Tools) != 0 || len(builder.req.Messages) != 4 {
		t.Fatalf("unexpected tools or history: %+v", builder.req.Messages)
	}
	for _, result := range builder.req.Messages[2:] {
		if success, known := result.ToolSucceeded(); !known || success || result.Role != messages.MessageRoleTool {
			t.Fatalf("missing failure receipt: %+v", result)
		}
	}
}

func TestExecuteWithToolsRetainsInterruptedBatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := &tools.Func{Name: "first", Run: func(context.Context, tools.Args) (string, error) {
		cancel()
		return "completed", nil
	}}
	second := &tools.Func{Name: "second", Run: func(context.Context, tools.Args) (string, error) {
		t.Error("second tool ran after cancellation")
		return "", nil
	}}
	registry := tools.NewToolRegistry([]tools.Tool{first, second})
	defer registry.Close()
	model := &sequentialLLM{responses: []messages.ChatMessage{{
		Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
		ToolCalls: []messages.ChatMessageToolCall{{ID: "1", Name: "first", Arguments: "{}"}, {ID: "2", Name: "second", Arguments: "{}"}},
	}}}
	builder := NewCompletionBuilder("test").WithUserMessage("go")
	_, err := builder.ExecuteWithTools(ctx, model, registry)
	if !errors.Is(err, context.Canceled) || len(builder.req.Messages) != 4 {
		t.Fatalf("interrupted history=%+v error=%v", builder.req.Messages, err)
	}
	if builder.req.Messages[2].Content != "completed" || builder.req.Messages[3].Content != ToolInterruptedContent {
		t.Fatalf("partial results lost: %+v", builder.req.Messages[2:])
	}
}

func TestExecuteWithToolsNilRegistryReturnsCalls(t *testing.T) {
	model := &sequentialLLM{responses: []messages.ChatMessage{{
		Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
		ToolCalls: []messages.ChatMessageToolCall{{ID: "1", Name: "work", Arguments: "{}"}},
	}}}
	builder := NewCompletionBuilder("test").WithUserMessage("go")
	response, err := builder.ExecuteWithTools(context.Background(), model, nil)
	if err != nil || response == nil || len(response.ToolCalls) != 1 || model.callCount != 1 || len(builder.req.Messages) != 1 {
		t.Fatalf("nil registry: response=%+v error=%v calls=%d", response, err, model.callCount)
	}
}

// TestSimpleProcessorEmptyCompletionCompletes: a stream that ends with no
// content and no tool calls (a refusal, a content-filter stop) must still
// yield a Complete event carrying the stop reason; only a stream with no
// messages at all stays silent.
func TestSimpleProcessorEmptyCompletionCompletes(t *testing.T) {
	msgChan := make(chan messages.ChatMessage, 1)
	msgChan <- messages.ChatMessage{
		Role:       messages.MessageRoleAssistant,
		StopReason: messages.StopReasonContentFilter,
	}
	close(msgChan)

	var events []*messages.StreamEvent
	for event := range (&SimpleProcessor{}).ProcessMessagesToEvents(msgChan) {
		events = append(events, event)
	}
	if len(events) != 1 || events[0].Type != messages.EventTypeComplete {
		t.Fatalf("events = %#v, want a single Complete", events)
	}
	if got := events[0].Message.StopReason; got != messages.StopReasonContentFilter {
		t.Fatalf("stop reason = %q, want %q", got, messages.StopReasonContentFilter)
	}

	empty := make(chan messages.ChatMessage)
	close(empty)
	for event := range (&SimpleProcessor{}).ProcessMessagesToEvents(empty) {
		t.Fatalf("empty stream produced event %#v", event)
	}
}
