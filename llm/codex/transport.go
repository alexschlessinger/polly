package codex

import (
	"bytes"
	"io"
	"net/http"
	"sync"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

// maxErrorBody bounds how much of a failing response is read for its
// envelope.
const maxErrorBody = 1 << 20

// transport signs requests with the account's current token and account
// id, retries once with a refreshed token when the backend rejects it, and
// reads what each response says about the account's usage. It sits below
// llm/openai's retrier, which sees only the outcome.
type transport struct {
	base      http.RoundTripper
	login     contract.Login
	sessionID string
	call      *callState
}

// callState collects what one completion learns from its responses: the
// usage meters a success carried, the terminal refusal a failure carried,
// and why a refresh did not rescue a rejected token.
type callState struct {
	mu         sync.Mutex
	usage      *Usage
	terminal   *UsageError
	refreshErr error
	onUsage    func(Usage)
}

func (c *callState) setUsage(u Usage) {
	c.mu.Lock()
	c.usage = &u
	c.mu.Unlock()
	if c.onUsage != nil {
		c.onUsage(u)
	}
}

func (c *callState) setTerminal(e *UsageError) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.terminal = e
}

func (c *callState) setRefreshError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshErr = err
}

func (c *callState) terminalError() *UsageError {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.terminal
}

func (c *callState) refreshError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refreshErr
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	cred, err := t.login.Credential(req.Context())
	if err != nil {
		return nil, err
	}
	resp, err := t.send(req, cred, false)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && req.GetBody != nil {
		body := drain(resp)
		fresh, err := t.login.Refresh(req.Context(), cred.AccessToken)
		if err != nil {
			// The rejection stands; the reason the refresh could not
			// mend it is what the caller should hear.
			t.call.setRefreshError(err)
			resp.Body = io.NopCloser(bytes.NewReader(body))
			return resp, nil
		}
		if resp, err = t.send(req, fresh, true); err != nil {
			return nil, err
		}
	}
	t.observe(resp)
	return resp, nil
}

// send issues one signed attempt. RoundTrippers must not mutate the
// caller's request, so the attempt goes out on a clone; a resend takes a
// fresh body from GetBody because the first attempt consumed it.
func (t *transport) send(req *http.Request, cred contract.Credential, resend bool) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if resend {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		clone.Body = body
	}
	setHeaders(clone.Header, cred, t.sessionID, "text/event-stream")
	return t.base.RoundTrip(clone)
}

// observe reads the usage meters off a success, or the refusal out of a
// failure, leaving the body as it was for the retrier.
func (t *transport) observe(resp *http.Response) {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if usage, ok := parseUsage(resp.Header); ok {
			t.call.setUsage(usage)
		}
		return
	}
	body := drain(resp)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if refusal := parseUsageError(body); refusal != nil {
		t.call.setTerminal(refusal)
	}
}

// drain reads and closes a response body, bounded.
func drain(resp *http.Response) []byte {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	resp.Body.Close()
	return body
}
