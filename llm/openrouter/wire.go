package openrouter

import (
	"encoding/json"

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
