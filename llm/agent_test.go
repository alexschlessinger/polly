package llm

import (
	"context"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// sequentialLLM returns a fixed sequence of ChatMessages, one per call.
type sequentialLLM struct {
	responses []messages.ChatMessage
	callCount int
}

func (s *sequentialLLM) ChatCompletionStream(_ context.Context, _ *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	idx := s.callCount
	s.callCount++

	msgChan := make(chan messages.ChatMessage, 1)
	var msg messages.ChatMessage
	if idx < len(s.responses) {
		msg = s.responses[idx]
	} else {
		// fallback: plain end_turn text
		msg = messages.ChatMessage{
			Role:       messages.MessageRoleAssistant,
			Content:    "done",
			StopReason: messages.StopReasonEndTurn,
		}
	}
	msgChan <- msg
	close(msgChan)
	return processor.ProcessMessagesToEvents(msgChan)
}

// TestAgentResponseToolNudge: model returns text on first call, then returns
// end_turn with the response tool call present on second call. Agent should
// re-prompt once and then complete.
func TestAgentResponseToolNudge(t *testing.T) {
	fake := &sequentialLLM{
		responses: []messages.ChatMessage{
			{
				Role:       messages.MessageRoleAssistant,
				Content:    "here is my answer in plain text",
				StopReason: messages.StopReasonEndTurn,
			},
			{
				Role: messages.MessageRoleAssistant,
				ToolCalls: []messages.ChatMessageToolCall{
					{ID: "tc1", Name: "respond", Arguments: `{"text":"structured answer"}`},
				},
				StopReason: messages.StopReasonEndTurn,
			},
		},
	}

	agent := NewAgent(fake, nil, AgentConfig{
		MaxIterations: 5,
		ResponseTool:  "respond",
	})

	resp, err := agent.Run(context.Background(), &CompletionRequest{
		Messages: messages.User("hello"),
	}, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.callCount != 2 {
		t.Fatalf("expected 2 LLM calls, got %d", fake.callCount)
	}
	// The nudge message should be in AllMessages
	var nudgeFound bool
	for _, m := range resp.AllMessages {
		if m.Role == messages.MessageRoleUser && m.Content == "Respond using the respond tool." {
			nudgeFound = true
			break
		}
	}
	if !nudgeFound {
		t.Fatal("expected nudge message in AllMessages")
	}
	// Final message should contain the tool call
	if len(resp.Message.ToolCalls) == 0 || resp.Message.ToolCalls[0].Name != "respond" {
		t.Fatal("expected final message to contain respond tool call")
	}
}

// TestAgentResponseToolNoInfiniteLoop: model never calls the response tool.
// Agent should nudge once, then return normally on the second text response.
func TestAgentResponseToolNoInfiniteLoop(t *testing.T) {
	fake := &sequentialLLM{
		responses: []messages.ChatMessage{
			{
				Role:       messages.MessageRoleAssistant,
				Content:    "first plain text",
				StopReason: messages.StopReasonEndTurn,
			},
			{
				Role:       messages.MessageRoleAssistant,
				Content:    "second plain text, still no tool",
				StopReason: messages.StopReasonEndTurn,
			},
		},
	}

	agent := NewAgent(fake, nil, AgentConfig{
		MaxIterations: 10,
		ResponseTool:  "respond",
	})

	resp, err := agent.Run(context.Background(), &CompletionRequest{
		Messages: messages.User("hello"),
	}, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should make exactly 2 LLM calls: original + one nudge retry
	if fake.callCount != 2 {
		t.Fatalf("expected 2 LLM calls, got %d", fake.callCount)
	}
	// Final message is the second text response
	if resp.Message.Content != "second plain text, still no tool" {
		t.Fatalf("unexpected final message: %q", resp.Message.Content)
	}
}

// TestAgentResponseToolNotSetPassthrough: when ResponseTool is empty, text
// responses complete immediately without any nudge.
func TestAgentResponseToolNotSetPassthrough(t *testing.T) {
	fake := &sequentialLLM{
		responses: []messages.ChatMessage{
			{
				Role:       messages.MessageRoleAssistant,
				Content:    "direct answer",
				StopReason: messages.StopReasonEndTurn,
			},
		},
	}

	agent := NewAgent(fake, nil, AgentConfig{
		MaxIterations: 5,
		// ResponseTool not set
	})

	resp, err := agent.Run(context.Background(), &CompletionRequest{
		Messages: messages.User("hello"),
	}, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.callCount != 1 {
		t.Fatalf("expected 1 LLM call, got %d", fake.callCount)
	}
	if resp.Message.Content != "direct answer" {
		t.Fatalf("unexpected final message: %q", resp.Message.Content)
	}
}

// TestAgentResponseToolShortCircuit: when the model emits the response tool
// with StopReasonToolUse, the agent must execute the tool and then return
// immediately without making another LLM call. Otherwise a "wasted" extra
// completion is generated whose plain-text output the caller discards.
func TestAgentResponseToolShortCircuit(t *testing.T) {
	fake := &sequentialLLM{
		responses: []messages.ChatMessage{
			{
				Role: messages.MessageRoleAssistant,
				ToolCalls: []messages.ChatMessageToolCall{
					{ID: "tc1", Name: "respond", Arguments: `{"text":"structured answer"}`},
				},
				StopReason: messages.StopReasonToolUse,
			},
			{
				Role:       messages.MessageRoleAssistant,
				Content:    "wasted text the caller would never read",
				StopReason: messages.StopReasonEndTurn,
			},
		},
	}

	var executed bool
	respondTool := &tools.Func{
		Name: "respond",
		Run: func(_ context.Context, args tools.Args) (string, error) {
			executed = true
			return "ok", nil
		},
	}
	registry := tools.NewToolRegistry([]tools.Tool{respondTool})

	agent := NewAgent(fake, registry, AgentConfig{
		MaxIterations: 5,
		ResponseTool:  "respond",
	})

	resp, err := agent.Run(context.Background(), &CompletionRequest{
		Messages: messages.User("hi"),
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.callCount != 1 {
		t.Fatalf("expected exactly 1 LLM call after short-circuit, got %d", fake.callCount)
	}
	if !executed {
		t.Fatal("expected respond tool to be executed before short-circuit")
	}
	if len(resp.Message.ToolCalls) == 0 || resp.Message.ToolCalls[0].Name != "respond" {
		t.Fatalf("expected final message to carry the respond tool call, got %#v", resp.Message)
	}
	if resp.IterationCount != 1 {
		t.Fatalf("expected IterationCount=1, got %d", resp.IterationCount)
	}
}

// TestAgentDenialShortCircuits: when every tool in a batch is denied, the
// agent must stop after filling denial stubs rather than kicking off another
// LLM call to editorialize. The generated messages should include the
// assistant tool-use turn and the denial stubs, and nothing more.
func TestAgentDenialShortCircuits(t *testing.T) {
	fake := &sequentialLLM{
		responses: []messages.ChatMessage{
			{
				Role: messages.MessageRoleAssistant,
				ToolCalls: []messages.ChatMessageToolCall{
					{ID: "tc1", Name: "bash", Arguments: `{"command":"ls"}`},
				},
				StopReason: messages.StopReasonToolUse,
			},
			{
				Role:       messages.MessageRoleAssistant,
				Content:    "should never run",
				StopReason: messages.StopReasonEndTurn,
			},
		},
	}

	agent := NewAgent(fake, nil, AgentConfig{MaxIterations: 5})

	resp, err := agent.Run(context.Background(), &CompletionRequest{
		Messages: messages.User("ls"),
	}, &AgentCallbacks{
		ApproveToolCalls: func(calls []messages.ChatMessageToolCall) []bool {
			return make([]bool, len(calls))
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.callCount != 1 {
		t.Fatalf("expected exactly 1 LLM call before short-circuit, got %d", fake.callCount)
	}
	if len(resp.AllMessages) != 2 {
		t.Fatalf("expected 2 generated messages (assistant tool_use + denial stub), got %d", len(resp.AllMessages))
	}
	if resp.AllMessages[1].Content != ToolDeniedContent {
		t.Fatalf("expected second message to be denial stub, got %q", resp.AllMessages[1].Content)
	}
}

// TestStripDeniedExchangesRemovesPair: the filter should drop the denied
// tool-result message and the assistant tool_use that proposed it.
func TestStripDeniedExchangesRemovesPair(t *testing.T) {
	msgs := []messages.ChatMessage{
		{
			Role: messages.MessageRoleAssistant,
			ToolCalls: []messages.ChatMessageToolCall{
				{ID: "tc1", Name: "bash", Arguments: `{"command":"ls"}`},
			},
		},
		{
			Role:       messages.MessageRoleTool,
			Content:    ToolDeniedContent,
			ToolCallID: "tc1",
			ToolName:   "bash",
		},
	}
	out := StripDeniedExchanges(msgs)
	if len(out) != 0 {
		t.Fatalf("expected all messages stripped, got %d: %#v", len(out), out)
	}
}

// TestStripDeniedExchangesPreservesPartial: in a mixed batch where one tool
// was denied and another ran, the filter should remove only the denied pair
// and keep the approved tool call and its result.
func TestStripDeniedExchangesPreservesPartial(t *testing.T) {
	msgs := []messages.ChatMessage{
		{
			Role: messages.MessageRoleAssistant,
			ToolCalls: []messages.ChatMessageToolCall{
				{ID: "tc1", Name: "bash", Arguments: `{"command":"ls"}`},
				{ID: "tc2", Name: "grep", Arguments: `{"pattern":"foo"}`},
			},
		},
		{
			Role:       messages.MessageRoleTool,
			Content:    ToolDeniedContent,
			ToolCallID: "tc1",
			ToolName:   "bash",
		},
		{
			Role:       messages.MessageRoleTool,
			Content:    "match: foo",
			ToolCallID: "tc2",
			ToolName:   "grep",
		},
	}
	out := StripDeniedExchanges(msgs)
	if len(out) != 2 {
		t.Fatalf("expected 2 messages (assistant + approved tool result), got %d", len(out))
	}
	if len(out[0].ToolCalls) != 1 || out[0].ToolCalls[0].ID != "tc2" {
		t.Fatalf("expected only tc2 to survive on assistant message, got %#v", out[0].ToolCalls)
	}
	if out[1].ToolCallID != "tc2" {
		t.Fatalf("expected surviving tool result to be tc2, got %q", out[1].ToolCallID)
	}
}

// TestStripDeniedExchangesKeepsAssistantContent: if an assistant message
// carried both text and a denied tool call, the text survives.
func TestStripDeniedExchangesKeepsAssistantContent(t *testing.T) {
	msgs := []messages.ChatMessage{
		{
			Role:    messages.MessageRoleAssistant,
			Content: "let me check",
			ToolCalls: []messages.ChatMessageToolCall{
				{ID: "tc1", Name: "bash", Arguments: `{"command":"ls"}`},
			},
		},
		{
			Role:       messages.MessageRoleTool,
			Content:    ToolDeniedContent,
			ToolCallID: "tc1",
		},
	}
	out := StripDeniedExchanges(msgs)
	if len(out) != 1 {
		t.Fatalf("expected 1 surviving message, got %d", len(out))
	}
	if out[0].Content != "let me check" || len(out[0].ToolCalls) != 0 {
		t.Fatalf("expected content preserved and tool_calls empty, got %#v", out[0])
	}
}
