package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/images"
	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
	"golang.org/x/image/bmp"
)

type wrappedImage struct {
	image.Image
}

func TestSplitDroppedPaths(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"plain", "/tmp/a.png", []string{"/tmp/a.png"}},
		{"multiple", "/tmp/a.png /tmp/b.png", []string{"/tmp/a.png", "/tmp/b.png"}},
		{"single quoted", "'/tmp/with space.png'", []string{"/tmp/with space.png"}},
		{"double quoted", `"C:\Users\alex\shot.png"`, []string{`C:\Users\alex\shot.png`}},
		{"escaped space", `/tmp/Screen\ Shot.png`, []string{"/tmp/Screen Shot.png"}},
		{"windows separators survive", `C:\Users\alex\shot.png`, []string{`C:\Users\alex\shot.png`}},
		{"newline separated", "/tmp/a.png\n/tmp/b.png", []string{"/tmp/a.png", "/tmp/b.png"}},
		{"trailing space", "/tmp/a.png ", []string{"/tmp/a.png"}},
		{"unterminated quote", "'/tmp/a.png", nil},
		{"empty", "   ", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitDroppedPaths(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("splitDroppedPaths(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("splitDroppedPaths(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// sendPasted drives text through the REPL as one bracketed paste, using the
// same per-key events the terminal pump would deliver.
func sendPasted(r *managedREPL, text string) {
	send := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}) }
	send(pasteStartID)
	for _, ch := range text {
		switch ch {
		case '\n':
			send("<Enter>")
		case ' ':
			send("<Space>")
		case '\t':
			send("<Tab>")
		default:
			send(string(ch))
		}
	}
	send(pasteEndID)
}

func TestPasteConvertsDroppedImagePaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Screen Shot.png")
	writeImageFixture(t, path, 8, 6)

	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	sendPasted(r, strings.ReplaceAll(path, " ", `\ `))

	if got := r.model.ed.text(); got != "[image #1] " {
		t.Fatalf("editor after image-path paste = %q, want %q", got, "[image #1] ")
	}
	att, ok := r.model.attachments[1]
	if !ok || att.Path != path {
		t.Fatalf("attachment registry = %+v, want path %q at #1", r.model.attachments, path)
	}
}

func TestPasteKeepsProseAndNonImagePathsAsText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	writeImageFixture(t, path, 8, 6)

	prose := "see " + path
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	sendPasted(r, prose)
	if got := r.model.ed.text(); got != prose {
		t.Fatalf("prose paste = %q, want %q", got, prose)
	}

	r = newManagedREPL(&Config{}, "ctx", 0, 0)
	missing := filepath.Join(dir, "missing.png")
	sendPasted(r, missing)
	if got := r.model.ed.text(); got != missing {
		t.Fatalf("missing-file paste = %q, want %q", got, missing)
	}
	if len(r.model.attachments) != 0 {
		t.Fatalf("no attachments expected, got %+v", r.model.attachments)
	}
}

func TestPromptAttachmentsResolveInTokenOrder(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.png")
	second := filepath.Join(dir, "b.png")
	writeImageFixture(t, first, 4, 4)
	writeImageFixture(t, second, 4, 4)

	m := newReplModel()
	tokenA := m.registerAttachment(first, "a.png")
	tokenB := m.registerAttachment(second, "b.png")
	if tokenA != "[image #1]" || tokenB != "[image #2]" {
		t.Fatalf("tokens = %q, %q", tokenA, tokenB)
	}

	got, err := m.promptAttachments("compare [image #2] with [image #1], again [image #2]")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Path != second || got[1].Path != first {
		t.Fatalf("promptAttachments = %+v, want [%s %s]", got, second, first)
	}
	if atts, err := m.promptAttachments("no tokens here"); err != nil || atts != nil {
		t.Fatalf("expected nil for token-free prompt, got %+v", atts)
	}
	if atts, err := m.promptAttachments("compare [image #2] with [image #9]"); err == nil || atts != nil || !strings.Contains(err.Error(), "[image #9]") {
		t.Fatalf("unknown token result = %+v, %v", atts, err)
	}
}

func TestPromptAttachmentsIgnoresBareTypedPaths(t *testing.T) {
	dir := t.TempDir()
	rel := filepath.Join(".assets", "polly.png")
	if err := os.MkdirAll(filepath.Join(dir, ".assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeImageFixture(t, filepath.Join(dir, rel), 6, 6)

	m := newReplModel()
	m.imageBaseDir = dir

	// Typed paths stay prose: the model is expected to call view_image.
	got, err := m.promptAttachments("what is (" + rel + "), exactly?")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("typed path attachments = %+v, want none", got)
	}

	// Registry tokens still attach, and a mention of a nonexistent file is
	// inert prose, never an error.
	other := filepath.Join(dir, "clip.png")
	writeImageFixture(t, other, 4, 4)
	token := m.registerAttachment(other, "clipboard image")
	got, err = m.promptAttachments("compare " + rel + " with " + token)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != other {
		t.Fatalf("token-only attachments = %+v, want [%s]", got, other)
	}
	if atts, err := m.promptAttachments("styles.png is mentioned but does not exist"); err != nil || atts != nil {
		t.Fatalf("missing file should stay prose, got %+v, %v", atts, err)
	}
}

func TestPromptAttachmentsRejectsMoreThanMaximumUniqueImages(t *testing.T) {
	dir := t.TempDir()
	m := newReplModel()
	var prompt strings.Builder
	for i := 0; i <= maxPromptAttachments; i++ {
		path := filepath.Join(dir, fmt.Sprintf("%02d.png", i))
		writeImageFixture(t, path, 2, 2)
		if i > 0 {
			prompt.WriteByte(' ')
		}
		prompt.WriteString(m.registerAttachment(path, filepath.Base(path)))
	}

	got, err := m.promptAttachments(prompt.String())
	if err == nil || got != nil {
		t.Fatalf("17-image result = %+v, %v; want rejection", got, err)
	}
	if !strings.Contains(err.Error(), "17 unique image attachments") || !strings.Contains(err.Error(), "maximum is 16") {
		t.Fatalf("17-image error = %q", err)
	}
}

func TestPrepareImageForUploadPassthroughAndDownscale(t *testing.T) {
	dir := t.TempDir()

	small := filepath.Join(dir, "small.png")
	writeImageFixture(t, small, 32, 16)
	part, err := prepareImageForUpload(small)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(small)
	if part.MimeType != "image/png" || part.ImageData != base64.StdEncoding.EncodeToString(raw) {
		t.Fatalf("small image should ship byte-for-byte as png, got mime %q", part.MimeType)
	}
	if part.FileName != "small.png" {
		t.Fatalf("FileName = %q", part.FileName)
	}

	webpPath := filepath.Join(dir, "small.webp")
	webpData, err := base64.StdEncoding.DecodeString("UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEAAUAmJaQAA3AA/v89WAAAAA==")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(webpPath, webpData, 0o600); err != nil {
		t.Fatal(err)
	}
	part, err = prepareImageForUpload(webpPath)
	if err != nil {
		t.Fatal(err)
	}
	if part.MimeType != "image/webp" || part.ImageData != base64.StdEncoding.EncodeToString(webpData) {
		t.Fatalf("small WebP was not passed through byte-for-byte: mime=%q", part.MimeType)
	}

	big := filepath.Join(dir, "big.png")
	writeImageFixture(t, big, 2000, 500)
	part, err = prepareImageForUpload(big)
	if err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(part.ImageData)
	if err != nil {
		t.Fatal(err)
	}
	config, format, err := image.DecodeConfig(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if format != "png" || config.Width != uploadMaxLongEdge || config.Height != 392 {
		t.Fatalf("downscaled = %s %dx%d, want png %dx392", format, config.Width, config.Height, uploadMaxLongEdge)
	}

	photo := filepath.Join(dir, "photo.jpg")
	img := image.NewRGBA(image.Rect(0, 0, 40, 30))
	file, err := os.Create(photo)
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(file, img, nil); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	part, err = prepareImageForUpload(photo)
	if err != nil {
		t.Fatal(err)
	}
	photoData, err := os.ReadFile(photo)
	if err != nil {
		t.Fatal(err)
	}
	if part.MimeType != "image/jpeg" || part.ImageData != base64.StdEncoding.EncodeToString(photoData) {
		t.Fatalf("small JPEG was not passed through byte-for-byte: mime=%q", part.MimeType)
	}
}

func TestPrepareImageForUploadAppliesEXIFOrientationWhenResizing(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 1600, 800))
	for y := 0; y < 800; y++ {
		for x := 0; x < 1600; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x / 8), G: uint8(y / 4), B: 120, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	oriented := jpegWithEXIFOrientation(t, encoded.Bytes(), 6)
	if got := images.JPEGOrientation(oriented); got != 6 {
		t.Fatalf("fixture EXIF orientation = %d, want 6", got)
	}
	path := filepath.Join(t.TempDir(), "portrait.jpg")
	if err := os.WriteFile(path, oriented, 0o600); err != nil {
		t.Fatal(err)
	}

	part, err := prepareImageForUpload(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(part.ImageData)
	if err != nil {
		t.Fatal(err)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if format != "jpeg" || part.MimeType != "image/jpeg" || config.Width != 784 || config.Height != 1568 {
		t.Fatalf("oriented resize = %s %s %dx%d, want JPEG 784x1568", format, part.MimeType, config.Width, config.Height)
	}
	if got := images.JPEGOrientation(data); got != 1 {
		t.Fatalf("resized JPEG retained stale EXIF orientation %d", got)
	}
}

func TestApplyEXIFOrientationNRGBAFastPathMatchesGenericPath(t *testing.T) {
	bounds := image.Rect(2, 3, 5, 5)
	src := image.NewNRGBA(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			src.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 31), G: uint8(y * 37), B: uint8(x + y), A: 255})
		}
	}

	for orientation := 2; orientation <= 8; orientation++ {
		fast, ok := images.ApplyEXIFOrientation(src, orientation).(*image.NRGBA)
		if !ok {
			t.Fatalf("orientation %d did not use NRGBA destination", orientation)
		}
		generic := images.ApplyEXIFOrientation(wrappedImage{Image: src}, orientation)
		if fast.Bounds() != generic.Bounds() {
			t.Fatalf("orientation %d bounds = %v, want %v", orientation, fast.Bounds(), generic.Bounds())
		}
		for y := fast.Bounds().Min.Y; y < fast.Bounds().Max.Y; y++ {
			for x := fast.Bounds().Min.X; x < fast.Bounds().Max.X; x++ {
				got := fast.NRGBAAt(x, y)
				want := color.NRGBAModel.Convert(generic.At(x, y)).(color.NRGBA)
				if got != want {
					t.Fatalf("orientation %d pixel (%d,%d) = %#v, want %#v", orientation, x, y, got, want)
				}
			}
		}
	}
}

func TestPreparePortableImageRequestUpgradesLegacyMainImages(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 1600, 800))
	var jpegData bytes.Buffer
	if err := jpeg.Encode(&jpegData, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	history := []messages.ChatMessage{{
		Role: messages.MessageRoleUser,
		Parts: []messages.ContentPart{
			{Type: "image_base64", ImageData: base64.StdEncoding.EncodeToString(jpegData.Bytes()), MimeType: "image/jpg", FileName: "legacy.jpg"},
			{Type: "image_base64", ImageData: base64.StdEncoding.EncodeToString([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><circle r="2"/></svg>`)), MimeType: "image/svg+xml", FileName: "legacy.svg"},
		},
	}}

	prepared, err := preparePortableImageRequest(history)
	if err != nil {
		t.Fatal(err)
	}
	if got := history[0].Parts[0].MimeType; got != "image/jpg" {
		t.Fatalf("request migration mutated durable history MIME to %q", got)
	}
	jpegPart := prepared[0].Parts[0]
	data, err := base64.StdEncoding.DecodeString(jpegPart.ImageData)
	if err != nil {
		t.Fatal(err)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if jpegPart.MimeType != "image/jpeg" || config.Width != 1568 || config.Height != 784 {
		t.Fatalf("legacy JPEG upgrade = %s %dx%d", jpegPart.MimeType, config.Width, config.Height)
	}
	svgPart := prepared[0].Parts[1]
	if svgPart.Type != "text" || svgPart.FileName != "legacy.svg" || svgPart.Text != "[legacy SVG image omitted: legacy.svg]" {
		t.Fatalf("legacy SVG upgrade = %#v", svgPart)
	}
}

func jpegWithEXIFOrientation(t *testing.T, jpegData []byte, orientation uint16) []byte {
	t.Helper()
	if len(jpegData) < 2 || jpegData[0] != 0xff || jpegData[1] != 0xd8 {
		t.Fatal("fixture is not a JPEG")
	}
	var tiff bytes.Buffer
	tiff.WriteString("II")
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(42))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(8))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(1))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0x0112))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(3))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(1))
	_ = binary.Write(&tiff, binary.LittleEndian, orientation)
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(0))
	payload := append([]byte{'E', 'x', 'i', 'f', 0, 0}, tiff.Bytes()...)
	if len(payload)+2 > 0xffff {
		t.Fatal("EXIF fixture is too large")
	}
	segment := []byte{0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(segment[2:], uint16(len(payload)+2))
	segment = append(segment, payload...)
	out := make([]byte, 0, len(jpegData)+len(segment))
	out = append(out, jpegData[:2]...)
	out = append(out, segment...)
	out = append(out, jpegData[2:]...)
	return out
}

func TestPrepareImageForUploadRejectsOversizedFileBeforeReading(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized.png")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(int64(maxLocalImageBytes) + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareImageForUpload(path); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized attachment error = %v", err)
	}
	if _, err := loadLocalImage(path); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized display image error = %v", err)
	}
}

func TestPrepareImageForUploadNormalizesGIFAndBMPToPNG(t *testing.T) {
	dir := t.TempDir()
	palette := color.Palette{
		color.RGBA{R: 255, A: 255},
		color.RGBA{B: 255, A: 255},
	}
	first := image.NewPaletted(image.Rect(0, 0, 3, 2), palette)
	second := image.NewPaletted(image.Rect(0, 0, 3, 2), palette)
	for i := range second.Pix {
		second.Pix[i] = 1
	}

	gifPath := filepath.Join(dir, "animated.gif")
	gifFile, err := os.Create(gifPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := gif.EncodeAll(gifFile, &gif.GIF{Image: []*image.Paletted{first, second}, Delay: []int{1, 1}}); err != nil {
		gifFile.Close()
		t.Fatal(err)
	}
	if err := gifFile.Close(); err != nil {
		t.Fatal(err)
	}

	bmpPath := filepath.Join(dir, "still.bmp")
	bmpImage := image.NewRGBA(image.Rect(0, 0, 3, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 3; x++ {
			bmpImage.Set(x, y, color.RGBA{G: 255, A: 255})
		}
	}
	bmpFile, err := os.Create(bmpPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := bmp.Encode(bmpFile, bmpImage); err != nil {
		bmpFile.Close()
		t.Fatal(err)
	}
	if err := bmpFile.Close(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		path string
		want color.RGBA
	}{
		{name: "GIF first frame", path: gifPath, want: color.RGBA{R: 255, A: 255}},
		{name: "BMP", path: bmpPath, want: color.RGBA{G: 255, A: 255}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			part, err := prepareImageForUpload(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			if part.MimeType != "image/png" {
				t.Fatalf("normalized MIME = %q, want image/png", part.MimeType)
			}
			data, err := base64.StdEncoding.DecodeString(part.ImageData)
			if err != nil {
				t.Fatal(err)
			}
			img, format, err := image.Decode(bytes.NewReader(data))
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

func TestPrepareImageForUploadKeepsDownscaledGIFAsPNG(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wide.gif")
	palette := color.Palette{color.RGBA{R: 255, A: 255}, color.RGBA{B: 255, A: 255}}
	frame := image.NewPaletted(image.Rect(0, 0, 2000, 500), palette)
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := gif.Encode(file, frame, nil); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	part, err := prepareImageForUpload(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(part.ImageData)
	if err != nil {
		t.Fatal(err)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if part.MimeType != "image/png" || format != "png" || config.Width != uploadMaxLongEdge || config.Height != 392 {
		t.Fatalf("downscaled GIF = mime %q format %q %dx%d", part.MimeType, format, config.Width, config.Height)
	}
}

func TestPrepareImageForUploadShrinksBMPWithoutJPEGFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "noisy.bmp")
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
	var originalPNG bytes.Buffer
	if err := png.Encode(&originalPNG, img); err != nil {
		t.Fatal(err)
	}
	if originalPNG.Len() <= uploadMaxBytes {
		t.Fatalf("fixture PNG is %d bytes; want more than %d to exercise iterative shrinking", originalPNG.Len(), uploadMaxBytes)
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

	part, err := prepareImageForUpload(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(part.ImageData)
	if err != nil {
		t.Fatal(err)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if part.MimeType != "image/png" || format != "png" {
		t.Fatalf("size-limited BMP = mime %q format %q, want PNG", part.MimeType, format)
	}
	if len(data) > uploadMaxBytes {
		t.Fatalf("size-limited BMP is %d bytes, want at most %d", len(data), uploadMaxBytes)
	}
	if config.Width >= size || config.Height >= size {
		t.Fatalf("size-limited BMP stayed %dx%d; iterative shrinking did not run", config.Width, config.Height)
	}
}

func TestPreparedMessageTranscriptImagesCacheAcceptedBytes(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheRoot)

	dir := t.TempDir()
	source := filepath.Join(dir, "accepted.png")
	writeImageFixture(t, source, 7, 5)
	msg, err := buildREPLUserMessage("inspect", []composerAttachment{{Path: source, Label: "accepted.png"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Parts) != 2 {
		t.Fatalf("prepared parts = %+v", msg.Parts)
	}
	wantBytes, err := base64.StdEncoding.DecodeString(msg.Parts[1].ImageData)
	if err != nil {
		t.Fatal(err)
	}

	first := preparedMessageTranscriptImages(msg)
	if len(first) != 1 {
		t.Fatalf("first materialization = %+v", first)
	}
	wantDir := filepath.Join(cacheRoot, "pollytool", "attachments")
	if filepath.Dir(first[0].Path) != wantDir || filepath.Ext(first[0].Path) != ".png" {
		t.Fatalf("materialized path = %q, want PNG under %q", first[0].Path, wantDir)
	}
	gotBytes, err := os.ReadFile(first[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBytes, wantBytes) {
		t.Fatal("cached preview bytes differ from the accepted message payload")
	}

	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	second := preparedMessageTranscriptImages(msg)
	if len(second) != 1 || second[0].Path != first[0].Path {
		t.Fatalf("reused materialization = %+v, want path %q", second, first[0].Path)
	}
	gotBytes, err = os.ReadFile(second[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBytes, wantBytes) {
		t.Fatal("reused cache entry no longer matches the accepted bytes")
	}
}

func TestBuildREPLUserMessage(t *testing.T) {
	msg, err := buildREPLUserMessage("plain prompt", nil)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "plain prompt" || len(msg.Parts) != 0 {
		t.Fatalf("text-only message = %+v", msg)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	writeImageFixture(t, path, 8, 8)
	msg, err = buildREPLUserMessage("look [image #1]", []composerAttachment{{Path: path, Label: "shot.png"}})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "" || len(msg.Parts) != 2 {
		t.Fatalf("multimodal message shape = %+v", msg)
	}
	if msg.Parts[0].Type != "text" || msg.Parts[0].Text != "look [image #1]" {
		t.Fatalf("leading text part = %+v", msg.Parts[0])
	}
	if msg.Parts[1].Type != "image_base64" || msg.Parts[1].MimeType != "image/png" {
		t.Fatalf("image part = %+v", msg.Parts[1])
	}

	gone := filepath.Join(dir, "gone.png")
	writeImageFixture(t, gone, 8, 8)
	m := newReplModel()
	goneToken := m.registerAttachment(gone, "gone.png")
	attachments, err := m.promptAttachments("inspect " + goneToken)
	if err != nil || len(attachments) != 1 {
		t.Fatalf("resolved attachment = %+v, %v", attachments, err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if _, err := buildREPLUserMessage("inspect "+goneToken, attachments); err == nil {
		t.Fatal("attachment removed after resolution should fail the build")
	}
}

func TestBeginManagedTurnEchoesPreparedAttachmentThumbnails(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	writeImageFixture(t, path, 8, 8)

	m := newReplModel()
	token := m.registerAttachment(path, "shot.png")
	prompt := "describe " + token + " " + string(transcriptImageMarker(0))
	attachments, err := m.promptAttachments(prompt)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := buildREPLUserMessage(prompt, attachments)
	if err != nil {
		t.Fatal(err)
	}
	m.beginManagedTurn(managedTurnInput{displayText: prompt, userMessage: msg})

	idx := len(m.transcript) - 1
	entry := m.transcript[idx]
	if !strings.Contains(entry, "image: shot.png") {
		t.Fatalf("user echo lacks attachment caption: %q", entry)
	}
	if !strings.ContainsRune(entry, transcriptImageMarker(0)) {
		t.Fatal("user echo lacks thumbnail slot markers")
	}
	// The prompt's own private-use rune was stripped; only the slot rows carry
	// marker runes, all below the caption line.
	if strings.ContainsRune(strings.SplitN(entry, "\n", 2)[0], transcriptImageMarker(0)) {
		t.Fatal("pasted marker rune survived in the echoed prompt line")
	}
	imgs := m.transcriptImages[idx]
	if len(imgs) != 1 || imgs[0].DisplayPath != "shot.png" || imgs[0].Path == path {
		t.Fatalf("sidecar images = %+v", imgs)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(imgs[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("rendered preview does not use the prepared attachment bytes")
	}
}

func TestClipboardImageCommands(t *testing.T) {
	dest := "/cache/clip-1.png"
	has := func(names ...string) func(string) (string, error) {
		return func(name string) (string, error) {
			for _, n := range names {
				if n == name {
					return "/bin/" + name, nil
				}
			}
			return "", errors.New("not found")
		}
	}
	env := func(pairs map[string]string) func(string) string {
		return func(key string) string { return pairs[key] }
	}

	cmds := clipboardImageCommands("darwin", env(nil), has("pngpaste"), dest)
	if len(cmds) != 3 || cmds[0].argv[0] != "pngpaste" || cmds[1].argv[0] != "osascript" {
		t.Fatalf("darwin with pngpaste = %+v", cmds)
	}
	cmds = clipboardImageCommands("darwin", env(nil), has(), dest)
	if len(cmds) != 2 || cmds[0].argv[0] != "osascript" {
		t.Fatalf("darwin without pngpaste = %+v", cmds)
	}
	cmds = clipboardImageCommands("linux", env(map[string]string{"WAYLAND_DISPLAY": "wayland-0"}), has("wl-paste", "xclip"), dest)
	if len(cmds) != 2 || cmds[0].argv[0] != "wl-paste" || !cmds[0].toStdout || cmds[1].argv[0] != "xclip" {
		t.Fatalf("wayland linux = %+v", cmds)
	}
	cmds = clipboardImageCommands("linux", env(nil), has("xclip"), dest)
	if len(cmds) != 1 || cmds[0].argv[0] != "xclip" {
		t.Fatalf("x11 linux = %+v", cmds)
	}
	if cmds = clipboardImageCommands("linux", env(nil), has(), dest); len(cmds) != 0 {
		t.Fatalf("bare linux should have no candidates, got %+v", cmds)
	}
	cmds = clipboardImageCommands("windows", env(nil), has(), dest)
	if len(cmds) != 1 || cmds[0].argv[0] != "powershell" {
		t.Fatalf("windows = %+v", cmds)
	}
}

func TestReplAttachCommand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "board layout.png")
	writeImageFixture(t, path, 8, 8)

	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	ctx := newManagedReplCommandContext(r)

	handled, quit := r.runCommand("/attach '" + path + "'")
	if !handled || quit {
		t.Fatalf("handled=%v quit=%v", handled, quit)
	}
	if got := r.model.ed.text(); got != "[image #1] " {
		t.Fatalf("editor after /attach = %q", got)
	}

	res := replAttachCommand(ctx, []string{"/attach", filepath.Join(dir, "missing.png")})
	if res.quit || res.err != nil {
		t.Fatalf("attach error path should reply, not fail: %+v", res)
	}
	if got := r.model.ed.text(); got != "[image #1] " {
		t.Fatalf("failed attach must not touch the editor, got %q", got)
	}
}

func TestStripTranscriptImageMarkers(t *testing.T) {
	in := "a" + string(transcriptImageMarker(0)) + "b" + string(transcriptImageMarker(255)) + "c\uE200d"
	if got := stripTranscriptImageMarkers(in); got != "abc\uE200d" {
		t.Fatalf("stripTranscriptImageMarkers = %q", got)
	}
}
