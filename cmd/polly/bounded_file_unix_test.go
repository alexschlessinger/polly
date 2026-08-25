//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package main

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestPrepareImageForUploadRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replacement.png")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := prepareImageForUpload(path)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO attachment was accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO attachment blocked instead of being rejected")
	}
	if _, err := loadLocalImage(path); err == nil {
		t.Fatal("FIFO display image was accepted")
	}
}
