package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// TestProcessEventsDrainsAbandonedStream: a canceled turn must not strand
// the goroutine feeding the event channel. The producer here emits far more
// events than the channel buffers, then closes, the way a processor does once
// its provider notices the cancellation.
func TestProcessEventsDrainsAbandonedStream(t *testing.T) {
	events := make(chan *messages.StreamEvent, 2)
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		defer close(events)
		for i := 0; i < 200; i++ {
			events <- &messages.StreamEvent{Type: messages.EventTypeContent, Content: "x"}
		}
		events <- &messages.StreamEvent{Type: messages.EventTypeComplete, Message: &messages.ChatMessage{Role: messages.MessageRoleAssistant}}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	agent := &Agent{}
	if _, _, err := agent.processEvents(ctx, events, nil); err == nil {
		t.Fatal("processEvents returned no error for a canceled context")
	}
	select {
	case <-producerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the event producer stayed blocked after the consumer left")
	}
}

func TestProcessEventsCancellationWithoutAnEvent(t *testing.T) {
	for _, closed := range []bool{false, true} {
		events := make(chan *messages.StreamEvent)
		if closed {
			close(events)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		done := make(chan error, 1)
		go func() { _, _, err := (&Agent{}).processEvents(ctx, events, nil); done <- err }()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("closed=%v error=%v", closed, err)
			}
		case <-time.After(time.Second):
			t.Error("cancellation waited for a stream event")
		}
		if !closed {
			close(events)
		}
	}
}
