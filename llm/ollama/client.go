package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
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
	ErrorMessage string `json:"error"`
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

// Chat posts the request and invokes fn for each response line: many
// incremental chunks when streaming, a single complete response otherwise.
// A non-nil error from fn stops the stream and is returned.
func (c *Client) Chat(ctx context.Context, req *ChatRequest, fn func(ChatResponse) error) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("ollama: encoding request: %w", err)
	}

	endpoint := c.base.JoinPath("/api/chat").String()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("ollama: building request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("ollama: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errorFromResponse(resp)
	}

	scanner := bufio.NewScanner(resp.Body)
	// Match the official client's 8MB line allowance.
	scanner.Buffer(make([]byte, 1024), 8<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		// The server reports failures mid-stream as an error line.
		var probe struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(line, &probe) == nil && probe.Error != "" {
			return StatusError{StatusCode: resp.StatusCode, ErrorMessage: probe.Error}
		}
		var chunk ChatResponse
		if err := json.Unmarshal(line, &chunk); err != nil {
			return fmt.Errorf("ollama: invalid response line: %w", err)
		}
		if err := fn(chunk); err != nil {
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return out
	}
	if json.Unmarshal(body, &out) != nil || out.ErrorMessage == "" {
		out.ErrorMessage = strings.TrimSpace(string(body))
	}
	return out
}
