package openai

import (
	"strconv"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/streaming"
)

func TestArgumentBuffersKeepInterleavedSnapshots(t *testing.T) {
	adapter := NewChatAdapter()
	state := streaming.NewStreamState()
	appendDelta := func(index int, delta string) {
		t.Helper()
		if err := adapter.ProcessChunk(&ChatCompletionChunk{Choices: []ChatChunkChoice{{
			Delta: ChatDelta{ToolCalls: []ChatToolCallDelta{{
				Index: int64(index), Function: ChatToolCallFunc{Arguments: delta},
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
	adapter := NewResponsesAdapter("test")
	state := streaming.NewStreamState()
	process := func(event *ResponseStreamEvent) {
		t.Helper()
		if err := adapter.ProcessChunk(event, state); err != nil {
			t.Fatal(err)
		}
	}
	process(&ResponseStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: 4, Delta: `{"x":`})
	partial := state.GetToolCalls()[0].Arguments
	process(&ResponseStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: 4, Delta: "1}"})
	process(&ResponseStreamEvent{Type: "response.function_call_arguments.done", OutputIndex: 4, Arguments: `{"x":2}`})
	if got := state.GetToolCalls()[0].Arguments; got != `{"x":2}` {
		t.Fatalf("done arguments = %q", got)
	}
	process(&ResponseStreamEvent{Type: "response.output_item.done", OutputIndex: 4, Item: &ResponseOutputItem{
		Type: "function_call", Arguments: `{"x":3}`,
	}})
	if got := state.GetToolCalls()[0].Arguments; got != `{"x":3}` || partial != `{"x":` {
		t.Fatalf("final arguments = %q; old snapshot = %q", got, partial)
	}
}

func BenchmarkStreamedToolArguments(b *testing.B) {
	const chunk = "12345678901234567890"
	for _, size := range []int{100000, 1000000} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(size))
			for b.Loop() {
				adapter := NewChatAdapter()
				state := streaming.NewStreamState()
				for range size / len(chunk) {
					adapter.handleIndexedToolCall(0, ChatToolCallDelta{Function: ChatToolCallFunc{Arguments: chunk}}, state)
				}
				if len(state.GetToolCalls()[0].Arguments) != size {
					b.Fatal("incorrect arguments length")
				}
			}
		})
	}
}
