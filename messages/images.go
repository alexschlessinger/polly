package messages

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"io"
	"path/filepath"
	"strings"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/images"
)

// The portable image contract: the request shape every native multimodal
// client accepts. Inline images are prepared PNG, JPEG, or WebP bytes within
// the upload bounds, and a request carries a bounded number of them.
const (
	// MaxImagesPerMessage bounds the images one message may carry. It is a
	// sanity cap, not a provider limit.
	MaxImagesPerMessage = 16
	// MaxEncodedImageHistoryBytes keeps the projected inline request safely
	// below the tightest native-client request ceiling while leaving room
	// for JSON and conversation text.
	MaxEncodedImageHistoryBytes = 16 << 20
	// MaxPortableEncodedImageBytes is Anthropic's direct-API cap on each
	// base64-encoded image. It is the decimal limit (not 10 MiB) so the
	// shared request shape remains portable across direct and compatible
	// endpoints.
	MaxPortableEncodedImageBytes = 10_000_000
	// MaxPortableRequestImages is the smallest native-client request limit:
	// Anthropic's 200k-context models accept 100 images across the entire
	// request, including earlier turns.
	MaxPortableRequestImages = 100
)

// Clone returns a copy of m that shares no parts, tool calls, artifact
// references, or metadata with the original.
func (m ChatMessage) Clone() ChatMessage {
	m.Parts = append([]ContentPart(nil), m.Parts...)
	for i := range m.Parts {
		if m.Parts[i].Artifact != nil {
			ref := *m.Parts[i].Artifact
			m.Parts[i].Artifact = &ref
		}
	}
	m.ToolCalls = append([]ChatMessageToolCall(nil), m.ToolCalls...)
	if m.Metadata != nil {
		metadata := make(map[string]any, len(m.Metadata))
		for key, value := range m.Metadata {
			metadata[key] = value
		}
		m.Metadata = metadata
	}
	return m
}

// ModelVisible returns the messages of history a provider may see: every
// message that is not an internal marker.
func ModelVisible(history []ChatMessage) []ChatMessage {
	visible := make([]ChatMessage, 0, len(history))
	for _, msg := range history {
		if msg.Role != MessageRoleInternal {
			visible = append(visible, msg)
		}
	}
	return visible
}

// ImagePart applies the portable image contract to raw bytes at an ingestion
// boundary, before they enter durable history. fileName is display metadata
// only; the format is always detected from the bytes.
func ImagePart(data []byte, fileName string) (ContentPart, error) {
	norm, err := images.NormalizeForModel(data, fileName)
	if err != nil {
		return ContentPart{}, err
	}
	return ContentPart{
		Type:      "image_base64",
		ImageData: base64.StdEncoding.EncodeToString(norm.Data),
		MimeType:  norm.MIMEType,
		FileName:  norm.FileName,
	}, nil
}

// PortableImagePart returns part unchanged unless it is an inline image that
// fails the portable contract, in which case it returns the normalized
// upgrade with the original Reference carried over. Callers keep their own
// failure policy; this is the one place the "upgrade if nonportable" test
// lives.
func PortableImagePart(part ContentPart) (ContentPart, error) {
	upgraded, _, err := portableImagePart(part)
	return upgraded, err
}

func portableImagePart(part ContentPart) (upgraded ContentPart, changed bool, err error) {
	if part.Type != "image_base64" || ValidatePortableImagePart(part) == nil {
		return part, false, nil
	}
	if upgraded, err = upgradeLegacyImagePart(part); err != nil {
		return ContentPart{}, false, err
	}
	upgraded.Reference = part.Reference
	return upgraded, true, nil
}

func upgradeLegacyImagePart(part ContentPart) (ContentPart, error) {
	decoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(part.ImageData))
	data, err := io.ReadAll(io.LimitReader(decoder, int64(images.MaxSourceBytes)+1))
	if err != nil || len(data) == 0 {
		return ContentPart{}, fmt.Errorf("invalid or empty base64 data")
	}
	if len(data) > images.MaxSourceBytes {
		return ContentPart{}, fmt.Errorf("decoded image exceeds the %d MiB preparation limit", images.MaxSourceBytes>>20)
	}
	if strings.EqualFold(strings.TrimSpace(part.MimeType), "image/svg+xml") || strings.EqualFold(filepath.Ext(part.FileName), ".svg") {
		label := strings.TrimSpace(part.FileName)
		if label == "" {
			label = "legacy.svg"
		}
		return ContentPart{Type: "text", Text: "[legacy SVG image omitted: " + label + "]", FileName: part.FileName}, nil
	}
	return ImagePart(data, part.FileName)
}

// ValidatePortableImagePart reports why an inline image part fails the
// portable contract: encoded size, decodability, dimensions, or a MIME type
// that does not match its bytes.
func ValidatePortableImagePart(part ContentPart) error {
	if len(part.ImageData) > MaxPortableEncodedImageBytes {
		return fmt.Errorf("encoded image uses %d bytes; per-image portable limit is 10,000,000 bytes (10 MB)", len(part.ImageData))
	}
	data, err := base64.StdEncoding.DecodeString(part.ImageData)
	if err != nil || len(data) == 0 {
		return fmt.Errorf("invalid or empty base64 data")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return fmt.Errorf("invalid raster image data")
	}
	if max(config.Width, config.Height) > images.UploadMaxLongEdge ||
		int64(config.Width)*int64(config.Height) > images.MaxSourcePixels {
		return fmt.Errorf("image dimensions %dx%d exceed the prepared-image bounds", config.Width, config.Height)
	}
	if _, decodedFormat, err := image.Decode(bytes.NewReader(data)); err != nil || decodedFormat != format {
		return fmt.Errorf("invalid %s image data", format)
	}
	wantMIME, ok := images.PortableMIMEType(format)
	if !ok {
		return fmt.Errorf("unsupported image format %q", format)
	}
	if part.MimeType != wantMIME {
		return fmt.Errorf("image MIME %q does not match %q bytes", part.MimeType, format)
	}
	return nil
}

// NormalizeImages upgrades every legacy inline image in history that can be
// upgraded and leaves the rest as they are. Messages are copied only when a
// part changes, so history itself is never mutated.
func NormalizeImages(history []ChatMessage) []ChatMessage {
	normalized, _ := normalizeImages(history, false)
	return normalized
}

// PortableImageRequest returns history with every legacy inline image
// upgraded, or the reason it cannot be a portable request: an oversized
// encoded budget, an image that cannot be normalized, or a shape no native
// client accepts. history itself is never mutated.
func PortableImageRequest(history []ChatMessage) ([]ChatMessage, error) {
	// Reject an oversized request before decoding legacy base64 into a
	// second in-memory copy.
	if err := ValidateEncodedImageBudget(history); err != nil {
		return nil, err
	}
	normalized, err := normalizeImages(history, true)
	if err != nil {
		return nil, err
	}
	if err := ValidatePortableImageRequest(normalized); err != nil {
		return nil, err
	}
	return normalized, nil
}

func normalizeImages(history []ChatMessage, strict bool) ([]ChatMessage, error) {
	normalized := make([]ChatMessage, len(history))
	for i, msg := range history {
		normalized[i] = msg
		cloned := false
		for j, part := range msg.Parts {
			upgraded, changed, err := portableImagePart(part)
			if err != nil {
				if strict {
					return nil, fmt.Errorf("model-visible message %d has a legacy image that cannot be normalized: %w", i+1, err)
				}
				continue
			}
			if !changed {
				continue
			}
			if !cloned {
				normalized[i], cloned = msg.Clone(), true
			}
			normalized[i].Parts[j] = upgraded
		}
	}
	return normalized, nil
}

// ValidateImageMessage checks one prepared message against the portable
// contract, counting stored image artifacts toward its per-message cap.
func ValidateImageMessage(msg ChatMessage) error {
	if _, err := PortableImageRequest([]ChatMessage{msg}); err != nil {
		return err
	}
	count := 0
	for _, part := range msg.Parts {
		if part.Type == "image_base64" || part.Type == "image_url" ||
			(part.Artifact != nil && part.Artifact.Kind == artifacts.KindImage) {
			count++
		}
	}
	if count > MaxImagesPerMessage {
		return fmt.Errorf("model-visible message 1 has %d images; portable maximum is %d", count, MaxImagesPerMessage)
	}
	return nil
}

// ValidatePortableImageRequest enforces the common request shape accepted by
// every native multimodal client. It deliberately validates the whole visible
// history, not only the candidate: a legacy image in an earlier turn is
// replayed to the provider too and can otherwise poison a restored draft or a
// new prompt.
func ValidatePortableImageRequest(history []ChatMessage) error {
	if err := ValidateEncodedImageBudget(history); err != nil {
		return err
	}
	totalImages := 0
	for messageIndex, msg := range history {
		imageCount := 0
		for _, part := range msg.Parts {
			switch part.Type {
			case "image_base64":
				imageCount++
				totalImages++
				if imageCount > MaxImagesPerMessage {
					return fmt.Errorf("model-visible message %d has %d images; portable maximum is %d", messageIndex+1, imageCount, MaxImagesPerMessage)
				}
				if totalImages > MaxPortableRequestImages {
					return fmt.Errorf("model-visible history has %d images; portable request maximum is %d", totalImages, MaxPortableRequestImages)
				}
				if err := ValidatePortableImagePart(part); err != nil {
					return fmt.Errorf("model-visible message %d has a nonportable image: %w", messageIndex+1, err)
				}
			case "image_url":
				return fmt.Errorf("model-visible message %d has an image URL; portable requests require prepared PNG, JPEG, or WebP bytes", messageIndex+1)
			}
		}
	}
	return nil
}

// ValidateEncodedImageBudget rejects a history whose inline images, base64
// parts and data URLs alike, would exceed the portable request budget.
func ValidateEncodedImageBudget(history []ChatMessage) error {
	total := 0
	for _, msg := range history {
		for _, part := range msg.Parts {
			switch part.Type {
			case "image_base64":
				total += len(part.ImageData)
			case "image_url":
				if strings.HasPrefix(part.ImageURL, "data:") {
					if comma := strings.IndexByte(part.ImageURL, ','); comma >= 0 {
						total += len(part.ImageURL) - comma - 1
					}
				}
			}
			if total > MaxEncodedImageHistoryBytes {
				return fmt.Errorf("encoded images in model-visible history would use %d bytes; portable limit is %d MiB", total, MaxEncodedImageHistoryBytes>>20)
			}
		}
	}
	return nil
}
