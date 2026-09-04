package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/alexschlessinger/pollytool/schema"
)

const (
	// editFileMaxBytes bounds how large a file edit_file will load; larger
	// files are better handled by wrapped commands.
	editFileMaxBytes = 16 << 20
	// editSnippetContext and editSnippetMaxLines bound the numbered
	// verification snippet returned after a successful edit.
	editSnippetContext  = 2
	editSnippetMaxLines = 20
)

// editFileTool performs an exact literal string replacement in a local text
// file. Reads honor the registry's base sandbox read policy and writes its
// write policy, so the tool cannot change what a sandboxed command could not.
type editFileTool struct {
	NativeTool
	registry *ToolRegistry
}

// NewEditFileTool creates the edit_file tool bound to registry's sandbox
// policy.
func NewEditFileTool(registry *ToolRegistry) Tool {
	return &editFileTool{registry: registry}
}

func (t *editFileTool) GetName() string { return "edit_file" }

func (t *editFileTool) GetSchema() *schema.ToolSchema {
	return schema.Tool(
		"edit_file",
		"Replace an exact literal string in a local text file. old_string must match the file byte-for-byte (indentation and line endings included) and occur exactly once unless replace_all is set; never include the \"N: \" line-number prefix that read_file displays. Use write_file to create or fully replace a file.",
		schema.Params{
			"path":        schema.S("Filesystem path of the file to edit"),
			"old_string":  schema.S("Exact literal text to replace"),
			"new_string":  schema.S("Replacement text (may be empty to delete old_string)"),
			"replace_all": schema.Bool("Replace every occurrence instead of requiring old_string to be unique (default false)"),
		},
		"path", "old_string", "new_string",
	)
}

func (t *editFileTool) Execute(ctx context.Context, raw map[string]any) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	args := Args(raw)
	path := strings.TrimSpace(args.String("path"))
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	oldString := args.String("old_string")
	if oldString == "" {
		return "", fmt.Errorf("old_string is required; use write_file to create a new file")
	}
	newString := args.String("new_string")
	if oldString == newString {
		return "", fmt.Errorf("old_string and new_string are identical")
	}
	abs, err := resolveLocalPath(path)
	if err != nil {
		return "", err
	}
	routes, resolved := localRoutes(abs)
	if err := checkReadPolicy(t.registry, routes...); err != nil {
		return "", err
	}
	if err := checkWritePolicy(t.registry, routes...); err != nil {
		return "", err
	}
	// The whole edit runs under one lock and through one descriptor: the
	// text read is exactly the text replaced, and a concurrent edit_file or
	// write_file of the same file waits instead of racing this one.
	localFileMu.Lock()
	defer localFileMu.Unlock()
	f, info, err := openLocalRegular(resolved, os.O_RDWR, 0)
	if err != nil {
		return "", describeOpenError("edit", abs, err)
	}
	defer f.Close()
	if info.Size() > editFileMaxBytes {
		return "", fmt.Errorf("%s is %d bytes; edit_file handles files up to %d bytes", abs, info.Size(), editFileMaxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, editFileMaxBytes+1))
	if err != nil {
		return "", fmt.Errorf("edit %s: %w", abs, err)
	}
	if int64(len(data)) > editFileMaxBytes {
		return "", fmt.Errorf("%s is larger than %d bytes; edit_file handles files up to %d bytes", abs, editFileMaxBytes, editFileMaxBytes)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return "", fmt.Errorf("%s looks like binary data; edit_file only edits text", abs)
	}
	content := string(data)
	count := strings.Count(content, oldString)
	switch {
	case count == 0:
		return "", fmt.Errorf("old_string was not found in %s. Matching is exact, including whitespace and line endings; re-read the file with read_file and copy the text precisely, without the \"N: \" line-number prefix", abs)
	case count > 1 && !args.Bool("replace_all"):
		return "", fmt.Errorf("old_string occurs %d times in %s; provide a longer string that is unique, or set replace_all to replace every occurrence", count, abs)
	}
	replacements := 1
	updated := strings.Replace(content, oldString, newString, 1)
	if args.Bool("replace_all") {
		replacements = count
		updated = strings.ReplaceAll(content, oldString, newString)
	}
	if err := rewriteFile(f, updated); err != nil {
		return "", fmt.Errorf("edit %s: %w", abs, err)
	}

	result := fmt.Sprintf("Edited %s: %d replacement(s).", abs, replacements)
	if snippet, err := editSnippet(ctx, updated, strings.Index(content, oldString), newString); err == nil && snippet != "" {
		result += "\n" + snippet
	}
	return result, nil
}

// rewriteFile replaces the contents of the open file with content through
// the same descriptor the content was read from.
func rewriteFile(f *os.File, content string) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		return err
	}
	return f.Sync()
}

// editSnippet renders numbered lines around the first replacement so the
// model can verify the edit without a follow-up read. firstIdx is the byte
// index of the first replacement, which the unchanged prefix keeps identical
// between the old and new content.
func editSnippet(ctx context.Context, updated string, firstIdx int, newString string) (string, error) {
	if firstIdx < 0 {
		return "", nil
	}
	startLine := 1 + strings.Count(updated[:firstIdx], "\n")
	offset := max(1, startLine-editSnippetContext)
	limit := min(startLine-offset+strings.Count(newString, "\n")+1+editSnippetContext, editSnippetMaxLines)
	return PageLines(ctx, strings.NewReader(updated), "file", offset, limit, "")
}
