package llm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestRequiredResponseToolActuallySucceeds(t *testing.T) {
	for _, kind := range []string{"success", "failure", "denied", "plain", "end_turn", "max_tokens"} {
		t.Run(kind, func(t *testing.T) {
			msg := messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "r", Name: "respond", Arguments: `{}`}}}
			if kind == "plain" {
				msg = messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done", StopReason: messages.StopReasonEndTurn}
			}
			if kind == "end_turn" {
				msg.StopReason = messages.StopReasonEndTurn
			}
			if kind == "max_tokens" {
				msg.StopReason = messages.StopReasonMaxTokens
			}
			model := &sequentialLLM{responses: []messages.ChatMessage{msg}}
			executed := 0
			registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "respond", Run: func(context.Context, tools.Args) (string, error) {
				executed++
				if kind == "failure" {
					return "bad", errors.New("invalid")
				}
				return "ok", nil
			}}})
			defer registry.Close()
			a := NewAgent(model, registry, AgentConfig{MaxIterations: 2, ResponseTool: "respond", RequireResponseToolSuccess: true})
			defer a.Close()
			cb := &AgentCallbacks{}
			if kind == "denied" {
				cb.ApproveToolCalls = func([]messages.ChatMessageToolCall) []bool { return []bool{false} }
			}
			_, err := a.Run(context.Background(), &CompletionRequest{Messages: messages.User("test")}, cb)
			valid := kind == "success" || kind == "end_turn"
			if (err == nil) != valid || model.callCount != 1 {
				t.Fatalf("error=%v calls=%d", err, model.callCount)
			}
			if valid && executed != 1 {
				t.Fatal("completed without execution")
			}
		})
	}
}

func TestRequiredResponseToolUsesHostCorrection(t *testing.T) {
	response := func(id string) messages.ChatMessage {
		return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: id, Name: "respond", Arguments: `{}`}}}
	}
	model := &sequentialLLM{responses: []messages.ChatMessage{response("bad"), response("good")}}
	n := 0
	registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "respond", Run: func(context.Context, tools.Args) (string, error) {
		n++
		if n == 1 {
			return "invalid", errors.New("invalid")
		}
		return "ok", nil
	}}})
	defer registry.Close()
	a := NewAgent(model, registry, AgentConfig{MaxIterations: 3, ResponseTool: "respond", RequireResponseToolSuccess: true})
	defer a.Close()
	result, err := a.Run(context.Background(), &CompletionRequest{}, &AgentCallbacks{ContinueAfterFinal: func(context.Context, *messages.ChatMessage) ([]messages.ChatMessage, error) {
		if n == 1 {
			return messages.User("correct the result"), nil
		}
		return nil, nil
	}})
	if err != nil || n != 2 || model.callCount != 2 {
		t.Fatalf("n=%d calls=%d err=%v", n, model.callCount, err)
	}
	for _, msg := range result.AllMessages {
		if strings.Contains(msg.Content, "Respond using the respond tool.") {
			t.Fatal("legacy reminder competed with host correction")
		}
	}
}
