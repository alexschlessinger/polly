package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/internal/httpx"
)

// Client talks to an Ollama server's native API.
type Client struct {
	base       *url.URL
	httpClient *http.Client
}

// NewClient returns a client for the given server. A nil httpClient uses
// http.DefaultClient; polly passes one with a bearer-auth transport when an
// API key is configured.
func NewClient(base *url.URL, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{base: base, httpClient: httpClient}
}

// StatusError is a non-2xx response from the server.
type StatusError struct {
	StatusCode   int
	Status       string
	ErrorMessage string
}

func (e StatusError) Error() string {
	switch {
	case e.Status != "" && e.ErrorMessage != "":
		return fmt.Sprintf("%s: %s", e.Status, e.ErrorMessage)
	case e.Status != "":
		return e.Status
	default:
		return e.ErrorMessage
	}
}

// Retryable reports temporary HTTP failures. Mid-stream errors with a 2xx
// status have no structured retry classification and remain terminal.
func (e StatusError) Retryable() bool {
	return e.StatusCode == 408 || e.StatusCode == 409 || e.StatusCode == 429 || e.StatusCode >= 500 && e.StatusCode < 600
}

// Chat posts the request and invokes fn for each response line: many
// incremental chunks when streaming, a single complete response otherwise.
// HTTP failures use the context's shared retry budget when present; standalone
// calls retain their no-retry behavior. A non-nil error from fn stops the stream
// and is returned.
func (c *Client) Chat(ctx context.Context, req *ChatRequest, fn func(ChatResponse) error) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("ollama: encoding request: %w", err)
	}

	endpoint := c.base.JoinPath("/api/chat").String()
	retrier := httpx.Retrier{Client: c.httpClient, MaxRetries: 0, Prefix: "ollama", ErrorFromResponse: errorFromResponse}
	resp, err := retrier.Do(ctx, func() (*http.Request, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		return httpReq, nil
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	// Match the official client's 8MB line allowance.
	scanner.Buffer(make([]byte, 1024), 8<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		// The server reports failures mid-stream as an error line.
		var envelope struct {
			ChatResponse
			Error json.RawMessage `json:"error"`
		}
		decodeErr := json.Unmarshal(line, &envelope)
		if len(envelope.Error) > 0 {
			var message string
			if json.Unmarshal(envelope.Error, &message) == nil && message != "" {
				return StatusError{StatusCode: resp.StatusCode, ErrorMessage: message}
			}
		}
		if err := decodeErr; err != nil {
			return fmt.Errorf("ollama: invalid response line: %w", err)
		}
		if err := fn(envelope.ChatResponse); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("ollama: reading response: %w", err)
	}
	return nil
}

// errorFromResponse converts a non-2xx response into a StatusError, using the
// standard {"error": "..."} body when present.
func errorFromResponse(resp *http.Response) error {
	out := StatusError{StatusCode: resp.StatusCode, Status: resp.Status}
	message, body, ok := httpx.ReadError[string](resp)
	if ok && *message != "" {
		out.ErrorMessage = *message
	} else {
		out.ErrorMessage = strings.TrimSpace(body)
	}
	return out
}
