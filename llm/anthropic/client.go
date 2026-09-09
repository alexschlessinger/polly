package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"net/url"

	"github.com/alexschlessinger/pollytool/llm/internal/httpx"
)

const (
	defaultBaseURL = "https://api.anthropic.com/v1"
	apiVersion     = "2023-06-01"
)

// Client talks to the Anthropic Messages API with an API key.
type Client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	maxRetries int
}

// NewClient returns a client for the public Anthropic endpoint. Request
// lifetimes are governed by the caller's context; the client itself sets no
// timeout so streams can run long. Like the official SDK, transient failures
// (408/409/429/5xx and transport errors) are retried twice with backoff,
// honoring Retry-After.
func NewClient(apiKey string) *Client {
	return &Client{
		apiKey:     apiKey,
		baseURL:    defaultBaseURL,
		httpClient: &http.Client{},
		maxRetries: httpx.DefaultMaxRetries,
	}
}

// APIError is the error object of the API's standard envelope
// {"type":"error","error":{...}}, returned for non-2xx responses and
// mid-stream error events.
type APIError struct {
	StatusCode int    `json:"-"`
	Type       string `json:"type,omitempty"`
	Message    string `json:"message,omitempty"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("anthropic api error %d (%s): %s", e.StatusCode, e.Type, e.Message)
}

// ModelInfo is the slice of GET /v1/models/{id} polly uses: the model's
// advertised context window arrives as max_input_tokens.
type ModelInfo struct {
	ID             string `json:"id"`
	MaxInputTokens int    `json:"max_input_tokens,omitempty"`
}

// GetModel fetches model metadata in a single best-effort attempt; callers
// treat failures as "window unknown" rather than retrying.
func (c *Client) GetModel(ctx context.Context, model string) (*ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models/"+url.PathEscape(model), nil)
	if err != nil {
		return nil, fmt.Errorf("anthropic: building request: %w", err)
	}
	req.Header.Set("anthropic-version", apiVersion)
	if c.apiKey != "" {
		req.Header.Set("x-api-key", c.apiKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errorFromResponse(resp)
	}
	out := &ModelInfo{}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return nil, fmt.Errorf("anthropic: decoding model info: %w", err)
	}
	return out, nil
}

// CreateMessage performs a non-streaming completion.
func (c *Client) CreateMessage(ctx context.Context, req *MessageRequest) (*Message, error) {
	r := *req
	r.Stream = false
	resp, err := c.post(ctx, &r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	out := &Message{}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return nil, fmt.Errorf("anthropic: decoding response: %w", err)
	}
	return out, nil
}

// CreateMessageStream performs a streaming completion over SSE. The iterator
// yields events until the stream ends or the caller breaks; a non-nil error
// yield reports a transport, protocol, or mid-stream API failure. Unknown
// event types are passed through for the consumer to ignore.
func (c *Client) CreateMessageStream(ctx context.Context, req *MessageRequest) iter.Seq2[*StreamEvent, error] {
	return func(yield func(*StreamEvent, error) bool) {
		r := *req
		r.Stream = true
		resp, err := c.post(ctx, &r)
		if err != nil {
			yield(nil, err)
			return
		}
		defer resp.Body.Close()

		// event: lines are redundant with the payload's type field and are
		// ignored by the scanner.
		for data, err := range httpx.ScanSSE(resp.Body, nil) {
			if err != nil {
				yield(nil, fmt.Errorf("anthropic: %w", err))
				return
			}
			event := &StreamEvent{}
			if err := json.Unmarshal(data, event); err != nil {
				yield(nil, fmt.Errorf("anthropic: invalid stream event: %w", err))
				return
			}
			if event.Type == EventError {
				apiErr := event.Error
				if apiErr == nil {
					apiErr = &APIError{Type: "error"}
				}
				apiErr.StatusCode = resp.StatusCode
				yield(nil, apiErr)
				return
			}
			if !yield(event, nil) {
				return
			}
		}
	}
}

// post sends the request with retries and returns the response with its body
// still open. Non-2xx statuses that survive the retry budget are drained and
// returned as *APIError.
func (c *Client) post(ctx context.Context, body *MessageRequest) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: encoding request: %w", err)
	}
	retrier := httpx.Retrier{
		Client: c.httpClient, MaxRetries: c.maxRetries, Prefix: "anthropic",
		ErrorFromResponse: func(resp *http.Response) error { return errorFromResponse(resp) },
	}
	return retrier.Do(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/messages", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("anthropic-version", apiVersion)
		if c.apiKey != "" {
			req.Header.Set("x-api-key", c.apiKey)
		}
		return req, nil
	})
}

// errorFromResponse converts a non-2xx response into an *APIError, falling
// back to the raw body when it isn't the standard envelope.
func errorFromResponse(resp *http.Response) *APIError {
	apiErr, body, ok := httpx.ReadError[APIError](resp)
	if !ok {
		return &APIError{StatusCode: resp.StatusCode, Type: resp.Status, Message: body}
	}
	apiErr.StatusCode = resp.StatusCode
	return apiErr
}
