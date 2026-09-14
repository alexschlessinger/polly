// Package openrouter talks to the OpenRouter gateway. OpenRouter speaks the
// OpenAI dialects, so the transport comes from llm/openai; this package adds
// the gateway's extensions — unified reasoning controls, reasoning replay,
// provider routing, session affinity — over either dialect, and reads its
// model catalog with per-route capabilities and reasoning policy.
package openrouter

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

// DefaultBaseURL is the public gateway.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

// API is the OpenAI dialect the gateway is spoken to in. Both carry the
// same extensions; reasoning replay is recorded per dialect, so a reply
// made over one is not replayed over the other.
type API string

const (
	// ChatCompletionsAPI is the default: chat/completions with reasoning
	// details on each assistant turn.
	ChatCompletionsAPI API = "chat_completions"
	// ResponsesAPI is the stateless responses endpoint, with reasoning
	// items passed back verbatim.
	ResponsesAPI API = "responses"
)

// Option configures a Provider.
type Option func(*Provider)

// WithAPI selects the dialect; ChatCompletionsAPI when not given.
func WithAPI(api API) Option {
	return func(p *Provider) { p.api = api }
}

var _ contract.LLM = (*Provider)(nil)

// Provider sends completions to one OpenRouter endpoint.
type Provider struct {
	client  *openai.Client
	baseURL string
	api     API
	// endpoint is the normalized gateway identity recorded on replies, so a
	// later request replays reasoning only to the gateway that produced it.
	endpoint string
}

// NewProvider returns a provider for baseURL, or the public gateway when
// empty.
func NewProvider(apiKey, baseURL string, opts ...Option) *Provider {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		trimmed = DefaultBaseURL
	}
	p := &Provider{client: openai.NewClient(apiKey, trimmed), baseURL: trimmed, api: ChatCompletionsAPI, endpoint: Endpoint(trimmed)}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// API reports the dialect the provider speaks.
func (p *Provider) API() API { return p.api }

// ChatCompletionStream implements the event-based streaming interface.
func (p *Provider) ChatCompletionStream(ctx context.Context, req *contract.CompletionRequest, processor contract.EventStreamProcessor) <-chan *messages.StreamEvent {
	var adapter streaming.ProviderAdapter = NewChatAdapter(p.endpoint, req.Model)
	send := p.streamChat
	if p.api == ResponsesAPI {
		adapter = NewResponsesAdapter(p.endpoint, req.Model)
		send = p.streamResponses
	}
	return contract.RunStream(ctx, req.Timeout, req.Deadline, processor, adapter, func(ctx context.Context, streamCore *streaming.StreamingCore) {
		if err := send(ctx, req, streamCore); err != nil {
			streamCore.EmitError(err)
		}
	})
}

// streamChat sends the request over Chat Completions with the gateway's
// extensions.
func (p *Provider) streamChat(ctx context.Context, req *contract.CompletionRequest, streamCore *streaming.StreamingCore) error {
	params := p.chatRequest(req)
	isStreaming := req.IsStreaming()
	slog.Debug("openrouter_chat_completion_started", "stream", isStreaming, "base_url", p.baseURL)
	if isStreaming {
		return openai.StreamChat(ctx, p.client, params, streamCore)
	}
	return openai.CompleteChat(ctx, p.client, params, streamCore)
}

// streamResponses sends the request over the Responses API with the same
// extensions.
func (p *Provider) streamResponses(ctx context.Context, req *contract.CompletionRequest, streamCore *streaming.StreamingCore) error {
	params := p.responsesRequest(req)
	isStreaming := req.IsStreaming()
	slog.Debug("openrouter_responses_started", "stream", isStreaming, "base_url", p.baseURL)
	if isStreaming {
		return openai.StreamResponses(ctx, p.client, params, streamCore)
	}
	return openai.CompleteResponses(ctx, p.client, params, streamCore)
}

// reasoning is the unified control for the request. Preparation recorded
// the model's capabilities on the request and reported any adaptation; the
// wire form follows from the same facts.
func reasoning(req *contract.CompletionRequest) *contract.OpenRouterReasoning {
	return contract.ResolveOpenRouterRequestThinking(req.ThinkingEffort, req.KnownCapabilities()).Request
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
		Reasoning:             reasoning(req),
		SessionID:             req.CacheSessionID,
		Provider:              routing(req.ModelHost),
	}
	for i, msg := range base.Messages {
		params.Messages[i] = ChatMessage{ChatMessage: msg}
		params.Messages[i].Reasoning, params.Messages[i].ReasoningDetails = ChatReplay(req.Messages[i], p.endpoint, req.Model)
	}
	params.ChatCompletionRequest.Messages = nil
	return params
}

// responsesRequest builds the Responses body: the OpenAI shape with the
// gateway's reasoning items replayed verbatim ahead of each assistant turn,
// the unified reasoning control in place of OpenAI's, session affinity, and
// any pinned route. The OpenAI builder already asks for encrypted reasoning
// and disables server-side state, which the gateway requires.
func (p *Provider) responsesRequest(req *contract.CompletionRequest) *ResponsesRequest {
	base := openai.BuildResponsesRequestWith(req, p.responsesReplay)
	base.Reasoning = nil
	params := &ResponsesRequest{
		ResponsesRequest: *base,
		SessionID:        req.CacheSessionID,
		Provider:         routing(req.ModelHost),
	}
	if control := reasoning(req); control != nil {
		params.Reasoning = &ResponsesReasoning{OpenRouterReasoning: *control}
		if control.Effort != "" || control.MaxTokens > 0 {
			params.Reasoning.Summary = "auto"
		}
	}
	return params
}

// responsesReplay returns the reasoning items a newly attributed reply
// recorded, to be passed back untouched ahead of that turn.
func (p *Provider) responsesReplay(msg messages.ChatMessage, model string) []openai.ResponseInputItem {
	var items []json.RawMessage
	if err := json.Unmarshal(ResponsesReplay(msg, p.endpoint, model), &items); err != nil {
		return nil
	}
	out := make([]openai.ResponseInputItem, 0, len(items))
	for _, item := range items {
		out = append(out, openai.ResponseInputItem{Raw: item})
	}
	return out
}

// routing pins the request to one upstream host, or nil for automatic routing.
func routing(host string) *Routing {
	if host == "" {
		return nil
	}
	return &Routing{Only: []string{host}, AllowFallbacks: false}
}
