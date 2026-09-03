// Package images applies the portable image contract shared by every
// ingestion boundary: user attachments in cmd/polly and model-initiated views
// through the view_image tool. Bytes that pass NormalizeForModel are safe to
// enter durable history and provider requests.
package images

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
	"path/filepath"
	"strings"

	_ "image/gif"

	_ "golang.org/x/image/bmp"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

const (
	// MaxSourceBytes and MaxSourcePixels bound source material before any
	// caller fully decodes image data.
	MaxSourceBytes  = 32 << 20
	MaxSourcePixels = 40_000_000

	// Provider-bound images are downscaled so an offhand screenshot does not
	// ship megabytes of pixels the model cannot use. 1568px is the largest
	// useful long edge for current vision models; the byte cap stays under
	// typical per-image API limits with room for base64 growth.
	UploadMaxLongEdge = 1568
	UploadMaxBytes    = 4 << 20
)

// Validate applies the common encoded-size and decoded-pixel bounds before any
// caller fully decodes image data.
func Validate(data []byte) (image.Config, string, error) {
	if len(data) == 0 || len(data) > MaxSourceBytes {
		return image.Config{}, "", fmt.Errorf("image size is outside the supported range")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return image.Config{}, "", fmt.Errorf("unsupported image format or invalid image data: %w", err)
	}
	if config.Width <= 0 || config.Height <= 0 ||
		int64(config.Width)*int64(config.Height) > MaxSourcePixels {
		return image.Config{}, "", fmt.Errorf("image dimensions are outside the supported range")
	}
	return config, format, nil
}

// Normalized is a portable model-ready image: PNG/JPEG/WebP bytes within the
// upload dimension and byte caps.
type Normalized struct {
	Data     []byte
	MIMEType string
	FileName string
	Width    int
	Height   int
}

// NormalizeForModel validates raster bytes and converts them to the portable
// upload shape. fileName is display metadata only; the format is always
// detected from the bytes.
func NormalizeForModel(data []byte, fileName string) (Normalized, error) {
	fileName = filepath.Base(strings.TrimSpace(fileName))
	if fileName == "" || fileName == "." || fileName == string(filepath.Separator) {
		fileName = "attachment"
	}
	config, format, err := Validate(data)
	if err != nil {
		return Normalized{}, fmt.Errorf("%s: %w", fileName, err)
	}

	mimeType, passthrough := PortableMIMEType(format)
	var src image.Image
	if !passthrough {
		switch format {
		case "gif", "bmp":
			src, _, err = image.Decode(bytes.NewReader(data))
			if err != nil {
				return Normalized{}, fmt.Errorf("%s: invalid %s image data", fileName, format)
			}
			var normalized bytes.Buffer
			if err := png.Encode(&normalized, src); err != nil {
				return Normalized{}, fmt.Errorf("%s: encode normalized PNG: %w", fileName, err)
			}
			data = normalized.Bytes()
			mimeType = "image/png"
		default:
			return Normalized{}, fmt.Errorf("%s: unsupported image format %q", fileName, format)
		}
	}

	if max(config.Width, config.Height) <= UploadMaxLongEdge && len(data) <= UploadMaxBytes {
		return Normalized{Data: data, MIMEType: mimeType, FileName: fileName, Width: config.Width, Height: config.Height}, nil
	}

	if src == nil {
		src, _, err = image.Decode(bytes.NewReader(data))
		if err != nil {
			return Normalized{}, fmt.Errorf("%s: invalid image data: %w", fileName, err)
		}
	}
	if format == "jpeg" {
		src = ApplyEXIFOrientation(src, JPEGOrientation(data))
	}
	scaled := Fit(src, UploadMaxLongEdge, UploadMaxLongEdge)

	encoded, mimeType, err := encodeUploadImage(scaled, format)
	if err != nil {
		return Normalized{}, fmt.Errorf("%s: encode image: %w", fileName, err)
	}
	bounds := scaled.Bounds()
	return Normalized{Data: encoded, MIMEType: mimeType, FileName: fileName, Width: bounds.Dx(), Height: bounds.Dy()}, nil
}

// Fit scales src down to fit within maxWidth x maxHeight, preserving aspect
// ratio. Images already within bounds are returned unchanged.
func Fit(src image.Image, maxWidth, maxHeight int) image.Image {
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 || maxWidth <= 0 || maxHeight <= 0 {
		return src
	}
	targetWidth, targetHeight := FitDimensions(width, height, maxWidth, maxHeight)
	if targetWidth == width && targetHeight == height && bounds.Min.X == 0 && bounds.Min.Y == 0 {
		return src
	}
	dst := image.NewNRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, bounds, xdraw.Over, nil)
	return dst
}

// FitDimensions returns the aspect-preserving dimensions that fit width x
// height within maxWidth x maxHeight.
func FitDimensions(width, height, maxWidth, maxHeight int) (int, int) {
	if width <= 0 || height <= 0 || maxWidth <= 0 || maxHeight <= 0 {
		return 0, 0
	}
	scale := math.Min(float64(maxWidth)/float64(width), float64(maxHeight)/float64(height))
	return max(1, int(math.Round(float64(width)*scale))), max(1, int(math.Round(float64(height)*scale)))
}

func encodeUploadImage(img image.Image, sourceFormat string) ([]byte, string, error) {
	pngOnly := sourceFormat == "gif" || sourceFormat == "bmp"
	current := img
	for {
		data, mimeType, err := encodeUploadImageAttempt(current, sourceFormat, pngOnly)
		if err != nil {
			return nil, "", err
		}
		if len(data) <= UploadMaxBytes {
			return data, mimeType, nil
		}

		bounds := current.Bounds()
		width, height := bounds.Dx(), bounds.Dy()
		if width <= 1 && height <= 1 {
			return nil, "", fmt.Errorf("image cannot be encoded within the %d-byte upload limit", UploadMaxBytes)
		}
		// Encoded size is approximately proportional to pixel area. Leave a
		// little margin so incompressible PNGs normally converge in one pass,
		// while the 0.9 ceiling guarantees progress for a near-limit image.
		scale := math.Sqrt(float64(UploadMaxBytes)/float64(len(data))) * 0.95
		if scale > 0.9 {
			scale = 0.9
		}
		if scale <= 0 || math.IsNaN(scale) || math.IsInf(scale, 0) {
			scale = 0.5
		}
		targetWidth := max(1, int(math.Floor(float64(width)*scale)))
		targetHeight := max(1, int(math.Floor(float64(height)*scale)))
		if targetWidth == width && width > 1 {
			targetWidth--
		}
		if targetHeight == height && height > 1 {
			targetHeight--
		}
		current = Fit(current, targetWidth, targetHeight)
	}
}

func encodeUploadImageAttempt(img image.Image, sourceFormat string, pngOnly bool) ([]byte, string, error) {
	if sourceFormat == "jpeg" {
		data, err := encodeJPEG(img)
		return data, "image/jpeg", err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, "", err
	}
	// A photographic PNG can stay huge after downscaling; JPEG is the only
	// remaining lever for native PNG/WebP input. Normalized GIF/BMP payloads
	// deliberately stay PNG and instead shrink further until they fit.
	if buf.Len() > UploadMaxBytes && !pngOnly {
		data, err := encodeJPEG(img)
		return data, "image/jpeg", err
	}
	return buf.Bytes(), "image/png", nil
}

func encodeJPEG(img image.Image) ([]byte, error) {
	bounds := img.Bounds()
	flat := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(flat, flat.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), img, bounds.Min, draw.Over)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, flat, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// PortableMIMEType maps a decoded image format name to the MIME type of the
// portable upload shape, and reports false for formats NormalizeForModel must
// convert or reject. It is the one table for what "portable" means.
func PortableMIMEType(format string) (mimeType string, portable bool) {
	switch format {
	case "png":
		return "image/png", true
	case "jpeg":
		return "image/jpeg", true
	case "webp":
		return "image/webp", true
	default:
		return "", false
	}
}
