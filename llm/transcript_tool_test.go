package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

func TestRenderTranscriptSkipsInternalAndElidesRecallResults(t *testing.T) {
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleInternal, Content: "app state"},
		{Role: messages.MessageRoleUser, Content: "find the needle"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "b", Name: "bash", Arguments: `{"cmd":"grep"}`}}},
		{Role: messages.MessageRoleTool, ToolName: "bash", ToolCallID: "b", Content: "needle found on line 7"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "r", Name: "read_artifact", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolName: "read_artifact", ToolCallID: "r", Content: "RECALLED PAYLOAD"},
	}
	rendered := renderTranscript(history)
	if strings.Contains(rendered, "app state") {
		t.Fatalf("internal message leaked into the rendering: %q", rendered)
	}
	if !strings.Contains(rendered, "=== message 1: user ===") {
		t.Fatalf("internal message shifted transcript indexes: %q", rendered)
	}
	for _, want := range []string{`[tool call bash {"cmd":"grep"}]`, "needle found on line 7", "=== message 3: tool bash ===", "[read_artifact result not rendered"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendering missing %q: %q", want, rendered)
		}
	}
	if strings.Contains(rendered, "RECALLED PAYLOAD") {
		t.Fatalf("recall result was inlined into the rendering: %q", rendered)
	}
}

func TestReadTranscriptToolPagesAndSearches(t *testing.T) {
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "alpha\nbeta\ngamma"},
	}
	tool := &readTranscriptTool{snapshot: func() []messages.ChatMessage { return history }}

	found, err := tool.Execute(context.Background(), map[string]any{"query": "beta"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(found, "beta") || strings.Contains(found, "gamma") {
		t.Fatalf("query result = %q", found)
	}

	page, err := tool.Execute(context.Background(), map[string]any{"offset": 2, "limit": 2})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page, "alpha") || !strings.Contains(page, "beta") || strings.Contains(page, "gamma") {
		t.Fatalf("page result = %q", page)
	}

	if _, err := tool.Execute(context.Background(), map[string]any{"offset": 0}); err == nil {
		t.Fatal("offset 0 was accepted")
	}
	if _, err := tool.Execute(context.Background(), map[string]any{"limit": artifactReadMaxLines + 1}); err == nil {
		t.Fatal("oversized limit was accepted")
	}
}

type transcriptSearcherLLM struct{ calls int }

func (l *transcriptSearcherLLM) ChatCompletionStream(_ context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	var response messages.ChatMessage
	if l.calls == 0 {
		response = messages.ChatMessage{
			Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
			ToolCalls: []messages.ChatMessageToolCall{{ID: "recall", Name: "read_transcript", Arguments: `{"query":"NEEDLE-9C41"}`}},
		}
	} else {
		response = messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Content: "done"}
	}
	l.calls++
	input := make(chan messages.ChatMessage, 1)
	input <- response
	close(input)
	return processor.ProcessMessagesToEvents(input)
}

func TestAgentRecoversOmittedConversationViaReadTranscript(t *testing.T) {
	agent := NewAgent(&transcriptSearcherLLM{}, nil, AgentConfig{ArtifactStore: newTestArtifactStore()})
	// The needle sits on its own line so the recall result stays small; the
	// filler line forces the exchange out of the projection.
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "remember the code NEEDLE-9C41\n" + strings.Repeat("x", 8_000)},
		{Role: messages.MessageRoleAssistant, Content: "noted"},
		{Role: messages.MessageRoleUser, Content: "what was the code?"},
	}

	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: history, MaxContextTokens: 2_000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Projection.OmittedExchanges == 0 {
		t.Fatalf("fixture did not force omission: %+v", response.Projection)
	}
	var recall messages.ChatMessage
	for _, msg := range response.AllMessages {
		if msg.Role == messages.MessageRoleTool && msg.ToolName == "read_transcript" {
			recall = msg
		}
	}
	succeeded, known := recall.ToolSucceeded()
	if !known || !succeeded || !strings.Contains(recall.Content, "NEEDLE-9C41") {
		t.Fatalf("omitted conversation was not recoverable: %#v", recall)
	}
}
