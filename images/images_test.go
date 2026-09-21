package images

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/image/bmp"
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

// PortableMIMEType is the one table for the portable upload formats, and
// NormalizeForModel never produces anything outside it.
func TestPortableMIMEType(t *testing.T) {
	for format, want := range map[string]string{"png": "image/png", "jpeg": "image/jpeg", "webp": "image/webp"} {
		if got, ok := PortableMIMEType(format); !ok || got != want {
			t.Fatalf("PortableMIMEType(%q) = %q, %v; want %q", format, got, ok, want)
		}
	}
	for _, format := range []string{"gif", "bmp", "tiff", ""} {
		if got, ok := PortableMIMEType(format); ok || got != "" {
			t.Fatalf("PortableMIMEType(%q) = %q, %v; want not portable", format, got, ok)
		}
	}
}

// A payload that needs the byte-cap shrink loop reports the dimensions it was
// actually encoded at, and a BMP stays PNG through every iteration.
func TestNormalizeForModelShrinksBMPWithoutJPEGFallback(t *testing.T) {
	const size = 1400
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	state := uint32(0x12345678)
	for i := 0; i < len(img.Pix); i += 4 {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		img.Pix[i+0] = byte(state)
		img.Pix[i+1] = byte(state >> 8)
		img.Pix[i+2] = byte(state >> 16)
		img.Pix[i+3] = 0xff
	}
	var asPNG bytes.Buffer
	if err := png.Encode(&asPNG, img); err != nil {
		t.Fatal(err)
	}
	if asPNG.Len() <= UploadMaxBytes {
		t.Fatalf("fixture PNG is %d bytes; want more than %d to exercise iterative shrinking", asPNG.Len(), UploadMaxBytes)
	}
	var asBMP bytes.Buffer
	if err := bmp.Encode(&asBMP, img); err != nil {
		t.Fatal(err)
	}

	norm, err := NormalizeForModel(asBMP.Bytes(), "noisy.bmp")
	if err != nil {
		t.Fatal(err)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(norm.Data))
	if err != nil {
		t.Fatal(err)
	}
	if norm.MIMEType != "image/png" || format != "png" {
		t.Fatalf("size-limited BMP = mime %q format %q, want PNG", norm.MIMEType, format)
	}
	if len(norm.Data) > UploadMaxBytes {
		t.Fatalf("size-limited BMP is %d bytes, want at most %d", len(norm.Data), UploadMaxBytes)
	}
	if config.Width >= size || config.Height >= size {
		t.Fatalf("size-limited BMP stayed %dx%d; iterative shrinking did not run", config.Width, config.Height)
	}
	if norm.Width != config.Width || norm.Height != config.Height {
		t.Fatalf("reported %dx%d, encoded %dx%d", norm.Width, norm.Height, config.Width, config.Height)
	}
}

// DecodeBoundedConfig is the header-only probe: it applies the same size and
// dimension bounds as a full read without decoding pixels.
func TestDecodeBoundedConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pic.png")
	var buf bytes.Buffer
	if err := png.Encode(&buf, testImage(4, 3)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	config, format, err := DecodeBoundedConfig(path, 1<<20)
	if err != nil || format != "png" || config.Width != 4 || config.Height != 3 {
		t.Fatalf("DecodeBoundedConfig = %+v, %q, %v", config, format, err)
	}
	if _, _, err := DecodeBoundedConfig(path, int64(buf.Len())-1); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversize error = %v, want the bounded-file limit error", err)
	}
	if _, _, err := DecodeBoundedConfig(dir, 1<<20); err == nil {
		t.Fatal("directory probed without error")
	}
	garbage := filepath.Join(dir, "garbage.png")
	if err := os.WriteFile(garbage, []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeBoundedConfig(garbage, 1<<20); err == nil {
		t.Fatal("garbage probed without error")
	}
}

// DecodeBoundedFile is read + validate + decode in one step, keeping the
// bounded reader's limit error for the oversize case.
func TestDecodeBoundedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pic.png")
	var buf bytes.Buffer
	if err := png.Encode(&buf, testImage(4, 3)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	img, format, err := DecodeBoundedFile(path, 1<<20)
	if err != nil || format != "png" || img.Bounds().Dx() != 4 || img.Bounds().Dy() != 3 {
		t.Fatalf("DecodeBoundedFile = %v, %q, %v", img, format, err)
	}
	if _, _, err := DecodeBoundedFile(path, int64(buf.Len())-1); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversize error = %v, want the bounded-file limit error", err)
	}
	garbage := filepath.Join(dir, "garbage.png")
	if err := os.WriteFile(garbage, []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeBoundedFile(garbage, 1<<20); err == nil {
		t.Fatal("garbage decoded without error")
	}
}
