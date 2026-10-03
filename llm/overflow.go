package llm

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// ContextOverflowError is a provider's refusal of a request too long for the
// model's context window, with what the provider said about the request.
// A zero count is one the provider did not state.
type ContextOverflowError struct {
	// Window is the context window the provider enforces.
	Window int
	// Input is the request's input tokens as the provider counted them.
	Input int
	// Output is the output tokens the provider counted against the window.
	Output int
	Err    error
}

func (e *ContextOverflowError) Error() string {
	if e.Err == nil {
		return "request exceeds the model's context window"
	}
	return e.Err.Error()
}
func (e *ContextOverflowError) Unwrap() error { return e.Err }

// overflowSigns are how providers word a context overflow: OpenAI's code
// (which OpenRouter reuses as its error_type), and the messages of OpenAI,
// OpenRouter, DeepSeek, vLLM, Anthropic, Bedrock, Gemini, xAI, Mistral,
// llama.cpp, Ollama and LM Studio (which names the window in the phrase:
// "exceeds the 64256-token context window").
var overflowSigns = regexp.MustCompile(`(?i)context_length_exceeded|maximum context length|prompt is too long|input is too long|exceeds the (?:[\d,]+-token )?context window|exceeds the maximum number of tokens|maximum prompt length is|exceed_context_size_error|exceeds the available context size|exceeded max context length|exceed context limit`)

// rateLimitSigns veto an overflow: a per-minute token limit is not a window.
var rateLimitSigns = regexp.MustCompile(`(?i)rate.?limit|tokens per min|\bTPM\b|quota`)

var (
	overflowWindow = []*regexp.Regexp{
		regexp.MustCompile(`(?i)maximum context length is (\d[\d,]*)`),
		regexp.MustCompile(`(?i)tokens > (\d[\d,]*) maximum`),
		regexp.MustCompile(`(?i)maximum number of tokens allowed \(?(\d[\d,]*)`),
		regexp.MustCompile(`(?i)maximum prompt length is (\d[\d,]*)`),
		regexp.MustCompile(`(?i)(\d[\d,]*) maximum context length`),
		regexp.MustCompile(`"n_ctx"\s*:\s*(\d[\d,]*)`),
		regexp.MustCompile(`(?i)context limit: [\d,]+ \+ [\d,]+ > (\d[\d,]*)`),
		regexp.MustCompile(`(?i)(\d[\d,]*)-token context window`),
	}
	overflowInput = []*regexp.Regexp{
		regexp.MustCompile(`(?i)prompt is too long: (\d[\d,]*) tokens`),
		regexp.MustCompile(`(?i)(\d[\d,]*) in the messages`),
		regexp.MustCompile(`(?i)your messages resulted in (\d[\d,]*) tokens`),
		regexp.MustCompile(`(?i)prompt contains (\d[\d,]*)(?: input)? tokens`),
		regexp.MustCompile(`(?i)input token count \((\d[\d,]*)\)`),
		regexp.MustCompile(`(?i)request contains (\d[\d,]*) tokens`),
		regexp.MustCompile(`"n_prompt_tokens"\s*:\s*(\d[\d,]*)`),
		regexp.MustCompile(`(?i)context limit: (\d[\d,]*) \+`),
		regexp.MustCompile(`(?i)too long: (\d[\d,]*) tokens`),
	}
	overflowOutput = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(\d[\d,]*) (?:in the output|in the completion)`),
		regexp.MustCompile(`(?i)requested (\d[\d,]*) output tokens`),
		regexp.MustCompile(`(?i)context limit: [\d,]+ \+ (\d[\d,]*) >`),
	}
	// overflowRequested is a total of input and output, as OpenRouter states
	// it: "(A of text input, B of tool input, C in the output)".
	overflowRequested = regexp.MustCompile(`(?i)you requested (?:about )?(\d[\d,]*) tokens`)
)

// contextOverflow reports whether err is a provider's refusal of a request
// too long for the context window, and what the provider said about it.
func contextOverflow(err error) (*ContextOverflowError, bool) {
	if err == nil {
		return nil, false
	}
	var typed *ContextOverflowError
	if errors.As(err, &typed) {
		return typed, true
	}
	text := err.Error()
	if !overflowSigns.MatchString(text) || rateLimitSigns.MatchString(text) {
		return nil, false
	}
	overflow := &ContextOverflowError{
		Err:    err,
		Window: firstCount(text, overflowWindow),
		Input:  firstCount(text, overflowInput),
		Output: firstCount(text, overflowOutput),
	}
	if overflow.Input == 0 {
		if requested := firstCount(text, []*regexp.Regexp{overflowRequested}); requested > overflow.Output {
			overflow.Input = requested - overflow.Output
		}
	}
	return overflow, true
}

// firstCount is the first count patterns find in text, read whether or not
// its digits are grouped with commas.
func firstCount(text string, patterns []*regexp.Regexp) int {
	for _, pattern := range patterns {
		if match := pattern.FindStringSubmatch(text); match != nil {
			if n, err := strconv.Atoi(strings.ReplaceAll(match[1], ",", "")); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}
