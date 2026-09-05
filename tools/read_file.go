package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/alexschlessinger/pollytool/schema"
)

// binarySniffBytes bounds the probe used to distinguish text from binary
// content: a NUL byte in the probe classifies the file as binary, matching
// the common tooling heuristic.
const binarySniffBytes = 8 << 10

// readFileTool pages a local text file into model context with the same
// bounded-read UX as read_artifact: numbered lines, literal search, and raw
// byte windows with exact continuation offsets. File access honors the
// registry's base sandbox policy so the tool cannot see what a sandboxed
// command could not.
type readFileTool struct {
	NativeTool
	registry *ToolRegistry
}

// NewReadFileTool creates the read_file tool bound to registry's sandbox
// policy.
func NewReadFileTool(registry *ToolRegistry) Tool {
	return &readFileTool{registry: registry}
}

func (t *readFileTool) GetName() string { return "read_file" }

func (t *readFileTool) GetSchema() *schema.ToolSchema {
	description := "Read a bounded section of a local text file as numbered lines, search it literally, or page raw bytes."
	// search_files is absent without zg; steering toward it then would only
	// invite calls to a tool the model cannot see (mirrors bash).
	if t.registry.hasVisibleTool("search_files") {
		description += " For discovery, prefer search_files before broad file reads; read only the sections its snippets do not already answer."
	}
	description += " Truncated output reports the exact continuation offset. Use view_image for images."
	return schema.Tool(
		"read_file",
		description,
		schema.Params{
			"path":        schema.S("Filesystem path of the file to read"),
			"offset":      schema.Int("1-based starting line (default 1)"),
			"limit":       schema.Int("Maximum lines or matches (default 200, maximum 500)"),
			"query":       schema.S("Optional case-sensitive literal search"),
			"byte_offset": schema.Int("0-based byte position; returns a raw byte window instead of numbered lines (for files with very long lines)"),
		},
		"path",
	)
}

func (t *readFileTool) Execute(ctx context.Context, raw map[string]any) (string, error) {
	args := Args(raw)
	path := strings.TrimSpace(args.String("path"))
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	abs, err := resolveLocalPath(path)
	if err != nil {
		return "", err
	}
	routes, resolved := localRoutes(abs)
	if err := checkReadPolicy(t.registry, routes...); err != nil {
		return "", err
	}
	f, info, err := openLocalRegular(resolved, os.O_RDONLY, 0)
	if err != nil {
		return "", describeOpenError("read", abs, err)
	}
	defer f.Close()

	probe := make([]byte, binarySniffBytes)
	n, err := io.ReadFull(f, probe)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", fmt.Errorf("read %s: %w", abs, err)
	}
	if bytes.IndexByte(probe[:n], 0) >= 0 {
		return fmt.Sprintf("%s looks like binary data (%d bytes); read_file only pages text. Use view_image for images.", abs, info.Size()), nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("read %s: %w", abs, err)
	}

	if _, hasByteOffset := raw["byte_offset"]; hasByteOffset {
		for _, key := range []string{"offset", "limit", "query"} {
			if _, conflict := raw[key]; conflict {
				return "", fmt.Errorf("byte_offset cannot be combined with offset, limit, or query")
			}
		}
		byteOffset := int64(args.Int("byte_offset", -1))
		if byteOffset < 0 {
			return "", fmt.Errorf("byte_offset must be at least 0")
		}
		if byteOffset >= info.Size() {
			return fmt.Sprintf("File has no content at or after byte %d.", byteOffset), nil
		}
		text, err := PageByteWindow(ctx, f, "file", abs, info.Size(), byteOffset)
		if errors.Is(err, ErrPageSizeMismatch) {
			return "", fmt.Errorf("%s changed while it was being read; retry", abs)
		}
		return text, err
	}

	offset := args.Int("offset", 1)
	if offset < 1 {
		return "", fmt.Errorf("offset must be at least 1")
	}
	limit := args.Int("limit", PageDefaultLines)
	if limit < 1 || limit > PageMaxLines {
		return "", fmt.Errorf("limit must be between 1 and %d", PageMaxLines)
	}
	return PageLines(ctx, f, "file", offset, limit, args.String("query"))
}
