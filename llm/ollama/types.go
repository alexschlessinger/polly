// Package ollama is a minimal client for the Ollama native chat API
// (/api/chat), covering the slice polly uses. It replaces
// github.com/ollama/ollama/api, whose module dragged in an ordered-map stack
// that pinned an extra YAML library into the binary.
//
// Struct fields mirror the wire format one-to-one. Streaming is NDJSON: one
// ChatResponse object per line, the last marked done.
package ollama

import "encoding/json"

// ImageData is raw image bytes; JSON encoding renders it as base64, which is
// what the API expects.
type ImageData []byte

// Message is one conversation turn. Role is "system", "user", "assistant",
// or "tool".
type Message struct {
	Role      string      `json:"role"`
	Content   string      `json:"content"`
	Thinking  string      `json:"thinking,omitempty"`
	Images    []ImageData `json:"images,omitempty"`
	ToolCalls []ToolCall  `json:"tool_calls,omitempty"`
	ToolName  string      `json:"tool_name,omitempty"`
	// ToolCallID, on a tool response, names the call it answers when the
	// server issued call IDs.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// ToolCall is a tool invocation requested by the model.
type ToolCall struct {
	// ID is set by some models/servers; when present it must be echoed back
	// on the matching tool response.
	ID       string           `json:"id,omitempty"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction carries the call payload; arguments arrive as a JSON
// object, not a string.
type ToolCallFunction struct {
	Index     int            `json:"index"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// Tool declares one callable function.
type Tool struct {
	Type     string       `json:"type"` // "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction describes a function and its parameters.
type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  ToolParameters `json:"parameters"`
}

// ToolParameters is a tool's parameter schema. Properties is always
// serialized, matching the official client.
type ToolParameters struct {
	Type       string         `json:"type"`
	Required   []string       `json:"required,omitempty"`
	Properties map[string]any `json:"properties"`
}

// ChatRequest is the body for POST /api/chat. Stream is a pointer because
// the server defaults to streaming when the field is absent; polly always
// sends an explicit value. Think takes true/false or a level string
// depending on the model. Options carries sampler settings (num_predict,
// temperature) and is always serialized, matching the official client.
type ChatRequest struct {
	Model    string          `json:"model"`
	Messages []Message       `json:"messages"`
	Stream   *bool           `json:"stream,omitempty"`
	Format   json.RawMessage `json:"format,omitempty"`
	Tools    []Tool          `json:"tools,omitempty"`
	Options  map[string]any  `json:"options"`
	Think    any             `json:"think,omitempty"`
}

// DoneReasonLength is the DoneReason of a reply cut off by num_predict.
const DoneReasonLength = "length"

// ChatResponse is one NDJSON line of a chat: an incremental chunk, or the
// final summary when Done is true (which carries the token counts and
// DoneReason: "stop", or "length" when num_predict truncated the reply).
type ChatResponse struct {
	Model           string  `json:"model"`
	Message         Message `json:"message"`
	Done            bool    `json:"done"`
	DoneReason      string  `json:"done_reason,omitempty"`
	PromptEvalCount int     `json:"prompt_eval_count,omitempty"`
	EvalCount       int     `json:"eval_count,omitempty"`
}
