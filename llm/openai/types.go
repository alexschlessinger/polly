// Package openai is a minimal REST client for the OpenAI API and
// OpenAI-compatible servers, covering the slice polly uses: Chat Completions
// (the dialect every compatible provider speaks — DeepSeek, OpenRouter,
// HuggingFace, custom base URLs), the Responses API (native OpenAI), and
// embeddings. It replaces github.com/openai/openai-go, whose generated type
// surface polly used only a corner of.
//
// Struct fields mirror the wire format one-to-one. Response parsing is
// deliberately lenient — unknown fields, event types, and item types are
// ignored — because the servers on the other end are not all OpenAI.
package openai

import "encoding/json"

// FlexString is a string that tolerates non-string JSON values by decoding
// them to "". Compatible servers occasionally put objects where OpenAI
// documents strings (e.g. arguments on exotic output items); polly ignores
// those values, and a hard unmarshal failure would kill the whole stream.
type FlexString string

func (s *FlexString) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err == nil {
		*s = FlexString(str)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Chat Completions (chat/completions)
// ---------------------------------------------------------------------------

// ChatMessage is one request message. Content holds a string (system, tool,
// assistant), a []ChatContentPart (user), or nil (assistant with only tool
// calls — the key must be absent, not null).
type ChatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`
	ToolCalls  []ChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	// ReasoningContent replays DeepSeek reasoning on assistant turns;
	// DeepSeek's reasoning models 400 when it is omitted on follow-ups.
	// Standard OpenAI ignores it.
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// ChatContentPart is one element of a user message's content array.
type ChatContentPart struct {
	Type     string        `json:"type"` // "text" or "image_url"
	Text     string        `json:"text,omitempty"`
	ImageURL *ChatImageURL `json:"image_url,omitempty"`
}

// ChatImageURL carries an image by URL or data: URI.
type ChatImageURL struct {
	URL string `json:"url"`
}

// ChatToolCall is a completed tool call: in assistant request messages and
// in non-streaming responses.
type ChatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // "function"
	Function ChatToolCallFunc `json:"function"`
}

// ChatToolCallFunc is the function payload of a tool call.
type ChatToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatTool declares one callable function.
type ChatTool struct {
	Type     string      `json:"type"` // "function"
	Function FunctionDef `json:"function"`
}

// FunctionDef describes a function and its raw JSON Schema parameters.
type FunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ResponseFormat selects structured output for Chat Completions.
type ResponseFormat struct {
	Type       string          `json:"type"` // "json_schema"
	JSONSchema *JSONSchemaSpec `json:"json_schema,omitempty"`
}

// JSONSchemaSpec is the nested json_schema object of a chat ResponseFormat.
type JSONSchemaSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Schema      map[string]any `json:"schema,omitempty"`
	Strict      *bool          `json:"strict,omitempty"`
}

// StreamOptions tunes streaming; include_usage puts token usage on a final
// chunk whose choices array is empty.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// ChatCompletionRequest is the body for POST chat/completions.
type ChatCompletionRequest struct {
	Model               string          `json:"model"`
	Messages            []ChatMessage   `json:"messages"`
	Temperature         *float64        `json:"temperature,omitempty"`
	MaxCompletionTokens *int64          `json:"max_completion_tokens,omitempty"`
	ReasoningEffort     ReasoningEffort `json:"reasoning_effort,omitempty"`
	ResponseFormat      *ResponseFormat `json:"response_format,omitempty"`
	Tools               []ChatTool      `json:"tools,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *StreamOptions  `json:"stream_options,omitempty"`
}

// ReasoningEffort is OpenAI's reasoning depth enum, shared by Chat
// Completions (reasoning_effort) and the Responses API (reasoning.effort).
type ReasoningEffort string

const (
	ReasoningEffortMinimal ReasoningEffort = "minimal"
	ReasoningEffortLow     ReasoningEffort = "low"
	ReasoningEffortMedium  ReasoningEffort = "medium"
	ReasoningEffortHigh    ReasoningEffort = "high"
	ReasoningEffortXhigh   ReasoningEffort = "xhigh"
)

// ChatUsage carries token accounting. Pointers to it preserve presence: a
// compatible server that omits usage yields nil, not zeros.
type ChatUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// ChatResponseMessage is the message of a non-streaming choice.
type ChatResponseMessage struct {
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content"` // DeepSeek
	ToolCalls        []ChatToolCall `json:"tool_calls"`
}

// ChatChoice is one non-streaming completion choice.
type ChatChoice struct {
	Message      ChatResponseMessage `json:"message"`
	FinishReason string              `json:"finish_reason"`
}

// ChatCompletion is a non-streaming chat/completions response.
type ChatCompletion struct {
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   *ChatUsage   `json:"usage"`
}

// ChatToolCallDelta is an incremental tool-call fragment in a stream chunk.
// Index correlates fragments; servers that omit it get 0, matching the
// official SDK's tolerance.
type ChatToolCallDelta struct {
	Index    int64            `json:"index"`
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ChatToolCallFunc `json:"function"`
}

// ChatDelta is the incremental payload of a streaming choice.
type ChatDelta struct {
	Content          string              `json:"content"`
	ReasoningContent string              `json:"reasoning_content"` // DeepSeek
	ToolCalls        []ChatToolCallDelta `json:"tool_calls"`
}

// ChatChunkChoice is one streaming choice.
type ChatChunkChoice struct {
	Delta        ChatDelta `json:"delta"`
	FinishReason string    `json:"finish_reason"`
}

// ChatCompletionChunk is one streaming chunk. With include_usage, the final
// chunk carries Usage and an empty Choices array.
type ChatCompletionChunk struct {
	Choices []ChatChunkChoice `json:"choices"`
	Usage   *ChatUsage        `json:"usage"`
}

// ---------------------------------------------------------------------------
// Responses API (responses) — native OpenAI only
// ---------------------------------------------------------------------------

// ResponseInputItem is one input list entry. Type selects the shape:
// "message" (user turns, and assistant replays which also set ID/Status),
// "function_call", or "function_call_output". User messages leave Type
// empty — the API infers it, and the official SDK omits the key too.
type ResponseInputItem struct {
	Type string `json:"type,omitempty"`

	// message
	Role    string `json:"role,omitempty"`
	Content any    `json:"content,omitempty"` // []ResponseInputContent (user) or []ResponseOutputContent (assistant)
	ID      string `json:"id,omitempty"`
	Status  string `json:"status,omitempty"`

	// function_call and function_call_output
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// Output is required on function_call_output items even when the tool
	// produced nothing — a pointer so the empty string still serializes
	// while every other item type omits the key.
	Output *string `json:"output,omitempty"`
}

// ResponseInputContent is one part of a user message: input_text or
// input_image.
type ResponseInputContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Detail   string `json:"detail,omitempty"` // input_image: "auto"
	ImageURL string `json:"image_url,omitempty"`
}

// ResponseOutputContent is one part of a replayed assistant message. The
// API requires annotations on output_text, so it always serializes.
type ResponseOutputContent struct {
	Type        string `json:"type"` // "output_text"
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

// ReasoningParam configures reasoning for the Responses API.
type ReasoningParam struct {
	Effort  ReasoningEffort `json:"effort,omitempty"`
	Summary string          `json:"summary,omitempty"` // "auto"
}

// TextConfig selects the Responses output format.
type TextConfig struct {
	Format *TextFormat `json:"format,omitempty"`
}

// TextFormat is a flat json_schema output format (unlike chat, which nests
// it under a json_schema key).
type TextFormat struct {
	Type        string         `json:"type"` // "json_schema"
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Schema      map[string]any `json:"schema,omitempty"`
	Strict      *bool          `json:"strict,omitempty"`
}

// ResponsesTool declares one function tool. Strict is a pointer because the
// API distinguishes absent from false, and polly always sends it.
type ResponsesTool struct {
	Type        string         `json:"type"` // "function"
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Strict      *bool          `json:"strict,omitempty"`
}

// ResponsesRequest is the body for POST responses.
type ResponsesRequest struct {
	Model           string              `json:"model"`
	Input           []ResponseInputItem `json:"input"`
	Instructions    string              `json:"instructions,omitempty"`
	Temperature     *float64            `json:"temperature,omitempty"`
	MaxOutputTokens *int64              `json:"max_output_tokens,omitempty"`
	Reasoning       *ReasoningParam     `json:"reasoning,omitempty"`
	Text            *TextConfig         `json:"text,omitempty"`
	Tools           []ResponsesTool     `json:"tools,omitempty"`
	Stream          bool                `json:"stream,omitempty"`
}

// ResponseStatus is the terminal (or in-progress) state of a Response.
type ResponseStatus string

const (
	ResponseStatusCompleted  ResponseStatus = "completed"
	ResponseStatusIncomplete ResponseStatus = "incomplete"
	ResponseStatusFailed     ResponseStatus = "failed"
	ResponseStatusCancelled  ResponseStatus = "cancelled"
	ResponseStatusInProgress ResponseStatus = "in_progress"
)

// ResponseUsage carries Responses token accounting.
type ResponseUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

// IncompleteDetails says why a response stopped early.
type IncompleteDetails struct {
	Reason string `json:"reason"` // "max_output_tokens", "content_filter"
}

// ResponseItemContent is one content part of an output item: output_text or
// refusal on messages, reasoning_text on reasoning items.
type ResponseItemContent struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

// ResponseReasoningSummary is one summary part of a reasoning item.
type ResponseReasoningSummary struct {
	Type string `json:"type"` // "summary_text"
	Text string `json:"text"`
}

// ResponseOutputItem is one output list entry; polly consumes "message",
// "reasoning", and "function_call" and ignores everything else.
type ResponseOutputItem struct {
	Type    string                     `json:"type"`
	ID      string                     `json:"id"`
	Status  string                     `json:"status"`
	Content []ResponseItemContent      `json:"content"`
	Summary []ResponseReasoningSummary `json:"summary"`

	// function_call
	CallID    string     `json:"call_id"`
	Name      string     `json:"name"`
	Arguments FlexString `json:"arguments"`
}

// Response is a complete Responses API result, and the payload of terminal
// stream events.
type Response struct {
	ID                string               `json:"id"`
	Status            ResponseStatus       `json:"status"`
	Output            []ResponseOutputItem `json:"output"`
	Usage             *ResponseUsage       `json:"usage"`
	IncompleteDetails *IncompleteDetails   `json:"incomplete_details"`
}

// ResponseStreamEvent is one Responses SSE event. Type selects which fields
// are populated; unknown types must be ignored — the event vocabulary grows
// over time. Error events arrive as ordinary events (type "error"), not
// transport failures, so callers decide how to surface them.
type ResponseStreamEvent struct {
	Type        string              `json:"type"`
	Delta       FlexString          `json:"delta"`
	OutputIndex int64               `json:"output_index"`
	Item        *ResponseOutputItem `json:"item"`
	Response    *Response           `json:"response"`
	Name        string              `json:"name"`
	Arguments   string              `json:"arguments"`

	// error events
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ---------------------------------------------------------------------------
// Embeddings (embeddings)
// ---------------------------------------------------------------------------

// EmbeddingRequest is the body for POST embeddings.
type EmbeddingRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions *int64   `json:"dimensions,omitempty"`
}

// EmbeddingData is one embedding vector.
type EmbeddingData struct {
	Embedding []float64 `json:"embedding"`
}

// EmbeddingUsage carries embeddings token accounting.
type EmbeddingUsage struct {
	PromptTokens int64 `json:"prompt_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

// EmbeddingResponse is the embeddings result.
type EmbeddingResponse struct {
	Model string          `json:"model"`
	Data  []EmbeddingData `json:"data"`
	Usage *EmbeddingUsage `json:"usage"`
}
