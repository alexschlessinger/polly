package messages

import (
	"strconv"
	"strings"
	"testing"
)

func fragmentedProcessorMessages(content string) <-chan ChatMessage {
	ch := make(chan ChatMessage, 10)
	go func() {
		defer close(ch)
		for start := 0; start < len(content); start += 20 {
			end := start + 20
			if end > len(content) {
				end = len(content)
			}
			ch <- ChatMessage{Role: MessageRoleAssistant, Content: content[start:end], Reasoning: content[start:end]}
		}
		ch <- ChatMessage{Role: MessageRoleAssistant, StopReason: StopReasonEndTurn, Metadata: map[string]any{"final": true}}
	}()
	return ch
}

func TestStreamProcessorFragmentedTextAndReasoning(t *testing.T) {
	want := strings.Repeat("αbeta🙂", 2000)
	var content, reasoning strings.Builder
	var complete *ChatMessage
	for event := range NewStreamProcessor().ProcessMessagesToEvents(fragmentedProcessorMessages(want)) {
		switch event.Type {
		case EventTypeContent:
			content.WriteString(event.Content)
		case EventTypeReasoning:
			reasoning.WriteString(event.Content)
		case EventTypeComplete:
			if complete != nil {
				t.Fatal("duplicate completion event")
			}
			complete = event.Message
		default:
			t.Fatalf("unexpected event: %+v", event)
		}
	}
	if complete == nil || complete.Content != want || complete.Reasoning != want || content.String() != want || reasoning.String() != want {
		t.Fatal("fragmented text or reasoning was lost, duplicated, or corrupted")
	}
	if complete.StopReason != StopReasonEndTurn || complete.Metadata["final"] != true {
		t.Fatalf("completion lost terminal metadata: %+v", complete)
	}
}

func BenchmarkStreamProcessorFragmentedOutput(b *testing.B) {
	for _, size := range []int{100000, 1000000} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			content := strings.Repeat("x", size)
			b.ReportAllocs()
			b.SetBytes(int64(size * 2))
			for b.Loop() {
				for event := range NewStreamProcessor().ProcessMessagesToEvents(fragmentedProcessorMessages(content)) {
					if event.Type == EventTypeComplete && (len(event.Message.Content) != size || len(event.Message.Reasoning) != size) {
						b.Fatal("incorrect accumulated text length")
					}
				}
			}
		})
	}
}
