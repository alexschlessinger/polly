package llm

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/anthropic"
	"github.com/alexschlessinger/pollytool/llm/gemini"
	"github.com/alexschlessinger/pollytool/llm/internal/httpx"
	"github.com/alexschlessinger/pollytool/llm/openai"
)

// providerError decodes an error body as the provider's client does.
func providerError[E any](t *testing.T, status int, body string, stamp func(*E)) error {
	t.Helper()
	apiErr, ok := httpx.ParseEnvelope[E]([]byte(body))
	if !ok {
		t.Fatalf("not an error envelope: %s", body)
	}
	if stamp != nil {
		stamp(apiErr)
	}
	return streamEventError{err: fmt.Errorf("stream: %w", any(apiErr).(error))}
}

func openAIError(t *testing.T, body string) error {
	return providerError(t, http.StatusBadRequest, body, func(e *openai.APIError) { e.StatusCode = http.StatusBadRequest })
}

// TestContextOverflowReadsProviderRejections holds the detector to rejection
// bodies as providers send them.
func TestContextOverflowReadsProviderRejections(t *testing.T) {
	cases := []struct {
		name                  string
		err                   error
		window, input, output int
	}{
		{"openrouter",
			openAIError(t, `{"error":{"code":400,"message":"This endpoint's maximum context length is 204800 tokens. However, you requested about 250598 tokens (230395 of text input, 4203 of tool input, 16000 in the output). Please reduce the length of either one, or use the context-compression plugin to compress your prompt automatically.","metadata":{"error_type":"context_length_exceeded","provider_name":null}}}`),
			204800, 234598, 16000},
		{"openrouter error_type alone",
			openAIError(t, `{"error":{"code":400,"message":"Request rejected","metadata":{"error_type":"context_length_exceeded"}}}`),
			0, 0, 0},
		{"deepseek",
			openAIError(t, `{"error":{"message":"This model's maximum context length is 1048576 tokens. However, you requested 1787370 tokens (1403370 in the messages, 384000 in the completion). Please reduce the length of the messages or completion.","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`),
			1048576, 1403370, 384000},
		{"openai",
			openAIError(t, `{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","param":"input","code":"context_length_exceeded"}}`),
			0, 0, 0},
		{"openai before 2025",
			openAIError(t, `{"error":{"message":"This model's maximum context length is 128000 tokens. However, your messages resulted in 130512 tokens. Please reduce the length of the messages.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`),
			128000, 130512, 0},
		{"vllm",
			openAIError(t, `{"object":"error","error":{"message":"This model's maximum context length is 32768 tokens. However, you requested 4096 output tokens and your prompt contains 30000 input tokens, for a total of 34096 tokens. Please reduce the length of the input prompt or the number of requested output tokens.","type":"BadRequestError","code":400}}`),
			32768, 30000, 4096},
		{"llama.cpp",
			openAIError(t, `{"error":{"code":400,"message":"the request exceeds the available context size, try increasing it","type":"exceed_context_size_error","n_prompt_tokens":40000,"n_ctx":32768}}`),
			0, 0, 0},
		{"anthropic",
			providerError(t, http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 208310 tokens > 200000 maximum"}}`, func(e *anthropic.APIError) { e.StatusCode = http.StatusBadRequest }),
			200000, 208310, 0},
		{"anthropic, input and max_tokens",
			providerError(t, http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"input length and `+"`max_tokens`"+` exceed context limit: 188240 + 21333 > 200000, decrease input length or `+"`max_tokens`"+` and try again"}}`, func(e *anthropic.APIError) { e.StatusCode = http.StatusBadRequest }),
			200000, 188240, 21333},
		{"bedrock",
			errors.New("ValidationException: Input is too long for requested model."),
			0, 0, 0},
		{"gemini",
			providerError[gemini.APIError](t, http.StatusBadRequest, `{"error":{"code":400,"message":"The input token count exceeds the maximum number of tokens allowed 1048576.","status":"INVALID_ARGUMENT"}}`, nil),
			1048576, 0, 0},
		{"gemini before 2026",
			providerError[gemini.APIError](t, http.StatusBadRequest, `{"error":{"code":400,"message":"The input token count (1200000) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`, nil),
			1048576, 1200000, 0},
		{"xai",
			openAIError(t, `{"error":{"message":"This model's maximum prompt length is 131072 but the request contains 150000 tokens.","code":"invalid_request_error"}}`),
			131072, 150000, 0},
		{"ollama",
			errors.New("400 Bad Request: prompt too long; exceeded max context length by 812 tokens"),
			0, 0, 0},
		{"lm studio",
			openAIError(t, `{"error":{"message":"Message too long: 77300 tokens exceeds the 64256-token context window. Try increasing the Context Length in Model settings, or shorten the conversation.","type":"invalid_request_error","param":null,"code":null}}`),
			64256, 77300, 0},
		{"lm studio, grouped digits",
			openAIError(t, `{"error":{"message":"Message too long: 77,300 tokens exceeds the 64,256-token context window. Try increasing the Context Length in Model settings, or shorten the conversation.","type":"invalid_request_error","param":null,"code":null}}`),
			64256, 77300, 0},
		{"typed",
			&ContextOverflowError{Window: 10, Input: 11, Err: errors.New("too long")},
			10, 11, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			overflow, ok := contextOverflow(c.err)
			if !ok {
				t.Fatalf("%v: not recognized as an overflow", c.err)
			}
			if overflow.Window != c.window || overflow.Input != c.input || overflow.Output != c.output {
				t.Fatalf("%v: window %d input %d output %d, want %d %d %d", c.err, overflow.Window, overflow.Input, overflow.Output, c.window, c.input, c.output)
			}
			if !errors.Is(overflow, c.err) && overflow.Err != c.err {
				t.Fatalf("the overflow does not wrap the provider's error")
			}
		})
	}
}

// TestContextOverflowIgnoresOtherRejections keeps rate limits and other bad
// requests from being answered with smaller ones.
func TestContextOverflowIgnoresOtherRejections(t *testing.T) {
	for _, err := range []error{
		openAIError(t, `{"error":{"message":"Rate limit reached for gpt-5.5 on tokens per min (TPM): Limit 30000, Used 25000, Requested 10000.","type":"tokens","code":"rate_limit_exceeded"}}`),
		providerError(t, http.StatusRequestEntityTooLarge, `{"type":"error","error":{"type":"request_too_large","message":"Request exceeds the maximum allowed number of bytes."}}`, func(e *anthropic.APIError) { e.StatusCode = http.StatusRequestEntityTooLarge }),
		openAIError(t, `{"error":{"message":"messages: roles must alternate between user and assistant","type":"invalid_request_error"}}`),
		errors.New("context deadline exceeded"),
		nil,
	} {
		if overflow, ok := contextOverflow(err); ok {
			t.Errorf("%v recognized as an overflow: %+v", err, overflow)
		}
	}
}

// An overflow built without its provider error, as a host's own rejection
// may be, still reads as one.
func TestContextOverflowWithoutACauseHasAMessage(t *testing.T) {
	err := &ContextOverflowError{Window: 8_000}
	if err.Error() == "" || err.Unwrap() != nil {
		t.Fatalf("Error() = %q, Unwrap() = %v", err.Error(), err.Unwrap())
	}
}
