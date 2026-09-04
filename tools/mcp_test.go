package tools

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
)

type stubMCPTransport struct {
	connect func(context.Context) (mcp.Connection, error)
}

func (t stubMCPTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	return t.connect(ctx)
}

type stubMCPConnection struct {
	cleanupComplete *atomic.Bool
	writeObserved   chan<- bool
	writeErr        error
	closeErr        error
	closed          chan struct{}
	closeOnce       sync.Once
	closeCalls      atomic.Int32
}

func (c *stubMCPConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, io.EOF
	}
}

func (c *stubMCPConnection) Write(context.Context, jsonrpc.Message) error {
	if c.writeObserved != nil {
		c.writeObserved <- c.cleanupComplete.Load()
	}
	return c.writeErr
}

func (c *stubMCPConnection) Close() error {
	c.closeCalls.Add(1)
	c.closeOnce.Do(func() { close(c.closed) })
	return c.closeErr
}

func (*stubMCPConnection) SessionID() string { return "" }

func TestSandboxCleanupTransportRunsBeforeMCPHandshake(t *testing.T) {
	var cleanupComplete atomic.Bool
	writeObserved := make(chan bool, 1)
	stopHandshake := errors.New("stop test MCP handshake")
	conn := &stubMCPConnection{
		cleanupComplete: &cleanupComplete,
		writeObserved:   writeObserved,
		writeErr:        stopHandshake,
		closed:          make(chan struct{}),
	}
	transport := &sandboxCleanupTransport{
		transport: stubMCPTransport{connect: func(context.Context) (mcp.Connection, error) {
			return conn, nil
		}},
		cleanup: func() error {
			cleanupComplete.Store(true)
			return nil
		},
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "cleanup-test", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Connect(ctx, transport, nil); err == nil {
		t.Fatal("Client.Connect unexpectedly succeeded")
	}

	select {
	case cleaned := <-writeObserved:
		if !cleaned {
			t.Fatal("MCP handshake started before sandbox descriptors were released")
		}
	case <-ctx.Done():
		t.Fatalf("MCP handshake was not observed: %v", ctx.Err())
	}
}

func TestSandboxCleanupTransportCleansAfterSuccessfulCommandStart(t *testing.T) {
	catPath, err := exec.LookPath("cat")
	if err != nil {
		t.Skip("cat is unavailable")
	}

	var cleanupComplete atomic.Bool
	transport := &sandboxCleanupTransport{
		transport: &mcp.CommandTransport{
			Command:           exec.Command(catPath),
			TerminateDuration: time.Second,
		},
		cleanup: func() error {
			cleanupComplete.Store(true)
			return nil
		},
	}

	conn, err := transport.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	if !cleanupComplete.Load() {
		t.Fatal("sandbox descriptors were not released before Connect returned")
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close command connection: %v", err)
	}
}

func TestSandboxCleanupTransportCleansAfterCommandStartFailure(t *testing.T) {
	var cleanupCalls atomic.Int32
	transport := &sandboxCleanupTransport{
		transport: &mcp.CommandTransport{
			Command: exec.Command(filepath.Join(t.TempDir(), "missing-mcp-server")),
		},
		cleanup: func() error {
			cleanupCalls.Add(1)
			return nil
		},
	}

	conn, err := transport.Connect(context.Background())
	if err == nil {
		t.Fatal("Connect unexpectedly succeeded")
	}
	if conn != nil {
		t.Fatal("Connect returned a connection after command start failure")
	}
	if got := cleanupCalls.Load(); got != 1 {
		t.Fatalf("cleanup calls = %d, want 1", got)
	}
}

func TestSandboxCleanupTransportClosesConnectionOnCleanupError(t *testing.T) {
	cleanupErr := errors.New("cleanup failed")
	closeErr := errors.New("close failed")
	conn := &stubMCPConnection{
		closeErr: closeErr,
		closed:   make(chan struct{}),
	}
	transport := &sandboxCleanupTransport{
		transport: stubMCPTransport{connect: func(context.Context) (mcp.Connection, error) {
			return conn, nil
		}},
		cleanup: func() error { return cleanupErr },
	}

	gotConn, err := transport.Connect(context.Background())
	if gotConn != nil {
		t.Fatal("Connect returned a connection after sandbox cleanup failure")
	}
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("Connect error %v does not wrap cleanup error", err)
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("Connect error %v does not wrap connection close error", err)
	}
	if got := conn.closeCalls.Load(); got != 1 {
		t.Fatalf("connection close calls = %d, want 1", got)
	}
}

type recordingRoundTripper struct {
	requests []*http.Request
}

func (r *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.requests = append(r.requests, req)
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

func TestHeaderRoundTripperInjectsOnlyForEndpointOrigin(t *testing.T) {
	base := &recordingRoundTripper{}
	rt := &headerRoundTripper{
		base:    base,
		headers: map[string]string{"Authorization": "Bearer secret", "X-Api-Key": "k"},
		origin:  "https://mcp.example.com",
	}
	for _, target := range []string{"https://mcp.example.com/sse", "https://MCP.example.com/messages?x=1"} {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		if _, err := rt.RoundTrip(req); err != nil {
			t.Fatal(err)
		}
		sent := base.requests[len(base.requests)-1]
		if sent.Header.Get("Authorization") != "Bearer secret" || sent.Header.Get("X-Api-Key") != "k" {
			t.Fatalf("headers missing for same-origin request %s: %v", target, sent.Header)
		}
		if req.Header.Get("Authorization") != "" {
			t.Fatal("round tripper mutated the caller's request")
		}
	}
	for _, target := range []string{"https://evil.example.com/sse", "http://mcp.example.com/sse", "https://mcp.example.com:8443/sse"} {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		if _, err := rt.RoundTrip(req); err != nil {
			t.Fatal(err)
		}
		sent := base.requests[len(base.requests)-1]
		if sent.Header.Get("Authorization") != "" || sent.Header.Get("X-Api-Key") != "" {
			t.Fatalf("credentials leaked to %s: %v", target, sent.Header)
		}
	}
}

func TestMCPHTTPClientDropsCredentialsAcrossRedirect(t *testing.T) {
	var gotAuth, gotKey string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotKey = r.Header.Get("X-Api-Key")
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()
	var originAuth string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originAuth = r.Header.Get("Authorization")
		http.Redirect(w, r, other.URL+"/elsewhere", http.StatusFound)
	}))
	defer origin.Close()

	client := mcpHTTPClient(origin.URL+"/sse", map[string]string{"Authorization": "Bearer secret", "X-Api-Key": "k"}, 5*time.Second)
	resp, err := client.Get(origin.URL + "/sse")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if originAuth != "Bearer secret" {
		t.Fatalf("origin did not receive the configured header: %q", originAuth)
	}
	if gotAuth != "" || gotKey != "" {
		t.Fatalf("redirect target received credentials: auth=%q key=%q", gotAuth, gotKey)
	}
}

func TestMCPHTTPClientDoesNotTimeOutResponseBodies(t *testing.T) {
	client := mcpHTTPClient("https://mcp.example.com/sse", nil, 7*time.Second)
	if client.Timeout != 0 {
		t.Fatalf("client.Timeout = %v, want none so event streams stay open", client.Timeout)
	}
	rt, ok := client.Transport.(*headerRoundTripper)
	if !ok {
		t.Fatalf("transport = %T", client.Transport)
	}
	transport, ok := rt.base.(*http.Transport)
	if !ok {
		t.Fatalf("base transport = %T", rt.base)
	}
	if transport.ResponseHeaderTimeout != 7*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %v, want 7s", transport.ResponseHeaderTimeout)
	}
}
