package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestAdmissionCheckpointsAfterWholeBatch(t *testing.T) {
	model := &promptCacheRecordingLLM{responses: []messages.ChatMessage{{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "t", Name: "work", Arguments: `{}`}}}, {Role: messages.MessageRoleAssistant, Content: "done", StopReason: messages.StopReasonEndTurn}}}
	done := false
	registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "work", Run: func(context.Context, tools.Args) (string, error) { done = true; return "result", nil }}})
	a := NewAgent(model, registry, AgentConfig{MaxIterations: 3})
	defer a.Close()
	admitted := false
	var checkpoints [][]messages.ChatMessage
	response, err := a.Run(context.Background(), &CompletionRequest{}, &AgentCallbacks{
		AdmitInput: func(context.Context) ([]messages.ChatMessage, error) {
			if done && !admitted {
				admitted = true
				return messages.User("peer reply"), nil
			}
			return nil, nil
		},
		Checkpoint: func(_ context.Context, c AgentCheckpoint) error {
			checkpoints = append(checkpoints, cloneMessages(c.Generated))
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.PersistedMessages != 4 {
		t.Fatalf("persisted=%d", response.PersistedMessages)
	}
	if len(checkpoints) < 2 || len(checkpoints[1]) != 3 || checkpoints[1][1].Role != messages.MessageRoleTool || checkpoints[1][2].Content != "peer reply" {
		t.Fatalf("unordered admission: %+v", checkpoints)
	}
	if len(model.requests) != 2 || model.requests[0].PromptCacheKey == "" || model.requests[0].PromptCacheKey != model.requests[1].PromptCacheKey {
		t.Fatal("admitted peer message changed or removed the stable prompt cache key")
	}
	request := &model.requests[1]
	for name, wire := range map[string]any{"anthropic": (&AnthropicClient{}).buildRequestParams(request), "openai": buildResponsesRequestParams(request)} {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			body := string(data)
			result := strings.Index(body, `"result"`)
			peer := strings.Index(body, `"peer reply"`)
			if result < 0 || peer <= result || strings.Count(body, `"peer reply"`) != 1 {
				t.Fatalf("adapter reordered or duplicated admitted input: %s", body)
			}
		})
	}
}

func TestAdmissionFailedGateDoesNotCommitInput(t *testing.T) {
	model := &sequentialLLM{}
	a := NewAgent(model, tools.NewToolRegistry(nil), AgentConfig{MaxIterations: 1})
	defer a.Close()
	persisted := 0
	_, err := a.Run(context.Background(), &CompletionRequest{}, &AgentCallbacks{
		AdmitInput:         func(context.Context) ([]messages.ChatMessage, error) { return messages.User("peer reply"), nil },
		BeforeFirstRequest: func(ProjectionStats) error { return errors.New("gate rejected") },
		Checkpoint:         func(_ context.Context, c AgentCheckpoint) error { persisted += len(c.Generated); return nil },
	})
	if err == nil || model.callCount != 0 || persisted != 0 {
		t.Fatalf("gate consumed input: %v calls=%d persisted=%d", err, model.callCount, persisted)
	}
}

func TestExclusiveBatchStartsNothing(t *testing.T) {
	calls := 0
	registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "apply", Exclusive: true, Run: func(context.Context, tools.Args) (string, error) { calls++; return "", nil }}, &tools.Func{Name: "write", Run: func(context.Context, tools.Args) (string, error) { calls++; return "", nil }}})
	model := &sequentialLLM{responses: []messages.ChatMessage{{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "a", Name: "apply", Arguments: `{}`}, {ID: "b", Name: "write", Arguments: `{}`}}}}}
	a := NewAgent(model, registry, AgentConfig{MaxIterations: 1})
	defer a.Close()
	response, err := a.Run(context.Background(), &CompletionRequest{}, nil)
	if err == nil || !strings.Contains(err.Error(), "only tool") || calls != 0 || len(response.AllMessages) != 3 {
		t.Fatalf("batch effects: %v calls=%d %+v", err, calls, response)
	}
}

func TestDeniedBatchContinuesOnOutstandingCoordination(t *testing.T) {
	model := &sequentialLLM{responses: []messages.ChatMessage{{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "denied", Name: "work", Arguments: `{}`}}}}}
	registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "work", Run: func(context.Context, tools.Args) (string, error) { t.Error("denied tool ran"); return "", nil }}})
	agent := NewAgent(model, registry, AgentConfig{MaxIterations: 3})
	defer agent.Close()
	settles := 0
	response, err := agent.Run(context.Background(), &CompletionRequest{}, &AgentCallbacks{
		ApproveToolCalls: func([]messages.ChatMessageToolCall) []bool { return []bool{false} },
		ContinueAfterFinal: func(context.Context, *messages.ChatMessage) ([]messages.ChatMessage, error) {
			settles++
			if settles == 1 {
				return messages.User("work remains"), nil
			}
			return nil, nil
		},
	})
	// The denial receipts, the continuation prompt, then the answer to it.
	if err != nil || settles != 2 || model.callCount != 2 || len(response.AllMessages) != 4 || response.AllMessages[2].Role != messages.MessageRoleUser {
		t.Fatalf("denial settlement: %v settles=%d calls=%d messages=%d", err, settles, model.callCount, len(response.AllMessages))
	}
}

func TestContinuationOnTheLastIterationEndsWithTheAnswer(t *testing.T) {
	model := &sequentialLLM{responses: []messages.ChatMessage{{Role: messages.MessageRoleAssistant, Content: "final answer", StopReason: messages.StopReasonEndTurn}}}
	agent := NewAgent(model, tools.NewToolRegistry(nil), AgentConfig{MaxIterations: 1})
	defer agent.Close()
	response, err := agent.Run(context.Background(), &CompletionRequest{}, &AgentCallbacks{
		ContinueAfterFinal: func(context.Context, *messages.ChatMessage) ([]messages.ChatMessage, error) {
			return messages.User("work remains"), nil
		},
	})
	if !errors.Is(err, ErrMaxIterations) || model.callCount != 1 {
		t.Fatalf("last-iteration continuation: %v calls=%d", err, model.callCount)
	}
	// History must not end in the unanswered continuation prompt.
	if len(response.AllMessages) != 1 || response.AllMessages[0].Role != messages.MessageRoleAssistant || response.AllMessages[0].StopReason != messages.StopReasonMaxIterations {
		t.Fatalf("history: %+v", response.AllMessages)
	}
	if response.Message == nil || response.Message.Content != "final answer" || response.Message.StopReason != messages.StopReasonMaxIterations {
		t.Fatalf("answer lost: %+v", response.Message)
	}
}
