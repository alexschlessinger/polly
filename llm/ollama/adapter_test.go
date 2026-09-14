package ollama

import (
	"testing"

	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

// TestOllamaAdapterLengthDoneReasonIsMaxTokens: done_reason "length" means
// num_predict cut the reply off; it must not read as a normal end of turn.
func TestOllamaAdapterLengthDoneReasonIsMaxTokens(t *testing.T) {
	adapter := NewAdapter()
	state := streaming.NewStreamState()
	if err := adapter.ProcessChunk(&ChatResponse{Done: true, DoneReason: DoneReasonLength}, state); err != nil {
		t.Fatal(err)
	}
	if got := state.GetStopReason(); got != messages.StopReasonMaxTokens {
		t.Fatalf("stop reason = %q, want max_tokens", got)
	}

	state = streaming.NewStreamState()
	if err := adapter.ProcessChunk(&ChatResponse{Done: true, DoneReason: "stop"}, state); err != nil {
		t.Fatal(err)
	}
	if got := state.GetStopReason(); got != messages.StopReasonEndTurn {
		t.Fatalf("stop reason = %q, want end_turn", got)
	}
}

// TestOllamaAdapterNoDoneNoStopReason: without the done chunk the stream has
// no stop reason, which is what lets CompleteStream refuse a cut-off reply.
func TestOllamaAdapterNoDoneNoStopReason(t *testing.T) {
	adapter := NewAdapter()
	state := streaming.NewStreamState()
	if err := adapter.ProcessChunk(&ChatResponse{Message: Message{Content: "partial"}}, state); err != nil {
		t.Fatal(err)
	}
	if got := state.GetStopReason(); got != "" {
		t.Fatalf("stop reason = %q before done, want none", got)
	}
}

func TestIsSyntheticCallID(t *testing.T) {
	adapter := NewAdapter()
	state := streaming.NewStreamState()
	adapter.handleToolCalls([]ToolCall{{Function: ToolCallFunction{Name: "f"}}}, state)
	synthetic := state.GetToolCalls()[0].ID
	cases := map[string]bool{
		synthetic:                   true,
		"gemini_call_0123abcd_2":    true,
		"call_0123abcd_7":           true, // the earlier Ollama shape, still in saved sessions
		"gemini-0123abcd-0":         true, // the earlier Gemini shape, still in saved sessions
		"call_abc123":               false,
		"call_0123abcd":             false,
		"call_xyz_1":                false,
		"toolu_01ABC":               false,
		"ollama_call_zz_1":          false,
		"ollama_call_0123abcd":      false,
		"gemini-native-looking-id1": false,
	}
	for id, want := range cases {
		if got := streaming.IsSyntheticCallID(id); got != want {
			t.Errorf("streaming.IsSyntheticCallID(%q) = %v, want %v", id, got, want)
		}
		if native := streaming.NativeCallID(id); (native == "") != want {
			t.Errorf("streaming.NativeCallID(%q) = %q", id, native)
		}
	}
}

// TestSyntheticToolCallIDsUniqueAcrossStreams guards against the ID collision
// that let denial stripping erase unrelated exchanges: every stream gets a
// fresh adapter, and each adapter must namespace its synthetic IDs.
func TestSyntheticToolCallIDsUniqueAcrossStreams(t *testing.T) {
	o1, o2 := NewAdapter(), NewAdapter()
	if o1.idPrefix == "" || o1.idPrefix == o2.idPrefix {
		t.Errorf("adapter prefixes not unique: %q vs %q", o1.idPrefix, o2.idPrefix)
	}
}
