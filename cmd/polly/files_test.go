package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"golang.org/x/image/bmp"
)

const onePixelWebPBase64 = "UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEAAUAmJaQAA3AA/v89WAAAAA=="

func TestReadFilePortableImagePassthrough(t *testing.T) {
	dir := t.TempDir()

	pngPath := filepath.Join(dir, "small.png")
	writeImageFixture(t, pngPath, 8, 6)

	jpegPath := filepath.Join(dir, "small.jpg")
	jpegFile, err := os.Create(jpegPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(jpegFile, image.NewRGBA(image.Rect(0, 0, 8, 6)), nil); err != nil {
		jpegFile.Close()
		t.Fatal(err)
	}
	if err := jpegFile.Close(); err != nil {
		t.Fatal(err)
	}

	webpPath := filepath.Join(dir, "small.webp")
	webpData, err := base64.StdEncoding.DecodeString(onePixelWebPBase64)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(webpPath, webpData, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		path     string
		wantMIME string
	}{
		{name: "PNG", path: pngPath, wantMIME: "image/png"},
		{name: "JPEG", path: jpegPath, wantMIME: "image/jpeg"},
		{name: "WebP", path: webpPath, wantMIME: "image/webp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			part, err := readFile(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			got := decodeImagePart(t, part)
			if part.MimeType != tc.wantMIME || !bytes.Equal(got, want) {
				t.Fatalf("readFile MIME=%q bytesEqual=%v, want %q byte-for-byte", part.MimeType, bytes.Equal(got, want), tc.wantMIME)
			}
		})
	}
}

func TestReadFileNormalizesGIFAndBMPToPNG(t *testing.T) {
	dir := t.TempDir()
	gifPath := filepath.Join(dir, "animated.gif")
	writeAnimatedGIFFile(t, gifPath)
	bmpPath := filepath.Join(dir, "still.bmp")
	writeBMPFile(t, bmpPath)

	for _, tc := range []struct {
		name string
		path string
		want color.RGBA
	}{
		{name: "GIF first frame", path: gifPath, want: color.RGBA{R: 255, A: 255}},
		{name: "BMP", path: bmpPath, want: color.RGBA{G: 255, A: 255}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			part, err := readFile(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			if part.MimeType != "image/png" {
				t.Fatalf("normalized MIME = %q, want image/png", part.MimeType)
			}
			img, format, err := image.Decode(bytes.NewReader(decodeImagePart(t, part)))
			if err != nil {
				t.Fatal(err)
			}
			if format != "png" {
				t.Fatalf("normalized format = %q, want png", format)
			}
			got := color.RGBAModel.Convert(img.At(0, 0)).(color.RGBA)
			if got != tc.want {
				t.Fatalf("normalized first pixel = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestReadFileRejectsSVGImage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vector.svg")
	if err := os.WriteFile(path, []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="2" height="2"></svg>`), 0o600); err != nil {
		t.Fatal(err)
	}
	part, err := readFile(path)
	if err == nil || part != nil {
		t.Fatalf("SVG read = %+v, %v; want unsupported-format error", part, err)
	}
	if !strings.Contains(err.Error(), "unsupported image format") {
		t.Fatalf("SVG error = %q", err)
	}
}

func TestFetchURLNormalizesRasterAndRejectsSVG(t *testing.T) {
	dir := t.TempDir()
	gifPath := filepath.Join(dir, "animated.gif")
	writeAnimatedGIFFile(t, gifPath)
	gifData, err := os.ReadFile(gifPath)
	if err != nil {
		t.Fatal(err)
	}
	svgData := []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="2" height="2"></svg>`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/animated.gif":
			w.Header().Set("Content-Type", "image/gif")
			_, _ = w.Write(gifData)
		case "/vector.svg":
			w.Header().Set("Content-Type", "image/svg+xml")
			_, _ = w.Write(svgData)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	part, err := fetchURL(server.URL + "/animated.gif")
	if err != nil {
		t.Fatal(err)
	}
	if part.MimeType != "image/png" {
		t.Fatalf("fetched GIF MIME = %q, want image/png", part.MimeType)
	}
	if _, format, err := image.Decode(bytes.NewReader(decodeImagePart(t, part))); err != nil || format != "png" {
		t.Fatalf("fetched GIF format = %q, %v; want png", format, err)
	}

	part, err = fetchURL(server.URL + "/vector.svg")
	if err == nil || part != nil {
		t.Fatalf("fetched SVG = %+v, %v; want unsupported-format error", part, err)
	}
	if !strings.Contains(err.Error(), "unsupported image format") {
		t.Fatalf("fetched SVG error = %q", err)
	}
}

func TestFetchURLSniffsExtensionlessRasterAndRejectsOversize(t *testing.T) {
	dir := t.TempDir()
	pngPath := filepath.Join(dir, "fixture.png")
	writeImageFixture(t, pngPath, 8, 6)
	pngData, err := os.ReadFile(pngPath)
	if err != nil {
		t.Fatal(err)
	}
	webpData, err := base64.StdEncoding.DecodeString(onePixelWebPBase64)
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/opaque-png":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(pngData)
		case "/opaque-webp":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(webpData)
		case "/too-large":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprint(maxLocalImageBytes+1))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	for _, tc := range []struct {
		name     string
		path     string
		wantMIME string
		wantData []byte
	}{
		{name: "PNG", path: "/opaque-png", wantMIME: "image/png", wantData: pngData},
		{name: "WebP", path: "/opaque-webp", wantMIME: "image/webp", wantData: webpData},
	} {
		t.Run(tc.name, func(t *testing.T) {
			part, err := fetchURL(server.URL + tc.path)
			if err != nil {
				t.Fatal(err)
			}
			got := decodeImagePart(t, part)
			if part.MimeType != tc.wantMIME || !bytes.Equal(got, tc.wantData) {
				t.Fatalf("fetched opaque %s MIME=%q bytesEqual=%v", tc.name, part.MimeType, bytes.Equal(got, tc.wantData))
			}
		})
	}

	part, err := fetchURL(server.URL + "/too-large")
	if err == nil || part != nil || !strings.Contains(err.Error(), "response exceeds the 32 MiB download limit") {
		t.Fatalf("oversize response = %+v, %v", part, err)
	}
}

func TestOneShotRejectsSeventeenImagesBeforeModelCall(t *testing.T) {
	dir := t.TempDir()
	files := make([]string, 0, maxPromptAttachments+1)
	for i := 0; i <= maxPromptAttachments; i++ {
		path := filepath.Join(dir, fmt.Sprintf("image-%02d.png", i))
		writeImageFixture(t, path, 2, 2)
		files = append(files, path)
	}
	msg, err := buildMessageWithFiles("inspect", files)
	if err != nil {
		t.Fatal(err)
	}
	store := sessions.NewSyncMapSessionStore(nil)
	session, err := store.Get("seventeen-one-shot")
	if err != nil {
		t.Fatal(err)
	}
	state := &conversationState{session: session}
	_, err = executeTurnWithUserMessage(context.Background(), &Config{}, state, msg, nil, nil, nil, false)
	if err == nil || !strings.Contains(err.Error(), "portable maximum is 16") {
		t.Fatalf("17-image one-shot error = %v", err)
	}
	if history := session.GetHistory(); len(history) != 0 {
		t.Fatalf("rejected one-shot entered durable history: %#v", history)
	}
}

func decodeImagePart(t *testing.T, part *messages.ContentPart) []byte {
	t.Helper()
	if part == nil || part.Type != "image_base64" {
		t.Fatalf("image part = %+v", part)
	}
	data, err := base64.StdEncoding.DecodeString(part.ImageData)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeAnimatedGIFFile(t *testing.T, path string) {
	t.Helper()
	palette := color.Palette{
		color.RGBA{R: 255, A: 255},
		color.RGBA{B: 255, A: 255},
	}
	first := image.NewPaletted(image.Rect(0, 0, 3, 2), palette)
	second := image.NewPaletted(image.Rect(0, 0, 3, 2), palette)
	for i := range second.Pix {
		second.Pix[i] = 1
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := gif.EncodeAll(file, &gif.GIF{Image: []*image.Paletted{first, second}, Delay: []int{1, 1}}); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeBMPFile(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 3, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 3; x++ {
			img.Set(x, y, color.RGBA{G: 255, A: 255})
		}
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := bmp.Encode(file, img); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
