package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL    = "https://api.openai.com/v1/"
	defaultMaxRetries = 2
)

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
		maxRetries: defaultMaxRetries,
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
// stream ends or a "[DONE]" sentinel arrives. event:/id: lines are ignored
// (payload type fields are authoritative), comment lines — OpenRouter's
// ": OPENROUTER PROCESSING" keep-alives among them — are skipped, and data
// segments accumulate across lines per the SSE spec.
func (c *Client) streamData(ctx context.Context, path string, body any) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		resp, err := c.post(ctx, path, body)
		if err != nil {
			yield(nil, err)
			return
		}
		defer resp.Body.Close()

		var data []byte
		flush := func() (keepGoing, done bool) {
			if len(data) == 0 {
				return true, false
			}
			payload := data
			data = nil
			if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
				return false, true
			}
			return yield(payload, nil), false
		}

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1024), 256<<20)
		for scanner.Scan() {
			line := scanner.Bytes()
			switch {
			case len(line) == 0:
				keepGoing, done := flush()
				if !keepGoing || done {
					return
				}
			case line[0] == ':': // SSE comment/keep-alive
			default:
				if rest, ok := bytes.CutPrefix(line, []byte("data:")); ok {
					rest = bytes.TrimPrefix(rest, []byte(" "))
					if len(data) > 0 {
						data = append(data, '\n')
					}
					data = append(data, rest...)
				}
			}
		}
		if err := scanner.Err(); err != nil {
			yield(nil, fmt.Errorf("openai: reading stream: %w", err))
			return
		}
		flush()
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

	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("openai: building request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("openai: request failed: %w", err)
			}
			lastErr = fmt.Errorf("openai: request failed: %w", err)
			if attempt >= c.maxRetries {
				return nil, lastErr
			}
			if err := sleepBeforeRetry(ctx, nil, attempt); err != nil {
				return nil, lastErr
			}
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}

		apiErr := errorFromResponse(resp)
		resp.Body.Close()
		if !retryableStatus(resp.StatusCode) || attempt >= c.maxRetries {
			return nil, apiErr
		}
		lastErr = apiErr
		if err := sleepBeforeRetry(ctx, resp, attempt); err != nil {
			return nil, lastErr
		}
	}
}

func retryableStatus(code int) bool {
	return code == http.StatusRequestTimeout ||
		code == http.StatusConflict ||
		code == http.StatusTooManyRequests ||
		code >= 500
}

// sleepBeforeRetry waits out the server's Retry-After(-Ms) hint when present,
// otherwise an exponential backoff (0.5s doubling, 8s cap). Returns early
// with the context's error if it is cancelled while waiting.
func sleepBeforeRetry(ctx context.Context, resp *http.Response, attempt int) error {
	delay := time.Duration(math.Min(8, 0.5*math.Pow(2, float64(attempt))) * float64(time.Second))
	if resp != nil {
		if ms := resp.Header.Get("Retry-After-Ms"); ms != "" {
			if v, err := strconv.Atoi(ms); err == nil && v >= 0 {
				delay = time.Duration(v) * time.Millisecond
			}
		} else if ra := resp.Header.Get("Retry-After"); ra != "" {
			if v, err := strconv.Atoi(ra); err == nil && v >= 0 {
				delay = time.Duration(v) * time.Second
			} else if at, err := http.ParseTime(ra); err == nil {
				delay = max(time.Until(at), 0)
			}
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type errorEnvelope struct {
	Error *APIError `json:"error"`
}

// errorFromResponse converts a non-2xx response into an *APIError, falling
// back to the raw body when it isn't the standard envelope — compatible
// servers return all sorts of shapes.
func errorFromResponse(resp *http.Response) *APIError {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return &APIError{StatusCode: resp.StatusCode, Type: resp.Status}
	}
	var envelope errorEnvelope
	if json.Unmarshal(body, &envelope) == nil && envelope.Error != nil {
		envelope.Error.StatusCode = resp.StatusCode
		return envelope.Error
	}
	return &APIError{StatusCode: resp.StatusCode, Type: resp.Status, Message: string(body)}
}
