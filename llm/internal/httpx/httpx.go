// Package httpx holds the HTTP plumbing every provider client shares: a
// retrying POST with the official SDKs' backoff policy, bounded error-body
// decoding, and a server-sent-events scanner. Each provider keeps its own
// request shapes, headers, and error type; only the transport rules live
// here, so a fix to one lands for all of them.
package httpx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"math"
	"net/http"
	"strconv"
	"time"
)

// DefaultMaxRetries is how many times a transient failure is retried, like
// the official SDKs.
const DefaultMaxRetries = 2

// Retrier sends requests with the retry policy the official SDKs use:
// transport errors and 408/409/429/5xx responses are retried up to
// MaxRetries times with exponential backoff (0.5s doubling, 8s cap),
// honoring a Retry-After or Retry-After-Ms hint.
type Retrier struct {
	Client     *http.Client
	MaxRetries int
	// Prefix labels transport errors ("anthropic", "openai").
	Prefix string
	// ErrorFromResponse converts a non-2xx response, body still open, into
	// the provider's error type.
	ErrorFromResponse func(*http.Response) error
}

// Do sends the request newRequest builds, rebuilding it for every attempt so
// each carries a fresh body, and returns the first 2xx response with its
// body still open. A non-2xx status that survives the retry budget is
// drained and returned through ErrorFromResponse.
func (r Retrier) Do(ctx context.Context, newRequest func() (*http.Request, error)) (*http.Response, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := newRequest()
		if err != nil {
			return nil, fmt.Errorf("%s: building request: %w", r.Prefix, err)
		}
		resp, err := r.Client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("%s: request failed: %w", r.Prefix, err)
			}
			lastErr = fmt.Errorf("%s: request failed: %w", r.Prefix, err)
			if attempt >= r.MaxRetries {
				return nil, lastErr
			}
			if err := sleepBeforeRetry(ctx, nil, attempt); err != nil {
				return nil, lastErr
			}
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}

		apiErr := r.ErrorFromResponse(resp)
		resp.Body.Close()
		if !retryableStatus(resp.StatusCode) || attempt >= r.MaxRetries {
			return nil, apiErr
		}
		lastErr = apiErr
		if err := sleepBeforeRetry(ctx, resp, attempt); err != nil {
			return nil, lastErr
		}
	}
}

func retryableStatus(code int) bool {
	return code == http.StatusRequestTimeout ||
		code == http.StatusConflict ||
		code == http.StatusTooManyRequests ||
		code >= 500
}

// sleepBeforeRetry waits out the server's Retry-After(-Ms) hint when present,
// otherwise an exponential backoff. Returns early with the context's error
// if it is cancelled while waiting.
func sleepBeforeRetry(ctx context.Context, resp *http.Response, attempt int) error {
	delay := time.Duration(math.Min(8, 0.5*math.Pow(2, float64(attempt))) * float64(time.Second))
	if resp != nil {
		if ms := resp.Header.Get("Retry-After-Ms"); ms != "" {
			if v, err := strconv.Atoi(ms); err == nil && v >= 0 {
				delay = time.Duration(v) * time.Millisecond
			}
		} else if ra := resp.Header.Get("Retry-After"); ra != "" {
			if v, err := strconv.Atoi(ra); err == nil && v >= 0 {
				delay = time.Duration(v) * time.Second
			} else if at, err := http.ParseTime(ra); err == nil {
				delay = max(time.Until(at), 0)
			}
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// maxErrorBody bounds how much of a failing response is read for its
// message.
const maxErrorBody = 1 << 20

// ReadError drains a non-2xx body and decodes the providers' shared error
// envelope {"error": E}. When the envelope is present it is returned with ok
// set; otherwise the raw body (empty when it could not be read) is returned
// for the caller's fallback error.
func ReadError[E any](resp *http.Response) (envelope *E, body string, ok bool) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil {
		return nil, "", false
	}
	if e, ok := ParseEnvelope[E](raw); ok {
		return e, string(raw), true
	}
	return nil, string(raw), false
}

// ParseEnvelope decodes {"error": E} from raw, reporting whether an error
// object was present.
func ParseEnvelope[E any](raw []byte) (*E, bool) {
	var envelope struct {
		Error *E `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Error != nil {
		return envelope.Error, true
	}
	return nil, false
}

// maxSSELine bounds one stream line. Thinking deltas, tool-input JSON, and
// inline image data can make very long lines; the official SDKs allow far
// more than this costs.
const maxSSELine = 256 << 20

// ScanSSE yields the data payload of each server-sent event on r: data:
// lines accumulate, joined by newlines, until the blank line that ends the
// event. Comment lines (keep-alives) and the event:, id:, and retry: fields
// are skipped; payload type fields are authoritative. Any other line is
// handed to stray, whose non-nil error is yielded in place and the stream
// continues; a nil stray ignores such lines. A read failure ends the stream
// with an error wrapped as "reading stream".
func ScanSSE(r io.Reader, stray func(line []byte) error) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		var data []byte
		flush := func() bool {
			if len(data) == 0 {
				return true
			}
			payload := data
			data = nil
			return yield(payload, nil)
		}

		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 1024), maxSSELine)
		for scanner.Scan() {
			line := scanner.Bytes()
			switch {
			case len(line) == 0:
				if !flush() {
					return
				}
			case line[0] == ':':
			default:
				field, value, _ := bytes.Cut(line, []byte(":"))
				switch string(field) {
				case "data":
					value = bytes.TrimPrefix(value, []byte(" "))
					if len(data) > 0 {
						data = append(data, '\n')
					}
					data = append(data, value...)
				case "event", "id", "retry":
				default:
					if stray != nil {
						if err := stray(line); err != nil && !yield(nil, err) {
							return
						}
					}
				}
			}
		}
		if err := scanner.Err(); err != nil {
			yield(nil, fmt.Errorf("reading stream: %w", err))
			return
		}
		flush()
	}
}
