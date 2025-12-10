package llm

import (
	"context"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// LLM interface defines the contract for language model implementations
type LLM interface {
	// Event-based streaming method
	ChatCompletionStream(context.Context, *CompletionRequest, EventStreamProcessor) <-chan *messages.StreamEvent
}

// EventStreamProcessor processes message streams into events
type EventStreamProcessor interface {
	ProcessMessagesToEvents(<-chan messages.ChatMessage) <-chan *messages.StreamEvent
}

// Schema represents a JSON schema for structured output
type Schema struct {
	Raw    map[string]any // Raw JSON schema
	Strict bool           // Whether to enforce strict validation
}

// CompletionRequest contains all parameters for a completion request
type CompletionRequest struct {
	APIKey         string
	BaseURL        string
	Timeout        time.Duration
	Temperature    float32
	Model          string
	MaxTokens      int
	Messages       []messages.ChatMessage // Message history
	Tools          []tools.Tool           // Available tools
	ResponseSchema *Schema                // Optional schema for structured output
	ThinkingEffort ThinkingEffort          // Reasoning effort level: ThinkingOff, ThinkingLow, ThinkingMedium, ThinkingHigh
}
