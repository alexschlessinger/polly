package llm

import "net/http"

// ClientOption configures the provider router: the HTTP client requests go
// through, and the sign-ins that providers served on an account rather
// than an API key draw on.
type ClientOption func(*clientConfig)

type clientConfig struct {
	httpClient *http.Client
	logins     map[string]Login
}

// WithHTTPClient supplies a caller-owned client, reused without mutation.
// Nil uses a default client. Do not mutate the client while requests run.
func WithHTTPClient(client *http.Client) ClientOption {
	return func(c *clientConfig) { c.httpClient = client }
}

// resolveClientConfig applies the options once during construction.
func resolveClientConfig(opts ...ClientOption) clientConfig {
	var c clientConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&c)
		}
	}
	if c.httpClient == nil {
		c.httpClient = &http.Client{}
	}
	return c
}
