package codex

import (
	"strings"

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

// buildRequest converts a completion request into the backend's body. The
// backend serves a plan, not metered tokens, and takes no sampling or
// output-length knobs; it does require instructions, and it expects a
// reasoning block on every request.
func buildRequest(req *contract.CompletionRequest) *request {
	base := openai.BuildResponsesRequestWith(req, replayReasoning)
	out := &request{ResponsesRequest: *base}
	out.Temperature = nil
	out.MaxOutputTokens = nil
	if strings.TrimSpace(out.Instructions) == "" {
		out.Instructions = defaultInstructions
	}
	if out.Reasoning == nil {
		out.Reasoning = &openai.ReasoningParam{Summary: "auto"}
	}
	if len(out.Tools) > 0 {
		parallel := true
		out.ToolChoice, out.ParallelToolCalls = "auto", &parallel
	}
	if base.Text != nil {
		out.Text = &textConfig{Format: base.Text.Format}
	}
	out.ResponsesRequest.Text = nil
	return out
}
