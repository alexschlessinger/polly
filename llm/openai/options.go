package openai

import (
	"net/http"

	"github.com/alexschlessinger/pollytool/llm/internal/httpx"
)

// ClientOption configures the HTTP client used by this package.
type ClientOption = httpx.ClientOption

// WithHTTPClient supplies a caller-owned client, reused without mutation.
// Nil uses a default client. Do not mutate the client while requests run.
func WithHTTPClient(client *http.Client) ClientOption { return httpx.WithHTTPClient(client) }

// WithTerminalError marks API errors that no retry can fix (a retryable
// status carrying, say, an exhausted usage allowance): the client returns
// them at once instead of waiting out the server's Retry-After hint.
func WithTerminalError(fn func(error) bool) ClientOption { return httpx.WithTerminalError(fn) }
