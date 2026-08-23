package tools

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCleanupAfterConnectTransportRunsOnStartError(t *testing.T) {
	cleanupCalled := false
	transport := &cleanupAfterConnectTransport{
		transport: &mcp.CommandTransport{
			Command: exec.Command(filepath.Join(t.TempDir(), "missing-mcp-server")),
		},
		cleanup: func() error {
			cleanupCalled = true
			return nil
		},
	}

	if _, err := transport.Connect(context.Background()); err == nil {
		t.Fatal("Connect returned nil error for missing MCP server")
	}
	if !cleanupCalled {
		t.Fatal("cleanup was not called after command start failed")
	}
}

func TestCleanupAfterConnectTransportRunsBeforeInitializationCompletes(t *testing.T) {
	clientTransport, peerTransport := mcp.NewInMemoryTransports()
	peer, err := peerTransport.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()

	cleanupCalled := make(chan struct{})
	transport := &cleanupAfterConnectTransport{
		transport: clientTransport,
		cleanup: func() error {
			close(cleanupCalled)
			return nil
		},
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "cleanup-test", Version: "1.0.0"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connectResult := make(chan error, 1)
	go func() {
		_, err := client.Connect(ctx, transport, nil)
		connectResult <- err
	}()

	select {
	case <-cleanupCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup did not run at the transport Connect boundary")
	}
	select {
	case err := <-connectResult:
		t.Fatalf("Client.Connect returned before the unread peer was closed: %v", err)
	default:
	}

	cancel()
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-connectResult:
		if err == nil {
			t.Fatal("Client.Connect returned nil error after its peer closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Client.Connect did not return after its peer closed")
	}
}
