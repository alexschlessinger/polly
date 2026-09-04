package adapters

import (
	"testing"

	"github.com/alexschlessinger/pollytool/llm/ollama"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

// TestOllamaAdapterLengthDoneReasonIsMaxTokens: done_reason "length" means
// num_predict cut the reply off; it must not read as a normal end of turn.
func TestOllamaAdapterLengthDoneReasonIsMaxTokens(t *testing.T) {
	adapter := NewOllamaAdapter()
	state := streaming.NewStreamState()
	if err := adapter.ProcessChunk(&ollama.ChatResponse{Done: true, DoneReason: ollama.DoneReasonLength}, state); err != nil {
		t.Fatal(err)
	}
	if got := state.GetStopReason(); got != messages.StopReasonMaxTokens {
		t.Fatalf("stop reason = %q, want max_tokens", got)
	}

	state = streaming.NewStreamState()
	if err := adapter.ProcessChunk(&ollama.ChatResponse{Done: true, DoneReason: "stop"}, state); err != nil {
		t.Fatal(err)
	}
	if got := state.GetStopReason(); got != messages.StopReasonEndTurn {
		t.Fatalf("stop reason = %q, want end_turn", got)
	}
}

// TestOllamaAdapterNoDoneNoStopReason: without the done chunk the stream has
// no stop reason, which is what lets CompleteStream refuse a cut-off reply.
func TestOllamaAdapterNoDoneNoStopReason(t *testing.T) {
	adapter := NewOllamaAdapter()
	state := streaming.NewStreamState()
	if err := adapter.ProcessChunk(&ollama.ChatResponse{Message: ollama.Message{Content: "partial"}}, state); err != nil {
		t.Fatal(err)
	}
	if got := state.GetStopReason(); got != "" {
		t.Fatalf("stop reason = %q before done, want none", got)
	}
}

func TestIsSyntheticOllamaCallID(t *testing.T) {
	adapter := NewOllamaAdapter()
	state := streaming.NewStreamState()
	adapter.handleToolCalls([]ollama.ToolCall{{Function: ollama.ToolCallFunction{Name: "f"}}}, state)
	synthetic := state.GetToolCalls()[0].ID
	cases := map[string]bool{
		synthetic:              true,
		"call_0123abcd_7":      true, // the earlier shape, still in saved sessions
		"call_abc123":          false,
		"call_0123abcd":        false,
		"call_xyz_1":           false,
		"toolu_01ABC":          false,
		"ollama_call_zz_1":     false,
		"ollama_call_0123abcd": false,
	}
	for id, want := range cases {
		if got := IsSyntheticOllamaCallID(id); got != want {
			t.Errorf("IsSyntheticOllamaCallID(%q) = %v, want %v", id, got, want)
		}
	}
}
