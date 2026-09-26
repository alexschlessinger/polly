package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type testError struct {
	Message string `json:"message"`
	Status  int    `json:"-"`
}

func (e *testError) Error() string { return e.Message }

func testErrorFromResponse(resp *http.Response) error {
	apiErr, body, ok := ReadError[testError](resp)
	if !ok {
		apiErr = &testError{Message: body}
	}
	apiErr.Status = resp.StatusCode
	return apiErr
}

func TestRetrierRetriesTransientStatusesAndFailsFastOnClientErrors(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r)
		if body != "payload" {
			t.Errorf("attempt %d body = %q, want a fresh payload", calls.Load(), body)
		}
		if calls.Add(1) <= 2 {
			w.Header().Set("Retry-After-Ms", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"message":"busy"}}`)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer server.Close()

	retrier := Retrier{Client: server.Client(), MaxRetries: 2, Prefix: "test", ErrorFromResponse: testErrorFromResponse}
	newRequest := func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, strings.NewReader("payload"))
	}
	resp, err := retrier.Do(context.Background(), newRequest)
	if err != nil {
		t.Fatalf("Do after retries: %v", err)
	}
	resp.Body.Close()
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", calls.Load())
	}

	var badCalls atomic.Int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badCalls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "plain text")
	}))
	defer bad.Close()
	retrier.Client = bad.Client()
	_, err = retrier.Do(context.Background(), func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodPost, bad.URL, strings.NewReader("payload"))
	})
	var apiErr *testError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 || apiErr.Message != "plain text" {
		t.Fatalf("err = %v, want fallback 400 error with raw body", err)
	}
	if badCalls.Load() != 1 {
		t.Fatalf("400 was retried: %d calls", badCalls.Load())
	}
}

// TestRetrierReturnsTerminalErrorsWithoutWaiting pins the Terminal hook: a
// retryable status whose converted error the caller marks terminal comes
// back at once, without spending the retry budget or waiting out the
// server's Retry-After hint.
func TestRetrierReturnsTerminalErrorsWithoutWaiting(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"allowance exhausted"}}`)
	}))
	defer server.Close()
	terminal := func(err error) bool {
		var apiErr *testError
		return errors.As(err, &apiErr) && apiErr.Message == "allowance exhausted"
	}
	retrier := Retrier{Client: server.Client(), MaxRetries: 2, Prefix: "test", ErrorFromResponse: testErrorFromResponse, Terminal: terminal}
	done := make(chan error, 1)
	go func() {
		_, err := retrier.Do(context.Background(), func() (*http.Request, error) {
			return http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, strings.NewReader("payload"))
		})
		done <- err
	}()
	select {
	case err := <-done:
		var apiErr *testError
		if !errors.As(err, &apiErr) || apiErr.Status != 429 {
			t.Fatalf("err = %v, want the 429 error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("terminal 429 waited out its Retry-After")
	}
	if calls.Load() != 1 {
		t.Fatalf("terminal 429 was retried: %d calls", calls.Load())
	}
}

func TestScanSSEJoinsDataAndReportsStrayLines(t *testing.T) {
	stream := ": keep-alive\n" +
		"event: ping\n" +
		"data: {\"a\":\n" +
		"data: 1}\n" +
		"\n" +
		"{\"error\":\"bare\"}\n" +
		"data:{\"b\":2}\n" +
		"\n" +
		"data: tail"
	var got []string
	var strays []string
	stray := func(line []byte) error {
		strays = append(strays, string(line))
		return errors.New("stray")
	}
	var errs int
	for data, err := range ScanSSE(strings.NewReader(stream), stray) {
		if err != nil {
			errs++
			continue
		}
		got = append(got, string(data))
	}
	want := []string{"{\"a\":\n1}", "{\"b\":2}", "tail"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("payloads = %q, want %q", got, want)
	}
	if errs != 1 || len(strays) != 1 || strays[0] != `{"error":"bare"}` {
		t.Fatalf("stray lines = %q (%d errors), want the bare envelope once", strays, errs)
	}
}

func readAll(r *http.Request) (string, error) {
	var b strings.Builder
	buf := make([]byte, 64)
	for {
		n, err := r.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			return b.String(), nil
		}
	}
}
