package messages

import "github.com/alexschlessinger/pollytool/tools"

// StreamEventType represents the type of streaming event
type StreamEventType string

const (
	// EventTypeContent represents incremental content being streamed
	EventTypeContent StreamEventType = "content"
	// EventTypeToolCall represents a tool call event
	EventTypeToolCall StreamEventType = "tool_call"
	// EventTypeComplete represents the complete message
	EventTypeComplete StreamEventType = "complete"
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
}
