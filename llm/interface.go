package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	ijs "github.com/invopop/jsonschema"
	"github.com/xeipuuv/gojsonschema"

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

// Validate checks that a JSON string conforms to this schema.
func (s *Schema) Validate(jsonStr string) error {
	if s == nil {
		return nil
	}
	schemaBytes, err := json.Marshal(s.Raw)
	if err != nil {
		return fmt.Errorf("schema marshal error: %w", err)
	}
	result, err := gojsonschema.Validate(
		gojsonschema.NewBytesLoader(schemaBytes),
		gojsonschema.NewStringLoader(jsonStr),
	)
	if err != nil {
		return fmt.Errorf("schema validation error: %w", err)
	}
	if !result.Valid() {
		var msgs []string
		for _, e := range result.Errors() {
			msgs = append(msgs, e.String())
		}
		return fmt.Errorf("validation failed: %s", strings.Join(msgs, "; "))
	}
	return nil
}

// SchemaFor generates a strict JSON schema from a Go struct using reflection.
// Fields are derived from json tags; descriptions from jsonschema tags.
func SchemaFor(v any) *Schema {
	r := new(ijs.Reflector)
	r.DoNotReference = true
	s := r.Reflect(v)
	s.AdditionalProperties = ijs.FalseSchema
	raw, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return &Schema{Raw: m, Strict: true}
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
