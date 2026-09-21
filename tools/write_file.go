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

// BindExecutionContext rebuilds the tool from the bound registry's own
// factory, under the context's root and policy.
func (t *writeFileTool) BindExecutionContext(bound *ToolRegistry, _ ExecutionContext) (Tool, error) {
	return rebindNative(bound, t.GetName())
}

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
	out, err := t.ExecuteOutput(ctx, raw)
	return out.Text, err
}

// ExecuteOutput writes the file and reports the change as FileChanges in
// Data: a creation diffs as all added, an overwrite against the previous
// content. Text is what the model sees and does not include the diff.
func (t *writeFileTool) ExecuteOutput(ctx context.Context, raw map[string]any) (ToolOutput, error) {
	text, change, err := t.write(ctx, raw)
	if err != nil {
		return ToolOutput{}, err
	}
	return ToolOutput{Text: text, Data: change}, nil
}

func (t *writeFileTool) write(ctx context.Context, raw map[string]any) (string, FileChanges, error) {
	var none FileChanges
	if err := ctx.Err(); err != nil {
		return "", none, err
	}
	args := Args(raw)
	path := strings.TrimSpace(args.String("path"))
	if path == "" {
		return "", none, fmt.Errorf("path is required")
	}
	rawContent, ok := raw["content"]
	if !ok {
		return "", none, fmt.Errorf("content is required")
	}
	content, ok := rawContent.(string)
	if !ok {
		return "", none, fmt.Errorf("content must be a string")
	}
	abs, routes, resolved, err := resolveLocalRoutes(t.registry, path)
	if err != nil {
		return "", none, err
	}
	if err := checkWritePolicy(t.registry, routes...); err != nil {
		return "", none, err
	}
	localFileMu.Lock()
	defer localFileMu.Unlock()
	existing, err := os.Lstat(resolved)
	if err != nil && !os.IsNotExist(err) {
		return "", none, fmt.Errorf("write %s: %w", abs, err)
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return "", none, fmt.Errorf("write %s: %w", abs, err)
	}
	// The file is opened for reading as well so the previous content can be
	// diffed before rewriteFile truncates it through the same descriptor.
	f, info, err := openLocalRegular(resolved, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return "", none, describeOpenError("write", abs, err)
	}
	var old []byte
	oldTruncated := false
	if existing != nil {
		// The write still proceeds when the old content is too large or
		// unreadable; the change just loses its body.
		data, tooLarge, err := readBoundedRegular(f, info, changeMaxFileBytes)
		if err != nil || tooLarge {
			oldTruncated = true
		} else {
			old = data
		}
	}
	if err := rewriteFile(f, content); err != nil {
		_ = f.Close()
		return "", none, fmt.Errorf("write %s: %w", abs, err)
	}
	if err := f.Close(); err != nil {
		return "", none, fmt.Errorf("write %s: %w", abs, err)
	}
	root := t.registry.changeRoot()
	var change FileChange
	if oldTruncated {
		change = FileChange{Path: changePath(root, abs), Kind: ChangeModified, Truncated: true, CountsUnknown: true}
	} else {
		change = DiffFileChange(changePath(root, abs), string(old), content, existing != nil, true)
	}
	changes := FileChanges{Root: root, Tracked: true, Changes: []FileChange{change}}
	if existing != nil {
		return fmt.Sprintf("Overwrote %s (%d bytes, %d lines; was %d bytes).", abs, len(content), countLines(content), existing.Size()), changes, nil
	}
	return fmt.Sprintf("Created %s (%d bytes, %d lines).", abs, len(content), countLines(content)), changes, nil
}

func countLines(content string) int {
	lines := strings.Count(content, "\n")
	if content != "" && !strings.HasSuffix(content, "\n") {
		lines++
	}
	return lines
}
