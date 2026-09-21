package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestAgentCallerOwnsPersistenceCallbacks(t *testing.T) {
	for _, rejectJournal := range []bool{false, true} {
		name := "checkpointed tool result"
		if rejectJournal {
			name = "journal veto"
		}
		t.Run(name, func(t *testing.T) {
			journalErr := errors.New("intent not durable")
			var admitted, journaled, executed bool
			registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "work", Run: func(context.Context, tools.Args) (string, error) {
				if !journaled || rejectJournal {
					t.Error("tool started without durable intent")
				}
				executed = true
				return "result", nil
			}}})
			defer registry.Close()
			model := &sequentialLLM{responses: []messages.ChatMessage{{
				Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
				ToolCalls: []messages.ChatMessageToolCall{{ID: "work", Name: "work", Arguments: `{}`}},
			}}}
			agent := NewAgent(model, registry, AgentConfig{MaxIterations: 3})
			defer agent.Close()
			var final AgentCheckpoint
			finals := 0
			response, err := agent.Run(context.Background(), &CompletionRequest{}, &AgentCallbacks{
				AdmitInput: func(context.Context) ([]messages.ChatMessage, error) {
					if admitted {
						return nil, nil
					}
					admitted = true
					return messages.User("caller input"), nil
				},
				Checkpoint: func(_ context.Context, checkpoint AgentCheckpoint) error {
					if checkpoint.Final {
						final = checkpoint
						finals++
					}
					return nil
				},
				JournalToolBatch: func(_ context.Context, checkpoint AgentCheckpoint) error {
					if executed || len(checkpoint.Generated) != 2 || checkpoint.Generated[0].Content != "caller input" || len(checkpoint.Generated[1].ToolCalls) != 1 {
						t.Errorf("journal did not precede tool execution with admitted input: %+v", checkpoint)
					}
					journaled = true
					if rejectJournal {
						return journalErr
					}
					return nil
				},
			})
			var wantErr error
			wantCalls := 2
			if rejectJournal {
				wantErr, wantCalls = journalErr, 1
			}
			if !errors.Is(err, wantErr) || !errors.Is(final.Err, wantErr) || model.callCount != wantCalls || !admitted || !journaled || executed == rejectJournal || finals != 1 {
				t.Fatalf("err=%v final=%v calls=%d admitted=%v journaled=%v executed=%v finals=%d", err, final.Err, model.callCount, admitted, journaled, executed, finals)
			}
			if response == nil || response.PersistedMessages != len(response.AllMessages) || len(final.Generated) != len(response.AllMessages) {
				t.Fatalf("caller checkpoint did not own generated prefix: %+v", response)
			}
		})
	}
}
