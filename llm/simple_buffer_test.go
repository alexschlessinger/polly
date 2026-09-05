package llm

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

type fragmentedCompletionLLM struct{ content string }

func (f fragmentedCompletionLLM) ChatCompletionStream(_ context.Context, _ *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	ch := make(chan messages.ChatMessage, 10)
	go func() {
		defer close(ch)
		for start := 0; start < len(f.content); start += 20 {
			end := start + 20
			if end > len(f.content) {
				end = len(f.content)
			}
			ch <- messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: f.content[start:end]}
		}
		ch <- messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn}
	}()
	return processor.ProcessMessagesToEvents(ch)
}

func TestSimpleCompletionFragmentedOutput(t *testing.T) {
	want := strings.Repeat("αbeta🙂", 2000)
	client := fragmentedCompletionLLM{content: want}
	got, err := NewCompletionBuilder("test").Execute(context.Background(), client)
	if err != nil || got != want {
		t.Fatalf("fragmented completion mismatch: length=%d err=%v", len(got), err)
	}
	var complete *messages.ChatMessage
	for event := range client.ChatCompletionStream(context.Background(), nil, &SimpleProcessor{}) {
		if event.Type == messages.EventTypeComplete {
			complete = event.Message
		}
	}
	if complete == nil || complete.Content != want || complete.StopReason != messages.StopReasonEndTurn {
		t.Fatal("SimpleProcessor lost fragmented text or terminal metadata")
	}
}

func BenchmarkCompletionBuilderFragmentedOutput(b *testing.B) {
	for _, size := range []int{100000, 1000000} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			client := fragmentedCompletionLLM{content: strings.Repeat("x", size)}
			builder := NewCompletionBuilder("test")
			b.ReportAllocs()
			b.SetBytes(int64(size))
			for b.Loop() {
				got, err := builder.Execute(context.Background(), client)
				if err != nil || len(got) != size {
					b.Fatalf("incorrect completion: length=%d err=%v", len(got), err)
				}
			}
		})
	}
}
