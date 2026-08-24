// Package anthropic is a minimal REST client for the Anthropic Messages API
// (api.anthropic.com/v1/messages), covering the slice polly uses: streaming
// and non-streaming message creation with tools, thinking, and structured
// output. It replaces github.com/anthropics/anthropic-sdk-go, whose generated
// type surface polly used only a corner of.
//
// Struct fields mirror the wire format one-to-one; names and enum values
// match the JSON the API documents.
package anthropic

import "encoding/json"

// ContentBlock is a message content block in either direction. Type selects
// which fields are meaningful: "text", "image", "thinking", "tool_use", or
// "tool_result".
type ContentBlock struct {
	Type string `json:"type"`

	// Text carries "text" blocks.
	Text string `json:"text,omitempty"`

	// Source carries "image" blocks.
	Source *ImageSource `json:"source,omitempty"`

	// Thinking and Signature carry "thinking" blocks. The signature is an
	// opaque token that must be replayed with its thinking text verbatim, or
	// the API rejects the conversation on later turns.
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	// ID, Name, and Input carry "tool_use" blocks. Input must always be
	// present on the wire, so senders use "{}" for parameterless calls.
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// ToolUseID, Content, and IsError carry "tool_result" blocks; results
	// nest their payload as text blocks.
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   []*ContentBlock `json:"content,omitempty"`
	IsError   *bool           `json:"is_error,omitempty"`
}

// ImageSource is the inline payload of an "image" block.
type ImageSource struct {
	Type      string `json:"type"` // "base64"
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
}

// MessageParam is one conversation turn in a request. Role is "user" or
// "assistant".
type MessageParam struct {
	Role    string          `json:"role"`
	Content []*ContentBlock `json:"content"`
}

// ThinkingConfig enables extended thinking. Modern models use
// {type:"adaptive", display:"summarized"}; legacy models use
// {type:"enabled", budget_tokens:N} with N in [1024, max_tokens).
type ThinkingConfig struct {
	Type         string `json:"type"` // "adaptive" or "enabled"
	Display      string `json:"display,omitempty"`
	BudgetTokens int64  `json:"budget_tokens,omitempty"`
}

const (
	ThinkingTypeAdaptive = "adaptive"
	ThinkingTypeEnabled  = "enabled"

	// DisplaySummarized keeps thinking text in the response stream; the
	// default "omitted" returns only signatures.
	DisplaySummarized = "summarized"
)

// Effort is the adaptive-thinking depth passed via output_config.
type Effort string

const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortXHigh  Effort = "xhigh"
	EffortMax    Effort = "max"
)

// OutputConfig pairs with adaptive thinking to control response effort.
type OutputConfig struct {
	Effort Effort `json:"effort,omitempty"`
}

// Tool declares one callable function. InputSchema.Properties accepts a raw
// JSON Schema properties map.
type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	InputSchema InputSchema `json:"input_schema"`
}

// InputSchema is a tool's parameter schema.
type InputSchema struct {
	Type       string         `json:"type"` // "object"
	Properties map[string]any `json:"properties,omitempty"`
	Required   []string       `json:"required,omitempty"`
}

// ToolChoice directs tool selection; polly only uses {type:"any"} to force
// some tool call.
type ToolChoice struct {
	Type string `json:"type"`
}

// MessageRequest is the body for POST /v1/messages. System sits at the top
// level as text blocks, not inside Messages.
type MessageRequest struct {
	Model        string          `json:"model"`
	MaxTokens    int64           `json:"max_tokens"`
	Messages     []MessageParam  `json:"messages"`
	System       []*ContentBlock `json:"system,omitempty"`
	Temperature  *float64        `json:"temperature,omitempty"`
	Thinking     *ThinkingConfig `json:"thinking,omitempty"`
	OutputConfig *OutputConfig   `json:"output_config,omitempty"`
	Tools        []*Tool         `json:"tools,omitempty"`
	ToolChoice   *ToolChoice     `json:"tool_choice,omitempty"`
	Stream       bool            `json:"stream,omitempty"`
}

// StopReason reports why the model stopped.
type StopReason string

const (
	StopReasonEndTurn      StopReason = "end_turn"
	StopReasonMaxTokens    StopReason = "max_tokens"
	StopReasonStopSequence StopReason = "stop_sequence"
	StopReasonToolUse      StopReason = "tool_use"
	StopReasonRefusal      StopReason = "refusal"
)

// Usage carries token accounting. In streams, input_tokens arrives on
// message_start and output_tokens cumulatively on message_delta events.
type Usage struct {
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
}

// Message is a complete (non-streaming) response, and the message_start
// payload in streams.
type Message struct {
	ID         string          `json:"id,omitempty"`
	Role       string          `json:"role,omitempty"`
	Content    []*ContentBlock `json:"content,omitempty"`
	StopReason StopReason      `json:"stop_reason,omitempty"`
	Usage      *Usage          `json:"usage,omitempty"`
}

// Stream event types. Unknown types must be ignored by consumers — the API
// adds event types over time.
const (
	EventMessageStart      = "message_start"
	EventContentBlockStart = "content_block_start"
	EventContentBlockDelta = "content_block_delta"
	EventContentBlockStop  = "content_block_stop"
	EventMessageDelta      = "message_delta"
	EventMessageStop       = "message_stop"
	EventPing              = "ping"
	EventError             = "error"
)

// Delta types within content_block_delta events.
const (
	DeltaText      = "text_delta"
	DeltaInputJSON = "input_json_delta"
	DeltaThinking  = "thinking_delta"
	DeltaSignature = "signature_delta"
)

// StreamDelta merges the delta payloads of content_block_delta (text,
// partial_json, thinking, signature) and message_delta (stop_reason).
type StreamDelta struct {
	Type        string     `json:"type,omitempty"`
	Text        string     `json:"text,omitempty"`
	PartialJSON string     `json:"partial_json,omitempty"`
	Thinking    string     `json:"thinking,omitempty"`
	Signature   string     `json:"signature,omitempty"`
	StopReason  StopReason `json:"stop_reason,omitempty"`
}

// StreamEvent is one SSE event. Type selects which fields are populated;
// the payload's own type field is authoritative (the event: line repeats it).
type StreamEvent struct {
	Type         string        `json:"type"`
	Message      *Message      `json:"message,omitempty"`       // message_start
	Index        int64         `json:"index,omitempty"`         // content_block_*
	ContentBlock *ContentBlock `json:"content_block,omitempty"` // content_block_start
	Delta        *StreamDelta  `json:"delta,omitempty"`         // content_block_delta, message_delta
	Usage        *Usage        `json:"usage,omitempty"`         // message_delta
	Error        *APIError     `json:"error,omitempty"`         // error
}
