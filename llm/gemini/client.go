package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/internal/httpx"
)

const defaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

// Client talks to the Gemini Developer API with an API key.
type Client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	maxRetries int
}

// NewClient returns a client for the public Gemini API endpoint. Request
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

// APIError is the error object of the API's standard error envelope
// {"error": {...}}, returned for non-2xx responses and mid-stream failures.
type APIError struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Status  string `json:"status,omitempty"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gemini api error %d (%s): %s", e.Code, e.Status, e.Message)
}

// GenerateContent performs a non-streaming completion.
func (c *Client) GenerateContent(ctx context.Context, model string, req *GenerateContentRequest) (*GenerateContentResponse, error) {
	resp, err := c.post(ctx, modelPath(model)+":generateContent", req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	out := &GenerateContentResponse{}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return nil, fmt.Errorf("gemini: decoding response: %w", err)
	}
	return out, nil
}

// GenerateContentStream performs a streaming completion over SSE. The
// iterator yields chunks until the stream ends or the caller breaks; a non-nil
// error yield reports a transport, protocol, or mid-stream API failure.
func (c *Client) GenerateContentStream(ctx context.Context, model string, req *GenerateContentRequest) iter.Seq2[*GenerateContentResponse, error] {
	return func(yield func(*GenerateContentResponse, error) bool) {
		resp, err := c.post(ctx, modelPath(model)+":streamGenerateContent?alt=sse", req)
		if err != nil {
			yield(nil, err)
			return
		}
		defer resp.Body.Close()

		// The API signals mid-stream failures as a bare JSON error envelope
		// instead of a data: event; the scanner hands such lines to
		// streamLineError.
		for data, err := range httpx.ScanSSE(resp.Body, streamLineError) {
			if err != nil {
				if !yield(nil, fmt.Errorf("gemini: %w", err)) {
					return
				}
				continue
			}
			chunk := &GenerateContentResponse{}
			if err := json.Unmarshal(data, chunk); err != nil {
				if !yield(nil, fmt.Errorf("gemini: invalid stream chunk: %w", err)) {
					return
				}
				continue
			}
			if !yield(chunk, nil) {
				return
			}
		}
	}
}

// BatchEmbedContents embeds all requests in one call. Entries with an empty
// Model get the endpoint model in the "models/<name>" form the API requires
// on every entry.
func (c *Client) BatchEmbedContents(ctx context.Context, model string, requests []*EmbedContentRequest) (*BatchEmbedContentsResponse, error) {
	name := modelPath(model)
	for _, r := range requests {
		if r.Model == "" {
			r.Model = name
		}
	}
	resp, err := c.post(ctx, name+":batchEmbedContents", &BatchEmbedContentsRequest{Requests: requests})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	out := &BatchEmbedContentsResponse{}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return nil, fmt.Errorf("gemini: decoding response: %w", err)
	}
	return out, nil
}

// post sends a JSON body with retries and returns the response with its body
// still open. Non-2xx statuses that survive the retry budget are drained and
// returned as *APIError.
func (c *Client) post(ctx context.Context, path string, body any) (*http.Response, error) {
	var payload []byte
	var err error
	if req, ok := body.(*GenerateContentRequest); ok && req != nil {
		payload, err = req.MarshalJSON()
	} else {
		payload, err = json.Marshal(body)
	}
	if err != nil {
		return nil, fmt.Errorf("gemini: encoding request: %w", err)
	}
	retrier := httpx.Retrier{Client: c.httpClient, MaxRetries: c.maxRetries, Prefix: "gemini", ErrorFromResponse: errorFromResponse}
	return retrier.Do(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+path, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			req.Header.Set("x-goog-api-key", c.apiKey)
		}
		return req, nil
	})
}

// ModelInfo is the slice of GET /v1beta/models/{model} polly uses: the
// model's advertised context window arrives as inputTokenLimit.
type ModelInfo struct {
	Name            string `json:"name"`
	InputTokenLimit int    `json:"inputTokenLimit,omitempty"`
}

// GetModel fetches model metadata in a single best-effort attempt; callers
// treat failures as "window unknown" rather than retrying.
func (c *Client) GetModel(ctx context.Context, model string) (*ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/"+modelPath(model), nil)
	if err != nil {
		return nil, fmt.Errorf("gemini: building request: %w", err)
	}
	if c.apiKey != "" {
		req.Header.Set("x-goog-api-key", c.apiKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errorFromResponse(resp)
	}
	out := &ModelInfo{}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return nil, fmt.Errorf("gemini: decoding model info: %w", err)
	}
	return out, nil
}

// modelPath returns the URL resource path for a model name, tolerating names
// already given in resource form.
func modelPath(model string) string {
	if strings.HasPrefix(model, "models/") || strings.HasPrefix(model, "tunedModels/") {
		return model
	}
	return "models/" + model
}

// errorFromResponse converts a non-2xx response into an *APIError, falling
// back to the raw body when it isn't the standard envelope.
func errorFromResponse(resp *http.Response) error {
	apiErr, body, ok := httpx.ReadError[APIError](resp)
	if !ok {
		return &APIError{Code: resp.StatusCode, Status: resp.Status, Message: body}
	}
	return apiErr
}

// streamLineError interprets a non-data stream line: either the API's error
// envelope or garbage worth surfacing verbatim.
func streamLineError(line []byte) error {
	if apiErr, ok := httpx.ParseEnvelope[APIError](line); ok {
		return apiErr
	}
	const maxQuoted = 512
	if len(line) > maxQuoted {
		line = line[:maxQuoted]
	}
	return fmt.Errorf("gemini: invalid stream chunk: %q", line)
}
