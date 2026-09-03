package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/images"
	"github.com/alexschlessinger/pollytool/messages"
)

// readFile reads a file and returns its content as base64 if it's an image
func readFile(path string) (*messages.ContentPart, error) {
	// Check if file exists
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("cannot access file %s: %w", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory", path)
	}

	// Bound local reads just like URL downloads. The helper checks both the
	// opened file's size and the bytes read, so a large or concurrently growing
	// file cannot be buffered without limit.
	data, err := images.ReadBoundedFile(path, maxLocalImageBytes)
	if err != nil {
		return nil, fmt.Errorf("cannot read file %s: %w", path, err)
	}

	if looksLikeImageInput(path, http.DetectContentType(data)) {
		return prepareImageBytesForUpload(data, filepath.Base(path))
	}

	// Return as text content
	return &messages.ContentPart{
		Type:     "text",
		Text:     string(data),
		FileName: filepath.Base(path),
	}, nil
}

// looksLikeImageInput routes anything identified as an image through the
// portable raster validator. Common image extensions are also recognized when
// a server or local MIME detector labels the bytes generically. Unsupported
// formats such as SVG then fail explicitly instead of entering durable history
// as either base64 image data or misleading text.
func looksLikeImageInput(fileName, mimeType string) bool {
	if idx := strings.IndexByte(mimeType, ';'); idx >= 0 {
		mimeType = mimeType[:idx]
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(mimeType)), "image/") {
		return true
	}
	switch strings.ToLower(filepath.Ext(fileName)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp",
		".svg", ".tif", ".tiff", ".heic", ".heif", ".avif", ".ico":
		return true
	default:
		return false
	}
}

// isURL checks if a string is a valid HTTP/HTTPS URL
func isURL(str string) bool {
	u, err := url.Parse(str)
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// fetchURL fetches content from a URL and returns it as a ContentPart
func fetchURL(urlStr string) (*messages.ContentPart, error) {
	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// Make request
	resp, err := client.Get(urlStr)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch URL %s: %w", urlStr, err)
	}
	defer resp.Body.Close()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch URL %s: HTTP %d %s", urlStr, resp.StatusCode, resp.Status)
	}

	if resp.ContentLength > maxLocalImageBytes {
		return nil, fmt.Errorf("failed to fetch URL %s: response exceeds the %d MiB download limit", urlStr, maxLocalImageBytes>>20)
	}

	// Bound even chunked or dishonest responses before buffering them. The
	// extra byte distinguishes an exactly-full response from an oversized one.
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxLocalImageBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read response from %s: %w", urlStr, err)
	}
	if len(data) > maxLocalImageBytes {
		return nil, fmt.Errorf("failed to fetch URL %s: response exceeds the %d MiB download limit", urlStr, maxLocalImageBytes>>20)
	}

	// Get MIME type from Content-Type header
	mimeType := resp.Header.Get("Content-Type")
	if idx := strings.Index(mimeType, ";"); idx != -1 {
		mimeType = strings.TrimSpace(mimeType[:idx])
	}
	// A generic or non-image response type is not authoritative for image
	// ingestion. Sniff the bytes so extensionless CDN/download URLs still enter
	// the portable raster preparation path.
	if mimeType == "" || !strings.HasPrefix(strings.ToLower(mimeType), "image/") {
		mimeType = http.DetectContentType(data)
	}

	// Extract filename from URL
	u, _ := url.Parse(urlStr)
	fileName := filepath.Base(u.Path)
	if fileName == "" || fileName == "/" || fileName == "." {
		fileName = "downloaded-file"
	}

	if looksLikeImageInput(fileName, mimeType) {
		return prepareImageBytesForUpload(data, fileName)
	}

	// Return as text content
	return &messages.ContentPart{
		Type:     "text",
		Text:     string(data),
		FileName: fileName,
	}, nil
}

// processFiles reads all specified files and returns content parts
func processFiles(paths []string) ([]messages.ContentPart, error) {
	var parts []messages.ContentPart

	for _, path := range paths {
		var part *messages.ContentPart
		var err error

		// Check if path is a URL
		if isURL(path) {
			// Fetch from URL
			part, err = fetchURL(path)
			if err != nil {
				return nil, fmt.Errorf("error fetching URL %s: %w", path, err)
			}
		} else {
			// Handle local file
			// Expand home directory if needed
			if strings.HasPrefix(path, "~/") {
				home, err := os.UserHomeDir()
				if err == nil {
					path = filepath.Join(home, path[2:])
				}
			}

			// Read the file
			part, err = readFile(path)
			if err != nil {
				return nil, fmt.Errorf("error reading file %s: %w", path, err)
			}
		}

		parts = append(parts, *part)
	}

	return parts, nil
}

// buildMessageWithFiles creates a message with text and file content
func buildMessageWithFiles(prompt string, files []string) (messages.ChatMessage, error) {
	msg := messages.ChatMessage{
		Role: messages.MessageRoleUser,
	}

	// Process files if any
	if len(files) > 0 {
		parts, err := processFiles(files)
		if err != nil {
			return msg, err
		}

		// Add text prompt as first part if present
		if prompt != "" {
			msg.Parts = append(msg.Parts, messages.ContentPart{
				Type: "text",
				Text: prompt,
			})
		}

		// Add file parts
		msg.Parts = append(msg.Parts, parts...)
	} else {
		// Simple text message
		msg.Content = prompt
	}

	return msg, nil
}
