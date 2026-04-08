package llm

import (
	"context"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
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
