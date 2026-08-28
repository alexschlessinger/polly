package images

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"testing"
)

func testImage(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.NRGBA{R: 255, A: 255})
	return img
}

func TestNormalizeForModelPassthroughPNG(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, testImage(4, 3)); err != nil {
		t.Fatalf("encode: %v", err)
	}
	norm, err := NormalizeForModel(buf.Bytes(), "/some/dir/pic.png")
	if err != nil {
		t.Fatalf("NormalizeForModel: %v", err)
	}
	if norm.MIMEType != "image/png" || norm.FileName != "pic.png" || norm.Width != 4 || norm.Height != 3 {
		t.Fatalf("unexpected result: %+v", norm)
	}
	if !bytes.Equal(norm.Data, buf.Bytes()) {
		t.Fatal("small PNG should pass through unchanged")
	}
}

func TestNormalizeForModelConvertsGIF(t *testing.T) {
	var buf bytes.Buffer
	if err := gif.Encode(&buf, testImage(4, 3), nil); err != nil {
		t.Fatalf("encode: %v", err)
	}
	norm, err := NormalizeForModel(buf.Bytes(), "anim.gif")
	if err != nil {
		t.Fatalf("NormalizeForModel: %v", err)
	}
	if norm.MIMEType != "image/png" {
		t.Fatalf("expected GIF normalized to PNG, got %s", norm.MIMEType)
	}
}

func TestNormalizeForModelDownscalesOversized(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, testImage(UploadMaxLongEdge+100, 20)); err != nil {
		t.Fatalf("encode: %v", err)
	}
	norm, err := NormalizeForModel(buf.Bytes(), "wide.png")
	if err != nil {
		t.Fatalf("NormalizeForModel: %v", err)
	}
	if norm.Width > UploadMaxLongEdge || norm.Height > UploadMaxLongEdge {
		t.Fatalf("expected downscale within %d, got %dx%d", UploadMaxLongEdge, norm.Width, norm.Height)
	}
	if len(norm.Data) > UploadMaxBytes {
		t.Fatalf("normalized data exceeds byte cap: %d", len(norm.Data))
	}
}

func TestNormalizeForModelRejectsGarbage(t *testing.T) {
	if _, err := NormalizeForModel([]byte("not an image"), ""); err == nil {
		t.Fatal("expected error for invalid data")
	}
}

func TestNormalizeForModelFileNameFallback(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, testImage(2, 2)); err != nil {
		t.Fatalf("encode: %v", err)
	}
	norm, err := NormalizeForModel(buf.Bytes(), "  ")
	if err != nil {
		t.Fatalf("NormalizeForModel: %v", err)
	}
	if norm.FileName != "attachment" {
		t.Fatalf("expected fallback name, got %q", norm.FileName)
	}
}
