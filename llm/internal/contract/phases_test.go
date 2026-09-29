package contract

import (
	"context"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

type interruptedCommentary struct{}

func (interruptedCommentary) ChatCompletionStream(ctx context.Context, _ *CompletionRequest, p EventStreamProcessor) <-chan *messages.StreamEvent {
	ch := make(chan messages.ChatMessage, 2)
	ch <- messages.ChatMessage{Role: messages.MessageRoleAssistant, TextBlocks: []messages.AssistantText{{ID: "a", Phase: messages.PhaseCommentary, Text: "Still checking."}}}
	failure := messages.ChatMessage{}
	failure.SetError(context.Canceled)
	ch <- failure
	close(ch)
	return p.ProcessMessagesToEvents(ctx, ch)
}

func TestCompletePreservesInterruptedCommentaryAsPartial(t *testing.T) {
	reply, err := Complete(context.Background(), interruptedCommentary{}, &CompletionRequest{})
	if (err == nil || err.Error() != context.Canceled.Error()) || reply == nil || len(reply.TextBlocks) != 1 || reply.TextBlocks[0].Text != "Still checking." || reply.Content != "" || reply.StopReason != "" {
		t.Fatalf("partial=%#v err=%v", reply, err)
	}
}
