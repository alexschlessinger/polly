package messages

import (
	"context"
	"testing"
	"time"
)

func TestStreamProcessorCancellationClosesBlockedInputAndOutput(t *testing.T) {
	for _, pending := range []int{0, 32} {
		t.Run(map[bool]string{true: "output", false: "input"}[pending > 0], func(t *testing.T) {
			input := make(chan ChatMessage, pending)
			for i := 0; i < pending; i++ {
				input <- ChatMessage{Role: MessageRoleAssistant, Content: "chunk"}
			}
			ctx, cancel := context.WithCancel(context.Background())
			events := NewStreamProcessor().ProcessMessagesToEvents(ctx, input)
			if pending > 0 {
				<-events
			}
			cancel()
			// Keep input open: cancellation must finish without the producer closing it.
			timeout := time.After(time.Second)
			for {
				select {
				case _, ok := <-events:
					if !ok {
						return
					}
				case <-timeout:
					t.Fatal("processor did not close after cancellation")
				}
			}
		})
	}
}
