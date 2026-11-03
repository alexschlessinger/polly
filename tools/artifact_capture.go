package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/alexschlessinger/pollytool/artifacts"
)

// ReadArtifactFile captures the exact bytes of a regular file, resolving path
// against registry's execution root and enforcing its read policy. maxBytes
// bounds both the initial size and the bytes read if the file grows. It never
// silently truncates a capture. The caller owns persistence and publication.
func ReadArtifactFile(ctx context.Context, registry *ToolRegistry, path string, maxBytes int64) (blob artifacts.Blob, err error) {
	if err := ctx.Err(); err != nil {
		return blob, err
	}
	// readBoundedRegular probes one byte beyond the limit; leave room for it.
	if strings.TrimSpace(path) == "" || maxBytes < 0 || maxBytes == math.MaxInt64 {
		return blob, NewToolError("a file path and byte limit between 0 and MaxInt64-1 are required", "invalid_args")
	}
	abs, f, info, err := openLocalRead(registry, "capture", path)
	if err != nil {
		return blob, NewToolError(err.Error(), "file_capture_failed")
	}
	defer func() {
		err = errors.Join(err, f.Close())
		if err != nil {
			blob = artifacts.Blob{}
		}
	}()
	data, tooLarge, err := readBoundedRegular(f, info, maxBytes)
	if err != nil {
		return blob, fmt.Errorf("capture %s: %w", abs, err)
	}
	if tooLarge {
		return blob, NewToolError(fmt.Sprintf("capture %s exceeds the remaining limit of %d bytes", abs, maxBytes), "file_too_large")
	}
	if err := ctx.Err(); err != nil {
		return blob, err
	}
	mime := http.DetectContentType(data)
	kind := artifacts.KindBinary
	switch {
	case strings.HasPrefix(mime, "text/"):
		kind = artifacts.KindText
	case strings.HasPrefix(mime, "image/"):
		kind = artifacts.KindImage
	}
	return artifacts.Blob{Kind: kind, MIMEType: mime, Name: path, Data: data}, nil
}
