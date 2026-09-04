//go:build unix

package tools

import (
	"context"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReadFileRejectsNamedPipeWithoutBlocking(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	tool := NewReadFileTool(NewToolRegistry(nil))
	done := make(chan error, 1)
	go func() {
		_, err := tool.Execute(context.Background(), map[string]any{"path": fifo})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "named pipe") {
			t.Fatalf("expected named pipe rejection, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read_file blocked on a FIFO")
	}
}
