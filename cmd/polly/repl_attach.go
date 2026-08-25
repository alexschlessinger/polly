package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/alexschlessinger/pollytool/messages"
)

// Composer attachments follow the token-in-text model: attaching an image
// inserts a literal "[image #N]" token at the cursor and records N → path in a
// session-scoped registry. The prompt stays an ordinary string through history,
// the queue, and /retry; tokens resolve to files only when a turn starts.
// Deleting a token from the composer is how an attachment is dropped.

const (
	// maxPromptAttachments bounds how many images one prompt can carry. It is a
	// sanity cap, not a provider limit; token scans and paste conversion both
	// honor it.
	maxPromptAttachments = 16

	// Provider-bound images are downscaled so an offhand screenshot does not
	// ship megabytes of pixels the model cannot use. 1568px is the largest
	// useful long edge for current vision models; the byte cap stays under
	// typical per-image API limits with room for base64 growth.
	uploadMaxLongEdge = 1568
	uploadMaxBytes    = 4 << 20
)

type composerAttachment struct {
	Path  string
	Label string
}

var attachmentTokenPattern = regexp.MustCompile(`\[image #([0-9]+)\]`)

func attachmentToken(n int) string {
	return fmt.Sprintf("[image #%d]", n)
}

// registerAttachment records a validated image path and returns the composer
// token that now names it. Numbers are never reused within a session, so a
// token in history or the queue keeps meaning the same file. Caller must hold
// m.mu.
func (m *replModel) registerAttachment(path, label string) string {
	if m.attachments == nil {
		m.attachments = make(map[int]composerAttachment)
	}
	m.attachmentSeq++
	m.attachments[m.attachmentSeq] = composerAttachment{Path: path, Label: label}
	return attachmentToken(m.attachmentSeq)
}

// promptAttachments resolves everything a prompt references, in appearance
// order, deduplicated by path: "[image #N]" tokens through the session
// registry, and bare typed paths — any whitespace-delimited word with an image
// extension that resolves to an existing local file. Tokens that were never
// registered in this session stay ordinary text, as does prose that merely
// looks path-shaped. Caller must hold m.mu.
func (m *replModel) promptAttachments(prompt string) []composerAttachment {
	type ref struct {
		pos int
		att composerAttachment
	}
	var refs []ref

	var tokenSpans [][2]int
	if len(m.attachments) > 0 && strings.Contains(prompt, "[image #") {
		for _, loc := range attachmentTokenPattern.FindAllStringSubmatchIndex(prompt, -1) {
			n, err := strconv.Atoi(prompt[loc[2]:loc[3]])
			if err != nil {
				continue
			}
			att, ok := m.attachments[n]
			if !ok {
				continue
			}
			tokenSpans = append(tokenSpans, [2]int{loc[0], loc[1]})
			refs = append(refs, ref{pos: loc[0], att: att})
		}
	}

	for _, word := range splitPromptWords(prompt) {
		inToken := false
		for _, span := range tokenSpans {
			if word.pos >= span[0] && word.pos < span[1] {
				inToken = true
				break
			}
		}
		if inToken {
			continue
		}
		candidate := trimPromptPathPunctuation(word.text)
		// The extension check is a cheap prefilter so ordinary prose never
		// touches the filesystem.
		if candidate == "" || !supportedLocalImageExtension(candidate) {
			continue
		}
		img, ok := resolveLocalTranscriptImage(candidate, "", m.imageBaseDir)
		if !ok {
			continue
		}
		refs = append(refs, ref{pos: word.pos, att: composerAttachment{Path: img.Path, Label: filepath.Base(img.Path)}})
	}

	if len(refs) == 0 {
		return nil
	}
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].pos < refs[j].pos })
	var out []composerAttachment
	seen := make(map[string]struct{}, len(refs))
	for _, r := range refs {
		if _, dup := seen[r.att.Path]; dup {
			continue
		}
		seen[r.att.Path] = struct{}{}
		out = append(out, r.att)
		if len(out) >= maxPromptAttachments {
			break
		}
	}
	return out
}

type promptWord struct {
	pos  int
	text string
}

func splitPromptWords(s string) []promptWord {
	var words []promptWord
	start := -1
	for i, r := range s {
		if unicode.IsSpace(r) {
			if start >= 0 {
				words = append(words, promptWord{pos: start, text: s[start:i]})
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		words = append(words, promptWord{pos: start, text: s[start:]})
	}
	return words
}

// trimPromptPathPunctuation peels the punctuation prose hangs on a mentioned
// path — "(.assets/polly.png)," — without touching the path's own characters.
func trimPromptPathPunctuation(word string) string {
	return strings.TrimLeft(strings.TrimRight(word, ".,;:!?)]}>\"'`"), "([{<\"'`")
}

// promptAttachmentImages projects a prompt's attachments into transcript
// sidecar images for the user-echo thumbnail. Files that vanished since attach
// are skipped here; the turn itself still fails loudly when it cannot read
// them. Caller must hold m.mu.
func (m *replModel) promptAttachmentImages(prompt string) []transcriptImage {
	attachments := m.promptAttachments(prompt)
	if len(attachments) == 0 {
		return nil
	}
	images := make([]transcriptImage, 0, len(attachments))
	for _, att := range attachments {
		if img, ok := resolveLocalTranscriptImage(att.Path, att.Label, m.imageBaseDir); ok {
			images = append(images, img)
		}
	}
	return images
}

// splitDroppedPaths splits text into the path tokens a terminal drag-drop
// produces: plain, single- or double-quoted, or backslash-escaped-space paths,
// separated by whitespace. It returns nil when the text has quoting problems;
// whether the tokens are actually image paths is the caller's question.
func splitDroppedPaths(text string) []string {
	var paths []string
	runes := []rune(text)
	i := 0
	for i < len(runes) {
		switch r := runes[i]; {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			i++
		case r == '\'' || r == '"':
			quote := r
			end := i + 1
			for end < len(runes) && runes[end] != quote {
				end++
			}
			if end >= len(runes) {
				return nil
			}
			paths = append(paths, string(runes[i+1:end]))
			i = end + 1
		default:
			var b strings.Builder
			for i < len(runes) {
				r := runes[i]
				if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
					break
				}
				// Only an escaped blank is unescaped: that is what terminals
				// emit for dropped paths with spaces. Any other backslash is
				// path data (Windows separators).
				if r == '\\' && i+1 < len(runes) && (runes[i+1] == ' ' || runes[i+1] == '\t') {
					b.WriteRune(runes[i+1])
					i += 2
					continue
				}
				b.WriteRune(r)
				i++
			}
			paths = append(paths, b.String())
		}
	}
	return paths
}

// pastedImageAttachments interprets a bracketed paste as a terminal drag-drop:
// it converts only when the entire paste is nothing but existing local image
// paths. Prose that merely contains a path stays text. Caller must hold m.mu.
func (m *replModel) pastedImageAttachments(text string) []transcriptImage {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	paths := splitDroppedPaths(text)
	if len(paths) == 0 || len(paths) > maxPromptAttachments {
		return nil
	}
	images := make([]transcriptImage, 0, len(paths))
	for _, path := range paths {
		img, ok := resolveLocalTranscriptImage(path, "", m.imageBaseDir)
		if !ok {
			return nil
		}
		images = append(images, img)
	}
	return images
}

// attachmentCacheDir is where clipboard captures land. Files must outlive the
// paste (scrollback thumbnails read them on every draw) but not the machine;
// stale captures are swept on REPL start.
func attachmentCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "pollytool", "attachments")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

const attachmentCacheMaxAge = 14 * 24 * time.Hour

func sweepAttachmentCache(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if now.Sub(info.ModTime()) > attachmentCacheMaxAge {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}

// prepareImageForUpload converts a local image file into a provider-ready
// content part. Images already inside the size caps ship byte-for-byte
// (animated GIFs survive); oversized ones are downscaled and re-encoded, JPEG
// staying JPEG and everything else becoming PNG.
func prepareImageForUpload(path string) (*messages.ContentPart, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > maxLocalImageBytes {
		return nil, fmt.Errorf("%s: image size is outside the supported range", filepath.Base(path))
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > maxLocalImagePixels {
		return nil, fmt.Errorf("%s: image dimensions are outside the supported range", filepath.Base(path))
	}

	if max(config.Width, config.Height) <= uploadMaxLongEdge && len(data) <= uploadMaxBytes {
		return &messages.ContentPart{
			Type:      "image_base64",
			ImageData: base64.StdEncoding.EncodeToString(data),
			MimeType:  imageFormatMimeType(format),
			FileName:  filepath.Base(path),
		}, nil
	}

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	scaled := fitImage(src, uploadMaxLongEdge, uploadMaxLongEdge)

	encoded, mimeType, err := encodeUploadImage(scaled, format)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return &messages.ContentPart{
		Type:      "image_base64",
		ImageData: base64.StdEncoding.EncodeToString(encoded),
		MimeType:  mimeType,
		FileName:  filepath.Base(path),
	}, nil
}

func encodeUploadImage(img image.Image, sourceFormat string) ([]byte, string, error) {
	if sourceFormat == "jpeg" {
		data, err := encodeJPEG(img)
		return data, "image/jpeg", err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, "", err
	}
	// A photographic PNG can stay huge after downscaling; JPEG is the only
	// remaining lever. Transparency is flattened onto white.
	if buf.Len() > uploadMaxBytes {
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

func imageFormatMimeType(format string) string {
	switch format {
	case "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "bmp":
		return "image/bmp"
	default:
		return "image/png"
	}
}

// buildREPLUserMessage assembles the durable user message for a managed-REPL
// turn. Text-only prompts stay simple Content strings — the shape /retry and
// resumed-session summaries already understand — and attachments make the
// message multimodal with the prompt (tokens included) as its leading text
// part.
func buildREPLUserMessage(prompt string, attachments []composerAttachment) (messages.ChatMessage, error) {
	if len(attachments) == 0 {
		return messages.ChatMessage{Role: messages.MessageRoleUser, Content: prompt}, nil
	}
	parts := make([]messages.ContentPart, 0, len(attachments)+1)
	if prompt != "" {
		parts = append(parts, messages.ContentPart{Type: "text", Text: prompt})
	}
	for _, att := range attachments {
		part, err := prepareImageForUpload(att.Path)
		if err != nil {
			return messages.ChatMessage{}, err
		}
		parts = append(parts, *part)
	}
	return messages.ChatMessage{Role: messages.MessageRoleUser, Parts: parts}, nil
}
