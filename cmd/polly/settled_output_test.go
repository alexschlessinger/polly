package main

import (
	"errors"
	"strings"
	"testing"

	"bytes"
	"context"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"sync/atomic"
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

// A turn that settlement reopened holds several answers; tool preambles, tool
// results and the synthetic coordination messages between them are not answers.
func TestAnswerBlocksSkipToolPreamblesAndSyntheticInput(t *testing.T) {
	reopened := &llm.AgentResponse{
		Message: &messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "second\n"},
		AllMessages: []messages.ChatMessage{
			{Role: messages.MessageRoleAssistant, Content: "first"},
			{Role: messages.MessageRoleUser, Content: "Coordination is still outstanding: task t", Metadata: map[string]any{messages.MetadataKeyAgentSynthetic: true}},
			{Role: messages.MessageRoleAssistant, Content: "let me fix that", ToolCalls: []messages.ChatMessageToolCall{{ID: "c", Name: "swarm_control"}}},
			{Role: messages.MessageRoleTool, ToolCallID: "c", ToolName: "swarm_control", Content: "canceled"},
			{Role: messages.MessageRoleAssistant, Content: "second\n"},
		},
	}
	if got := answerBlocks(reopened); len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("answer blocks = %q", got)
	}
	if got := settledAnswer(reopened, errors.New("blocked"), "s1"); got != "first\n\nsecond" {
		t.Fatalf("settled answer = %q", got)
	}
	blank := &llm.AgentResponse{Message: &messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: " "}}
	if got := answerBlocks(blank); len(got) != 0 {
		t.Fatalf("blank answer counted: %q", got)
	}
}

// One-shot settled output prints every answer block of a turn that swarm
// settlement reopened, whether the parent then resolves the blocker or not.
func TestSettledOutputPrintsEveryAnswerBlock(t *testing.T) {
	for _, resolved := range []bool{true, false} {
		t.Run(map[bool]string{true: "resolved", false: "blocked"}[resolved], func(t *testing.T) {
			var calls atomic.Int32
			var taskID atomic.Value
			r := newSwarmTestREPL(t, integrationModel(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				switch calls.Add(1) {
				case 1:
					return spawnTestReply("first answer")
				case 2:
					last := req.Messages[len(req.Messages)-1]
					if last.Role != messages.MessageRoleUser || !strings.HasPrefix(last.Content, "Coordination is still outstanding: task ") {
						t.Errorf("continuation input: %+v", last)
					}
					if resolved {
						return spawnTestToolCall("swarm_control", `{"action":"cancel_task","id":"`+taskID.Load().(string)+`"}`)
					}
					return spawnTestReply("second answer")
				default:
					return spawnTestReply("second answer")
				}
			}), nil)
			ctx := context.Background()
			task, err := r.state.swarm.CreateTask(ctx, "pending work", "review", nil, "")
			if err != nil {
				t.Fatal(err)
			}
			taskID.Store(task.ID)
			config := &Config{}
			ui := newLineTurnUI(config, nil)
			var stdout, stderr bytes.Buffer
			ui.writer, ui.errWriter = &stdout, &stderr
			code, err := executeTurnWithUserMessage(ctx, config, r.state, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "go"}, nil, nil, ui, false)
			if got := strings.TrimSpace(stdout.String()); got != "first answer\n\nsecond answer" {
				t.Fatalf("stdout = %q", stdout.String())
			}
			if resolved {
				if err != nil || code != 0 || calls.Load() != 3 {
					t.Fatalf("resolved turn: code %d err %v calls %d", code, err, calls.Load())
				}
			} else if err == nil || code == 0 || !strings.Contains(err.Error(), "pending; resolve its dependencies") || calls.Load() != 2 {
				t.Fatalf("blocked turn: code %d err %v calls %d", code, err, calls.Load())
			}
			waitSwarmIdle(t, r.state.swarm)
		})
	}
}
