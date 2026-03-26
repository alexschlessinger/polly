package llm

import (
	"context"
	"encoding/json"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/skills"
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

// SchemaFromJSON parses a JSON schema string into a strict Schema.
// Returns nil if the string is empty or invalid.
func SchemaFromJSON(s string) *Schema {
	if s == "" {
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil
	}
	return &Schema{Raw: raw, Strict: true}
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
	ThinkingEffort ThinkingEffort         // Reasoning effort level: ThinkingOff, ThinkingLow, ThinkingMedium, ThinkingHigh
	Stream         *bool                  // nil = streaming (default), false = non-streaming
	Skills         *skills.Catalog        // Optional skill catalog for automatic system prompt augmentation
}

// ResolvedMessages returns a copy of Messages with skill prompt injected.
// No-op when Skills is nil or empty.
func (r *CompletionRequest) ResolvedMessages() []messages.ChatMessage {
	out := make([]messages.ChatMessage, len(r.Messages))
	copy(out, r.Messages)
	if r.Skills == nil || r.Skills.IsEmpty() {
		return out
	}
	basePrompt := ""
	if len(out) > 0 && out[0].Role == messages.MessageRoleSystem {
		basePrompt = out[0].Content
	}
	runtimeSystem := r.Skills.RuntimeSystemPrompt(basePrompt)
	if len(out) > 0 && out[0].Role == messages.MessageRoleSystem {
		out[0].Content = runtimeSystem
		return out
	}
	return append([]messages.ChatMessage{{
		Role:    messages.MessageRoleSystem,
		Content: runtimeSystem,
	}}, out...)
}
