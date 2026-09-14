package llm

import (
	"context"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
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

// OpenRouterThinkingWords narrows named completion hints using cached facts.
func OpenRouterThinkingWords(c ModelCapabilities) []string {
	return contract.OpenRouterThinkingWords(c)
}

// SimpleProcessor is a basic implementation of EventStreamProcessor
type SimpleProcessor = contract.SimpleProcessor

// Collect calls ChatCompletionStream on the given LLM client and returns the final content string.
func Collect(ctx context.Context, client LLM, req *CompletionRequest) (string, error) {
	return contract.Collect(ctx, client, req)
}

// Float32Ptr returns a pointer to v for optional fields like
// CompletionRequest.Temperature, where nil means "don't send the field".
func Float32Ptr(v float32) *float32 { return contract.Float32Ptr(v) }

// Schema is a type alias so callers can use llm.Schema without importing schema.
type Schema = schema.Schema

// ToolSchema is a type alias so callers can use llm.ToolSchema without importing schema.
type ToolSchema = schema.ToolSchema

// SchemaFor generates a strict JSON schema from a Go struct using reflection.
func SchemaFor(v any) *Schema { return schema.SchemaFor(v) }

// SchemaFromJSON parses a JSON schema string into a strict Schema.
func SchemaFromJSON(s string) *Schema { return schema.SchemaFromJSON(s) }
