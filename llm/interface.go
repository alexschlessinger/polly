package llm

import (
	"context"
	"time"

	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
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

// Schema is a type alias so callers can use llm.Schema without importing schema.
type Schema = schema.Schema

// ToolSchema is a type alias so callers can use llm.ToolSchema without importing schema.
type ToolSchema = schema.ToolSchema

// SchemaFor generates a strict JSON schema from a Go struct using reflection.
func SchemaFor(v any) *Schema { return schema.SchemaFor(v) }

// SchemaFromJSON parses a JSON schema string into a strict Schema.
func SchemaFromJSON(s string) *Schema { return schema.SchemaFromJSON(s) }

// Float32Ptr returns a pointer to v. Convenience constructor for optional
// float32 fields like CompletionRequest.Temperature, where nil means "don't
// send the field" (some reasoning models reject `temperature` outright).
func Float32Ptr(v float32) *float32 { return &v }

// CompletionRequest contains all parameters for a completion request
type CompletionRequest struct {
	APIKey  string
	BaseURL string
	Timeout time.Duration
	// Temperature controls sampling when non-nil. Leave nil to omit the
	// parameter from the upstream request — required for reasoning models
	// (o1, o3, gpt-5.x) which 400 if temperature is supplied at all.
	Temperature    *float32
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

// runStream handles the common goroutine scaffolding for ChatCompletionStream.
// Each provider creates its adapter, then passes a function that does the
// provider-specific work with the StreamingCore.
func runStream(ctx context.Context, processor EventStreamProcessor, adapter streaming.ProviderAdapter, fn func(*streaming.StreamingCore)) <-chan *messages.StreamEvent {
	ch := make(chan messages.ChatMessage, 10)
	go func() {
		defer close(ch)
		fn(streaming.NewStreamingCore(ctx, ch, adapter))
	}()
	return processor.ProcessMessagesToEvents(ch)
}
