package gemini

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
)

const defaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

// Client talks to the Gemini Developer API with an API key.
type Client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

// NewClient returns a client for the public Gemini API endpoint. Request
// lifetimes are governed by the caller's context; the client itself sets no
// timeout so streams can run long.
func NewClient(apiKey string) *Client {
	return &Client{
		apiKey:     apiKey,
		baseURL:    defaultBaseURL,
		httpClient: &http.Client{},
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

		scanner := bufio.NewScanner(resp.Body)
		// A chunk carrying inline data or thought signatures can be a very
		// long single line; allow up to 256MB like the official SDK.
		scanner.Buffer(make([]byte, 1024), 256<<20)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 || line[0] == ':' { // blank or SSE comment/keep-alive
				continue
			}
			data, ok := bytes.CutPrefix(line, []byte("data:"))
			if !ok {
				// The API signals mid-stream failures as a bare JSON error
				// envelope instead of a data: event.
				if !yield(nil, streamLineError(line)) {
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
		if err := scanner.Err(); err != nil {
			yield(nil, fmt.Errorf("gemini: reading stream: %w", err))
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

// post sends a JSON body and returns the response with its body still open.
// Non-2xx statuses are drained and returned as *APIError.
func (c *Client) post(ctx context.Context, path string, body any) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("gemini: encoding request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+path, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("gemini: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("x-goog-api-key", c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini: request failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, errorFromResponse(resp)
	}
	return resp, nil
}

// modelPath returns the URL resource path for a model name, tolerating names
// already given in resource form.
func modelPath(model string) string {
	if strings.HasPrefix(model, "models/") || strings.HasPrefix(model, "tunedModels/") {
		return model
	}
	return "models/" + model
}

type errorEnvelope struct {
	Error *APIError `json:"error"`
}

// errorFromResponse converts a non-2xx response into an *APIError, falling
// back to the raw body when it isn't the standard envelope.
func errorFromResponse(resp *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return &APIError{Code: resp.StatusCode, Status: resp.Status}
	}
	var envelope errorEnvelope
	if json.Unmarshal(body, &envelope) == nil && envelope.Error != nil {
		return envelope.Error
	}
	return &APIError{Code: resp.StatusCode, Status: resp.Status, Message: string(body)}
}

// streamLineError interprets a non-data stream line: either the API's error
// envelope or garbage worth surfacing verbatim.
func streamLineError(line []byte) error {
	var envelope errorEnvelope
	if json.Unmarshal(line, &envelope) == nil && envelope.Error != nil {
		return envelope.Error
	}
	const maxQuoted = 512
	if len(line) > maxQuoted {
		line = line[:maxQuoted]
	}
	return fmt.Errorf("gemini: invalid stream chunk: %q", line)
}
