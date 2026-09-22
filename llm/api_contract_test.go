package llm

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestApprovalFailureAbortsEntireBatch(t *testing.T) {
	denied := errors.New("approval unavailable")
	for _, tc := range []struct {
		name      string
		decisions []bool
		err       error
		cancel    bool
		want      error
	}{
		{name: "nil", want: ErrInvalidToolApproval},
		{name: "short", decisions: []bool{true}, want: ErrInvalidToolApproval},
		{name: "long", decisions: []bool{true, true, true}, want: ErrInvalidToolApproval},
		{name: "error", err: denied, want: denied},
		{name: "canceled", decisions: []bool{true, true}, cancel: true, want: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invoked := 0
			registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "effect", Run: func(context.Context, tools.Args) (string, error) { invoked++; return "ok", nil }}})
			defer registry.Close()
			calls := []messages.ChatMessageToolCall{{ID: "1", Name: "effect", Arguments: `{}`}, {ID: "2", Name: "effect", Arguments: `{}`}}
			model := &sequentialLLM{responses: []messages.ChatMessage{{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: calls}}}
			agent := NewAgent(model, registry, AgentConfig{MaxIterations: 1, MaxParallelTools: 1})
			defer agent.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			response, err := agent.Run(ctx, &CompletionRequest{}, &AgentCallbacks{ApproveToolCalls: func(got context.Context, batch []messages.ChatMessageToolCall) ([]bool, error) {
				if got != ctx || !reflect.DeepEqual(batch, calls) {
					t.Fatal("approval received wrong context or calls")
				}
				if tc.cancel {
					cancel()
				}
				return tc.decisions, tc.err
			}})
			if !errors.Is(err, tc.want) || invoked != 0 {
				t.Fatalf("error=%v invoked=%d", err, invoked)
			}
			if response == nil || len(response.AllMessages) != 3 {
				t.Fatalf("partial batch lost its replayable outcomes: %+v", response)
			}
		})
	}
}

func TestTokenUsageSeparatesTotalsAndPeak(t *testing.T) {
	first := messages.ChatMessage{Role: messages.MessageRoleAssistant}
	first.SetTokenUsage(100, 10)
	second := messages.ChatMessage{Role: messages.MessageRoleAssistant}
	second.SetTokenUsage(150, 20)
	tool := messages.ChatMessage{Role: messages.MessageRoleTool}
	tool.SetTokenUsage(1000, 1000)
	got := (&AgentResponse{AllMessages: []messages.ChatMessage{first, tool, second}}).TokenUsage()
	if got != (TokenUsage{TotalInput: 250, TotalOutput: 30, PeakInput: 150}) {
		t.Fatalf("usage=%+v", got)
	}
}

func TestCompletePreparesAndPreservesMessage(t *testing.T) {
	final := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "answer", Reasoning: "thought", StopReason: messages.StopReasonMaxTokens, ToolCalls: []messages.ChatMessageToolCall{{ID: "1", Name: "echo", Arguments: `{}`}}}
	final.Parts = []messages.ContentPart{{Type: "text", Text: "part"}}
	final.SetTokenUsage(100, 20)
	final.SetPromptCacheUsage(50, 10)
	model := &completionRecordingLLM{}
	model.responses = []messages.ChatMessage{final}
	req := &CompletionRequest{Model: "custom/m", Messages: messages.User("hi"), Temperature: Float32Ptr(.5)}
	got, err := Complete(context.Background(), model, req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, &final) {
		t.Fatalf("completion=%+v want=%+v", got, final)
	}
	if model.requests[0].Temperature != nil || req.Temperature == nil {
		t.Fatal("preparation omitted or caller request changed")
	}
}

func TestCollectPreparesRequest(t *testing.T) {
	model := &completionRecordingLLM{}
	model.responses = []messages.ChatMessage{{Role: messages.MessageRoleAssistant, Content: "answer", StopReason: messages.StopReasonEndTurn}}
	text, err := Collect(context.Background(), model, &CompletionRequest{Model: "custom/m", Temperature: Float32Ptr(.5)})
	if err != nil || text != "answer" || model.requests[0].Temperature != nil {
		t.Fatalf("text=%q err=%v", text, err)
	}
}

func TestCompletePreservesPartialTextOnError(t *testing.T) {
	failure := errors.New("failed midstream")
	model := &flakyStreamLLM{failures: 1, err: failure, emitFirst: true}
	got, err := Complete(context.Background(), model, &CompletionRequest{})
	if !errors.Is(err, failure) || got == nil || got.Content != "partial" || got.StopReason != "" {
		t.Fatalf("message=%+v error=%v", got, err)
	}
}

// This client discovers an explicit unsupported parameter and records the wire input.
type completionRecordingLLM struct{ promptCacheRecordingLLM }

func (*completionRecordingLLM) GetModelInfo(context.Context, ModelTarget) (*ModelInfo, error) {
	return &ModelInfo{ModelCapabilities: ModelCapabilities{Parameters: map[string]bool{"temperature": false}, ParametersComplete: true}}, nil
}
