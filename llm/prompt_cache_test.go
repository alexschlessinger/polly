package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestDerivePromptCacheKeyIsStableAcrossDynamicHistoryAndToolOrder(t *testing.T) {
	alpha := testTool{name: "alpha", schema: schema.Tool("alpha", "first", schema.Params{"value": schema.S("value")})}
	zebra := testTool{name: "zebra", schema: schema.Tool("zebra", "last", nil)}
	base := &CompletionRequest{
		Model:          "openai/gpt-5.4",
		Temperature:    Float32Ptr(0.2),
		MaxTokens:      2048,
		ThinkingEffort: EffortLevel(LevelHigh),
		Tools:          []tools.Tool{zebra, alpha},
		ResponseSchema: &Schema{Strict: true, Raw: map[string]any{
			"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}},
		}},
	}
	firstHistory := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "stable system"},
		{Role: messages.MessageRoleUser, Content: "first user's private content"},
	}
	secondHistory := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "stable system"},
		{Role: messages.MessageRoleUser, Content: "different dynamic content"},
		{Role: messages.MessageRoleAssistant, Content: "old answer"},
	}

	first, err := derivePromptCacheKey(base, firstHistory)
	if err != nil {
		t.Fatal(err)
	}
	reordered := *base
	reordered.Tools = []tools.Tool{alpha, zebra}
	second, err := derivePromptCacheKey(&reordered, secondHistory)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("dynamic history or tool registration order changed key: %q != %q", first, second)
	}
	if len(first) != 64 || first != strings.ToLower(first) {
		t.Fatalf("prompt key = %q, want 64 lowercase hex characters", first)
	}

	changedSystem := append([]messages.ChatMessage(nil), secondHistory...)
	changedSystem[0].Content = "different system"
	third, err := derivePromptCacheKey(base, changedSystem)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("changing the resolved system prompt did not change the prompt cache key")
	}
}

type promptCacheRecordingLLM struct {
	requests  []CompletionRequest
	responses []messages.ChatMessage
}

func (r *promptCacheRecordingLLM) ChatCompletionStream(_ context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	copyReq := *req
	copyReq.Messages = append([]messages.ChatMessage(nil), req.Messages...)
	r.requests = append(r.requests, copyReq)

	index := len(r.requests) - 1
	response := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done", StopReason: messages.StopReasonEndTurn}
	if index < len(r.responses) {
		response = r.responses[index]
	}
	stream := make(chan messages.ChatMessage, 1)
	stream <- response
	close(stream)
	return processor.ProcessMessagesToEvents(stream)
}

func TestAgentDerivesPromptKeyAndAggregatesCacheUsage(t *testing.T) {
	first := messages.ChatMessage{
		Role: messages.MessageRoleAssistant,
		ToolCalls: []messages.ChatMessageToolCall{
			{ID: "call-1", Name: "lookup", Arguments: `{}`},
		},
		StopReason: messages.StopReasonToolUse,
	}
	first.SetPromptCacheUsage(120, 40)
	second := messages.ChatMessage{
		Role:       messages.MessageRoleAssistant,
		Content:    "done",
		StopReason: messages.StopReasonEndTurn,
	}
	second.SetPromptCacheUsage(180, 0)

	model := &promptCacheRecordingLLM{responses: []messages.ChatMessage{first, second}}
	registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{
		Name: "lookup",
		Run: func(context.Context, tools.Args) (string, error) {
			return "result", nil
		},
	}})
	agent := NewAgent(model, registry, AgentConfig{MaxIterations: 3})
	response, err := agent.Run(context.Background(), &CompletionRequest{
		Model: "openai/gpt-5.4",
		Messages: []messages.ChatMessage{
			{Role: messages.MessageRoleSystem, Content: "system"},
			{Role: messages.MessageRoleUser, Content: "question"},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.PromptCache.ReadInputTokens != 300 || response.PromptCache.WriteInputTokens != 40 {
		t.Fatalf("aggregate cache stats = %+v, want read=300 write=40", response.PromptCache)
	}
	if len(model.requests) != 2 {
		t.Fatalf("model calls = %d, want 2", len(model.requests))
	}
	for i, req := range model.requests {
		if len(req.PromptCacheKey) != 64 {
			t.Fatalf("request %d prompt key = %q, want 64 characters", i, req.PromptCacheKey)
		}
		if req.PromptCacheKey != model.requests[0].PromptCacheKey {
			t.Fatalf("tool loop changed stable prompt key: %q != %q", req.PromptCacheKey, model.requests[0].PromptCacheKey)
		}
	}
	if len(response.AllMessages) < 3 || response.AllMessages[0].GetCacheReadInputTokens() != 120 {
		t.Fatalf("assistant cache metadata was not retained in generated history: %#v", response.AllMessages)
	}
}

func TestAgentPreservesExplicitPromptCacheKey(t *testing.T) {
	model := &promptCacheRecordingLLM{}
	agent := NewAgent(model, nil, AgentConfig{MaxIterations: 1})
	const explicit = "caller-owned-key"
	_, err := agent.Run(context.Background(), &CompletionRequest{
		Model:          "openai/gpt-5.4",
		PromptCacheKey: explicit,
		Messages:       messages.User("hi"),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := model.requests[0].PromptCacheKey; got != explicit {
		t.Fatalf("explicit prompt key = %q, want %q", got, explicit)
	}
}
