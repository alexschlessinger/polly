package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alexschlessinger/pollytool/schema"
)

// writeFileTool writes model-provided content to a local file, creating
// missing parent directories. Writes honor the registry's base sandbox write
// policy so the tool cannot change what a sandboxed command could not.
type writeFileTool struct {
	NativeTool
	registry *ToolRegistry
}

// NewWriteFileTool creates the write_file tool bound to registry's sandbox
// policy.
func NewWriteFileTool(registry *ToolRegistry) Tool {
	return &writeFileTool{registry: registry}
}

func (t *writeFileTool) GetName() string { return "write_file" }

func (t *writeFileTool) GetSchema() *schema.ToolSchema {
	return schema.Tool(
		"write_file",
		"Write content to a local file, creating it (and any missing parent directories) or replacing its contents entirely. For a partial change to an existing file use edit_file instead.",
		schema.Params{
			"path":    schema.S("Filesystem path of the file to write"),
			"content": schema.S("Complete content to write to the file"),
		},
		"path", "content",
	)
}

func (t *writeFileTool) Execute(ctx context.Context, raw map[string]any) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	args := Args(raw)
	path := strings.TrimSpace(args.String("path"))
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	rawContent, ok := raw["content"]
	if !ok {
		return "", fmt.Errorf("content is required")
	}
	content, ok := rawContent.(string)
	if !ok {
		return "", fmt.Errorf("content must be a string")
	}
	abs, err := resolveLocalPath(path)
	if err != nil {
		return "", err
	}
	routes, resolved := localRoutes(abs)
	if err := checkWritePolicy(t.registry, routes...); err != nil {
		return "", err
	}
	localFileMu.Lock()
	defer localFileMu.Unlock()
	existing, err := os.Lstat(resolved)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("write %s: %w", abs, err)
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return "", fmt.Errorf("write %s: %w", abs, err)
	}
	f, _, err := openLocalRegular(resolved, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", describeOpenError("write", abs, err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write %s: %w", abs, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("write %s: %w", abs, err)
	}
	if existing != nil {
		return fmt.Sprintf("Overwrote %s (%d bytes, %d lines; was %d bytes).", abs, len(content), countLines(content), existing.Size()), nil
	}
	return fmt.Sprintf("Created %s (%d bytes, %d lines).", abs, len(content), countLines(content)), nil
}

func countLines(content string) int {
	lines := strings.Count(content, "\n")
	if content != "" && !strings.HasSuffix(content, "\n") {
		lines++
	}
	return lines
}
