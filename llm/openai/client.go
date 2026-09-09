package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/internal/httpx"
)

const defaultBaseURL = "https://api.openai.com/v1/"

// Client talks to the OpenAI API or any OpenAI-compatible server.
type Client struct {
	apiKey     string
	baseURL    string // normalized to end with "/"
	httpClient *http.Client
	maxRetries int
}

// NewClient returns a client for baseURL, or the public OpenAI endpoint when
// baseURL is empty. Request lifetimes are governed by the caller's context;
// the client itself sets no timeout so streams can run long. Like the
// official SDK, transient failures (408/409/429/5xx and transport errors)
// are retried twice with backoff, honoring Retry-After, and requests are
// unauthenticated when the key is empty (keyless compatible servers).
func NewClient(apiKey, baseURL string) *Client {
	return &Client{
		apiKey:     apiKey,
		baseURL:    normalizeBaseURL(baseURL),
		httpClient: &http.Client{},
		maxRetries: httpx.DefaultMaxRetries,
	}
}

// normalizeBaseURL applies the official SDK's join rule: endpoint paths
// resolve relative to the base, which requires the base path to end in "/"
// ("https://host/api/v1" + "chat/completions" must not eat the "v1").
func normalizeBaseURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return defaultBaseURL
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Scheme == "" {
		if !strings.HasSuffix(trimmed, "/") {
			trimmed += "/"
		}
		return trimmed
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	return u.String()
}

// APIError is the error object of the standard envelope {"error":{...}},
// returned for non-2xx responses. Compatible servers fill it loosely; absent
// fields stay empty.
type APIError struct {
	StatusCode int        `json:"-"`
	Type       string     `json:"type,omitempty"`
	Code       FlexString `json:"code,omitempty"`
	Message    string     `json:"message,omitempty"`
}

func (e *APIError) Error() string {
	label := e.Type
	if label == "" {
		label = string(e.Code)
	}
	return fmt.Sprintf("openai api error %d (%s): %s", e.StatusCode, label, e.Message)
}

// CreateChatCompletion performs a non-streaming chat completion.
func (c *Client) CreateChatCompletion(ctx context.Context, req *ChatCompletionRequest) (*ChatCompletion, error) {
	r := *req
	r.Stream = false
	r.StreamOptions = nil
	resp, err := c.post(ctx, "chat/completions", &r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	out := &ChatCompletion{}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return nil, fmt.Errorf("openai: decoding response: %w", err)
	}
	return out, nil
}

// StreamChatCompletion performs a streaming chat completion, requesting
// usage on the final chunk. A data payload carrying an error envelope — how
// some compatible servers report mid-stream failures — is surfaced as an
// error rather than silently dropped.
func (c *Client) StreamChatCompletion(ctx context.Context, req *ChatCompletionRequest) iter.Seq2[*ChatCompletionChunk, error] {
	return func(yield func(*ChatCompletionChunk, error) bool) {
		r := *req
		r.Stream = true
		r.StreamOptions = &StreamOptions{IncludeUsage: true}
		for data, err := range c.streamData(ctx, "chat/completions", &r) {
			if err != nil {
				yield(nil, err)
				return
			}
			var envelope struct {
				ChatCompletionChunk
				Error json.RawMessage `json:"error"`
			}
			decodeErr := json.Unmarshal(data, &envelope)
			if len(envelope.Error) > 0 {
				var apiErr *APIError
				if json.Unmarshal(envelope.Error, &apiErr) == nil && apiErr != nil {
					yield(nil, apiErr)
					return
				}
			}
			if err := decodeErr; err != nil {
				if !yield(nil, fmt.Errorf("openai: invalid stream chunk: %w", err)) {
					return
				}
				continue
			}
			if !yield(&envelope.ChatCompletionChunk, nil) {
				return
			}
		}
	}
}

// CreateResponse performs a non-streaming Responses API call.
func (c *Client) CreateResponse(ctx context.Context, req *ResponsesRequest) (*Response, error) {
	r := *req
	r.Stream = false
	resp, err := c.post(ctx, "responses", &r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	out := &Response{}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return nil, fmt.Errorf("openai: decoding response: %w", err)
	}
	return out, nil
}

// StreamResponse performs a streaming Responses API call. All events pass
// through, including type "error" — the Responses API reports model-side
// failures as ordinary events, and the consumer decides how to surface them.
func (c *Client) StreamResponse(ctx context.Context, req *ResponsesRequest) iter.Seq2[*ResponseStreamEvent, error] {
	return func(yield func(*ResponseStreamEvent, error) bool) {
		r := *req
		r.Stream = true
		for data, err := range c.streamData(ctx, "responses", &r) {
			if err != nil {
				yield(nil, err)
				return
			}
			event := &ResponseStreamEvent{}
			if err := json.Unmarshal(data, event); err != nil {
				if !yield(nil, fmt.Errorf("openai: invalid stream event: %w", err)) {
					return
				}
				continue
			}
			if !yield(event, nil) {
				return
			}
		}
	}
}

// CreateEmbeddings embeds all inputs in one call.
func (c *Client) CreateEmbeddings(ctx context.Context, req *EmbeddingRequest) (*EmbeddingResponse, error) {
	resp, err := c.post(ctx, "embeddings", req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	out := &EmbeddingResponse{}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return nil, fmt.Errorf("openai: decoding response: %w", err)
	}
	return out, nil
}

// streamData POSTs the body and yields each SSE data payload until the
// stream ends or a "[DONE]" sentinel arrives. Comment lines — OpenRouter's
// ": OPENROUTER PROCESSING" keep-alives among them — are skipped by the
// scanner, and payload type fields are authoritative over event: lines.
func (c *Client) streamData(ctx context.Context, path string, body any) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		resp, err := c.post(ctx, path, body)
		if err != nil {
			yield(nil, err)
			return
		}
		defer resp.Body.Close()

		for data, err := range httpx.ScanSSE(resp.Body, nil) {
			if err != nil {
				yield(nil, fmt.Errorf("openai: %w", err))
				return
			}
			if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
				return
			}
			if !yield(data, nil) {
				return
			}
		}
	}
}

// post sends the request with retries and returns the response with its body
// still open. Non-2xx statuses that survive the retry budget are drained and
// returned as *APIError.
func (c *Client) post(ctx context.Context, path string, body any) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai: encoding request: %w", err)
	}
	retrier := httpx.Retrier{
		Client: c.httpClient, MaxRetries: c.maxRetries, Prefix: "openai",
		ErrorFromResponse: func(resp *http.Response) error { return errorFromResponse(resp) },
	}
	return retrier.Do(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		return req, nil
	})
}

// errorFromResponse converts a non-2xx response into an *APIError, falling
// back to the raw body when it isn't the standard envelope — compatible
// servers return all sorts of shapes.
func errorFromResponse(resp *http.Response) *APIError {
	apiErr, body, ok := httpx.ReadError[APIError](resp)
	if !ok {
		return &APIError{StatusCode: resp.StatusCode, Type: resp.Status, Message: body}
	}
	apiErr.StatusCode = resp.StatusCode
	return apiErr
}
