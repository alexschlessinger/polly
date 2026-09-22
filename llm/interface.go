package llm

import (
	"context"
	"fmt"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
)

// The request contract lives in llm/internal/contract so provider packages
// can implement it without importing this package. These aliases keep the
// public surface here.

// LLM interface defines the contract for language model implementations
type LLM = contract.LLM

// EventStreamProcessor processes message streams into events
type EventStreamProcessor = contract.EventStreamProcessor

// CompletionRequest contains all parameters for a completion request
type CompletionRequest = contract.CompletionRequest

// StreamMode selects how the provider delivers a completion.
type StreamMode = contract.StreamMode

const (
	// Streaming delivers incremental provider output and is the default.
	Streaming = contract.Streaming
	// Buffered waits for the complete provider response.
	Buffered = contract.Buffered
)

// ModelCapabilities contains advertised facts; nil means unknown, not false.
type ModelCapabilities = contract.ModelCapabilities

// RequestAdaptation reports changes made only to the outgoing projection.
type RequestAdaptation = contract.RequestAdaptation

// ReplayCache memoizes provider-side message conversions for one run; see
// CompletionRequest.Replay.
type ReplayCache = contract.ReplayCache

// Model catalog and embedding types are shared with the provider packages
// that fetch them.
type (
	ModelTarget       = contract.ModelTarget
	ModelPrice        = contract.ModelPrice
	ModelEndpointInfo = contract.ModelEndpointInfo
	ModelInfo         = contract.ModelInfo
	ModelCatalog      = contract.ModelCatalog
	EmbeddingRequest  = contract.EmbeddingRequest
	EmbeddingResponse = contract.EmbeddingResponse
)

// ErrModelMetadataUnknown reports that no catalog describes the target.
var ErrModelMetadataUnknown = contract.ErrModelMetadataUnknown

// ThinkingLevel is an ordered, provider-agnostic reasoning level.
type ThinkingLevel = contract.ThinkingLevel

// ThinkingEffort is a tagged union describing how much reasoning effort to
// spend: Off (zero value), a named Level, a raw token Budget, or Dynamic.
type ThinkingEffort = contract.ThinkingEffort

const (
	LevelMinimal = contract.LevelMinimal
	LevelLow     = contract.LevelLow
	LevelMedium  = contract.LevelMedium
	LevelHigh    = contract.LevelHigh
	LevelXHigh   = contract.LevelXHigh
	LevelMax     = contract.LevelMax
)

// EffortOff returns the zero/disabled effort.
func EffortOff() ThinkingEffort { return contract.EffortOff() }

// EffortLevel returns an effort pinned to a named level.
func EffortLevel(l ThinkingLevel) ThinkingEffort { return contract.EffortLevel(l) }

// EffortBudget returns an effort expressed as a raw token budget.
func EffortBudget(tokens int) ThinkingEffort { return contract.EffortBudget(tokens) }

// EffortDynamic returns an effort that defers depth to the model.
func EffortDynamic() ThinkingEffort { return contract.EffortDynamic() }

// ParseThinkingEffort converts a string to a ThinkingEffort: an effort word,
// aliases included (empty means off), or a positive integer token budget.
func ParseThinkingEffort(s string) (ThinkingEffort, error) { return contract.ParseThinkingEffort(s) }

// ThinkingEffortWords lists every advertised word ParseThinkingEffort accepts.
func ThinkingEffortWords() []string { return contract.ThinkingEffortWords() }

// ThinkingEffortForms spells out every accepted effort form for usage text.
func ThinkingEffortForms() string { return contract.ThinkingEffortForms() }

// OpenRouterReasoning is OpenRouter's unified reasoning control as sent on
// the wire.
type OpenRouterReasoning = contract.OpenRouterReasoning

// OpenRouterThinking is a request-only resolution of an OpenRouter reasoning
// preference; the saved preference is never changed.
type OpenRouterThinking = contract.OpenRouterThinking

// ResolveOpenRouterThinking combines the preference with advertised policy.
func ResolveOpenRouterThinking(e ThinkingEffort, c ModelCapabilities) (OpenRouterThinking, error) {
	return contract.ResolveOpenRouterThinking(e, c)
}

// ResolveOpenRouterRequestThinking adapts an outgoing request's preference to
// its model, falling back to the provider default when the saved preference
// is unsupported. It is what the OpenRouter provider sends.
func ResolveOpenRouterRequestThinking(e ThinkingEffort, c ModelCapabilities) OpenRouterThinking {
	return contract.ResolveOpenRouterRequestThinking(e, c)
}

// Complete prepares a request and returns the full completion, including usage,
// reasoning, tool calls and stop reason. It does not execute tools. On failure it
// returns any streamed text/reasoning with the error; that message is not final.
func Complete(ctx context.Context, client LLM, req *CompletionRequest) (*messages.ChatMessage, error) {
	if client == nil || req == nil {
		return nil, fmt.Errorf("client and request are required")
	}
	prepared, _, err := Prepare(ctx, client, req, false)
	if err != nil {
		return nil, err
	}
	return contract.Complete(ctx, client, prepared)
}

// Collect prepares a request and returns its text, including partial text on error.
// Use Complete when usage, stop reason, reasoning or tool calls matter.
func Collect(ctx context.Context, client LLM, req *CompletionRequest) (string, error) {
	response, err := Complete(ctx, client, req)
	if response == nil {
		return "", err
	}
	return response.GetContent(), err
}

// Float32Ptr returns a pointer to v for optional fields like
// CompletionRequest.Temperature, where nil means "don't send the field".
func Float32Ptr(v float32) *float32 { return contract.Float32Ptr(v) }

// Schema is a type alias so callers can use llm.Schema without importing schema.
type Schema = schema.Schema

// ToolSchema is a type alias so callers can use llm.ToolSchema without importing schema.
type ToolSchema = schema.ToolSchema

// SchemaFor generates a strict JSON schema from a Go struct using reflection.
func SchemaFor(v any) (*Schema, error) { return schema.SchemaFor(v) }

// SchemaFromJSON parses a JSON schema string into a strict Schema.
func SchemaFromJSON(s string) (*Schema, error) { return schema.SchemaFromJSON(s) }

// MustSchemaFor constructs a static reflected schema and panics on error.
func MustSchemaFor(v any) *Schema { return schema.MustSchemaFor(v) }

// MustSchemaFromJSON constructs a static JSON schema and panics on error.
func MustSchemaFromJSON(s string) *Schema { return schema.MustSchemaFromJSON(s) }
