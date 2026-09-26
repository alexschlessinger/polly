package httpx

import "net/http"

// ClientOption configures the HTTP client used for provider requests.
type ClientOption func(*clientOptions)
type clientOptions struct {
	client   *http.Client
	terminal func(error) bool
}

// WithHTTPClient supplies a caller-owned client. It is reused without mutation;
// nil uses a default client. The caller must not mutate it while requests run.
func WithHTTPClient(client *http.Client) ClientOption {
	return func(o *clientOptions) { o.client = client }
}

// WithTerminalError marks converted non-2xx errors that no retry can fix,
// so the retrier returns them at once instead of waiting out the server's
// Retry-After hint.
func WithTerminalError(fn func(error) bool) ClientOption {
	return func(o *clientOptions) { o.terminal = fn }
}

// TerminalError resolves the terminal-error predicate, nil when none was
// given.
func TerminalError(opts ...ClientOption) func(error) bool {
	var o clientOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o.terminal
}

// HTTPClient resolves options once during construction.
func HTTPClient(opts ...ClientOption) *http.Client {
	var o clientOptions
	for _, opt := range opts {
		opt(&o)
	}
	if o.client != nil {
		return o.client
	}
	return &http.Client{}
}
