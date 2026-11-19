package openrouter

import "net/http"

// attributedClient leaves the shared client untouched: only OpenRouter
// completions should carry its app-attribution headers.
func attributedClient(client *http.Client) *http.Client {
	var c http.Client
	if client != nil {
		c = *client
	}
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.Transport = attributionTransport{base}
	return &c
}

type attributionTransport struct{ base http.RoundTripper }

func (t attributionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("HTTP-Referer", "https://github.com/alexschlessinger/polly")
	r.Header.Set("X-OpenRouter-Title", "Polly")
	return t.base.RoundTrip(r)
}
