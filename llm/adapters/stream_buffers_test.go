package adapters

import (
	"strconv"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/anthropic"
	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestOpenAIArgumentBuffersKeepInterleavedSnapshots(t *testing.T) {
	adapter := NewOpenAIAdapter()
	state := streaming.NewStreamState()
	appendDelta := func(index int, delta string) {
		t.Helper()
		if err := adapter.ProcessChunk(&openai.ChatCompletionChunk{Choices: []openai.ChatChunkChoice{{
			Delta: openai.ChatDelta{ToolCalls: []openai.ChatToolCallDelta{{
				Index: int64(index), Function: openai.ChatToolCallFunc{Arguments: delta},
			}}},
		}}}, state); err != nil {
			t.Fatal(err)
		}
	}
	appendDelta(3, `{"a":"`)
	first := state.GetToolCalls()[3].Arguments
	appendDelta(1, `{"b":`)
	appendDelta(3, strings.Repeat("a", 10000))
	appendDelta(1, "2}")
	appendDelta(3, `"}`)
	calls := state.GetToolCalls()
	if first != `{"a":"` || calls[1].Arguments != `{"b":2}` || calls[3].Arguments != `{"a":"`+strings.Repeat("a", 10000)+`"}` {
		t.Fatalf("interleaved argument snapshots were corrupted")
	}
}

func TestResponsesArgumentBuffersHonorFinalReplacements(t *testing.T) {
	adapter := NewOpenAIResponsesAdapter("test")
	state := streaming.NewStreamState()
	process := func(event *openai.ResponseStreamEvent) {
		t.Helper()
		if err := adapter.ProcessChunk(event, state); err != nil {
			t.Fatal(err)
		}
	}
	process(&openai.ResponseStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: 4, Delta: `{"x":`})
	partial := state.GetToolCalls()[0].Arguments
	process(&openai.ResponseStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: 4, Delta: "1}"})
	process(&openai.ResponseStreamEvent{Type: "response.function_call_arguments.done", OutputIndex: 4, Arguments: `{"x":2}`})
	if got := state.GetToolCalls()[0].Arguments; got != `{"x":2}` {
		t.Fatalf("done arguments = %q", got)
	}
	process(&openai.ResponseStreamEvent{Type: "response.output_item.done", OutputIndex: 4, Item: &openai.ResponseOutputItem{
		Type: "function_call", Arguments: `{"x":3}`,
	}})
	if got := state.GetToolCalls()[0].Arguments; got != `{"x":3}` || partial != `{"x":` {
		t.Fatalf("final arguments = %q; old snapshot = %q", got, partial)
	}
}

func TestAnthropicThinkingBuffersPreserveSignedBlocks(t *testing.T) {
	adapter := NewAnthropicAdapter()
	state := streaming.NewStreamState()
	process := func(event *anthropic.StreamEvent) {
		t.Helper()
		if err := adapter.ProcessChunk(event, state); err != nil {
			t.Fatal(err)
		}
	}
	for i, thinking := range []string{"first thought", "second thought"} {
		process(&anthropic.StreamEvent{Type: anthropic.EventContentBlockStart, ContentBlock: &anthropic.ContentBlock{Type: "thinking"}})
		for _, delta := range strings.SplitAfter(thinking, " ") {
			process(&anthropic.StreamEvent{Type: anthropic.EventContentBlockDelta, Delta: &anthropic.StreamDelta{Thinking: delta}})
		}
		process(&anthropic.StreamEvent{Type: anthropic.EventContentBlockDelta, Delta: &anthropic.StreamDelta{Signature: "signature" + strconv.Itoa(i)}})
		process(&anthropic.StreamEvent{Type: anthropic.EventContentBlockStop})
		if i == 0 {
			process(&anthropic.StreamEvent{Type: anthropic.EventContentBlockStart, ContentBlock: &anthropic.ContentBlock{Type: "redacted_thinking", Data: "opaque"}})
			process(&anthropic.StreamEvent{Type: anthropic.EventContentBlockStop})
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

func TestAnthropicArgumentBuffersResetBetweenBlocks(t *testing.T) {
	adapter := NewAnthropicAdapter()
	state := streaming.NewStreamState()
	for i, delta := range []string{`{"first":`, `{"second":`} {
		adapter.ProcessChunk(&anthropic.StreamEvent{Type: anthropic.EventContentBlockStart, ContentBlock: &anthropic.ContentBlock{Type: "tool_use", ID: strconv.Itoa(i)}}, state)
		adapter.ProcessChunk(&anthropic.StreamEvent{Type: anthropic.EventContentBlockDelta, Delta: &anthropic.StreamDelta{PartialJSON: delta}}, state)
		snapshot := state.GetToolCalls()[i].Arguments
		adapter.ProcessChunk(&anthropic.StreamEvent{Type: anthropic.EventContentBlockDelta, Delta: &anthropic.StreamDelta{PartialJSON: "true}"}}, state)
		adapter.ProcessChunk(&anthropic.StreamEvent{Type: anthropic.EventContentBlockStop}, state)
		if snapshot != delta {
			t.Fatalf("argument snapshot changed: %q", snapshot)
		}
	}
	calls := state.GetToolCalls()
	if len(calls) != 2 || calls[0].Arguments != `{"first":true}` || calls[1].Arguments != `{"second":true}` {
		t.Fatalf("tool blocks mixed arguments: %#v", calls)
	}
}

func BenchmarkStreamedToolArguments(b *testing.B) {
	const chunk = "12345678901234567890"
	for _, size := range []int{100000, 1000000} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(size))
			for b.Loop() {
				adapter := NewOpenAIAdapter()
				state := streaming.NewStreamState()
				for range size / len(chunk) {
					adapter.handleIndexedToolCall(0, openai.ChatToolCallDelta{Function: openai.ChatToolCallFunc{Arguments: chunk}}, state)
				}
				if len(state.GetToolCalls()[0].Arguments) != size {
					b.Fatal("incorrect arguments length")
				}
			}
		})
	}
}
