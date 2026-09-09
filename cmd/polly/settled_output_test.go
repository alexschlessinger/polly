package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestSettledAnswerKeepsTheModelsAnswerOnFailure(t *testing.T) {
	failure := errors.New("iteration limit")
	answered := &llm.AgentResponse{Message: &messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "the answer"}}
	if got := settledAnswer(answered, failure, "s1"); got != "the answer" {
		t.Fatalf("answer replaced by the error: %q", got)
	}
	// A run that failed before answering reports the blocker and the session.
	toolsOnly := &llm.AgentResponse{Message: &messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "x", Name: "bash"}}}}
	for _, resp := range []*llm.AgentResponse{nil, {}, toolsOnly} {
		got := settledAnswer(resp, failure, "s1")
		if !strings.HasPrefix(got, "Blocked: iteration limit") || !strings.Contains(got, "Session: s1") {
			t.Fatalf("blocker report: %q", got)
		}
	}
}
