package adapters

import (
	"testing"

	"github.com/alexschlessinger/pollytool/llm/anthropic"
	"github.com/alexschlessinger/pollytool/llm/gemini"
	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/llm/streaming"
)

func TestStreamingAdaptersParsePromptCacheUsage(t *testing.T) {
	int64Ptr := func(value int64) *int64 { return &value }
	t.Run("OpenAI chat", func(t *testing.T) {
		state := streaming.NewStreamState()
		adapter := NewOpenAIAdapter()
		err := adapter.ProcessChunk(&openai.ChatCompletionChunk{Usage: &openai.ChatUsage{
			PromptTokens: 20, CompletionTokens: 3,
			PromptTokensDetails: &openai.PromptTokenDetails{CachedTokens: int64Ptr(12), CacheWriteTokens: int64Ptr(4)},
		}}, state)
		if err != nil {
			t.Fatal(err)
		}
		assertStreamingCacheUsage(t, state, 20, 3, 12, 4)
	})

	t.Run("OpenAI Responses", func(t *testing.T) {
		state := streaming.NewStreamState()
		adapter := NewOpenAIResponsesAdapter()
		err := adapter.ProcessChunk(&openai.ResponseStreamEvent{
			Type: "response.completed",
			Response: &openai.Response{Status: openai.ResponseStatusCompleted, Usage: &openai.ResponseUsage{
				InputTokens: 30, OutputTokens: 5,
				InputTokensDetails: &openai.PromptTokenDetails{CachedTokens: int64Ptr(18), CacheWriteTokens: int64Ptr(6)},
			}},
		}, state)
		if err != nil {
			t.Fatal(err)
		}
		assertStreamingCacheUsage(t, state, 30, 5, 18, 6)
	})

	t.Run("Anthropic normalized input", func(t *testing.T) {
		state := streaming.NewStreamState()
		adapter := NewAnthropicAdapter()
		creation, read := int64(7), int64(11)
		err := adapter.ProcessChunk(&anthropic.StreamEvent{
			Type: anthropic.EventMessageStart,
			Message: &anthropic.Message{Usage: &anthropic.Usage{
				InputTokens: 5, CacheCreationInputTokens: &creation, CacheReadInputTokens: &read,
			}},
		}, state)
		if err != nil {
			t.Fatal(err)
		}
		err = adapter.ProcessChunk(&anthropic.StreamEvent{
			Type:  anthropic.EventMessageDelta,
			Usage: &anthropic.Usage{OutputTokens: 2},
		}, state)
		if err != nil {
			t.Fatal(err)
		}
		assertStreamingCacheUsage(t, state, 23, 2, 11, 7)
	})

	t.Run("Gemini", func(t *testing.T) {
		state := streaming.NewStreamState()
		adapter := NewGeminiAdapter()
		cached := int32(9)
		err := adapter.ProcessChunk(&gemini.GenerateContentResponse{UsageMetadata: &gemini.UsageMetadata{
			PromptTokenCount: 17, CandidatesTokenCount: 2, CachedContentTokenCount: &cached,
		}}, state)
		if err != nil {
			t.Fatal(err)
		}
		assertStreamingCacheUsage(t, state, 17, 2, 9, 0)
	})

	t.Run("omitted details are not inferred", func(t *testing.T) {
		state := streaming.NewStreamState()
		adapter := NewOpenAIAdapter()
		if err := adapter.ProcessChunk(&openai.ChatCompletionChunk{Usage: &openai.ChatUsage{
			PromptTokens: 20, PromptTokensDetails: &openai.PromptTokenDetails{},
		}}, state); err != nil {
			t.Fatal(err)
		}
		if state.HasPromptCacheUsage() {
			t.Fatal("adapter inferred cache usage from ordinary prompt tokens")
		}
	})
}

func assertStreamingCacheUsage(t *testing.T, state *streaming.StreamState, input, output, read, write int) {
	t.Helper()
	if state.GetInputTokens() != input || state.GetOutputTokens() != output {
		t.Fatalf("token usage = %d/%d, want %d/%d", state.GetInputTokens(), state.GetOutputTokens(), input, output)
	}
	if !state.HasPromptCacheUsage() {
		t.Fatal("cache usage was not marked as provider-reported")
	}
	if state.GetCacheReadInputTokens() != read || state.GetCacheWriteInputTokens() != write {
		t.Fatalf("cache usage = %d/%d, want %d/%d", state.GetCacheReadInputTokens(), state.GetCacheWriteInputTokens(), read, write)
	}
}
