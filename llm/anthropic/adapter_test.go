package anthropic

import (
	"strconv"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestMapStopReason(t *testing.T) {
	tests := []struct {
		input StopReason
		want  messages.StopReason
	}{
		{"end_turn", messages.StopReasonEndTurn},
		{"tool_use", messages.StopReasonToolUse},
		{"max_tokens", messages.StopReasonMaxTokens},
		{"refusal", messages.StopReasonContentFilter},
		{"stop_sequence", messages.StopReasonEndTurn},
		{"unknown_value", messages.StopReasonEndTurn},
	}

	for _, tt := range tests {
		t.Run(string(tt.input), func(t *testing.T) {
			got := MapStopReason(tt.input)
			if got != tt.want {
				t.Errorf("MapStopReason(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestThinkingBuffersPreserveSignedBlocks(t *testing.T) {
	adapter := NewAdapter()
	state := streaming.NewStreamState()
	process := func(event *StreamEvent) {
		t.Helper()
		if err := adapter.ProcessChunk(event, state); err != nil {
			t.Fatal(err)
		}
	}
	for i, thinking := range []string{"first thought", "second thought"} {
		process(&StreamEvent{Type: EventContentBlockStart, ContentBlock: &ContentBlock{Type: "thinking"}})
		for _, delta := range strings.SplitAfter(thinking, " ") {
			process(&StreamEvent{Type: EventContentBlockDelta, Delta: &StreamDelta{Thinking: delta}})
		}
		process(&StreamEvent{Type: EventContentBlockDelta, Delta: &StreamDelta{Signature: "signature" + strconv.Itoa(i)}})
		process(&StreamEvent{Type: EventContentBlockStop})
		if i == 0 {
			process(&StreamEvent{Type: EventContentBlockStart, ContentBlock: &ContentBlock{Type: "redacted_thinking", Data: "opaque"}})
			process(&StreamEvent{Type: EventContentBlockStop})
		}
	}
	msg := &messages.ChatMessage{}
	adapter.EnrichFinalMessage(msg, state)
	blocks := msg.Metadata["anthropic_thinking_blocks"].([]map[string]any)
	if len(blocks) != 3 || blocks[0]["thinking"] != "first thought" || blocks[0]["signature"] != "signature0" ||
		blocks[1]["data"] != "opaque" || blocks[2]["thinking"] != "second thought" || blocks[2]["signature"] != "signature1" {
		t.Fatalf("signed thinking blocks changed: %#v", blocks)
	}
}

func TestArgumentBuffersResetBetweenBlocks(t *testing.T) {
	adapter := NewAdapter()
	state := streaming.NewStreamState()
	for i, delta := range []string{`{"first":`, `{"second":`} {
		adapter.ProcessChunk(&StreamEvent{Type: EventContentBlockStart, ContentBlock: &ContentBlock{Type: "tool_use", ID: strconv.Itoa(i)}}, state)
		adapter.ProcessChunk(&StreamEvent{Type: EventContentBlockDelta, Delta: &StreamDelta{PartialJSON: delta}}, state)
		snapshot := state.GetToolCalls()[i].Arguments
		adapter.ProcessChunk(&StreamEvent{Type: EventContentBlockDelta, Delta: &StreamDelta{PartialJSON: "true}"}}, state)
		adapter.ProcessChunk(&StreamEvent{Type: EventContentBlockStop}, state)
		if snapshot != delta {
			t.Fatalf("argument snapshot changed: %q", snapshot)
		}
	}
	calls := state.GetToolCalls()
	if len(calls) != 2 || calls[0].Arguments != `{"first":true}` || calls[1].Arguments != `{"second":true}` {
		t.Fatalf("tool blocks mixed arguments: %#v", calls)
	}
}

// TestAdapterParsesPromptCacheUsage: cache creation and read tokens are
// normalized into the input count, and reported as provider-supplied.
func TestAdapterParsesPromptCacheUsage(t *testing.T) {
	state := streaming.NewStreamState()
	adapter := NewAdapter()
	creation, read := int64(7), int64(11)
	err := adapter.ProcessChunk(&StreamEvent{
		Type: EventMessageStart,
		Message: &Message{Usage: &Usage{
			InputTokens: 5, CacheCreationInputTokens: &creation, CacheReadInputTokens: &read,
		}},
	}, state)
	if err != nil {
		t.Fatal(err)
	}
	err = adapter.ProcessChunk(&StreamEvent{
		Type:  EventMessageDelta,
		Usage: &Usage{OutputTokens: 2},
	}, state)
	if err != nil {
		t.Fatal(err)
	}
	if state.GetInputTokens() != 23 || state.GetOutputTokens() != 2 {
		t.Fatalf("token usage = %d/%d, want 23/2", state.GetInputTokens(), state.GetOutputTokens())
	}
	if !state.HasPromptCacheUsage() || state.GetCacheReadInputTokens() != 11 || state.GetCacheWriteInputTokens() != 7 {
		t.Fatalf("cache usage = %d/%d (reported=%t), want 11/7", state.GetCacheReadInputTokens(), state.GetCacheWriteInputTokens(), state.HasPromptCacheUsage())
	}
}
