package contract

import (
	"fmt"
	"slices"
	"strings"
)

// OpenRouterReasoning is OpenRouter's unified reasoning control as sent on
// the wire. Native OpenAI and other compatible providers retain their own
// request mappings.
type OpenRouterReasoning struct {
	Effort    string `json:"effort,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
	Enabled   *bool  `json:"enabled,omitempty"`
}

// OpenRouterThinking is a request-only resolution; the saved preference is
// never changed. Display is also suitable for settings UI without network I/O.
type OpenRouterThinking struct {
	Request *OpenRouterReasoning
	Display string
	Notice  string
}

// OpenRouterEfforts is the gateway's own effort vocabulary: the words its
// unified reasoning control accepts. Unlike a native provider, which clamps a
// level it cannot spell, OpenRouter rejects an unknown effort outright, and
// "max" is polly's top level with no gateway spelling. A model whose catalog
// record advertises a complete list of its own overrides this one.
var OpenRouterEfforts = slices.Clone(levelNames[:LevelMax])

// openRouterEffortOrder is the order a required minimum is chosen in: none
// first, then every level polly names, shallowest up.
var openRouterEffortOrder = append([]string{"none"}, levelNames[:]...)

// ResolveOpenRouterThinking combines the preference with advertised policy.
// Missing facts remain unknown; nil + Complete means unrestricted efforts.
func ResolveOpenRouterThinking(e ThinkingEffort, c ModelCapabilities) (OpenRouterThinking, error) {
	r := OpenRouterThinking{Display: e.String()}
	providerDefault := func() string {
		if c.ReasoningDefaultEnabled != nil && !*c.ReasoningDefaultEnabled && (c.ReasoningMandatory == nil || !*c.ReasoningMandatory) {
			return "off (provider default)"
		}
		if c.ReasoningDefaultEffort != nil && *c.ReasoningDefaultEffort != "" {
			return *c.ReasoningDefaultEffort + " (provider default)"
		}
		return "provider default"
	}
	if e.IsDynamic() {
		r.Display = "dynamic → " + providerDefault()
		return r, nil
	}
	if !e.IsEnabled() {
		switch {
		case c.ReasoningMandatory != nil && *c.ReasoningMandatory:
			if c.ReasoningEffortsComplete {
				for _, effort := range openRouterEffortOrder[1:] {
					if slices.Contains(effortVocabulary(OpenRouterEfforts, c), effort) {
						r.Request = &OpenRouterReasoning{Effort: effort}
						r.Display = "off → " + effort + " (required)"
						r.Notice = "Thinking " + r.Display + "; saved preference remains off"
						return r, nil
					}
				}
			}
			r.Display = "off → " + providerDefault() + " (required; minimum unknown)"
			r.Notice = "Thinking " + r.Display + "; saved preference remains off"
		case c.ReasoningMandatory != nil && !*c.ReasoningMandatory:
			r.Request = &OpenRouterReasoning{Enabled: new(bool)}
		default:
			r.Display = "off → provider default (effective thinking unknown)"
			r.Notice = "Thinking policy unavailable; using the provider default (effective thinking unknown); saved preference remains off"
		}
		return r, nil
	}
	if e.kind == kindLevel {
		level := e.String()
		if efforts := effortVocabulary(OpenRouterEfforts, c); !slices.Contains(efforts, level) {
			choices := strings.Join(efforts, ", ")
			if choices == "" {
				choices = "no named efforts; use dynamic"
			}
			return r, fmt.Errorf("OpenRouter thinking effort %q is unsupported; valid choices: %s", level, choices)
		}
		if c.Reasoning != nil && !*c.Reasoning {
			return r, fmt.Errorf("OpenRouter model does not support reasoning; valid choices: off, dynamic")
		}
		r.Request = &OpenRouterReasoning{Effort: level}
	} else if budget, ok := e.AsBudget(); ok {
		if c.ReasoningMaxTokens != nil && !*c.ReasoningMaxTokens || c.Reasoning != nil && !*c.Reasoning {
			return r, fmt.Errorf("OpenRouter model does not support a reasoning token budget; use an advertised effort or dynamic")
		}
		r.Request = &OpenRouterReasoning{MaxTokens: budget}
	}
	return r, nil
}

// ResolveOpenRouterRequestThinking adapts an outgoing request to its model: a
// saved preference can outlive the model that supported it, so explicit
// setting validation stays strict while the request falls back to dynamic.
func ResolveOpenRouterRequestThinking(e ThinkingEffort, c ModelCapabilities) OpenRouterThinking {
	resolved, err := ResolveOpenRouterThinking(e, c)
	if err == nil {
		return resolved
	}
	resolved, _ = ResolveOpenRouterThinking(EffortDynamic(), c)
	resolved.Display = e.String() + " → " + resolved.Display
	resolved.Notice = err.Error() + "; using provider default; saved thinking preference retained"
	return resolved
}

// OpenRouterThinkingWords narrows named completion hints using cached facts.
// Off remains a saved preference even for models that require thinking.
func OpenRouterThinkingWords(c ModelCapabilities) []string {
	words := []string{"off", "dynamic"}
	for _, word := range ThinkingEffortWords() {
		if word == "off" || word == "dynamic" {
			continue
		}
		if (!c.ReasoningEffortsComplete || c.ReasoningEfforts == nil || slices.Contains(c.ReasoningEfforts, word)) && (c.Reasoning == nil || *c.Reasoning) {
			words = append(words, word)
		}
	}
	return words
}
