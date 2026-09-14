// Package openrouter talks to the OpenRouter gateway. OpenRouter speaks the
// OpenAI dialects, so the transport comes from llm/openai; this package adds
// the gateway's extensions — unified reasoning controls, reasoning replay,
// provider routing, session affinity — and reads its model catalog with
// per-route capabilities and reasoning policy.
package openrouter

import (
	"context"
	"log/slog"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

// DefaultBaseURL is the public gateway.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

var _ contract.LLM = (*Provider)(nil)

// Provider sends completions to one OpenRouter endpoint over the Chat
// Completions dialect.
type Provider struct {
	client  *openai.Client
	baseURL string
	// endpoint is the normalized gateway identity recorded on replies, so a
	// later request replays reasoning only to the gateway that produced it.
	endpoint string
}

// NewProvider returns a provider for baseURL, or the public gateway when
// empty.
func NewProvider(apiKey, baseURL string) *Provider {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		trimmed = DefaultBaseURL
	}
	return &Provider{client: openai.NewClient(apiKey, trimmed), baseURL: trimmed, endpoint: Endpoint(trimmed)}
}

// ChatCompletionStream implements the event-based streaming interface.
func (p *Provider) ChatCompletionStream(ctx context.Context, req *contract.CompletionRequest, processor contract.EventStreamProcessor) <-chan *messages.StreamEvent {
	return contract.RunStream(ctx, req.Timeout, req.Deadline, processor, NewChatAdapter(p.endpoint, req.Model), func(ctx context.Context, streamCore *streaming.StreamingCore) {
		if err := p.streamChat(ctx, req, streamCore); err != nil {
			streamCore.EmitError(err)
		}
	})
}

// streamChat sends the request over Chat Completions with the gateway's
// extensions. Preparation recorded the model's capabilities on the request
// and reported any adaptation; the reasoning control follows from the same
// facts.
func (p *Provider) streamChat(ctx context.Context, req *contract.CompletionRequest, streamCore *streaming.StreamingCore) error {
	params := p.chatRequest(req)
	isStreaming := req.IsStreaming()
	slog.Debug("openrouter_chat_completion_started", "stream", isStreaming, "base_url", p.baseURL)
	if isStreaming {
		return openai.StreamChat(ctx, p.client, params, streamCore)
	}
	return openai.CompleteChat(ctx, p.client, params, streamCore)
}

// chatRequest builds the Chat Completions body: the OpenAI shape with the
// unified reasoning control in place of reasoning_effort, each assistant
// turn's attributed reasoning replayed, session affinity, and any pinned
// route.
func (p *Provider) chatRequest(req *contract.CompletionRequest) *ChatRequest {
	base := openai.BuildChatCompletionRequest(req)
	base.ReasoningEffort = ""
	params := &ChatRequest{
		ChatCompletionRequest: *base,
		Messages:              make([]ChatMessage, len(base.Messages)),
		Reasoning:             contract.ResolveOpenRouterRequestThinking(req.ThinkingEffort, req.KnownCapabilities()).Request,
		SessionID:             req.CacheSessionID,
		Provider:              routing(req.ModelHost),
	}
	for i, msg := range base.Messages {
		params.Messages[i] = ChatMessage{ChatMessage: msg}
		params.Messages[i].Reasoning, params.Messages[i].ReasoningDetails = Replay(req.Messages[i], p.endpoint, req.Model)
	}
	params.ChatCompletionRequest.Messages = nil
	return params
}

// routing pins the request to one upstream host, or nil for automatic routing.
func routing(host string) *Routing {
	if host == "" {
		return nil
	}
	return &Routing{Only: []string{host}, AllowFallbacks: false}
}
