package httpx

import "net/http"

// ClientOption configures the HTTP client used for provider requests.
type ClientOption func(*clientOptions)
type clientOptions struct{ client *http.Client }

// WithHTTPClient supplies a caller-owned client. It is reused without mutation;
// nil uses a default client. The caller must not mutate it while requests run.
func WithHTTPClient(client *http.Client) ClientOption {
	return func(o *clientOptions) { o.client = client }
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
