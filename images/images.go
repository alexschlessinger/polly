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
	"io"
	"math"
	"os"
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

// formatSpec is one row of the portable contract: how NormalizeForModel
// treats bytes whose header decodes as that format name.
type formatSpec struct {
	// mime is the portable MIME type the bytes may ship under unchanged;
	// empty means they are always re-encoded as PNG.
	mime string
	// jpeg re-encodes as JPEG rather than PNG after a downscale.
	jpeg bool
	// jpegFallback lets a PNG encoding over UploadMaxBytes drop to JPEG
	// instead of shrinking further.
	jpegFallback bool
	// orientation reads the stored orientation that a re-encode would
	// discard; nil when the format's metadata is not read.
	orientation func(data []byte) int
}

// formats is the one table for what "portable" means: every format
// NormalizeForModel accepts, and what it does with each. A format missing
// here is rejected.
var formats = map[string]formatSpec{
	"png":  {mime: "image/png", jpegFallback: true},
	"jpeg": {mime: "image/jpeg", jpeg: true, orientation: JPEGOrientation},
	"webp": {mime: "image/webp", jpegFallback: true},
	"gif":  {},
	"bmp":  {},
}

// validate applies the encoded-size bound and decodeConfig's dimension bounds
// to in-memory image data before any caller fully decodes it.
func validate(data []byte) (image.Config, string, error) {
	if len(data) == 0 || len(data) > MaxSourceBytes {
		return image.Config{}, "", fmt.Errorf("image size is outside the supported range")
	}
	return decodeConfig(bytes.NewReader(data))
}

// decodeConfig parses an image header from r and applies the decoded-pixel
// bound without reading pixels.
func decodeConfig(r io.Reader) (image.Config, string, error) {
	config, format, err := image.DecodeConfig(r)
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
	config, format, err := validate(data)
	if err != nil {
		return Normalized{}, fmt.Errorf("%s: %w", fileName, err)
	}
	spec, known := formats[format]
	if !known {
		return Normalized{}, fmt.Errorf("%s: unsupported image format %q", fileName, format)
	}
	if spec.mime != "" && max(config.Width, config.Height) <= UploadMaxLongEdge && len(data) <= UploadMaxBytes {
		return Normalized{Data: data, MIMEType: spec.mime, FileName: fileName, Width: config.Width, Height: config.Height}, nil
	}

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return Normalized{}, fmt.Errorf("%s: invalid %s image data: %w", fileName, format, err)
	}
	img := Fit(src, UploadMaxLongEdge, UploadMaxLongEdge)
	// Passthrough above never touches the bytes, so stored metadata survives.
	// A re-encode discards it, so the orientation it described is baked into
	// the pixels first; doing that after Fit remaps the downscaled image.
	if spec.orientation != nil {
		img = ApplyEXIFOrientation(img, spec.orientation(data))
	}
	norm, err := encodeUploadImage(img, spec)
	if err != nil {
		return Normalized{}, fmt.Errorf("%s: encode image: %w", fileName, err)
	}
	norm.FileName = fileName
	return norm, nil
}

// Fit scales src down to fit within maxWidth x maxHeight, preserving aspect
// ratio. Images already within bounds are returned unchanged.
func Fit(src image.Image, maxWidth, maxHeight int) image.Image {
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	targetWidth, targetHeight := FitDimensions(width, height, maxWidth, maxHeight)
	if targetWidth == 0 || (targetWidth == width && targetHeight == height) {
		return src
	}
	dst := image.NewNRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, bounds, xdraw.Src, nil)
	return dst
}

// FitDimensions returns the aspect-preserving dimensions that fit width x
// height within maxWidth x maxHeight, or 0 x 0 when any input is not positive.
func FitDimensions(width, height, maxWidth, maxHeight int) (int, int) {
	if width <= 0 || height <= 0 || maxWidth <= 0 || maxHeight <= 0 {
		return 0, 0
	}
	scale := math.Min(float64(maxWidth)/float64(width), float64(maxHeight)/float64(height))
	return max(1, int(math.Round(float64(width)*scale))), max(1, int(math.Round(float64(height)*scale)))
}

// encodeUploadImage encodes img within UploadMaxBytes, shrinking it until the
// encoding fits, and reports the dimensions actually encoded.
func encodeUploadImage(img image.Image, spec formatSpec) (Normalized, error) {
	for {
		data, mimeType, err := encodeUploadImageAttempt(img, spec)
		if err != nil {
			return Normalized{}, err
		}
		bounds := img.Bounds()
		width, height := bounds.Dx(), bounds.Dy()
		if len(data) <= UploadMaxBytes {
			return Normalized{Data: data, MIMEType: mimeType, Width: width, Height: height}, nil
		}
		if width <= 1 && height <= 1 {
			return Normalized{}, fmt.Errorf("image cannot be encoded within the %d-byte upload limit", UploadMaxBytes)
		}
		// Encoded size is roughly proportional to pixel area, so the square
		// root of the overshoot with a little margin normally converges in
		// one pass; the 0.9 ceiling guarantees progress for a near-limit image.
		scale := min(0.9, 0.95*math.Sqrt(float64(UploadMaxBytes)/float64(len(data))))
		img = Fit(img, max(1, int(float64(width)*scale)), max(1, int(float64(height)*scale)))
	}
}

func encodeUploadImageAttempt(img image.Image, spec formatSpec) ([]byte, string, error) {
	if spec.jpeg {
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
	if buf.Len() > UploadMaxBytes && spec.jpegFallback {
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
// convert or reject.
func PortableMIMEType(format string) (mimeType string, portable bool) {
	mimeType = formats[format].mime
	return mimeType, mimeType != ""
}

// FileVersion identifies the current pixels behind a path: the path with its
// size and modification time, or a missing marker when it cannot be read.
func FileVersion(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return path + ":missing"
	}
	return fmt.Sprintf("%s:%d:%d", path, info.Size(), info.ModTime().UnixNano())
}
