package llm

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestTranscriptCacheAppendReplaceAndSpill(t *testing.T) {
	a := &Agent{}
	a.setTranscript([]messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "system"},
		{Role: messages.MessageRoleTool, ToolCallID: "call", ToolName: "run", Content: "large tool result"},
	})
	first := a.renderedTranscript()
	snapshot := a.transcriptSnapshot()
	a.appendTranscript(
		messages.ChatMessage{Role: messages.MessageRoleInternal, Content: "private"},
		messages.ChatMessage{Role: messages.MessageRoleTool, ToolName: "read_transcript", Content: first},
		messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "answer"},
	)
	if got, want := a.renderedTranscript(), renderTranscript(a.transcriptSnapshot()); got != want {
		t.Fatalf("incremental rendering differs: %q != %q", got, want)
	}
	if first != renderTranscript(snapshot) || len(snapshot) != 2 {
		t.Fatal("appending changed a published transcript snapshot")
	}
	a.applyTranscriptSpills([]toolResultSpill{{ToolCallID: "call", ToolName: "run", Content: "large tool result", Receipt: "receipt", Ref: artifacts.Ref{ID: "artifact"}}})
	if got, want := a.renderedTranscript(), renderTranscript(a.transcriptSnapshot()); got != want || !strings.Contains(got, "receipt") {
		t.Fatalf("spill did not invalidate rendering: %q != %q", got, want)
	}
	if snapshot[1].Content != "large tool result" || first != renderTranscript(snapshot) {
		t.Fatal("spill changed an older snapshot or rendered string")
	}
	replacement := make([]messages.ChatMessage, len(a.transcriptSnapshot()))
	replacement[0] = messages.ChatMessage{Role: messages.MessageRoleUser, Content: "new run, same length"}
	a.setTranscript(replacement)
	if got, want := a.renderedTranscript(), renderTranscript(replacement); got != want || strings.Contains(got, "receipt") {
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
	for range 100 {
		a.appendTranscript(messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "answer"})
	}
	readers.Wait()
}

func TestTranscriptSpillPreservesSharedPartTail(t *testing.T) {
	parts := []messages.ContentPart{{Type: "text", Text: "prefix"}, {Type: "text", Text: "shared tail"}}
	a := &Agent{}
	a.setTranscript([]messages.ChatMessage{
		{Role: messages.MessageRoleTool, ToolCallID: "call", ToolName: "run", Content: "large result", Parts: parts[:1]},
		{Role: messages.MessageRoleUser, Parts: parts},
	})
	snapshot := a.transcriptSnapshot()
	a.applyTranscriptSpills([]toolResultSpill{{ToolCallID: "call", ToolName: "run", Content: "large result", Receipt: "receipt", Ref: artifacts.Ref{ID: "artifact"}}})
	if parts[1].Type != "text" || parts[1].Text != "shared tail" || snapshot[1].Parts[1].Artifact != nil {
		t.Fatal("spill overwrote another message's shared part tail")
	}
	if len(snapshot[0].Parts) != 1 || len(a.transcriptSnapshot()[0].Parts) != 2 {
		t.Fatal("spill did not preserve the old message while extending the new one")
	}
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
					renderTranscript(history)
				}
			}
		})
	}
}
