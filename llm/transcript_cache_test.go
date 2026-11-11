package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

// Full rendering is useful to existing compaction assertions, but is never the
// agent's recall implementation. Ordinary recall goes through the bounded pager.
func (a *Agent) renderedTranscript() string {
	r := a.openTranscript(context.Background())
	defer r.Close()
	text, err := io.ReadAll(r)
	if err != nil {
		panic(err)
	}
	return string(text)
}

func testReadTranscriptTool(history []messages.ChatMessage) *readTranscriptTool {
	return &readTranscriptTool{open: func(ctx context.Context) *transcriptReader {
		return newTranscriptReader(ctx, history, builtinProjectionTools(false).recall)
	}}
}

func TestTranscriptReaderAppendAndReplace(t *testing.T) {
	a := NewAgent(nil, nil, AgentConfig{})
	defer a.Close()
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
		t.Fatalf("streaming rendering differs: %q != %q", got, want)
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

func TestTranscriptReaderConcurrentReaders(t *testing.T) {
	a := &Agent{}
	a.setTranscript(messages.User("question"))
	tool := &readTranscriptTool{open: a.openTranscript}
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for range 100 {
				text, err := tool.Execute(context.Background(), map[string]any{"limit": 2})
				if err != nil || !strings.HasPrefix(text, "1: === message 1: user ===\n2: question\n") {
					t.Errorf("reader saw an incomplete prefix: %q, %v", text, err)
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

func TestTranscriptReaderStableViewAndCancellation(t *testing.T) {
	a := &Agent{}
	a.setTranscript(messages.User("old"))
	r := a.openTranscript(context.Background())
	// A view borrows history; replacements cannot mutate it while it is read.
	if a.transcriptMu.TryLock() {
		a.transcriptMu.Unlock()
		t.Fatal("reader did not hold a stable view")
	}
	other := a.openTranscript(context.Background())
	other.Close() // concurrent readers do not need a full snapshot each
	r.Close()
	a.setTranscript(messages.User("new"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tool := &readTranscriptTool{open: a.openTranscript}
	for _, args := range []map[string]any{{"query": "missing"}, {"byte_offset": 0}} {
		if _, err := tool.Execute(ctx, args); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled recall: %v", err)
		}
		if !a.transcriptMu.TryLock() {
			t.Fatal("canceled recall leaked its view")
		}
		a.transcriptMu.Unlock()
	}
}
