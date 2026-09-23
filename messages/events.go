package messages

import "github.com/alexschlessinger/pollytool/tools"

// StreamEventType represents the type of streaming event
type StreamEventType string

const (
	// EventTypeContent represents incremental content being streamed
	EventTypeContent StreamEventType = "content"
	// EventTypeReasoning represents incremental reasoning/thinking being streamed
	EventTypeReasoning StreamEventType = "reasoning"
	// EventTypeToolCall represents a tool call event
	EventTypeToolCall StreamEventType = "tool_call"
	// EventTypeComplete represents the complete message
	EventTypeComplete StreamEventType = "complete"
	// EventTypeUsage reports the provider's token usage for the response so
	// far. It may repeat with rising counts and precedes the complete event.
	EventTypeUsage StreamEventType = "usage"
	// EventTypeError represents an error during streaming
	EventTypeError StreamEventType = "error"
)

// StreamEvent represents a single event in the stream
type StreamEvent struct {
	Type     StreamEventType
	Content  string          // For incremental content chunks
	ToolCall *tools.ToolCall // For individual tool calls
	Message  *ChatMessage    // For the complete message
	Error    error           // For error events

	// Usage counts for the response so far, for usage events.
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	// CostUSD is the provider-billed cost so far, when CostReported.
	CostUSD      float64
	CostReported bool
}
