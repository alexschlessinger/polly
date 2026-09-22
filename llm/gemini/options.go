package gemini

import (
	"net/http"

	"github.com/alexschlessinger/pollytool/llm/internal/httpx"
)

// ClientOption configures the HTTP client used by this package.
type ClientOption = httpx.ClientOption

// WithHTTPClient supplies a caller-owned client, reused without mutation.
// Nil uses a default client. Do not mutate the client while requests run.
func WithHTTPClient(client *http.Client) ClientOption { return httpx.WithHTTPClient(client) }
