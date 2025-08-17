package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// supportedImageTypes lists common image MIME types
var supportedImageTypes = map[string]bool{
	"image/jpeg":    true,
	"image/jpg":     true,
	"image/png":     true,
	"image/gif":     true,
	"image/webp":    true,
	"image/bmp":     true,
	"image/svg+xml": true,
}

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

	// Read file content
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read file %s: %w", path, err)
	}

	// Detect MIME type
	mimeType := detectMimeType(path, data)

	// Check if it's an image
	if isImageType(mimeType) {
		// Return as base64 encoded image
		return &messages.ContentPart{
			Type:      "image_base64",
			ImageData: base64.StdEncoding.EncodeToString(data),
			MimeType:  mimeType,
			FileName:  filepath.Base(path),
		}, nil
	}

	// Return as text content
	return &messages.ContentPart{
		Type:     "text",
		Text:     string(data),
		FileName: filepath.Base(path),
	}, nil
}

// detectMimeType detects the MIME type of a file
func detectMimeType(path string, data []byte) string {
	// First try to detect from content
	mimeType := http.DetectContentType(data)

	// If that fails or gives generic type, try from extension
	if mimeType == "application/octet-stream" || mimeType == "" {
		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".jpg", ".jpeg":
			return "image/jpeg"
		case ".png":
			return "image/png"
		case ".gif":
			return "image/gif"
		case ".webp":
			return "image/webp"
		case ".bmp":
			return "image/bmp"
		case ".svg":
			return "image/svg+xml"
		case ".txt", ".md", ".rst", ".log":
			return "text/plain"
		case ".json":
			return "application/json"
		case ".xml":
			return "application/xml"
		case ".html", ".htm":
			return "text/html"
		case ".css":
			return "text/css"
		case ".js", ".mjs", ".ts", ".tsx", ".jsx":
			return "text/javascript"
		case ".py":
			return "text/x-python"
		case ".go":
			return "text/x-go"
		case ".java":
			return "text/x-java"
		case ".c", ".h":
			return "text/x-c"
		case ".cpp", ".cc", ".cxx", ".hpp":
			return "text/x-c++"
		case ".rs":
			return "text/x-rust"
		case ".sh", ".bash":
			return "text/x-shellscript"
		case ".yaml", ".yml":
			return "text/yaml"
		}
	}

	return mimeType
}

// isImageType checks if a MIME type is a supported image type
func isImageType(mimeType string) bool {
	// Remove parameters from MIME type (e.g., "image/jpeg; charset=utf-8" -> "image/jpeg")
	if idx := strings.Index(mimeType, ";"); idx != -1 {
		mimeType = strings.TrimSpace(mimeType[:idx])
	}
	return supportedImageTypes[mimeType]
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

	// Read body
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response from %s: %w", urlStr, err)
	}

	// Get MIME type from Content-Type header
	mimeType := resp.Header.Get("Content-Type")
	if mimeType == "" {
		// Fallback to content detection
		mimeType = http.DetectContentType(data)
	}

	// Clean up MIME type (remove parameters)
	if idx := strings.Index(mimeType, ";"); idx != -1 {
		mimeType = strings.TrimSpace(mimeType[:idx])
	}

	// Extract filename from URL
	u, _ := url.Parse(urlStr)
	fileName := filepath.Base(u.Path)
	if fileName == "" || fileName == "/" || fileName == "." {
		fileName = "downloaded-file"
	}

	// Check if it's an image
	if isImageType(mimeType) {
		// Return as base64 encoded image
		return &messages.ContentPart{
			Type:      "image_base64",
			ImageData: base64.StdEncoding.EncodeToString(data),
			MimeType:  mimeType,
			FileName:  fileName,
		}, nil
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
