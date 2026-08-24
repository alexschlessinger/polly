package main

import (
	"context"
	"os"
	"testing"
)

// TestClipboardCaptureManual exercises the real platform clipboard readers.
// It is skipped unless POLLYTOOL_CLIPBOARD_TEST=1 because it depends on an
// image being on the system clipboard and on desktop tooling being present.
//
//	osascript -e 'set the clipboard to (read POSIX file "/tmp/x.png" as «class PNGf»)'  # macOS
//	POLLYTOOL_CLIPBOARD_TEST=1 go test ./cmd/polly -run TestClipboardCaptureManual -v
func TestClipboardCaptureManual(t *testing.T) {
	if os.Getenv("POLLYTOOL_CLIPBOARD_TEST") != "1" {
		t.Skip("set POLLYTOOL_CLIPBOARD_TEST=1 with an image on the clipboard to run")
	}
	path, err := captureClipboardImage(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("captureClipboardImage: %v", err)
	}
	img, ok := resolveLocalTranscriptImage(path, "", "")
	if !ok {
		t.Fatalf("captured file %s is not a usable image", path)
	}
	t.Logf("captured %s (%dx%d)", path, img.Width, img.Height)
}
