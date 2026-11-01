package llm

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

func TestCompactionObserversKeepOnlySurvivingMessages(t *testing.T) {
	for _, summary := range []string{"accepted", "withdrawn"} {
		for _, stop := range []string{"complete", "provider failure", "checkpoint failure", "cancelled"} {
			t.Run(summary+"/"+stop, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				failure := errors.New("injected failure")
				model := &compactionLLM{reply: func(req *CompletionRequest, _ int) (messages.ChatMessage, error) {
					if isSummaryRequest(req) {
						text := "small summary"
						if summary == "withdrawn" {
							text = strings.Repeat("long ", 2_400)
						}
						return reply(text, 500), nil
					}
					if stop == "provider failure" {
						return messages.ChatMessage{}, failure
					}
					return reply("answer", 0), nil
				}}
				agent := NewAgent(model, nil, AgentConfig{})
				defer agent.Close()
				history := []messages.ChatMessage{
					{Role: messages.MessageRoleUser, Content: "old " + strings.Repeat("x", 7_300)},
					reply("old answer", 0),
					{Role: messages.MessageRoleUser, Content: "new"},
				}
				var observed, checkpointed []messages.ChatMessage
				admitted := false
				response, err := agent.Run(ctx, &CompletionRequest{Messages: history, MaxContextTokens: 2_000}, &AgentCallbacks{
					AdmitInput: func(context.Context) ([]messages.ChatMessage, error) {
						if admitted {
							return nil, nil
						}
						admitted = true
						return messages.User("peer input"), nil
					},
					OnMessage: func(index int, msg messages.ChatMessage) {
						if index != len(observed) || msg.DisplayID() == "" {
							t.Fatalf("unstable generated index/identity: index=%d observed=%d id=%q", index, len(observed), msg.DisplayID())
						}
						observed = append(observed, msg.Clone())
						msg.Metadata[messages.MetadataKeyDisplayID] = "observer mutation"
					},
					Checkpoint: func(_ context.Context, cp AgentCheckpoint) error {
						if cp.Request && stop == "checkpoint failure" {
							return failure
						}
						checkpointed = cloneMessages(cp.Generated)
						return nil
					},
					OnAdaptation: func(note RequestAdaptation) {
						if stop == "cancelled" && summary == "withdrawn" && note.Feature == FeatureCompactionFailure {
							cancel() // after withdrawal, before a replacement answer
						}
					},
					OnRequestProjection: func(int, ProjectionStats) {
						if stop == "cancelled" {
							cancel()
						}
					},
				})
				var wantErr error
				switch stop {
				case "provider failure", "checkpoint failure":
					wantErr = failure
				case "cancelled":
					wantErr = context.Canceled
				}
				if !errors.Is(err, wantErr) {
					t.Fatalf("error=%v want=%v", err, wantErr)
				}
				if !reflect.DeepEqual(observed, response.AllMessages) || !reflect.DeepEqual(checkpointed, response.AllMessages) {
					t.Fatalf("observer/checkpoint retained a phantom or missed surviving output: observed=%+v checkpoint=%+v generated=%+v", observed, checkpointed, response.AllMessages)
				}
				markers, inputs := 0, 0
				for _, msg := range observed {
					if _, ok := msg.Compaction(); ok {
						markers++
					}
					if msg.Content == "peer input" {
						inputs++
					}
				}
				wantMarkers, wantInputs, wantCalls := 0, 1, 1
				if summary == "accepted" {
					wantMarkers = 1
				}
				if stop == "checkpoint failure" {
					wantInputs = 0
				}
				if stop == "complete" || stop == "provider failure" {
					wantCalls = 2
				}
				if markers != wantMarkers || inputs != wantInputs || len(model.requests) != wantCalls || response.TokenUsage().TotalInput != 500 {
					t.Fatalf("markers=%d inputs=%d calls=%d usage=%+v", markers, inputs, len(model.requests), response.TokenUsage())
				}
			})
		}
	}
}
