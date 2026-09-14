package gemini

import (
	"testing"

	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestMapFinishReason(t *testing.T) {
	tests := []struct {
		input FinishReason
		want  messages.StopReason
	}{
		{FinishReasonStop, messages.StopReasonEndTurn},
		{FinishReasonMaxTokens, messages.StopReasonMaxTokens},
		{FinishReasonSafety, messages.StopReasonContentFilter},
		{FinishReasonRecitation, messages.StopReasonContentFilter},
		{FinishReasonBlocklist, messages.StopReasonContentFilter},
		{FinishReasonProhibitedContent, messages.StopReasonContentFilter},
		{FinishReasonSPII, messages.StopReasonContentFilter},
		{FinishReasonImageSafety, messages.StopReasonContentFilter},
		{FinishReasonImageProhibitedContent, messages.StopReasonContentFilter},
		{FinishReasonMalformedFunctionCall, messages.StopReasonError},
		{FinishReasonUnexpectedToolCall, messages.StopReasonError},
		{FinishReasonTooManyToolCalls, messages.StopReasonError},
		{FinishReasonLanguage, messages.StopReasonError},
		{FinishReasonOther, messages.StopReasonError},
		{FinishReasonImageRecitation, messages.StopReasonContentFilter},
		{FinishReasonNoImage, messages.StopReasonError},
		{FinishReasonUnspecified, messages.StopReasonError},
		// A reason this build does not know is never a healthy end of turn.
		{"MISSING_THOUGHT_SIGNATURE", messages.StopReasonError},
	}

	for _, tt := range tests {
		t.Run(string(tt.input), func(t *testing.T) {
			got := mapFinishReason(tt.input)
			if got != tt.want {
				t.Errorf("mapFinishReason(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestAdapterParsesPromptCacheUsage: cached content tokens are reported as
// provider-supplied cache reads; Gemini has no cache-write count.
func TestAdapterParsesPromptCacheUsage(t *testing.T) {
	state := streaming.NewStreamState()
	adapter := NewAdapter()
	cached := int32(9)
	err := adapter.ProcessChunk(&GenerateContentResponse{UsageMetadata: &UsageMetadata{
		PromptTokenCount: 17, CandidatesTokenCount: 2, CachedContentTokenCount: &cached,
	}}, state)
	if err != nil {
		t.Fatal(err)
	}
	if state.GetInputTokens() != 17 || state.GetOutputTokens() != 2 {
		t.Fatalf("token usage = %d/%d, want 17/2", state.GetInputTokens(), state.GetOutputTokens())
	}
	if !state.HasPromptCacheUsage() || state.GetCacheReadInputTokens() != 9 || state.GetCacheWriteInputTokens() != 0 {
		t.Fatalf("cache usage = %d/%d (reported=%t), want 9/0", state.GetCacheReadInputTokens(), state.GetCacheWriteInputTokens(), state.HasPromptCacheUsage())
	}
}

// TestSyntheticToolCallIDsUniqueAcrossStreams guards against the ID collision
// that let denial stripping erase unrelated exchanges: every stream gets a
// fresh adapter, and each adapter must namespace its synthetic IDs.
func TestSyntheticToolCallIDsUniqueAcrossStreams(t *testing.T) {
	g1, g2 := NewAdapter(), NewAdapter()
	if g1.idPrefix == "" || g1.idPrefix == g2.idPrefix {
		t.Errorf("adapter prefixes not unique: %q vs %q", g1.idPrefix, g2.idPrefix)
	}
}
