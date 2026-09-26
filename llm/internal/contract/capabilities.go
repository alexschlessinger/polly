package contract

// ModelCapabilities contains advertised facts; nil means unknown, not false.
// A non-nil modalities/parameters list is an authoritative complete list.
// ParameterServiceTier is the Parameters key a catalog sets when it knows
// whether a model can run on its provider's fast service tier.
const ParameterServiceTier = "service_tier"

type ModelCapabilities struct {
	Chat                    *bool    `json:"chat,omitempty"`
	InputModalities         []string `json:"inputModalities"`
	OutputModalities        []string `json:"outputModalities"`
	Tools                   *bool    `json:"tools,omitempty"`
	StructuredOutput        *bool    `json:"structuredOutput,omitempty"`
	Reasoning               *bool    `json:"reasoning,omitempty"`
	ReasoningMandatory      *bool    `json:"reasoningMandatory,omitempty"`
	ReasoningDefaultEnabled *bool    `json:"reasoningDefaultEnabled,omitempty"`
	ReasoningDefaultEffort  *string  `json:"reasoningDefaultEffort,omitempty"`
	ReasoningMaxTokens      *bool    `json:"reasoningMaxTokens,omitempty"`
	// ReasoningPolicy distinguishes model-wide gateway policy from a union
	// of route capabilities. A nil complete effort list means unrestricted.
	ReasoningPolicy          bool            `json:"reasoningPolicy,omitempty"`
	ReasoningEfforts         []string        `json:"reasoningEfforts"`
	ReasoningEffortsComplete bool            `json:"reasoningEffortsComplete,omitempty"`
	Sampling                 map[string]any  `json:"sampling,omitempty"`
	ReasoningOptions         map[string]any  `json:"reasoningOptions,omitempty"`
	ImageConstraints         map[string]any  `json:"imageConstraints,omitempty"`
	Parameters               map[string]bool `json:"parameters,omitempty"`
	ParametersComplete       bool            `json:"parametersComplete,omitempty"`
	ContextTokens            *int            `json:"contextTokens,omitempty"`
	InputTokens              *int            `json:"inputTokens,omitempty"`
	OutputTokens             *int            `json:"outputTokens,omitempty"`
	RuntimeContextTokens     *int            `json:"runtimeContextTokens,omitempty"`
	UnlimitedLimits          map[string]bool `json:"unlimitedLimits,omitempty"`
	MaxImages                *int            `json:"maxImages,omitempty"`
}

// ContextWindow returns the smallest advertised context limit, or zero when
// none is known.
func (c ModelCapabilities) ContextWindow() int {
	n := 0
	for _, p := range []*int{c.ContextTokens, c.InputTokens, c.RuntimeContextTokens} {
		if p != nil && *p > 0 && (n == 0 || *p < n) {
			n = *p
		}
	}
	return n
}

// RequestAdaptation reports changes made only to the outgoing projection.
type RequestAdaptation struct {
	Feature string `json:"feature"`
	Count   int    `json:"count"`
	Message string `json:"message"`
}
