package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/images"
	"github.com/alexschlessinger/pollytool/messages"
)

// Composer attachments follow the token-in-text model: attaching an image
// inserts a literal "[image #N]" token at the cursor and records N → path in a
// session-scoped registry. Accepting the prompt resolves those tokens into one
// durable message; queues and restored drafts carry that prepared payload, never the
// source path. Deleting a token from the composer drops the attachment.

const (
	// maxPromptAttachments bounds how many images one prompt can carry. It is a
	// sanity cap, not a provider limit; token scans and paste conversion both
	// honor it.
	maxPromptAttachments = 16

	// Provider-bound images are downscaled per the portable image contract in
	// the images package.
	uploadMaxLongEdge = images.UploadMaxLongEdge
	uploadMaxBytes    = images.UploadMaxBytes
)

type composerAttachment struct {
	Path      string
	Label     string
	Reference string
	Artifact  *artifacts.Ref
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

// rememberArtifactAttachments rebuilds the session-stable token registry from
// durable message references. New token numbers therefore never collide after
// a restart, and an old token can attach the exact immutable bytes again.
func (m *replModel) rememberArtifactAttachments(msg messages.ChatMessage) {
	if m.attachments == nil {
		m.attachments = make(map[int]composerAttachment)
	}
	if m.ambiguousAttachments == nil {
		m.ambiguousAttachments = make(map[int]bool)
	}
	for _, part := range msg.Parts {
		var imageRef *artifacts.Ref
		if part.Artifact != nil && part.Artifact.Kind == artifacts.KindImage {
			ref := *part.Artifact
			imageRef = &ref
		} else if part.Type == "image_base64" && part.ImageData != "" && m.artifactStore != nil {
			data, err := base64.StdEncoding.DecodeString(part.ImageData)
			if err == nil && len(data) > 0 {
				ref, putErr := m.artifactStore.Put(context.Background(), artifacts.Blob{
					Kind: artifacts.KindImage, MIMEType: part.MimeType, Name: part.FileName,
					ImageToken: part.Reference, Reference: part.Reference, Data: data,
				})
				if putErr == nil {
					imageRef = &ref
				}
			}
		}
		if imageRef == nil {
			continue
		}
		reference := strings.TrimSpace(part.Reference)
		if reference == "" {
			reference = strings.TrimSpace(imageRef.ImageToken)
		}
		if reference == "" {
			reference = strings.TrimSpace(imageRef.Reference)
		}
		match := attachmentTokenPattern.FindStringSubmatch(reference)
		if len(match) != 2 || match[0] != reference {
			continue
		}
		n, err := strconv.Atoi(match[1])
		if err != nil || n <= 0 {
			continue
		}
		if n > m.attachmentSeq {
			m.attachmentSeq = n
		}
		if m.ambiguousAttachments[n] {
			continue
		}
		ref := *imageRef
		ref.ImageToken = reference
		if previous, exists := m.attachments[n]; exists && previous.Artifact != nil && previous.Artifact.ID != ref.ID {
			// A legacy collision is ambiguous. Keep the number retired, but do
			// not silently point it at either payload.
			delete(m.attachments, n)
			m.ambiguousAttachments[n] = true
			continue
		}
		m.attachments[n] = composerAttachment{Label: ref.Name, Reference: reference, Artifact: &ref}
	}
}

func availableImageArtifact(store artifacts.Store, ref *artifacts.Ref) bool {
	if store == nil || ref == nil || ref.Kind != artifacts.KindImage || !artifacts.ValidID(ref.ID) || ref.Bytes < 0 || ref.Bytes > int64(maxLocalImageBytes) {
		return false
	}
	r, err := store.Open(context.Background(), ref.ID)
	if err != nil {
		return false
	}
	n, readErr := io.Copy(io.Discard, io.LimitReader(r, ref.Bytes+1))
	closeErr := r.Close()
	return readErr == nil && closeErr == nil && n == ref.Bytes
}

// promptAttachments resolves everything a prompt references, in appearance
// order, deduplicated by path: "[image #N]" tokens through the session
// registry, and bare typed paths — any whitespace-delimited word with an image
// extension that resolves to an existing local file. A token that was never
// registered in this session is an error: silently sending its placeholder as
// text would drop an attachment the user explicitly asked to include. Caller
// must hold m.mu.
func (m *replModel) promptAttachments(prompt string) ([]composerAttachment, error) {
	type ref struct {
		pos int
		att composerAttachment
	}
	var refs []ref

	var tokenSpans [][2]int
	if strings.Contains(prompt, "[image #") {
		for _, loc := range attachmentTokenPattern.FindAllStringSubmatchIndex(prompt, -1) {
			n, err := strconv.Atoi(prompt[loc[2]:loc[3]])
			if err != nil {
				return nil, fmt.Errorf("invalid attachment token %s", prompt[loc[0]:loc[1]])
			}
			att, ok := m.attachments[n]
			if !ok {
				if m.ambiguousAttachments[n] {
					return nil, fmt.Errorf("attachment token %s is ambiguous in this session", prompt[loc[0]:loc[1]])
				}
				return nil, fmt.Errorf("unknown attachment token %s", prompt[loc[0]:loc[1]])
			}
			tokenSpans = append(tokenSpans, [2]int{loc[0], loc[1]})
			att.Reference = prompt[loc[0]:loc[1]]
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
		return nil, nil
	}
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].pos < refs[j].pos })
	var out []composerAttachment
	seen := make(map[string]struct{}, len(refs))
	for _, r := range refs {
		identity := r.att.Path
		if r.att.Artifact != nil {
			identity = r.att.Artifact.ID
		}
		if _, dup := seen[identity]; dup {
			continue
		}
		seen[identity] = struct{}{}
		out = append(out, r.att)
	}
	if len(out) > maxPromptAttachments {
		return nil, fmt.Errorf("prompt has %d unique image attachments; maximum is %d", len(out), maxPromptAttachments)
	}
	return out, nil
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

// attachmentCacheDir holds clipboard captures and exact-byte prepared-message
// previews. Files must outlive the turn that created them (scrollback
// thumbnails read them on every draw) but not the machine; stale cache entries
// are swept on REPL start.
func attachmentCacheDir() (string, error) {
	base := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME"))
	if base == "" {
		var err error
		base, err = os.UserCacheDir()
		if err != nil {
			return "", err
		}
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
// content part. Eligible PNG, JPEG, and WebP images ship byte-for-byte. An
// animated GIF is reduced to its first frame, and GIF/BMP rasters are normalized
// to PNG so the durable message can be replayed through every native provider.
// They remain PNG after any size-driven downscaling; other oversized formats
// may be re-encoded as JPEG to meet the upload byte cap.
func prepareImageForUpload(path string) (*messages.ContentPart, error) {
	data, err := readBoundedRegularFile(path, maxLocalImageBytes)
	if err != nil {
		return nil, err
	}
	return prepareImageBytesForUpload(data, filepath.Base(path))
}

// prepareImageBytesForUpload applies the portable image contract at every
// ingestion boundary, before bytes enter durable history. fileName is display
// metadata only; the format is always detected from the bytes.
func prepareImageBytesForUpload(data []byte, fileName string) (*messages.ContentPart, error) {
	norm, err := images.NormalizeForModel(data, fileName)
	if err != nil {
		return nil, err
	}
	return &messages.ContentPart{
		Type:      "image_base64",
		ImageData: base64.StdEncoding.EncodeToString(norm.Data),
		MimeType:  norm.MIMEType,
		FileName:  norm.FileName,
	}, nil
}

func jpegEXIFOrientation(data []byte) int {
	return images.JPEGOrientation(data)
}

func applyEXIFOrientation(src image.Image, orientation int) image.Image {
	return images.ApplyEXIFOrientation(src, orientation)
}

// preparedMessageTranscriptImages materializes the exact portable image bytes
// accepted into a durable message. Transcript thumbnails then remain faithful
// if the original path is changed or removed before the turn is rendered.
func preparedMessageTranscriptImages(msg messages.ChatMessage) []transcriptImage {
	return preparedMessageTranscriptImagesWithStore(msg, nil)
}

func preparedMessageTranscriptImagesWithStore(msg messages.ChatMessage, store artifacts.Store) []transcriptImage {
	if store != nil {
		msg = cloneChatMessage(msg)
		for i, part := range msg.Parts {
			if part.Artifact == nil || part.Artifact.Kind != artifacts.KindImage {
				continue
			}
			if part.Artifact.Bytes < 0 || part.Artifact.Bytes > int64(maxLocalImageBytes) {
				continue
			}
			r, err := store.Open(context.Background(), part.Artifact.ID)
			if err != nil {
				continue
			}
			data, readErr := io.ReadAll(io.LimitReader(r, part.Artifact.Bytes+1))
			_ = r.Close()
			if readErr != nil || int64(len(data)) != part.Artifact.Bytes {
				continue
			}
			msg.Parts[i] = messages.ContentPart{
				Type: "image_base64", ImageData: base64.StdEncoding.EncodeToString(data), MimeType: part.Artifact.MIMEType, FileName: part.Artifact.Name, Reference: part.Artifact.ImageToken,
			}
		}
	}
	dir, err := attachmentCacheDir()
	if err != nil {
		return nil
	}
	return preparedMessageTranscriptImagesInDir(msg, dir)
}

func preparedMessageTranscriptImagesInDir(msg messages.ChatMessage, dir string) []transcriptImage {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil
	}
	images := make([]transcriptImage, 0, len(msg.Parts))
	for _, part := range msg.Parts {
		if part.Type != "image_base64" {
			continue
		}
		ext, ok := portableImageExtension(part.MimeType)
		if !ok {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(part.ImageData)
		if err != nil || len(data) == 0 {
			continue
		}
		digest := sha256.Sum256(data)
		path := filepath.Join(dir, fmt.Sprintf("prepared-%x%s", digest, ext))
		if existing, err := os.ReadFile(path); err != nil || !bytes.Equal(existing, data) {
			if err := os.WriteFile(path, data, 0o600); err != nil {
				continue
			}
		}
		label := strings.TrimSpace(part.FileName)
		if label == "" {
			label = "attachment" + ext
		}
		img, ok := resolveLocalTranscriptImage(path, label, dir)
		if !ok {
			continue
		}
		img.DisplayPath = label
		images = append(images, img)
	}
	return images
}

func portableImageExtension(mimeType string) (string, bool) {
	if idx := strings.IndexByte(mimeType, ';'); idx >= 0 {
		mimeType = mimeType[:idx]
	}
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/png":
		return ".png", true
	case "image/jpeg", "image/jpg":
		return ".jpg", true
	case "image/webp":
		return ".webp", true
	default:
		return "", false
	}
}

// buildREPLUserMessage assembles the durable user message for a managed-REPL
// turn. Text-only prompts stay simple Content strings — the shape restored drafts and
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
		if att.Artifact != nil {
			ref := *att.Artifact
			if att.Reference != "" {
				ref.ImageToken = att.Reference
			}
			parts = append(parts, messages.ContentPart{
				Type: "image_artifact", MimeType: ref.MIMEType, FileName: ref.Name, Reference: ref.ImageToken, Artifact: &ref,
			})
			continue
		}
		part, err := prepareImageForUpload(att.Path)
		if err != nil {
			return messages.ChatMessage{}, err
		}
		part.Reference = att.Reference
		parts = append(parts, *part)
	}
	return messages.ChatMessage{Role: messages.MessageRoleUser, Parts: parts}, nil
}
