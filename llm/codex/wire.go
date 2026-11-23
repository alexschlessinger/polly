package codex

import (
	"slices"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/llm/openai"
)

// request is the backend's Responses body: OpenAI's stateless shape plus
// the tool-routing and text controls the Codex client sends. Text shadows
// the embedded field so verbosity can ride next to the output format.
type request struct {
	openai.ResponsesRequest
	ToolChoice        string      `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool       `json:"parallel_tool_calls,omitempty"`
	Text              *textConfig `json:"text,omitempty"`
}

// textConfig is the backend's text block.
type textConfig struct {
	Verbosity string             `json:"verbosity,omitempty"`
	Format    *openai.TextFormat `json:"format,omitempty"`
}

// Streaming implements openai.ResponsesBody around the embedded body.
func (r *request) Streaming(on bool) openai.ResponsesBody {
	body := *r
	body.ResponsesRequest = *r.ResponsesRequest.Streaming(on).(*openai.ResponsesRequest)
	return &body
}

// SupportsServiceTier checks the signed-in account's model record. Priority
// retains its legacy behavior when tiers are unknown; Ultrafast is opt-in
// only when the selected model explicitly advertises it.
func SupportsServiceTier(tier string, caps contract.ModelCapabilities) bool {
	if caps.ServiceTierRuledOut() {
		return false
	}
	switch tier {
	case contract.ServiceTierPriority:
		return caps.ServiceTiers == nil || slices.Contains(caps.ServiceTiers, tier)
	case contract.ServiceTierUltrafast:
		return slices.Contains(caps.ServiceTiers, tier)
	}
	return false
}

// buildRequest converts a completion request into the backend's body. The
// backend serves a plan, not metered tokens, and refuses sampling and
// output-length knobs ("Unsupported parameter"); it expects a reasoning
// block on every request, and its models reason at levels up to max, so
// that level goes through where api.openai.com would fold it into xhigh.
// The selected tier is also used by the transport's routing hint.
func buildRequest(req *contract.CompletionRequest) *request {
	base := openai.BuildResponsesRequestWith(req, ReplayReasoningItems)
	out := &request{ResponsesRequest: *base}
	out.ServiceTier = ""
	if tier := req.RequestedServiceTier(); SupportsServiceTier(tier, req.KnownCapabilities()) {
		out.ServiceTier = tier
	}
	out.Temperature = nil
	out.MaxOutputTokens = nil
	if out.Reasoning == nil {
		out.Reasoning = &openai.ReasoningParam{Summary: "auto"}
	} else if effort := req.ThinkingEffort; effort.IsEnabled() && !effort.IsDynamic() && effort.AsLevel(contract.LevelMedium) == contract.LevelMax {
		out.Reasoning.Effort = "max"
	}
	// Tools and output formats go out the way the Codex client sends them:
	// never strict. A strict schema the backend cannot compile ends the
	// response before a token is produced, as an incomplete reply with no
	// output, so a schema's enforcement stays with the caller's own
	// validation, which polly's structured results already carry.
	loose := false
	for i := range out.Tools {
		out.Tools[i].Strict = &loose
	}
	if len(out.Tools) > 0 {
		parallel := true
		out.ToolChoice, out.ParallelToolCalls = "auto", &parallel
	}
	if base.Text != nil {
		out.Text = &textConfig{Format: base.Text.Format}
		if out.Text.Format != nil {
			out.Text.Format.Strict = &loose
		}
	}
	out.ResponsesRequest.Text = nil
	return out
}
