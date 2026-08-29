package anthropic

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
	"time"
)

const (
	defaultBaseURL    = "https://api.anthropic.com/v1"
	apiVersion        = "2023-06-01"
	defaultMaxRetries = 2
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
		maxRetries: defaultMaxRetries,
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

		// SSE events end on a blank line; data: segments accumulate across
		// lines. event: lines are redundant with the payload's type field
		// and are ignored.
		var data []byte
		flush := func() bool {
			if len(data) == 0 {
				return true
			}
			event := &StreamEvent{}
			err := json.Unmarshal(data, event)
			data = nil
			if err != nil {
				return yield(nil, fmt.Errorf("anthropic: invalid stream event: %w", err))
			}
			if event.Type == EventError {
				apiErr := event.Error
				if apiErr == nil {
					apiErr = &APIError{Type: "error"}
				}
				apiErr.StatusCode = resp.StatusCode
				return yield(nil, apiErr)
			}
			return yield(event, nil)
		}

		scanner := bufio.NewScanner(resp.Body)
		// Thinking deltas and tool-input JSON can make long lines; allow far
		// more than the official SDK's 32MB cap costs nothing.
		scanner.Buffer(make([]byte, 1024), 256<<20)
		for scanner.Scan() {
			line := scanner.Bytes()
			switch {
			case len(line) == 0:
				if !flush() {
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
				// Other fields (event:, id:, retry:) carry nothing we need.
			}
		}
		if err := scanner.Err(); err != nil {
			yield(nil, fmt.Errorf("anthropic: reading stream: %w", err))
			return
		}
		flush()
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

	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/messages", bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("anthropic: building request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("anthropic-version", apiVersion)
		if c.apiKey != "" {
			req.Header.Set("x-api-key", c.apiKey)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("anthropic: request failed: %w", err)
			}
			lastErr = fmt.Errorf("anthropic: request failed: %w", err)
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
// back to the raw body when it isn't the standard envelope.
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
