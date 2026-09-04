package adapters

import (
	"strings"
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

// TestGeminiAdapterBlockedPromptIsAnError: a promptFeedback block reason
// arrives with no candidates; it must surface as an error and a content
// filter stop, never as a blank successful reply.
func TestGeminiAdapterBlockedPromptIsAnError(t *testing.T) {
	adapter := NewGeminiAdapter()
	state := streaming.NewStreamState()
	err := adapter.ProcessChunk(&gemini.GenerateContentResponse{
		PromptFeedback: &gemini.PromptFeedback{BlockReason: gemini.BlockReasonSafety},
	}, state)
	if err == nil || !strings.Contains(err.Error(), "SAFETY") {
		t.Fatalf("ProcessChunk error = %v, want the block reason", err)
	}
	if got := state.GetStopReason(); got != messages.StopReasonContentFilter {
		t.Fatalf("stop reason = %q, want content_filter", got)
	}
	// The unspecified value is the enum's unused default, not a block.
	if err := adapter.ProcessChunk(&gemini.GenerateContentResponse{
		PromptFeedback: &gemini.PromptFeedback{BlockReason: gemini.BlockReasonUnspecified},
		Candidates:     []*gemini.Candidate{{FinishReason: gemini.FinishReasonStop}},
	}, streaming.NewStreamState()); err != nil {
		t.Fatalf("unspecified block reason errored: %v", err)
	}
}

// TestGeminiAdapterCountsThinkingTokens: thoughtsTokenCount is billed output
// reported beside candidatesTokenCount, so usage sums both.
func TestGeminiAdapterCountsThinkingTokens(t *testing.T) {
	adapter := NewGeminiAdapter()
	state := streaming.NewStreamState()
	if err := adapter.ProcessChunk(&gemini.GenerateContentResponse{
		Candidates:    []*gemini.Candidate{{FinishReason: gemini.FinishReasonStop}},
		UsageMetadata: &gemini.UsageMetadata{PromptTokenCount: 10, CandidatesTokenCount: 5, ThoughtsTokenCount: 40},
	}, state); err != nil {
		t.Fatal(err)
	}
	if state.GetInputTokens() != 10 || state.GetOutputTokens() != 45 {
		t.Fatalf("usage = %d in / %d out, want 10 / 45", state.GetInputTokens(), state.GetOutputTokens())
	}
}
