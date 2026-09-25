package codex

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

// DefaultBaseURL is the Codex backend; its Responses endpoint is
// {base}/responses.
const DefaultBaseURL = "https://chatgpt.com/backend-api/codex"

// Option configures a Provider.
type Option func(*Provider)

// WithHTTPClient supplies a caller-owned client, reused without mutation.
func WithHTTPClient(client *http.Client) Option {
	return func(p *Provider) { p.httpClient = client }
}

var _ contract.LLM = (*Provider)(nil)

// Provider sends completions to the Codex backend on a signed-in ChatGPT
// account. It speaks the stateless Responses dialect through llm/openai's
// transport, signs each request with the account's current token, and
// refreshes the sign-in once when the backend rejects it.
type Provider struct {
	login      contract.Login
	baseURL    string
	httpClient *http.Client
}

// NewProvider returns a provider for the backend at baseURL, or the public
// one when empty, drawing its credential from login.
func NewProvider(login contract.Login, baseURL string, opts ...Option) *Provider {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		trimmed = DefaultBaseURL
	}
	p := &Provider{login: login, baseURL: trimmed}
	for _, opt := range opts {
		opt(p)
	}
	if p.httpClient == nil {
		p.httpClient = &http.Client{}
	}
	return p
}

// ChatCompletionStream implements the event-based streaming interface.
func (p *Provider) ChatCompletionStream(ctx context.Context, req *contract.CompletionRequest, processor contract.EventStreamProcessor) <-chan *messages.StreamEvent {
	return contract.RunStream(ctx, req.Timeout, req.Deadline, processor, newAdapter(req.Model), func(ctx context.Context, core *streaming.StreamingCore) {
		if err := p.stream(ctx, req, core); err != nil {
			core.EmitError(err)
		}
	})
}

// stream sends one completion. The backend answers only streams, so a
// buffered request streams too and the consumer collects it. A missing
// sign-in fails here, before any request is built.
func (p *Provider) stream(ctx context.Context, req *contract.CompletionRequest, core *streaming.StreamingCore) error {
	if p.login == nil {
		return errors.New("codex: no sign-in is configured")
	}
	if _, err := p.login.Credential(ctx); err != nil {
		return err
	}
	call := &callState{onUsage: func(u Usage) { core.GetState().SetMetadata(usageStateKey, u) }}
	client := openai.NewClient("", p.baseURL, openai.WithHTTPClient(p.signing(req, call)), openai.WithTerminalError(isTerminal))
	slog.Debug("codex_responses_started", "base_url", p.baseURL, "model", req.Model, "fast", req.Fast)
	if err := openai.StreamResponses(ctx, client, buildRequest(req), core); err != nil {
		return describe(err, call)
	}
	return nil
}

// signing returns a copy of the provider's client whose transport signs
// requests for this call; the provider's client is never mutated.
func (p *Provider) signing(req *contract.CompletionRequest, call *callState) *http.Client {
	client := *p.httpClient
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	client.Transport = &transport{base: base, login: p.login, sessionID: req.CacheSessionID, routingHint: routingHint(req.Model, req.Fast), call: call}
	return &client
}
