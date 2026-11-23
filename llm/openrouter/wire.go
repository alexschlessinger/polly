package openrouter

import (
	"encoding/json"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/llm/openai"
)

// Routing pins a request to specific upstream hosts.
type Routing struct {
	Only           []string `json:"only"`
	AllowFallbacks bool     `json:"allow_fallbacks"`
}

// ChatRequest is OpenRouter's Chat Completions body: the OpenAI shape plus
// the gateway's extensions. Messages shadows the embedded list so each
// assistant turn can carry replayed reasoning.
type ChatRequest struct {
	openai.ChatCompletionRequest
	Messages []ChatMessage `json:"messages"`
	// Reasoning is the unified control; it replaces reasoning_effort.
	Reasoning *contract.OpenRouterReasoning `json:"reasoning,omitempty"`
	Provider  *Routing                      `json:"provider,omitempty"`
	// SessionID groups a conversation's requests for gateway-side caching.
	SessionID string `json:"session_id,omitempty"`
	// CacheControl asks for automatic prompt caching on models that cache
	// only on request.
	CacheControl *CacheControl `json:"cache_control,omitempty"`
	Plugins      []Plugin      `json:"plugins,omitempty"`
}

// CacheControl is the gateway's top-level automatic-caching request: the
// upstream places one breakpoint on the last cacheable block and advances it
// as the conversation grows. Omitting a TTL takes the five-minute default.
type CacheControl struct {
	Type string `json:"type"`
}

// explicitCacheFamily is the model family that caches only when asked. The
// gateway's other families cache implicitly, and a control sent to them could
// opt a request into billed cache writes it would not otherwise make.
const explicitCacheFamily = "anthropic/"

// cacheControl is the caching request for a session's model, or nil when
// the request has no session or its model caches without being asked.
func cacheControl(req *contract.CompletionRequest) *CacheControl {
	if req.CacheSessionID == "" || !strings.HasPrefix(strings.TrimPrefix(req.Model, "~"), explicitCacheFamily) {
		return nil
	}
	return &CacheControl{Type: "ephemeral"}
}

// Plugin configures one of the gateway's request plugins.
type Plugin struct {
	ID      string `json:"id"`
	Enabled *bool  `json:"enabled,omitempty"`
}

// noCompression turns off the gateway's context compression, which it
// applies by default on endpoints of 8,192 tokens or fewer: it drops
// messages from the middle of a request too long for the endpoint, which the
// agent compacts itself, and can part a tool call from its result. With it
// off, such a request is rejected, and the agent answers the rejection by
// compacting the conversation.
func noCompression() []Plugin {
	off := false
	return []Plugin{{ID: "context-compression", Enabled: &off}}
}

// Streaming implements openai.ChatBody around the embedded body.
func (r *ChatRequest) Streaming(on bool) openai.ChatBody {
	body := *r
	body.ChatCompletionRequest = *r.ChatCompletionRequest.Streaming(on).(*openai.ChatCompletionRequest)
	return &body
}

// ChatMessage is a request message with the reasoning an earlier reply
// produced: plain text, or the structured details some models sign.
type ChatMessage struct {
	openai.ChatMessage
	Reasoning string `json:"reasoning,omitempty"`
	// Raw JSON distinguishes an explicit [] from an absent replay payload.
	ReasoningDetails json.RawMessage `json:"reasoning_details,omitempty"`
}

// ResponsesRequest is OpenRouter's Responses body: the OpenAI shape plus the
// gateway's extensions. Reasoning shadows OpenAI's control with the unified
// one.
type ResponsesRequest struct {
	openai.ResponsesRequest
	Reasoning    *ResponsesReasoning `json:"reasoning,omitempty"`
	Provider     *Routing            `json:"provider,omitempty"`
	SessionID    string              `json:"session_id,omitempty"`
	CacheControl *CacheControl       `json:"cache_control,omitempty"`
	Plugins      []Plugin            `json:"plugins,omitempty"`
}

// Streaming implements openai.ResponsesBody around the embedded body.
func (r *ResponsesRequest) Streaming(on bool) openai.ResponsesBody {
	body := *r
	body.ResponsesRequest = *r.ResponsesRequest.Streaming(on).(*openai.ResponsesRequest)
	return &body
}

// ResponsesReasoning is the unified control as the Responses endpoint takes
// it, with the summary request OpenAI's dialect adds.
type ResponsesReasoning struct {
	contract.OpenRouterReasoning
	Summary string `json:"summary,omitempty"` // "auto"
}
