//go:build unix

package safefile

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestOpenRegularRejectsFIFOWithoutBlocking(t *testing.T) {
	dir := resolvedTempDir(t)
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := OpenRegular(fifo, os.O_RDONLY, 0)
		done <- err
	}()
	err := <-done
	var notRegular *NotRegularError
	if !errors.As(err, &notRegular) || notRegular.Mode&os.ModeNamedPipe == 0 {
		t.Fatalf("OpenRegular(fifo) error = %v, want NotRegularError for a named pipe", err)
	}
}
