package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// The Final checkpoint carries the run's outcome so persistence can tell a
// parked execution from a finished one inside the same commit.
func TestFinalCheckpointCarriesRunError(t *testing.T) {
	errParked := errors.New("parked")
	call := messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "call", Name: "work", Arguments: "{}"}}}
	answer := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "answer", StopReason: messages.StopReasonEndTurn}
	for _, tc := range []struct {
		name      string
		responses []messages.ChatMessage
		limit     int
		park      bool
		want      error
	}{
		{name: "success", responses: []messages.ChatMessage{answer}, limit: 2},
		{name: "park", responses: []messages.ChatMessage{call, answer}, limit: 4, park: true, want: errParked},
		{name: "exhausted", responses: []messages.ChatMessage{call, call}, limit: 1, want: ErrMaxIterations},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "work", Run: func(context.Context, tools.Args) (string, error) { return "result", nil }}})
			defer registry.Close()
			agent := NewAgent(&sequentialLLM{responses: tc.responses}, registry, AgentConfig{MaxIterations: tc.limit})
			defer agent.Close()
			finals := 0
			var finalErr error
			_, err := agent.Run(context.Background(), &CompletionRequest{}, &AgentCallbacks{
				AfterToolBatch: func(context.Context) error {
					if tc.park {
						return errParked
					}
					return nil
				},
				Checkpoint: func(_ context.Context, cp AgentCheckpoint) error {
					if !cp.Final {
						if cp.Err != nil {
							t.Errorf("non-final checkpoint carried %v", cp.Err)
						}
						return nil
					}
					finals++
					finalErr = cp.Err
					return nil
				},
			})
			if finals != 1 {
				t.Fatalf("final checkpoints = %d, want 1 (err=%v)", finals, err)
			}
			if tc.want == nil {
				if err != nil || finalErr != nil {
					t.Fatalf("success carried err=%v checkpoint=%v", err, finalErr)
				}
				return
			}
			if !errors.Is(err, tc.want) || !errors.Is(finalErr, tc.want) {
				t.Fatalf("run err=%v checkpoint err=%v, want %v in both", err, finalErr, tc.want)
			}
		})
	}
}
