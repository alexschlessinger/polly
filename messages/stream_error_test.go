package messages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestStreamErrorRetainsLiveCauseButSerializesText(t *testing.T) {
	cause := errors.New("transient failure")
	wrapped := fmt.Errorf("provider: %w", cause)
	message := ChatMessage{Role: MessageRoleAssistant}
	message.SetError(wrapped)
	events := make(chan ChatMessage, 1)
	events <- message
	close(events)
	var observed error
	for event := range NewStreamProcessor().ProcessMessagesToEvents(context.Background(), events) {
		if event.Type == EventTypeError {
			observed = event.Error
		}
	}
	if !errors.Is(observed, cause) {
		t.Fatalf("live error identity lost: %v", observed)
	}
	data, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	var saved ChatMessage
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.IsError() || saved.GetError() == nil || saved.GetError().Error() != wrapped.Error() || errors.Is(saved.GetError(), cause) {
		t.Fatalf("serialized error contract changed: %s %+v", data, saved.GetError())
	}
	if record := message.UsageRecord(); record.GetError() != nil {
		t.Fatal("usage record retained live error")
	}
}
