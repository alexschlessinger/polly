package llm

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

func TestTranscriptCacheAppendAndReplace(t *testing.T) {
	a := &Agent{}
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "system"},
		{Role: messages.MessageRoleTool, ToolCallID: "call", ToolName: "run", Content: "large tool result"},
	}
	a.setTranscript(history)
	first := a.renderedTranscript()
	a.appendTranscript(
		messages.ChatMessage{Role: messages.MessageRoleInternal, Content: "private"},
		messages.ChatMessage{Role: messages.MessageRoleTool, ToolName: "read_transcript", Content: first},
		messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "answer"},
	)
	if got, want := a.renderedTranscript(), renderTranscript(a.transcript, a.projectionTools().recall); got != want {
		t.Fatalf("incremental rendering differs: %q != %q", got, want)
	}
	if first != renderTranscript(history, a.projectionTools().recall) || len(history) != 2 {
		t.Fatal("appending changed caller-owned history or rendered text")
	}
	replacement := make([]messages.ChatMessage, len(a.transcript))
	replacement[0] = messages.ChatMessage{Role: messages.MessageRoleUser, Content: "new run, same length"}
	a.setTranscript(replacement)
	if got, want := a.renderedTranscript(), renderTranscript(replacement, a.projectionTools().recall); got != want || strings.Contains(got, "answer") {
		t.Fatalf("same-length run replacement reused stale text: %q", got)
	}
}

func TestTranscriptCacheConcurrentReaders(t *testing.T) {
	a := &Agent{}
	a.setTranscript(messages.User("question"))
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for range 100 {
				text := a.renderedTranscript()
				if !strings.HasPrefix(text, "=== message 1: user ===\nquestion\n") {
					t.Error("reader saw an incomplete prefix")
				}
			}
		})
	}
	for i := range 100 {
		callID := fmt.Sprintf("call-%d", i)
		a.appendTranscript(messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: callID, ToolName: "run", Content: "large result"})
	}
	readers.Wait()
}

func BenchmarkTranscriptRenderCache(b *testing.B) {
	history := make([]messages.ChatMessage, 1000)
	for i := range history {
		history[i] = messages.ChatMessage{Role: messages.MessageRoleUser, Content: strings.Repeat("transcript content ", 100)}
	}
	for _, cached := range []bool{false, true} {
		b.Run(fmt.Sprintf("cached=%t", cached), func(b *testing.B) {
			a := &Agent{}
			a.setTranscript(history)
			a.renderedTranscript()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if cached {
					a.renderedTranscript()
				} else {
					renderTranscript(history, a.projectionTools().recall)
				}
			}
		})
	}
}
