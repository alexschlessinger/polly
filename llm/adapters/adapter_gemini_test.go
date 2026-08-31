package adapters

import (
	"testing"

	"github.com/alexschlessinger/pollytool/llm/gemini"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

// TestGeminiAdapterFinishReasonWithToolCalls: tool calls promote only a
// healthy finish to tool_use. A terminal reason arriving after calls have
// accumulated — SAFETY, MAX_TOKENS, MALFORMED_FUNCTION_CALL — must survive,
// or the agent would execute the calls as if the turn were healthy.
func TestGeminiAdapterFinishReasonWithToolCalls(t *testing.T) {
	toolCallChunk := &gemini.GenerateContentResponse{
		Candidates: []*gemini.Candidate{{
			Content: &gemini.Content{Parts: []*gemini.Part{{
				FunctionCall: &gemini.FunctionCall{Name: "f", Args: map[string]any{}},
			}}},
		}},
	}
	finishChunk := func(fr gemini.FinishReason) *gemini.GenerateContentResponse {
		return &gemini.GenerateContentResponse{
			Candidates: []*gemini.Candidate{{FinishReason: fr}},
		}
	}

	tests := []struct {
		name   string
		finish gemini.FinishReason
		want   messages.StopReason
	}{
		{"stop_becomes_tool_use", gemini.FinishReasonStop, messages.StopReasonToolUse},
		{"safety_survives", gemini.FinishReasonSafety, messages.StopReasonContentFilter},
		{"max_tokens_survives", gemini.FinishReasonMaxTokens, messages.StopReasonMaxTokens},
		{"malformed_survives", gemini.FinishReasonMalformedFunctionCall, messages.StopReasonError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			adapter := NewGeminiAdapter()
			state := streaming.NewStreamState()
			if err := adapter.ProcessChunk(toolCallChunk, state); err != nil {
				t.Fatalf("tool call chunk: %v", err)
			}
			if err := adapter.ProcessChunk(finishChunk(tc.finish), state); err != nil {
				t.Fatalf("finish chunk: %v", err)
			}
			if got := state.GetStopReason(); got != tc.want {
				t.Fatalf("stop reason = %q, want %q", got, tc.want)
			}
		})
	}
}
