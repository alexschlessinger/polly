// Package contract holds the request contract shared by the llm package and
// its provider packages: the LLM and stream-processor interfaces, the
// completion request, the thinking-effort model, model capabilities, the
// stream watchdog, and the per-run caches providers read. The llm package
// re-exports the public surface, so callers never import this package.
package contract

import (
	"context"
	"strings"
	"time"

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
	ProcessMessagesToEvents(context.Context, <-chan messages.ChatMessage) <-chan *messages.StreamEvent
}

// Float32Ptr returns a pointer to v. Convenience constructor for optional
// float32 fields like CompletionRequest.Temperature, where nil means "don't
// send the field" (some reasoning models reject `temperature` outright).
func Float32Ptr(v float32) *float32 { return &v }

// StreamMode selects how the provider delivers a completion.
type StreamMode uint8

const (
	// Streaming delivers incremental provider output and is the default.
	Streaming StreamMode = iota
	// Buffered waits for the complete provider response.
	Buffered
)

// CompletionRequest contains all parameters for a completion request
type CompletionRequest struct {
	// ModelHost pins an OpenRouter upstream. Empty allows automatic routing.
	ModelHost string
	// Capabilities optionally supplies authoritative metadata for custom
	// clients. Preparation records the resolved capabilities here so
	// providers adapt the wire form from the same facts.
	Capabilities *ModelCapabilities
	APIKey       string
	BaseURL      string
	// Timeout is the stream stall budget, applied uniformly across providers:
	// a completion is canceled once no provider data has arrived for this
	// long (for a non-streaming call, once it has gone this long without
	// returning). It bounds silence, not total generation time — every chunk
	// resets the clock. Zero disables the watchdog.
	Timeout time.Duration
	// Deadline is the hard per-call wall-clock ceiling: the completion is
	// canceled once it has run this long in total, even while data is still
	// arriving. It backstops endpoints that trickle keepalive data forever.
	// Zero means no ceiling.
	Deadline time.Duration
	// Temperature controls sampling when non-nil. Leave nil to omit the
	// parameter from the upstream request — required for reasoning models
	// (o1, o3, gpt-5.x) which 400 if temperature is supplied at all.
	Temperature *float32
	Model       string
	MaxTokens   int
	// MaxContextTokens limits the deterministic provider-visible projection
	// used by Agent. Direct provider clients ignore it. Zero is unlimited.
	// Explicit budgets take precedence over discovered model context metadata.
	MaxContextTokens int
	// PromptCacheKey groups requests with the same stable agent prefix for
	// provider-side prompt caching. Agent derives one when this is empty.
	PromptCacheKey string
	// CacheSessionID is an opaque, stable per-session routing identity. It is
	// currently used only by providers that support session affinity.
	CacheSessionID string
	Messages       []messages.ChatMessage // Message history
	Tools          []tools.Tool           // Available tools
	ResponseSchema *schema.Schema         // Optional schema for structured output
	ThinkingEffort ThinkingEffort         // Reasoning effort: Off, a named Level, a raw token Budget, or Dynamic
	StreamMode     StreamMode             // Streaming (zero value) or Buffered
	Skills         *skills.Catalog        // Optional skill catalog for automatic system prompt augmentation

	// Replay memoizes provider-side message conversions for one run. Agent.Run
	// shares one across its requests; nil converts without memoizing.
	Replay *ReplayCache
}

// ResolvedMessages returns a copy of Messages with skill prompt injected.
// No-op when Skills is nil or empty.
func (r *CompletionRequest) ResolvedMessages() []messages.ChatMessage {
	out := make([]messages.ChatMessage, len(r.Messages))
	copy(out, r.Messages)
	if r.Skills == nil || r.Skills.IsEmpty() {
		return out
	}
	if len(out) > 0 && out[0].Role == messages.MessageRoleSystem {
		out[0].Content = r.Skills.RuntimeSystemPrompt(out[0].Content)
		return out
	}
	return append([]messages.ChatMessage{{
		Role:    messages.MessageRoleSystem,
		Content: r.Skills.RuntimeSystemPrompt(""),
	}}, out...)
}

// IsStreaming reports whether the request asks for incremental provider output.
func (r *CompletionRequest) IsStreaming() bool {
	return r.StreamMode != Buffered
}

// Provider returns the provider prefix of Model, or "" without one.
func (r *CompletionRequest) Provider() string {
	if p, _, ok := strings.Cut(r.Model, "/"); ok {
		return p
	}
	return ""
}

// IsOpenRouter reports whether the request's provider prefix names OpenRouter.
func (r *CompletionRequest) IsOpenRouter() bool {
	return strings.EqualFold(r.Provider(), "openrouter")
}

// KnownCapabilities returns the caller-supplied capabilities, or the
// all-unknown zero value when none were given.
func (r *CompletionRequest) KnownCapabilities() ModelCapabilities {
	if r.Capabilities != nil {
		return *r.Capabilities
	}
	return ModelCapabilities{}
}

// MetadataMapList decodes a metadata value that holds a list of objects: an
// adapter stores []map[string]any in-process, and a JSON session reload
// brings the same value back as []any.
func MetadataMapList(value any) []map[string]any {
	switch v := value.(type) {
	case []map[string]any:
		return v
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}
